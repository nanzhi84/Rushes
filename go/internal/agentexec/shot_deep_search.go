package agentexec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/media"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
	"github.com/nanzhi84/Rushes/go/internal/understanding"
)

const (
	maximumDeepSearchCandidates  = 8
	maximumDeepQueryCacheEntries = 256
)

type storedDeepShotFacts struct {
	ShotID           string                            `json:"shot_id"`
	SourceStartFrame int                               `json:"source_start_frame"`
	SourceEndFrame   int                               `json:"source_end_frame"`
	BoundaryVersion  int                               `json:"boundary_version"`
	Facets           []string                          `json:"facets"`
	Frames           []understanding.DeepFrameManifest `json:"frames"`
	Observations     []understanding.DeepObservation   `json:"observations"`
}

type deepResolvedCandidate struct {
	ref      rushestools.ShotRefInput
	asset    storage.Asset
	shot     storage.IndexedShot
	allFacts storedDeepShotFacts
}

type shotDeepQueryCache struct {
	mutex  sync.Mutex
	values map[string]rushestools.ShotDeepSearchResult
	order  []string
}

func newShotDeepQueryCache() *shotDeepQueryCache {
	return &shotDeepQueryCache{values: map[string]rushestools.ShotDeepSearchResult{}}
}

func (cache *shotDeepQueryCache) get(key string) (rushestools.ShotDeepSearchResult, bool) {
	if cache == nil {
		return rushestools.ShotDeepSearchResult{}, false
	}
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	value, exists := cache.values[key]
	if !exists {
		return rushestools.ShotDeepSearchResult{}, false
	}
	encoded, _ := json.Marshal(value)
	var cloned rushestools.ShotDeepSearchResult
	_ = json.Unmarshal(encoded, &cloned)
	return cloned, true
}

func (cache *shotDeepQueryCache) put(key string, value rushestools.ShotDeepSearchResult) {
	if cache == nil {
		return
	}
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	encoded, _ := json.Marshal(value)
	var cloned rushestools.ShotDeepSearchResult
	_ = json.Unmarshal(encoded, &cloned)
	if _, exists := cache.values[key]; !exists {
		cache.order = append(cache.order, key)
	}
	cache.values[key] = cloned
	for len(cache.order) > maximumDeepQueryCacheEntries {
		oldest := cache.order[0]
		cache.order = cache.order[1:]
		delete(cache.values, oldest)
	}
}

