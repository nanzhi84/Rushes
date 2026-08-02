package agentexec

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
	"github.com/nanzhi84/Rushes/go/internal/understanding"
)

const (
	shotSearchDefaultTopK    = 12
	shotSearchMaximumTopK    = 30
	shotSearchSynonymVersion = "shot-search-synonyms-v1"
)

type shotSynonymGroup struct {
	canonical       string
	aliases         []string
	negativeAliases []string
}

// shotSearchSynonyms is deliberately small, versioned and deterministic. It
// is a product vocabulary, not an embedding substitute: changes require a
// version bump and ranking regression tests.
var shotSearchSynonyms = []shotSynonymGroup{
	{canonical: "海边", aliases: []string{"海边", "海滩", "沙滩", "海岸", "海浪"}},
	{canonical: "落日", aliases: []string{"落日", "夕阳", "黄昏", "晚霞"}},
	{canonical: "远景", aliases: []string{"远景", "全景", "环境镜头", "建立镜头", "wide shot"}},
	{canonical: "特写", aliases: []string{"特写", "近景", "局部", "细节", "close-up", "close up"}},
	{canonical: "舞蹈", aliases: []string{"舞蹈", "跳舞", "舞者"}},
	{
		canonical: "背光", aliases: []string{"背光", "逆光", "backlight", "backlit"},
		negativeAliases: []string{
			"无背光", "没有背光", "未开启背光", "背光关闭", "键盘不亮",
			"暗光", "极低照度", "全黑",
		},
	},
}

type sourceRange struct {
	startFrame int
	endFrame   int
}

func overlapsAny(target sourceRange, values []sourceRange) bool {
	for _, value := range values {
		if target.startFrame < value.endFrame && value.startFrame < target.endFrame {
			return true
		}
	}
	return false
}

type frozenShotAsset struct {
	asset    storage.Asset
	snapshot storage.ShotIndexSnapshot
}

type indexedShot struct {
	candidate          rushestools.ShotCandidate
	rangeInfo          sourceRange
	quality            map[string]any
	boundaryConfidence *float64
}

type searchVariant struct {
	value  string
	weight float64
}

type searchConcept struct {
	label    string
	weight   float64
	variants []searchVariant
}

type negativeSearchConcept struct {
	label            string
	evidenceVariants []string
}

type shotSearchQuery struct {
	concepts []searchConcept
	negative []negativeSearchConcept
	phrase   string
}

type weightedSearchField struct {
	name   string
	value  string
	weight float64
}

func (exec *Executor) toolSearchShots(
	ctx context.Context,
	draftID string,
	input rushestools.ShotSearchInput,
) (rushestools.ShotSearchResult, error) {
	if input.MinDurationFrames < 0 || input.MaxDurationFrames < 0 ||
		input.MaxDurationFrames > 0 && input.MaxDurationFrames < input.MinDurationFrames {
		return rushestools.ShotSearchResult{}, errors.New("镜头时长筛选范围无效")
	}
	if input.TopK < 0 || input.TopK > shotSearchMaximumTopK {
		return rushestools.ShotSearchResult{}, fmt.Errorf(
			"top_k 必须在 1 到 %d 之间；省略时默认 %d",
			shotSearchMaximumTopK, shotSearchDefaultTopK,
		)
	}
	topK := input.TopK
	if topK == 0 {
		topK = shotSearchDefaultTopK
	}
	roleFilter, err := normalizeShotRoleFilter(input.SemanticRoles)
	if err != nil {
		return rushestools.ShotSearchResult{}, err
	}

	frozen, err := exec.freezeShotAssets(ctx, draftID, input.AssetIDs)
	if err != nil {
		return rushestools.ShotSearchResult{}, err
	}
	started := time.Now()
	frozen, readyIDs, pendingIDs, failedIDs, err := exec.awaitShotSearchReady(ctx, draftID, frozen)
	if err != nil {
		return rushestools.ShotSearchResult{}, err
	}
	frozenIDs := frozenShotAssetIDs(frozen)
	waitDuration := time.Since(started)
	if len(failedIDs) > 0 {
		return shotSearchBarrierFailure(
			input.Query, frozenIDs, readyIDs, pendingIDs, failedIDs, waitDuration,
			rushestools.ErrCodeShotIndexFailed,
			"基础镜头索引失败；修复列出的素材分析后可使用相同参数直接重试，shot.search 未返回任何部分结果。",
		), nil
	}
	if len(pendingIDs) > 0 {
		return shotSearchBarrierFailure(
			input.Query, frozenIDs, readyIDs, pendingIDs, nil, waitDuration,
			rushestools.ErrCodeShotIndexNotReady,
			"自动基础镜头索引仍未全部就绪；后台任务完成后使用相同参数直接重试，shot.search 未返回任何部分结果。",
		), nil
	}

	snapshotID := shotSearchSnapshotID(draftID, frozen)
	shots, err := exec.loadFrozenShotIndex(ctx, frozen, snapshotID)
	if err != nil {
		return rushestools.ShotSearchResult{}, err
	}
	used, err := exec.usedShotRanges(ctx, draftID, input.ExcludeUsed)
	if err != nil {
		return rushestools.ShotSearchResult{}, err
	}
	query := buildShotSearchQuery(input.Query)
	tagQueries := make([]shotSearchQuery, 0, len(input.Tags))
	for _, tag := range input.Tags {
		if strings.TrimSpace(tag) != "" {
			tagQueries = append(tagQueries, buildShotSearchQuery(tag))
		}
	}

	matches := make([]rushestools.ShotCandidate, 0, len(shots))
	for _, shot := range shots {
		candidate := shot.candidate
		if len(roleFilter) > 0 && candidate.SemanticRole != "" {
			if _, matchesRole := roleFilter[candidate.SemanticRole]; !matchesRole {
				continue
			}
		}
		if candidate.DurationFrames < input.MinDurationFrames ||
			input.MaxDurationFrames > 0 && candidate.DurationFrames > input.MaxDurationFrames {
			continue
		}
		if candidate.Quality == "unusable" || overlapsAny(shot.rangeInfo, used[candidate.AssetID]) {
			continue
		}
		fields := shotCandidateSearchFields(candidate)
		if len(tagQueries) > 0 && !matchesAnyTagQuery(tagQueries, fields) {
			continue
		}
		score, terms, evidence, matched := scoreShotCandidate(query, fields)
		if strings.TrimSpace(input.Query) != "" && !matched {
			continue
		}
		if strings.TrimSpace(input.Query) == "" {
			score = 0.5
		}
		if candidate.Quality == "usable" {
			score += 0.04
		}
		score -= shotQualityPenalty(shot.quality)
		if shot.boundaryConfidence != nil {
			score += 0.03 * max(0, min(1, *shot.boundaryConfidence))
		}
		candidate.MatchedTerms = terms
		candidate.MatchEvidence = evidence
		candidate.Score = roundShotScore(max(0, min(1, score)))
		matches = append(matches, candidate)
	}

	sortShotCandidates(matches)
	total := len(matches)
	selected := diversifyShotCandidates(matches, min(topK, total))
	return rushestools.ShotSearchResult{
		Status: string(rushestools.StatusSucceeded), Query: input.Query,
		IndexSnapshotID: snapshotID, SynonymVersion: shotSearchSynonymVersion,
		FrozenAssetIDs: frozenIDs, ReadyAssetIDs: readyIDs, SearchReady: true,
		WaitDurationMS: waitDuration.Milliseconds(), Shots: selected,
		TotalMatches: total, ReturnedCandidates: len(selected), Truncated: len(selected) < total,
	}, nil
}

func normalizeShotRoleFilter(values []string) (map[string]struct{}, error) {
	result := map[string]struct{}{}
	for _, role := range values {
		role = strings.ToLower(strings.TrimSpace(role))
		if role != "a_roll" && role != "b_roll" {
			return nil, fmt.Errorf("semantic_roles 只支持 a_roll 或 b_roll，收到 %q", role)
		}
		result[role] = struct{}{}
	}
	return result, nil
}