func (exec *Executor) toolDeepSearchShots(
	ctx context.Context,
	draftID string,
	input rushestools.ShotDeepSearchInput,
) (rushestools.ShotDeepSearchResult, error) {
	startedAt := time.Now()
	input.Query = strings.TrimSpace(input.Query)
	input.IndexSnapshotID = strings.TrimSpace(input.IndexSnapshotID)
	if input.Query == "" || input.IndexSnapshotID == "" || len(input.CandidateShots) < 1 ||
		len(input.CandidateShots) > maximumDeepSearchCandidates || input.ReturnTopK < 0 ||
		input.ReturnTopK > len(input.CandidateShots) || len([]rune(input.Query)) > 600 {
		return deepSearchFailure(
			input, rushestools.ErrCodeShotDeepInputInvalid,
			"shot.deep_search 需要 query、冻结 index_snapshot_id 和 1 到 8 个精确 candidate_shots；return_top_k 不能超过候选数。",
			"先调用 shot.search，再原样选择其 asset_id + shot_id；不要传源帧范围。", input.CandidateShots,
		), nil
	}
	criteria := []*[]string{&input.Requirements, &input.Exclusions, &input.Preferences}
	for _, values := range criteria {
		normalized, normalizeErr := normalizeDeepCriteriaInput(*values)
		if normalizeErr != nil {
			return deepSearchFailure(
				input, rushestools.ErrCodeShotDeepInputInvalid,
				"requirements、exclusions、preferences 每组最多 12 条；每条须非空、唯一且不超过 240 个字符。",
				"精简或去重条件后可直接重试。", nil,
			), nil
		}
		*values = normalized
	}
	if !exec.analyzer.HasVision() {
		return deepSearchFailure(
			input, rushestools.ErrCodeShotDeepVisionMissing,
			"当前环境未配置可用的视觉模型，无法执行新增帧深搜。",
			"配置视觉模型后使用相同 ShotRef 和快照直接重试。", nil,
		), nil
	}

	resolved, failure, err := exec.resolveDeepShotCandidates(ctx, draftID, input)
	if err != nil {
		return rushestools.ShotDeepSearchResult{}, err
	}
	if failure != nil {
		return *failure, nil
	}
	cacheKey := deepSearchQueryCacheKey(draftID, input)
	if cached, exists := exec.deepSearchCache.get(cacheKey); exists {
		cached.CacheHit = true
		cached.NewFrameCount = 0
		cached.ReusedFrameCount = 0
		for candidateIndex := range cached.Candidates {
			for frameIndex := range cached.Candidates[candidateIndex].FrameEvidence {
				cached.Candidates[candidateIndex].FrameEvidence[frameIndex].NewlyAdded = false
				cached.ReusedFrameCount++
			}
		}
		return cached, nil
	}
	intent := strings.Join(append(append(append(
		[]string{input.Query}, input.Requirements...), input.Exclusions...), input.Preferences...), " ")
	requiredFacets := understanding.DeepFacetsForIntent(intent)
	results := make([]rushestools.ShotDeepCandidate, 0, len(resolved))
	newFrameCount, reusedFrameCount := 0, 0
	for index := range resolved {
		candidate := &resolved[index]
		exec.recordProgress(draftID, map[string]any{
			"type": contracts.TurnStreamSubagentProgress, "tool": "shot.deep_search", "stage": "deep_frame_plan",
			"note":      fmt.Sprintf("规划深搜新增帧 %d/%d", index+1, len(resolved)),
			"completed": index, "total": len(resolved),
			"asset_id": candidate.asset.ID, "shot_id": candidate.shot.ShotID,
			"elapsed_ms": time.Since(startedAt).Milliseconds(),
		})
		missingFacets := missingDeepFacets(requiredFacets, candidate.allFacts.Facets)
		requestFacets := requiredFacets
		if len(missingFacets) > 0 {
			requestFacets = missingFacets
		}
		baseFrames := indexedShotFrameNumbers(candidate.shot)
		request := understanding.DeepShotAnalysisRequest{
			ShotID:           candidate.shot.ShotID,
			SourceStartFrame: candidate.shot.SourceStartFrame,
			SourceEndFrame:   candidate.shot.SourceEndFrame,
			BoundaryVersion:  candidate.shot.BoundaryVersion,
			Facets:           requestFacets, BaseFrameNumbers: baseFrames,
			ReusableFrames: candidate.allFacts.Frames,
			Query:          input.Query, Requirements: input.Requirements,
			Exclusions: input.Exclusions, Preferences: input.Preferences,
			RequireNewFrames: len(missingFacets) > 0,
		}
		source, kind, err := media.ResolveAssetSource(ctx, exec.database, candidate.asset.ID)
		if err != nil {
			return rushestools.ShotDeepSearchResult{}, err
		}
		if kind != "video" {
			return deepSearchFailure(
				input, rushestools.ErrCodeShotRefAssetMismatch,
				"candidate_shots 引用了非视频素材。",
				"从同一 shot.search 结果选择视频 ShotRef 后重试。", []rushestools.ShotRefInput{candidate.ref},
			), nil
		}

		exec.recordProgress(draftID, map[string]any{
			"type": contracts.TurnStreamSubagentProgress, "tool": "shot.deep_search", "stage": "deep_frame_extract",
			"note":      fmt.Sprintf("抽取并复核新增帧 %d/%d", index+1, len(resolved)),
			"completed": index, "total": len(resolved),
			"asset_id": candidate.asset.ID, "shot_id": candidate.shot.ShotID,
			"facets": requestFacets, "cache_hit": len(missingFacets) == 0,
			"elapsed_ms": time.Since(startedAt).Milliseconds(),
		})
		analysis, err := exec.inspectAndPersistDeepShot(ctx, source, *candidate, request, missingFacets)
		if err != nil {
			slog.Warn("镜头深搜失败", "asset_id", candidate.asset.ID, "shot_id", candidate.shot.ShotID,
				"duration_ms", time.Since(startedAt).Milliseconds(), "error", err)
			return deepSearchFailure(
				input, rushestools.ErrCodeShotDeepAnalysisFailed,
				"新增帧抽取、视觉复核或通用事实持久化失败。",
				"检查素材可解码性与视觉 provider 后，可用相同快照和 ShotRef 直接重试。",
				[]rushestools.ShotRefInput{candidate.ref},
			), nil
		}
		deepCandidate := buildDeepCandidate(input.IndexSnapshotID, *candidate, analysis)
		for _, frame := range deepCandidate.FrameEvidence {
			if frame.NewlyAdded {
				newFrameCount++
			} else {
				reusedFrameCount++
			}
		}
		results = append(results, deepCandidate)
		exec.recordProgress(draftID, map[string]any{
			"type": contracts.TurnStreamSubagentProgress, "tool": "shot.deep_search", "stage": "deep_persist",
			"note":      fmt.Sprintf("深搜通用事实 %d/%d 已就绪", index+1, len(resolved)),
			"completed": index + 1, "total": len(resolved),
			"asset_id": candidate.asset.ID, "shot_id": candidate.shot.ShotID,
			"new_frame_count": countNewDeepFrames(analysis.Frames),
			"elapsed_ms":      time.Since(startedAt).Milliseconds(),
		})
	}
	sortDeepCandidates(results)
	total := len(results)
	limit := input.ReturnTopK
	if limit == 0 {
		limit = total
	}
	results = append([]rushestools.ShotDeepCandidate(nil), results[:min(limit, total)]...)
	output := rushestools.ShotDeepSearchResult{
		Status: string(rushestools.StatusSucceeded), Query: input.Query,
		IndexSnapshotID: input.IndexSnapshotID,
		AnalyzerVersion: understanding.DeepShotAnalyzerVersion,
		Candidates:      results, TotalCandidates: total, ReturnedCandidates: len(results),
		NewFrameCount: newFrameCount, ReusedFrameCount: reusedFrameCount,
		CacheHit: newFrameCount == 0,
	}
	exec.deepSearchCache.put(cacheKey, output)
	slog.Info("镜头深搜完成", "analysis_type", understanding.DeepShotAnalysisType,
		"candidate_count", total, "new_frame_count", newFrameCount,
		"reused_frame_count", reusedFrameCount,
		"duration_ms", time.Since(startedAt).Milliseconds(), "status", "succeeded")
	return output, nil
}