func (exec *Executor) freezeShotAssets(
	ctx context.Context,
	draftID string,
	requestedAssetIDs []string,
) ([]frozenShotAsset, error) {
	assets, err := storage.ListDraftAssets(ctx, exec.database.Read(), draftID)
	if err != nil {
		return nil, err
	}
	eligible := make(map[string]storage.Asset, len(assets))
	for _, asset := range assets {
		if asset.Kind == "video" && asset.IngestStatus == "ready" && asset.Usable {
			eligible[asset.ID] = asset
		}
	}
	selected := make([]storage.Asset, 0, len(eligible))
	if len(requestedAssetIDs) == 0 {
		for _, asset := range eligible {
			selected = append(selected, asset)
		}
	} else {
		seen := map[string]struct{}{}
		invalid := []string{}
		for _, assetID := range requestedAssetIDs {
			assetID = strings.TrimSpace(assetID)
			asset, exists := eligible[assetID]
			if !exists {
				invalid = append(invalid, assetID)
				continue
			}
			if _, duplicate := seen[assetID]; duplicate {
				continue
			}
			seen[assetID] = struct{}{}
			selected = append(selected, asset)
		}
		if len(invalid) > 0 {
			sort.Strings(invalid)
			return nil, fmt.Errorf(
				"asset_ids 包含不存在、不可用、尚未 ingest ready 或非视频素材: %s",
				strings.Join(invalid, ", "),
			)
		}
	}
	if len(selected) == 0 {
		return nil, errors.New("当前冻结素材集没有可检索的视频素材")
	}
	sort.Slice(selected, func(left, right int) bool { return selected[left].ID < selected[right].ID })
	result := make([]frozenShotAsset, 0, len(selected))
	for _, asset := range selected {
		result = append(result, frozenShotAsset{asset: asset})
	}
	return result, nil
}