func normalizeDeepCriteriaInput(values []string) ([]string, error) {
	if len(values) > 12 {
		return nil, errors.New("deep criteria 超过 12 条")
	}
	result := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || len([]rune(value)) > 240 {
			return nil, errors.New("deep criterion 为空或过长")
		}
		if _, duplicate := seen[value]; duplicate {
			return nil, errors.New("deep criterion 重复")
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result, nil
}

func (exec *Executor) resolveDeepShotCandidates(
	ctx context.Context,
	draftID string,
	input rushestools.ShotDeepSearchInput,
) ([]deepResolvedCandidate, *rushestools.ShotDeepSearchResult, error) {
	snapshot, err := parseShotSearchSnapshot(input.IndexSnapshotID)
	if err != nil || snapshot.DraftID != draftID {
		failure := deepSearchFailure(
			input, rushestools.ErrCodeShotIndexSnapshotStale,
			"index_snapshot_id 无法验证、已损坏或不属于当前草稿。",
			"重新调用 shot.search，并使用新返回的 index_snapshot_id 与 ShotRef。", input.CandidateShots,
		)
		return nil, &failure, nil
	}
	assets, err := storage.ListDraftAssets(ctx, exec.database.Read(), draftID)
	if err != nil {
		return nil, nil, err
	}
	visible := map[string]storage.Asset{}
	for _, asset := range assets {
		visible[asset.ID] = asset
	}
	snapshotAssets := map[string]shotSearchSnapshotAsset{}
	for _, asset := range snapshot.Assets {
		snapshotAssets[asset.AssetID] = asset
	}
	rowsBySnapshot := map[string][]storage.IndexedShot{}
	for _, frozen := range snapshot.Assets {
		asset, assetVisible := visible[frozen.AssetID]
		if !assetVisible || asset.Hash != frozen.ContentHash || asset.Kind != "video" ||
			asset.IngestStatus != "ready" || !asset.Usable {
			failure := deepSearchFailure(input, rushestools.ErrCodeShotIndexSnapshotStale,
				"冻结素材集合、内容或可用状态已经变化。",
				"重新调用 shot.search 获取当前冻结快照后重试。", nil)
			return nil, &failure, nil
		}
		ready, readyErr := storage.ReadyShotIndexByContentHash(ctx, exec.database.Read(), asset.Hash)
		if readyErr != nil || ready.ID != frozen.BaseSnapshotID || ready.Generation != frozen.Generation {
			if readyErr != nil && !errors.Is(readyErr, storage.ErrNotFound) {
				return nil, nil, readyErr
			}
			failure := deepSearchFailure(input, rushestools.ErrCodeShotIndexSnapshotStale,
				"基础镜头索引代已变化，旧 ShotRef 的权威边界不可静默复用。",
				"重新调用 shot.search 获取新 index_snapshot_id 与候选。", nil)
			return nil, &failure, nil
		}
		if _, loaded := rowsBySnapshot[ready.ID]; !loaded {
			rows, listErr := storage.ListShotIndexShots(ctx, exec.database.Read(), ready.ID)
			if listErr != nil {
				return nil, nil, listErr
			}
			rowsBySnapshot[ready.ID] = rows
		}
	}
	seenRefs := map[string]struct{}{}
	resolved := make([]deepResolvedCandidate, 0, len(input.CandidateShots))
	for _, rawRef := range input.CandidateShots {
		ref := rushestools.ShotRefInput{
			AssetID: strings.TrimSpace(rawRef.AssetID), ShotID: strings.TrimSpace(rawRef.ShotID),
		}
		identity := ref.AssetID + "\x00" + ref.ShotID
		if ref.AssetID == "" || ref.ShotID == "" {
			failure := deepSearchFailure(input, rushestools.ErrCodeShotDeepInputInvalid,
				"candidate_shots 的 asset_id 和 shot_id 均不能为空。",
				"从 shot.search 结果原样复制精确 ShotRef 后重试。", []rushestools.ShotRefInput{rawRef})
			return nil, &failure, nil
		}
		if _, duplicate := seenRefs[identity]; duplicate {
			failure := deepSearchFailure(input, rushestools.ErrCodeShotDeepInputInvalid,
				"candidate_shots 包含重复 ShotRef。", "移除重复项后直接重试。", []rushestools.ShotRefInput{ref})
			return nil, &failure, nil
		}
		seenRefs[identity] = struct{}{}
		frozen, exists := snapshotAssets[ref.AssetID]
		asset, assetVisible := visible[ref.AssetID]
		if !exists || !assetVisible {
			failure := deepSearchFailure(input, rushestools.ErrCodeShotRefAssetMismatch,
				"ShotRef 的素材不属于冻结快照或当前草稿不可见。",
				"重新调用 shot.search 并选择当前草稿可见候选。", []rushestools.ShotRefInput{ref})
			return nil, &failure, nil
		}
		rows := rowsBySnapshot[frozen.BaseSnapshotID]
		shot, found := findIndexedShot(rows, ref.ShotID)
		if !found {
			if shotBelongsToAnotherFrozenAsset(rowsBySnapshot, snapshot, ref) {
				failure := deepSearchFailure(input, rushestools.ErrCodeShotRefAssetMismatch,
					"shot_id 属于冻结快照中的另一素材，asset_id 与 shot_id 不匹配。",
					"使用 shot.search 返回的原始 asset_id + shot_id 组合重试。", []rushestools.ShotRefInput{ref})
				return nil, &failure, nil
			}
			failure := deepSearchFailure(input, rushestools.ErrCodeShotRefNotFound,
				"冻结快照中不存在指定 shot_id。",
				"不要臆造 shot_id；重新调用 shot.search 后选择返回候选。", []rushestools.ShotRefInput{ref})
			return nil, &failure, nil
		}
		if shot.AssetContentHash != asset.Hash {
			failure := deepSearchFailure(input, rushestools.ErrCodeShotRefAssetMismatch,
				"shot 所有权与素材内容哈希不一致。",
				"重新调用 shot.search 获取有效 ShotRef。", []rushestools.ShotRefInput{ref})
			return nil, &failure, nil
		}
		facts, err := exec.loadStoredDeepFacts(
			ctx, asset.Hash, shot.ShotID, shot.SourceStartFrame, shot.SourceEndFrame, shot.BoundaryVersion,
		)
		if err != nil {
			return nil, nil, err
		}
		resolved = append(resolved, deepResolvedCandidate{
			ref: ref, asset: asset, shot: shot, allFacts: facts,
		})
	}
	return resolved, nil, nil
}

func shotBelongsToAnotherFrozenAsset(
	rowsBySnapshot map[string][]storage.IndexedShot,
	snapshot shotSearchSnapshot,
	ref rushestools.ShotRefInput,
) bool {
	for _, frozen := range snapshot.Assets {
		if frozen.AssetID == ref.AssetID {
			continue
		}
		if rows, exists := rowsBySnapshot[frozen.BaseSnapshotID]; exists {
			if _, found := findIndexedShot(rows, ref.ShotID); found {
				return true
			}
		}
	}
	return false
}

func findIndexedShot(values []storage.IndexedShot, shotID string) (storage.IndexedShot, bool) {
	for _, value := range values {
		if value.ShotID == shotID {
			return value, true
		}
	}
	return storage.IndexedShot{}, false
}

func (exec *Executor) loadStoredDeepFacts(
	ctx context.Context,
	contentHash, shotID string,
	sourceStartFrame, sourceEndFrame int,
	boundaryVersion int,
) (storedDeepShotFacts, error) {
	byShot, err := exec.loadStoredDeepFactsForContent(ctx, contentHash)
	if err != nil {
		return storedDeepShotFacts{}, err
	}
	if result, exists := byShot[deepFactsKey(
		shotID, sourceStartFrame, sourceEndFrame, boundaryVersion,
	)]; exists {
		return result, nil
	}
	return storedDeepShotFacts{
		ShotID: shotID, SourceStartFrame: sourceStartFrame, SourceEndFrame: sourceEndFrame,
		BoundaryVersion: boundaryVersion,
	}, nil
}

func (exec *Executor) loadStoredDeepFactsForContent(
	ctx context.Context,
	contentHash string,
) (map[string]storedDeepShotFacts, error) {
	analyses, err := storage.ListAssetAnalysesByContentType(
		ctx, exec.database.Read(), contentHash, understanding.DeepShotAnalysisType,
	)
	if err != nil {
		return nil, err
	}
	result := map[string]storedDeepShotFacts{}
	for _, analysis := range analyses {
		facts, err := decodeAssetAnalysisResult[storedDeepShotFacts](analysis)
		if err != nil {
			return nil, err
		}
		if err := validateStoredDeepFacts(facts); err != nil {
			return nil, fmt.Errorf("深搜通用事实 %s 损坏: %w", analysis.ID, err)
		}
		key := deepFactsKey(
			facts.ShotID, facts.SourceStartFrame, facts.SourceEndFrame, facts.BoundaryVersion,
		)
		combined := result[key]
		if combined.ShotID == "" {
			combined.ShotID = facts.ShotID
			combined.SourceStartFrame = facts.SourceStartFrame
			combined.SourceEndFrame = facts.SourceEndFrame
			combined.BoundaryVersion = facts.BoundaryVersion
		}
		combined.Facets = append(combined.Facets, facts.Facets...)
		frameIDs := map[string]struct{}{}
		for _, frame := range combined.Frames {
			frameIDs[frame.FrameID] = struct{}{}
		}
		for _, frame := range facts.Frames {
			if _, duplicate := frameIDs[frame.FrameID]; duplicate {
				continue
			}
			frameIDs[frame.FrameID] = struct{}{}
			combined.Frames = append(combined.Frames, frame)
		}
		observationIDs := map[string]struct{}{}
		for _, observation := range combined.Observations {
			identity := observation.Facet + "\x00" + observation.Statement + "\x00" + strings.Join(observation.FrameIDs, ",")
			observationIDs[identity] = struct{}{}
		}
		for _, observation := range facts.Observations {
			identity := observation.Facet + "\x00" + observation.Statement + "\x00" + strings.Join(observation.FrameIDs, ",")
			if _, duplicate := observationIDs[identity]; duplicate {
				continue
			}
			observationIDs[identity] = struct{}{}
			combined.Observations = append(combined.Observations, observation)
		}
		result[key] = combined
	}
	for key, facts := range result {
		facts.Facets = compactSortedStrings(facts.Facets)
		sort.SliceStable(facts.Frames, func(left, right int) bool {
			return facts.Frames[left].SourceFrame < facts.Frames[right].SourceFrame
		})
		result[key] = facts
	}
	return result, nil
}

func validateStoredDeepFacts(facts storedDeepShotFacts) error {
	if strings.TrimSpace(facts.ShotID) == "" || facts.SourceStartFrame < 0 ||
		facts.SourceEndFrame <= facts.SourceStartFrame || facts.BoundaryVersion < 1 ||
		len(facts.Facets) == 0 || len(facts.Frames) == 0 || len(facts.Observations) == 0 {
		return errors.New("核心字段不完整")
	}
	frameIDs := map[string]struct{}{}
	facets := map[string]struct{}{}
	for _, facet := range facts.Facets {
		facet = strings.TrimSpace(facet)
		if facet == "" {
			return errors.New("facet 无效")
		}
		facets[facet] = struct{}{}
	}
	for _, frame := range facts.Frames {
		if strings.TrimSpace(frame.FrameID) == "" || frame.SourceFrame < facts.SourceStartFrame ||
			frame.SourceFrame >= facts.SourceEndFrame || len(frame.ObjectHash) != 64 || frame.ObjectSize <= 0 {
			return errors.New("frame manifest 无效")
		}
		if _, duplicate := frameIDs[frame.FrameID]; duplicate {
			return errors.New("frame manifest 重复")
		}
		frameIDs[frame.FrameID] = struct{}{}
	}
	for _, observation := range facts.Observations {
		if strings.TrimSpace(observation.Facet) == "" || strings.TrimSpace(observation.Statement) == "" ||
			len(observation.FrameIDs) == 0 {
			return errors.New("objective observation 无效")
		}
		if _, covered := facets[observation.Facet]; !covered {
			return errors.New("objective observation facet 未声明")
		}
		seenObservationFrames := map[string]struct{}{}
		for _, frameID := range observation.FrameIDs {
			if _, exists := frameIDs[frameID]; !exists {
				return errors.New("objective observation 引用未知 frame")
			}
			if _, duplicate := seenObservationFrames[frameID]; duplicate {
				return errors.New("objective observation 重复引用 frame")
			}
			seenObservationFrames[frameID] = struct{}{}
		}
	}
	return nil
}

func deepFactsKey(shotID string, sourceStartFrame, sourceEndFrame, boundaryVersion int) string {
	return strings.Join([]string{
		strings.TrimSpace(shotID), fmt.Sprint(sourceStartFrame), fmt.Sprint(sourceEndFrame),
		fmt.Sprint(boundaryVersion),
	}, "\x00")
}

func deepObservationSearchText(values []understanding.DeepObservation) string {
	statements := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		statement := strings.TrimSpace(value.Statement)
		if statement == "" {
			continue
		}
		if _, duplicate := seen[statement]; duplicate {
			continue
		}
		seen[statement] = struct{}{}
		statements = append(statements, statement)
	}
	return strings.Join(statements, "；")
}