func (exec *Executor) awaitShotSearchReady(
	ctx context.Context,
	draftID string,
	frozen []frozenShotAsset,
) ([]frozenShotAsset, []string, []string, []string, error) {
	started := time.Now()
	interval := exec.jobPollInterval
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	timeout := exec.jobWaitTimeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	lastProgress := ""
	for {
		byContent := map[string]storage.ShotIndexSnapshot{}
		for _, item := range frozen {
			if _, resolved := byContent[item.asset.Hash]; resolved {
				continue
			}
			snapshot, err := storage.ReadyShotIndexByContentHash(ctx, exec.database.Read(), item.asset.Hash)
			if err == nil {
				byContent[item.asset.Hash] = snapshot
				continue
			}
			if !errors.Is(err, storage.ErrNotFound) {
				return nil, nil, nil, nil, err
			}
		}

		readyIDs, pendingIDs, failedIDs := []string{}, []string{}, []string{}
		for index := range frozen {
			if snapshot, ready := byContent[frozen[index].asset.Hash]; ready {
				frozen[index].snapshot = snapshot
				readyIDs = append(readyIDs, frozen[index].asset.ID)
				continue
			}
			status, err := exec.baseShotIndexJobStatus(ctx, frozen[index].asset)
			if err != nil {
				return nil, nil, nil, nil, err
			}
			if status == "pending" || status == "running" {
				pendingIDs = append(pendingIDs, frozen[index].asset.ID)
			} else {
				failedIDs = append(failedIDs, frozen[index].asset.ID)
			}
		}
		sort.Strings(readyIDs)
		sort.Strings(pendingIDs)
		sort.Strings(failedIDs)
		signature := strings.Join(readyIDs, ",") + "|" + strings.Join(pendingIDs, ",") + "|" + strings.Join(failedIDs, ",")
		if signature != lastProgress {
			exec.recordProgress(draftID, map[string]any{
				"type": contracts.TurnStreamSubagentProgress, "tool": "shot.search",
				"stage": "ensure_shot_base_index", "note": fmt.Sprintf(
					"基础镜头索引 %d/%d 已就绪", len(readyIDs), len(frozen),
				),
				"completed": len(readyIDs), "total": len(frozen),
				"ready_asset_ids": readyIDs, "pending_asset_ids": pendingIDs,
				"failed_asset_ids": failedIDs, "elapsed_ms": time.Since(started).Milliseconds(),
			})
			lastProgress = signature
		}
		if len(failedIDs) > 0 || len(pendingIDs) == 0 {
			return frozen, readyIDs, pendingIDs, failedIDs, nil
		}
		if time.Since(started) >= timeout {
			return frozen, readyIDs, pendingIDs, nil, nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, nil, nil, nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (exec *Executor) baseShotIndexJobStatus(ctx context.Context, asset storage.Asset) (string, error) {
	var status string
	err := exec.database.Read().QueryRowContext(ctx, `
		SELECT status FROM jobs
		WHERE kind='understand' AND idempotency_key=?`,
		understanding.BaseIndexIdempotencyKey(understanding.BaseIndexFingerprint(asset)),
	).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "missing", nil
	}
	return status, err
}

func shotSearchBarrierFailure(
	query string,
	frozenIDs, readyIDs, pendingIDs, failedIDs []string,
	waitDuration time.Duration,
	code rushestools.ToolErrorCode,
	recovery string,
) rushestools.ShotSearchResult {
	return rushestools.ShotSearchResult{
		Status: string(rushestools.StatusFailed), ErrorCode: string(code), Recovery: recovery,
		Query: query, SynonymVersion: shotSearchSynonymVersion,
		FrozenAssetIDs: frozenIDs, ReadyAssetIDs: readyIDs,
		PendingAssetIDs: pendingIDs, FailedAssetIDs: failedIDs,
		SearchReady: false, WaitDurationMS: waitDuration.Milliseconds(),
		Shots: []rushestools.ShotCandidate{},
	}
}

func frozenShotAssetIDs(values []frozenShotAsset) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, value.asset.ID)
	}
	sort.Strings(result)
	return result
}

func shotSearchSnapshotID(draftID string, values []frozenShotAsset) string {
	parts := []string{draftID, shotSearchSynonymVersion}
	for _, value := range values {
		parts = append(parts, strings.Join([]string{
			value.asset.ID, value.asset.Hash, value.snapshot.ID, fmt.Sprint(value.snapshot.Generation),
		}, ":"))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "shot_search_" + hex.EncodeToString(sum[:16])
}

func (exec *Executor) loadFrozenShotIndex(
	ctx context.Context,
	frozen []frozenShotAsset,
	searchSnapshotID string,
) ([]indexedShot, error) {
	shotsBySnapshot := map[string][]storage.IndexedShot{}
	result := []indexedShot{}
	for _, item := range frozen {
		rows, exists := shotsBySnapshot[item.snapshot.ID]
		if !exists {
			var err error
			rows, err = storage.ListShotIndexShots(ctx, exec.database.Read(), item.snapshot.ID)
			if err != nil {
				return nil, err
			}
			shotsBySnapshot[item.snapshot.ID] = rows
		}
		role := understanding.SuggestVisualRole(
			item.asset.Filename, stringValue(item.asset.RelDir), InterfaceString(item.snapshot.Summary["semantic_role"]),
		)
		for _, row := range rows {
			if row.SourceStartFrame < 0 || row.SourceEndFrame <= row.SourceStartFrame ||
				row.AssetContentHash != item.asset.Hash {
				return nil, fmt.Errorf("冻结索引 %s 含非法镜头 %s", item.snapshot.ID, row.ShotID)
			}
			qualityLabel := strings.TrimSpace(InterfaceString(row.Quality["label"]))
			result = append(result, indexedShot{
				candidate: rushestools.ShotCandidate{
					IndexSnapshotID: searchSnapshotID, ShotID: row.ShotID, AssetID: item.asset.ID,
					Filename: item.asset.Filename, SourceStartFrame: row.SourceStartFrame,
					SourceEndFrame: row.SourceEndFrame, DurationFrames: row.SourceEndFrame - row.SourceStartFrame,
					BoundaryVersion: row.BoundaryVersion, SemanticRole: role,
					Description: row.Description, Tags: append([]string(nil), row.Tags...),
					Quality: qualityLabel, Subjects: append([]string(nil), row.Subjects...),
					Actions: append([]string(nil), row.Actions...), Setting: append([]string(nil), row.Setting...),
					ShotScale: row.ShotScale, Composition: row.Composition,
					Lighting: append([]string(nil), row.Lighting...), Mood: append([]string(nil), row.Mood...),
					EditHints:    append([]string(nil), row.EditHints...),
					DeepCoverage: append([]string(nil), row.DeepCoverage...),
				},
				rangeInfo: sourceRange{startFrame: row.SourceStartFrame, endFrame: row.SourceEndFrame},
				quality:   row.Quality, boundaryConfidence: row.BoundaryConfidence,
			})
		}
	}
	return result, nil
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func (exec *Executor) usedShotRanges(
	ctx context.Context,
	draftID string,
	excludeUsed bool,
) (map[string][]sourceRange, error) {
	result := map[string][]sourceRange{}
	if !excludeUsed {
		return result, nil
	}
	document, err := timeline.Latest(ctx, exec.database, draftID)
	if errors.Is(err, storage.ErrNotFound) {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	for _, track := range document.Tracks {
		for _, clip := range track.Clips {
			if clip.AssetID == "" || clip.SourceEndFrame <= clip.SourceStartFrame {
				continue
			}
			result[clip.AssetID] = append(result[clip.AssetID], sourceRange{
				startFrame: clip.SourceStartFrame, endFrame: clip.SourceEndFrame,
			})
		}
	}
	return result, nil
}

func buildShotSearchQuery(raw string) shotSearchQuery {
	normalized := normalizeSearchText(raw)
	baseTokens := semanticWeightedTokens(normalized)
	concepts := []searchConcept{}
	negatives := []negativeSearchConcept{}
	for _, group := range shotSearchSynonyms {
		matchedAlias := ""
		for _, alias := range group.aliases {
			if strings.Contains(normalized, normalizeSearchText(alias)) {
				matchedAlias = normalizeSearchText(alias)
				break
			}
		}
		negated := false
		if matchedAlias != "" {
			negated = queryNegatesAlias(normalized, matchedAlias)
		}
		if negated {
			negatives = append(negatives, negativeSearchConcept{
				label: group.canonical, evidenceVariants: normalizedStrings(group.negativeAliases),
			})
			removeAliasTokens(baseTokens, group.aliases)
			continue
		}
		if matchedAlias == "" {
			continue
		}
		variants := []searchVariant{{value: matchedAlias, weight: 1}}
		seen := map[string]struct{}{matchedAlias: {}}
		for _, alias := range group.aliases {
			alias = normalizeSearchText(alias)
			if alias == "" {
				continue
			}
			if _, exists := seen[alias]; exists {
				continue
			}
			seen[alias] = struct{}{}
			variants = append(variants, searchVariant{value: alias, weight: 0.62})
		}
		concepts = append(concepts, searchConcept{
			label: group.canonical, weight: semanticTokenWeight(matchedAlias), variants: variants,
		})
		removeAliasTokens(baseTokens, group.aliases)
	}
	keys := make([]string, 0, len(baseTokens))
	for token := range baseTokens {
		keys = append(keys, token)
	}
	sort.Strings(keys)
	for _, token := range keys {
		concepts = append(concepts, searchConcept{
			label: token, weight: baseTokens[token], variants: []searchVariant{{value: token, weight: 1}},
		})
	}
	return shotSearchQuery{concepts: concepts, negative: negatives, phrase: normalized}
}

func queryNegatesAlias(query, alias string) bool {
	for _, prefix := range []string{"无", "没有", "没", "非", "不要", "排除", "避免"} {
		if strings.Contains(query, prefix+alias) {
			return true
		}
	}
	return false
}

func normalizedStrings(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value = normalizeSearchText(value); value != "" {
			result = append(result, value)
		}
	}
	return result
}

func removeAliasTokens(tokens map[string]float64, aliases []string) {
	for _, alias := range aliases {
		for token := range semanticWeightedTokens(alias) {
			delete(tokens, token)
		}
		delete(tokens, normalizeSearchText(alias))
	}
}

func semanticWeightedTokens(text string) map[string]float64 {
	result := map[string]float64{}
	for token := range SemanticTokens(text) {
		length := utf8.RuneCountInString(token)
		if length < 2 && !containsDigit(token) {
			continue
		}
		result[token] = semanticTokenWeight(token)
	}
	return result
}

func containsDigit(value string) bool {
	for _, character := range value {
		if unicode.IsDigit(character) {
			return true
		}
	}
	return false
}

func shotCandidateSearchFields(candidate rushestools.ShotCandidate) []weightedSearchField {
	return []weightedSearchField{
		{name: "主体", value: strings.Join(candidate.Subjects, " "), weight: 1},
		{name: "动作", value: strings.Join(candidate.Actions, " "), weight: 1},
		{name: "场景", value: strings.Join(candidate.Setting, " "), weight: 1},
		{name: "景别", value: candidate.ShotScale, weight: 1},
		{name: "标签", value: strings.Join(candidate.Tags, " "), weight: 0.82},
		{name: "镜头描述", value: candidate.Description, weight: 0.62},
		{name: "构图", value: candidate.Composition, weight: 0.48},
		{name: "光线", value: strings.Join(candidate.Lighting, " "), weight: 0.35},
		{name: "氛围", value: strings.Join(candidate.Mood, " "), weight: 0.35},
		{name: "剪辑提示", value: strings.Join(candidate.EditHints, " "), weight: 0.35},
		{name: "文件名", value: candidate.Filename, weight: 0.15},
		{name: "素材角色", value: candidate.SemanticRole, weight: 0.15},
	}
}

func scoreShotCandidate(
	query shotSearchQuery,
	fields []weightedSearchField,
) (float64, []string, []string, bool) {
	if len(query.concepts) == 0 && len(query.negative) == 0 {
		return 0, nil, nil, true
	}
	totalWeight, matchedWeight := 0.0, 0.0
	matchedTerms := []string{}
	evidence := []string{}
	for _, concept := range query.concepts {
		totalWeight += concept.weight
		bestScore, bestField, bestVariant := 0.0, "", ""
		for _, field := range fields {
			fieldText := normalizeSearchText(field.value)
			for _, variant := range concept.variants {
				if variant.value == "" || !semanticTextContains(fieldText, variant.value) {
					continue
				}
				score := field.weight * variant.weight
				if score > bestScore {
					bestScore, bestField, bestVariant = score, field.name, variant.value
				}
			}
		}
		if bestScore > 0 {
			matchedWeight += concept.weight * bestScore
			matchedTerms = append(matchedTerms, concept.label)
			evidence = append(evidence, fmt.Sprintf("%s命中：%s", bestField, bestVariant))
		}
	}
	for _, negative := range query.negative {
		negativeObserved := ""
		for _, field := range fields {
			fieldText := normalizeSearchText(field.value)
			for _, value := range negative.evidenceVariants {
				if semanticTextContains(fieldText, value) {
					negativeObserved = value
					break
				}
			}
		}
		if negativeObserved == "" {
			return 0, nil, nil, false
		}
		totalWeight++
		matchedWeight++
		matchedTerms = append(matchedTerms, "无"+negative.label)
		evidence = append(evidence, "否定条件命中："+negativeObserved)
	}
	if totalWeight == 0 || matchedWeight == 0 {
		return 0, matchedTerms, evidence, false
	}
	score := matchedWeight / totalWeight
	if query.phrase != "" && utf8.RuneCountInString(query.phrase) <= 24 {
		for _, field := range fields {
			if strings.Contains(normalizeSearchText(field.value), query.phrase) {
				score += 0.08
				evidence = append(evidence, field.name+"精确短语命中")
				break
			}
		}
	}
	matchedTerms = compactSortedStrings(matchedTerms)
	evidence = compactStringsStable(evidence, 12)
	return score, matchedTerms, evidence, true
}

func matchesAnyTagQuery(queries []shotSearchQuery, fields []weightedSearchField) bool {
	for _, query := range queries {
		_, _, _, matched := scoreShotCandidate(query, fields)
		if matched {
			return true
		}
	}
	return false
}

func semanticTextContains(text, term string) bool {
	if strings.Contains(text, term) {
		return true
	}
	_, exists := SemanticTokens(text)[term]
	return exists
}

func normalizeSearchText(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(value)), " "))
}