func (exec *Executor) inspectAndPersistDeepShot(
	ctx context.Context,
	source string,
	candidate deepResolvedCandidate,
	request understanding.DeepShotAnalysisRequest,
	missingFacets []string,
) (understanding.DeepShotAnalysisResult, error) {
	if len(missingFacets) == 0 {
		return exec.analyzer.InspectShotDeep(ctx, exec.database.Paths, source, request)
	}
	identity, err := newAssetAnalysisIdentity(
		candidate.asset.Hash, understanding.DeepShotAnalysisType,
		understanding.DeepShotAnalyzerVersion,
		map[string]any{
			"shot_id":            candidate.shot.ShotID,
			"source_start_frame": candidate.shot.SourceStartFrame,
			"source_end_frame":   candidate.shot.SourceEndFrame,
			"boundary_version":   candidate.shot.BoundaryVersion,
			"facets":             missingFacets,
		},
		understanding.DeepShotOutputSchemaVersion,
	)
	if err != nil {
		return understanding.DeepShotAnalysisResult{}, err
	}
	var output understanding.DeepShotAnalysisResult
	err = exec.withAnalysisSingleflight(identity, func() error {
		if cached, cacheErr := exec.cachedAssetAnalysis(ctx, identity); cacheErr == nil {
			facts, decodeErr := decodeAssetAnalysisResult[storedDeepShotFacts](cached)
			if decodeErr != nil {
				return decodeErr
			}
			request.RequireNewFrames = false
			request.ReusableFrames = append(request.ReusableFrames, facts.Frames...)
			var inspectErr error
			output, inspectErr = exec.analyzer.InspectShotDeep(ctx, exec.database.Paths, source, request)
			return inspectErr
		} else if !errors.Is(cacheErr, storage.ErrNotFound) {
			return cacheErr
		}
		analysis, inspectErr := exec.analyzer.InspectShotDeep(ctx, exec.database.Paths, source, request)
		if inspectErr != nil {
			return inspectErr
		}
		facts := storedDeepShotFacts{
			ShotID:           candidate.shot.ShotID,
			SourceStartFrame: candidate.shot.SourceStartFrame,
			SourceEndFrame:   candidate.shot.SourceEndFrame,
			BoundaryVersion:  candidate.shot.BoundaryVersion,
			Facets:           analysis.Facets, Frames: persistedDeepFrames(analysis.Frames),
			Observations: append([]understanding.DeepObservation(nil), analysis.Observations...),
		}
		resultMap, mapErr := assetAnalysisResultMap(facts)
		if mapErr != nil {
			return mapErr
		}
		objects := make([]reducer.AnalysisObjectRow, 0, len(facts.Frames))
		for _, frame := range facts.Frames {
			objects = append(objects, reducer.AnalysisObjectRow{Hash: frame.ObjectHash, Size: frame.ObjectSize})
		}
		if err := exec.persistAssetAnalyses(ctx, []reducer.AssetAnalysisRow{{
			ID: identity.ID, AssetContentHash: identity.AssetContentHash,
			AnalysisType: identity.AnalysisType, AnalyzerVersion: identity.AnalyzerVersion,
			NormalizedOptionsJSON: identity.NormalizedOptionsJSON,
			OutputSchemaVersion:   identity.OutputSchemaVersion,
			Result:                resultMap, Objects: objects,
		}}); err != nil {
			return err
		}
		output = analysis
		return nil
	})
	return output, err
}