func shotQualityPenalty(quality map[string]any) float64 {
	penalty := 0.0
	if value, ok := NumericValue(quality["overexposed_ratio"]); ok && value > 0.10 {
		penalty += min(0.12, (value-0.10)*0.15)
	}
	if value, ok := NumericValue(quality["sharpness"]); ok && value < 100 {
		penalty += min(0.10, (100-value)/1000)
	}
	return penalty
}

func sortShotCandidates(values []rushestools.ShotCandidate) {
	sort.SliceStable(values, func(left, right int) bool {
		if values[left].Score != values[right].Score {
			return values[left].Score > values[right].Score
		}
		if values[left].ShotID != values[right].ShotID {
			return values[left].ShotID < values[right].ShotID
		}
		return values[left].AssetID < values[right].AssetID
	})
}

func diversifyShotCandidates(
	candidates []rushestools.ShotCandidate,
	limit int,
) []rushestools.ShotCandidate {
	remaining := append([]rushestools.ShotCandidate(nil), candidates...)
	selected := make([]rushestools.ShotCandidate, 0, limit)
	assetCounts := map[string]int{}
	for len(selected) < limit && len(remaining) > 0 {
		bestIndex, bestAdjusted := 0, math.Inf(-1)
		for index, candidate := range remaining {
			adjusted := candidate.Score - 0.035*float64(assetCounts[candidate.AssetID])
			for _, existing := range selected {
				adjusted -= 0.02 * shotSemanticOverlap(candidate, existing)
			}
			if adjusted > bestAdjusted || adjusted == bestAdjusted && shotCandidateLess(candidate, remaining[bestIndex]) {
				bestIndex, bestAdjusted = index, adjusted
			}
		}
		selected = append(selected, remaining[bestIndex])
		assetCounts[remaining[bestIndex].AssetID]++
		remaining = append(remaining[:bestIndex], remaining[bestIndex+1:]...)
	}
	return selected
}

func shotCandidateLess(left, right rushestools.ShotCandidate) bool {
	if left.ShotID != right.ShotID {
		return left.ShotID < right.ShotID
	}
	return left.AssetID < right.AssetID
}

func shotSemanticOverlap(left, right rushestools.ShotCandidate) float64 {
	leftValues := SemanticTokens(strings.Join(append(append(
		append([]string{}, left.Tags...), left.Subjects...), left.Setting...), " "))
	rightValues := SemanticTokens(strings.Join(append(append(
		append([]string{}, right.Tags...), right.Subjects...), right.Setting...), " "))
	intersection, union := 0, len(leftValues)
	for value := range rightValues {
		if _, exists := leftValues[value]; exists {
			intersection++
		} else {
			union++
		}
	}
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

func roundShotScore(value float64) float64 {
	return math.Round(value*10000) / 10000
}

func compactSortedStrings(values []string) []string {
	result := compactStringsStable(values, 20)
	sort.Strings(result)
	return result
}

func compactStringsStable(values []string, limit int) []string {
	result := []string{}
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
		if limit > 0 && len(result) >= limit {
			break
		}
	}
	return result
}

func ShotSemanticText(candidate rushestools.ShotCandidate) string {
	return strings.TrimSpace(ShotAssetSemanticText(candidate) + " " + ShotSegmentSemanticText(candidate))
}