func indexedShotFrameNumbers(shot storage.IndexedShot) []int {
	result := []int{}
	for _, frame := range shot.RepresentativeFrames {
		if value, ok := NumericValue(frame["source_frame"]); ok {
			result = append(result, int(value))
		}
	}
	return result
}

func missingDeepFacets(required, existing []string) []string {
	covered := map[string]struct{}{}
	for _, value := range existing {
		covered[value] = struct{}{}
	}
	result := []string{}
	for _, value := range required {
		if _, exists := covered[value]; !exists {
			result = append(result, value)
		}
	}
	return result
}

func persistedDeepFrames(values []understanding.DeepFrameManifest) []understanding.DeepFrameManifest {
	result := make([]understanding.DeepFrameManifest, 0, len(values))
	for _, value := range values {
		value.NewlyAdded = false
		result = append(result, value)
	}
	return result
}

func countNewDeepFrames(values []understanding.DeepFrameManifest) int {
	total := 0
	for _, value := range values {
		if value.NewlyAdded {
			total++
		}
	}
	return total
}

func buildDeepCandidate(
	snapshotID string,
	candidate deepResolvedCandidate,
	analysis understanding.DeepShotAnalysisResult,
) rushestools.ShotDeepCandidate {
	requirements := convertToolDeepCriteria(analysis.Requirements)
	exclusions := convertToolDeepCriteria(analysis.Exclusions)
	preferences := convertToolDeepCriteria(analysis.Preferences)
	verification, score := applyDeepVerification(requirements, exclusions, preferences)
	allObservations := append([]understanding.DeepObservation(nil), candidate.allFacts.Observations...)
	allObservations = append(allObservations, analysis.Observations...)
	observations := make([]string, 0, len(allObservations))
	seenObservations := map[string]struct{}{}
	for _, observation := range allObservations {
		statement := strings.TrimSpace(observation.Statement)
		if statement == "" {
			continue
		}
		if _, duplicate := seenObservations[statement]; duplicate {
			continue
		}
		seenObservations[statement] = struct{}{}
		observations = append(observations, statement)
	}
	frames := make([]rushestools.ShotDeepFrameEvidence, 0, len(analysis.Frames))
	for _, frame := range analysis.Frames {
		frames = append(frames, rushestools.ShotDeepFrameEvidence{
			FrameID: frame.FrameID, SourceFrame: frame.SourceFrame,
			TimestampMS: frame.TimestampMS, Position: frame.Position,
			ObjectHash: frame.ObjectHash, ObjectSize: frame.ObjectSize,
			NewlyAdded: frame.NewlyAdded,
		})
	}
	coverage := append([]string(nil), candidate.allFacts.Facets...)
	coverage = append(coverage, analysis.Facets...)
	return rushestools.ShotDeepCandidate{
		IndexSnapshotID: snapshotID, AssetID: candidate.asset.ID, ShotID: candidate.shot.ShotID,
		SourceStartFrame: candidate.shot.SourceStartFrame,
		SourceEndFrame:   candidate.shot.SourceEndFrame,
		BoundaryVersion:  candidate.shot.BoundaryVersion,
		Verification:     verification, Score: score,
		Requirements: requirements, Exclusions: exclusions, Preferences: preferences,
		Observations: observations, FrameEvidence: frames,
		DeepCoverage: compactSortedStrings(coverage),
	}
}

func convertToolDeepCriteria(values []understanding.DeepCriterion) []rushestools.ShotDeepCriterionEvidence {
	result := make([]rushestools.ShotDeepCriterionEvidence, 0, len(values))
	for _, value := range values {
		result = append(result, rushestools.ShotDeepCriterionEvidence{
			Criterion: value.Criterion, Status: value.Status,
			Observation: value.Observation, FrameIDs: append([]string(nil), value.FrameIDs...),
		})
	}
	return result
}

func applyDeepVerification(
	requirements, exclusions, preferences []rushestools.ShotDeepCriterionEvidence,
) (string, float64) {
	observedRequirements, uncertainRequirements := 0, 0
	for _, criterion := range requirements {
		if criterion.Status == "refuted" {
			return "reject", 0
		}
		if criterion.Status == "observed" {
			observedRequirements++
		} else {
			uncertainRequirements++
		}
	}
	for _, criterion := range exclusions {
		if criterion.Status == "observed" {
			return "reject", 0
		}
	}
	preferenceScore := 0.0
	for _, criterion := range preferences {
		if criterion.Status == "observed" {
			preferenceScore += 0.04
		}
	}
	if len(requirements) == 0 || observedRequirements == len(requirements) {
		return "match", roundShotScore(min(1, 0.9+preferenceScore))
	}
	base := float64(observedRequirements) / float64(len(requirements))
	if observedRequirements > 0 {
		return "partial", roundShotScore(min(0.89, 0.4+0.45*base+preferenceScore))
	}
	if uncertainRequirements > 0 {
		return "uncertain", roundShotScore(min(0.49, 0.2+preferenceScore))
	}
	return "uncertain", roundShotScore(0.2 + preferenceScore)
}