func ShotAssetSemanticText(candidate rushestools.ShotCandidate) string {
	return strings.ToLower(strings.Join([]string{candidate.Filename, candidate.SemanticRole}, " "))
}

func ShotSegmentSemanticText(candidate rushestools.ShotCandidate) string {
	parts := []string{candidate.Description, strings.Join(candidate.Tags, " "),
		strings.Join(candidate.Subjects, " "), strings.Join(candidate.Actions, " "),
		strings.Join(candidate.Setting, " "), candidate.ShotScale, candidate.Composition,
		strings.Join(candidate.Lighting, " "), strings.Join(candidate.Mood, " "),
		strings.Join(candidate.EditHints, " ")}
	return strings.ToLower(strings.Join(parts, " "))
}

func TranscriptTextForSourceRange(utterances []map[string]any, startFrame, endFrame int) string {
	parts := []string{}
	for _, utterance := range utterances {
		startValue, startOK := NumericValue(utterance["source_start_frame"])
		endValue, endOK := NumericValue(utterance["source_end_frame"])
		if !startOK || !endOK || int(startValue) >= endFrame || int(endValue) <= startFrame {
			continue
		}
		if text := strings.TrimSpace(InterfaceString(utterance["text"])); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, " ")
}

func WeightedSemanticMatchScore(tokens map[string]struct{}, text string) float64 {
	if len(tokens) == 0 {
		return 0.5
	}
	textTokens := SemanticTokens(text)
	matchedWeight, totalWeight := 0.0, 0.0
	for token := range tokens {
		weight := semanticTokenWeight(token)
		totalWeight += weight
		if _, ok := textTokens[token]; ok || strings.Contains(text, token) {
			matchedWeight += weight
		}
	}
	if totalWeight == 0 {
		return 0
	}
	return matchedWeight / totalWeight
}

func semanticTokenWeight(token string) float64 {
	containsNumber := false
	onlyCJK := token != ""
	for _, value := range token {
		containsNumber = containsNumber || unicode.IsDigit(value)
		onlyCJK = onlyCJK && unicode.In(value, unicode.Han)
	}
	if containsNumber {
		return 4
	}
	length := utf8.RuneCountInString(token)
	if onlyCJK {
		if length == 1 {
			return 0.15
		}
		return 1
	}
	if length >= 3 {
		return 2
	}
	return 0.5
}

func SemanticMatchScore(tokens map[string]struct{}, text string) float64 {
	if len(tokens) == 0 {
		return 0.5
	}
	textTokens := SemanticTokens(text)
	matched := 0
	for token := range tokens {
		if _, ok := textTokens[token]; ok || strings.Contains(text, token) {
			matched++
		}
	}
	return float64(matched) / float64(len(tokens))
}

func SemanticTokens(text string) map[string]struct{} {
	result := map[string]struct{}{}
	lower := strings.ToLower(strings.TrimSpace(text))
	word := []rune{}
	flushWord := func() {
		if len(word) > 0 {
			result[string(word)] = struct{}{}
			word = word[:0]
		}
	}
	cjk := []rune{}
	flushCJK := func() {
		for index, value := range cjk {
			result[string(value)] = struct{}{}
			if index+1 < len(cjk) {
				result[string(cjk[index:index+2])] = struct{}{}
			}
		}
		cjk = cjk[:0]
	}
	for _, value := range lower {
		switch {
		case unicode.In(value, unicode.Han):
			flushWord()
			cjk = append(cjk, value)
		case unicode.IsLetter(value) || unicode.IsDigit(value):
			flushCJK()
			word = append(word, value)
		default:
			flushWord()
			flushCJK()
		}
	}
	flushWord()
	flushCJK()
	return result
}