func sortDeepCandidates(values []rushestools.ShotDeepCandidate) {
	rank := map[string]int{"match": 0, "partial": 1, "uncertain": 2, "reject": 3}
	sort.SliceStable(values, func(left, right int) bool {
		if rank[values[left].Verification] != rank[values[right].Verification] {
			return rank[values[left].Verification] < rank[values[right].Verification]
		}
		if values[left].Score != values[right].Score {
			return values[left].Score > values[right].Score
		}
		if values[left].ShotID != values[right].ShotID {
			return values[left].ShotID < values[right].ShotID
		}
		return values[left].AssetID < values[right].AssetID
	})
}

func deepSearchFailure(
	input rushestools.ShotDeepSearchInput,
	code rushestools.ToolErrorCode,
	message, recovery string,
	invalid []rushestools.ShotRefInput,
) rushestools.ShotDeepSearchResult {
	return rushestools.ShotDeepSearchResult{
		Status: string(rushestools.StatusFailed), ErrorCode: string(code),
		Message: message, Recovery: recovery, Query: input.Query,
		IndexSnapshotID:       input.IndexSnapshotID,
		InvalidCandidateShots: append([]rushestools.ShotRefInput(nil), invalid...),
		Candidates:            []rushestools.ShotDeepCandidate{},
	}
}

func deepSearchQueryCacheKey(draftID string, input rushestools.ShotDeepSearchInput) string {
	encoded, _ := json.Marshal(struct {
		DraftID string                          `json:"draft_id"`
		Input   rushestools.ShotDeepSearchInput `json:"input"`
	}{DraftID: draftID, Input: input})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}
