package agentexec

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
	"github.com/nanzhi84/Rushes/go/internal/understanding"
)

const assetListUsageNote = "asset_id 是后续调用使用的稳定素材 ID；filename 只用于识别素材，不是本地路径；kind 决定 video/audio/image/font 类型。duration_frames 的标尺是 timeline_fps；usable=false 的素材不可用于剪辑，ingest_status 与 understanding_status 分别表示导入和素材理解状态。rel_dir 与 suggested_visual_role 用于识别 A-roll/B-roll，音频按 suggested_role 区分 bgm/sfx。"

// understandInput/understandResult 是现有异步素材理解 job 的内部批处理载荷。
// 模型契约只通过 DetectShotsInput/DetectShotsResult 暴露单素材调用。
type understandInput struct {
	AssetIDs         []string
	Depth            string
	Focus            string
	MaxStepsPerAsset int
	ForceRefresh     bool
	RefreshNonce     string
}

type understandResult struct {
	DraftID          string
	JobID            string
	AssetIDs         []string
	Status           string
	Summaries        []rushestools.MaterialUnderstandingSummary
	CacheHitAssetIDs []string
	AnalyzedAssetIDs []string
	UsageNote        string
}

func (exec *Executor) ToolListAssets(
	ctx context.Context,
	draftID string,
	input rushestools.AssetListInput,
) (rushestools.AssetListResult, error) {
	assets, err := storage.ListDraftAssets(ctx, exec.database.Read(), draftID)
	if err != nil {
		return rushestools.AssetListResult{}, err
	}
	result := rushestools.AssetListResult{
		DraftID: draftID, Assets: []rushestools.AssetManifest{}, UsageNote: assetListUsageNote,
	}
	for _, asset := range assets {
		if input.Kind != "" && asset.Kind != input.Kind || input.After != "" && asset.ID <= input.After {
			continue
		}
		if input.OnlyUsable != nil && *input.OnlyUsable != asset.Usable {
			continue
		}
		duration, _ := NumericValue(asset.Probe["duration_sec"])
		suggestedRole := ""
		suggestedVisualRole := ""
		switch asset.Kind {
		case "audio":
			suggestedRole = understanding.ClassifyAudioRole(asset.Filename, duration)
		case "video":
			relDir := ""
			if asset.RelDir != nil {
				relDir = *asset.RelDir
			}
			understoodRole := ""
			if raw, summaryErr := storage.BestMaterialSummary(ctx, exec.database.Read(), asset.ID); summaryErr == nil {
				encoded, _ := json.Marshal(raw)
				var summary understanding.Summary
				if json.Unmarshal(encoded, &summary) == nil {
					understoodRole = summary.SemanticRole
				}
			}
			suggestedVisualRole = understanding.SuggestVisualRole(asset.Filename, relDir, understoodRole)
		}
		relDir := ""
		if asset.RelDir != nil {
			relDir = *asset.RelDir
		}
		result.Assets = append(result.Assets, rushestools.AssetManifest{
			AssetID: asset.ID, Filename: asset.Filename, Kind: asset.Kind,
			RelDir: relDir, SuggestedRole: suggestedRole, SuggestedVisualRole: suggestedVisualRole,
			DurationFrames: int(math.Round(duration * timeline.DefaultFPS)), TimelineFPS: timeline.DefaultFPS,
			Usable: asset.Usable, IngestStatus: asset.IngestStatus,
			UnderstandingStatus: asset.UnderstandingStatus,
		})
	}
	result.Total = len(result.Assets)
	limit := input.Limit
	if limit <= 0 || limit > 200 {
		limit = min(200, len(result.Assets))
	}
	if len(result.Assets) > limit {
		result.Assets = result.Assets[:limit]
		result.NextAfter = result.Assets[len(result.Assets)-1].AssetID
	}
	return result, nil
}

func (exec *Executor) toolDetectShots(
	ctx context.Context,
	draftID string,
	input rushestools.DetectShotsInput,
) (rushestools.DetectShotsResult, error) {
	assetID := strings.TrimSpace(input.AssetID)
	if assetID == "" {
		return rushestools.DetectShotsResult{}, errors.New("media.detect_shots 需要一个非空 asset_id")
	}
	internal, err := exec.executeDetectShots(ctx, draftID, understandInput{
		AssetIDs: []string{assetID}, Depth: input.Depth, Focus: input.Focus,
		MaxStepsPerAsset: input.MaxStepsPerAsset, ForceRefresh: input.ForceRefresh,
		RefreshNonce: input.RefreshNonce,
	})
	if err != nil {
		return rushestools.DetectShotsResult{}, err
	}
	result := rushestools.DetectShotsResult{
		DraftID: internal.DraftID, JobID: internal.JobID, AssetID: assetID,
		Status: internal.Status, UsageNote: internal.UsageNote,
		CacheHit: containsString(internal.CacheHitAssetIDs, assetID),
		Analyzed: containsString(internal.AnalyzedAssetIDs, assetID),
	}
	if len(internal.Summaries) > 0 {
		result.Summary = &internal.Summaries[0]
	}
	return result, nil
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func (exec *Executor) executeDetectShots(
	ctx context.Context,
	draftID string,
	input understandInput,
) (understandResult, error) {
	request, err := exec.prepareUnderstandRequest(ctx, draftID, input)
	if err != nil {
		return understandResult{}, err
	}
	idempotencyKey, err := understandIdempotencyKey(
		draftID, request, input.ForceRefresh, input.RefreshNonce,
	)
	if err != nil {
		return understandResult{}, err
	}
	if existing, found, findErr := exec.findUnderstandJob(ctx, idempotencyKey); findErr != nil {
		return understandResult{}, findErr
	} else if found {
		if !input.ForceRefresh && request.AllCacheHit &&
			(existing.Status == "failed" || existing.Status == "cancelled") {
			return exec.runUnderstandInline(ctx, draftID, request)
		}
		return exec.existingUnderstandResult(ctx, draftID, existing, request)
	}
	if shouldEnqueueUnderstand(request, input.ForceRefresh) {
		return exec.enqueueUnderstand(ctx, draftID, input, request, idempotencyKey)
	}
	return exec.runUnderstandInline(ctx, draftID, request)
}

type preparedUnderstandAsset struct {
	Asset       storage.Asset
	Options     understanding.AnalyzeOptions
	Fingerprint string
	CacheHit    bool
}

type preparedUnderstandRequest struct {
	AssetIDs         []string
	Assets           []preparedUnderstandAsset
	CacheHitAssetIDs []string
	AllCacheHit      bool
}

func (exec *Executor) prepareUnderstandRequest(
	ctx context.Context,
	draftID string,
	input understandInput,
) (preparedUnderstandRequest, error) {
	if len(input.AssetIDs) == 0 {
		return preparedUnderstandRequest{}, errors.New("media.detect_shots 需要一个 asset_id")
	}
	linkedAssets, err := storage.ListDraftAssets(ctx, exec.database.Read(), draftID)
	if err != nil {
		return preparedUnderstandRequest{}, err
	}
	assetByID := make(map[string]storage.Asset, len(linkedAssets))
	for _, asset := range linkedAssets {
		assetByID[asset.ID] = asset
	}
	assetIDs := make([]string, 0, len(input.AssetIDs))
	seen := make(map[string]struct{}, len(input.AssetIDs))
	for _, rawAssetID := range input.AssetIDs {
		assetID := strings.TrimSpace(rawAssetID)
		if assetID == "" {
			return preparedUnderstandRequest{}, errors.New("media.detect_shots 的 asset_id 不能为空")
		}
		if _, exists := seen[assetID]; exists {
			continue
		}
		seen[assetID] = struct{}{}
		asset, exists := assetByID[assetID]
		if !exists {
			return preparedUnderstandRequest{}, fmt.Errorf("素材 %s 不属于当前草稿", assetID)
		}
		if asset.Kind != "video" {
			return preparedUnderstandRequest{}, fmt.Errorf(
				"media.detect_shots 只接受可用 video 素材；素材 %s 的 kind=%s",
				assetID, asset.Kind,
			)
		}
		if !asset.Usable {
			return preparedUnderstandRequest{}, fmt.Errorf(
				"media.detect_shots 只接受可用 video 素材；素材 %s 当前不可用",
				assetID,
			)
		}
		assetIDs = append(assetIDs, assetID)
	}
	request := preparedUnderstandRequest{
		AssetIDs:         assetIDs,
		Assets:           make([]preparedUnderstandAsset, 0, len(assetIDs)),
		CacheHitAssetIDs: make([]string, 0, len(assetIDs)),
	}
	for _, assetID := range assetIDs {
		if err := ctx.Err(); err != nil {
			return preparedUnderstandRequest{}, err
		}
		asset := assetByID[assetID]
		options := understanding.NormalizeAnalyzeOptions(asset, understanding.AnalyzeOptions{
			Focus: input.Focus, Depth: input.Depth, MaxStepsPerAsset: input.MaxStepsPerAsset,
		})
		fingerprint := understanding.AnalysisFingerprint(asset, options)
		prepared := preparedUnderstandAsset{Asset: asset, Options: options, Fingerprint: fingerprint}
		if !input.ForceRefresh {
			if _, cacheErr := storage.MaterialSummaryByFingerprint(
				ctx, exec.database.Read(), assetID, fingerprint,
			); cacheErr == nil {
				prepared.CacheHit = true
				request.CacheHitAssetIDs = append(request.CacheHitAssetIDs, assetID)
			} else if !errors.Is(cacheErr, storage.ErrNotFound) {
				return preparedUnderstandRequest{}, cacheErr
			}
		}
		request.Assets = append(request.Assets, prepared)
	}
	request.AllCacheHit = len(request.CacheHitAssetIDs) == len(request.AssetIDs)
	return request, nil
}

func shouldEnqueueUnderstand(request preparedUnderstandRequest, forceRefresh bool) bool {
	if request.AllCacheHit {
		return false
	}
	return len(request.Assets) != 1 || request.Assets[0].Options.Depth != "scan" || forceRefresh
}

func (exec *Executor) runUnderstandInline(
	ctx context.Context,
	draftID string,
	request preparedUnderstandRequest,
) (understandResult, error) {
	runID := ""
	for _, prepared := range request.Assets {
		if !prepared.CacheHit {
			runID = RandomID("understand_inline")
			break
		}
	}
	summaries := make([]rushestools.MaterialUnderstandingSummary, 0, len(request.Assets))
	analyzedAssetIDs := make([]string, 0, len(request.Assets))
	for index, prepared := range request.Assets {
		if err := ctx.Err(); err != nil {
			return understandResult{}, err
		}
		asset := prepared.Asset
		if prepared.CacheHit {
			summary, err := exec.bestUnderstandingSummary(ctx, asset)
			if err != nil {
				return understandResult{}, err
			}
			summaries = append(summaries, summary)
			continue
		}
		started, err := reducer.Apply(ctx, exec.database, []contracts.Event{{
			Type:    "MaterialUnderstandingStarted",
			Payload: map[string]any{"asset_id": asset.ID, "job_id": runID},
		}}, reducer.Options{Actor: contracts.ActorAgent})
		if err != nil || started.Status != reducer.StatusApplied {
			return understandResult{}, errors.Join(err, fmt.Errorf("start reducer status: %s", started.Status))
		}
		summary, analyzeErr := exec.analyzer.AnalyzeWithOptions(
			ctx, exec.database, asset, prepared.Options, func(note string) {
				exec.recordProgress(draftID, map[string]any{
					"type": contracts.TurnStreamSubagentProgress, "tool": "media.detect_shots",
					"asset_id": asset.ID, "note": note, "completed": index, "total": len(request.Assets),
				})
			},
		)
		if analyzeErr != nil {
			cancelled := errors.Is(analyzeErr, context.Canceled)
			_, _ = reducer.Apply(context.WithoutCancel(ctx), exec.database, []contracts.Event{{
				Type: "MaterialUnderstandingFailed",
				Payload: map[string]any{
					"asset_id": asset.ID, "job_id": runID, "cancelled": cancelled,
					"failure": map[string]any{"error_code": string(rushestools.ErrCodeUnderstandingFailed), "message": analyzeErr.Error()},
				},
			}}, reducer.Options{Actor: contracts.ActorAgent})
			return understandResult{}, analyzeErr
		}
		var summaryMap map[string]any
		encoded, _ := json.Marshal(summary)
		_ = json.Unmarshal(encoded, &summaryMap)
		summaryID := RandomID("summary")
		completed, err := reducer.Apply(ctx, exec.database, []contracts.Event{{
			Type:    "MaterialUnderstandingCompleted",
			Payload: map[string]any{"asset_id": asset.ID, "job_id": runID, "summary_id": summaryID},
		}}, reducer.Options{
			Actor: contracts.ActorAgent,
			ResultRows: reducer.ResultRows{MaterialSummaries: []reducer.MaterialSummaryRow{{
				ID: summaryID, AssetID: asset.ID, Version: 0,
				Focus: StringPointerValue(prepared.Options.Focus), Status: "ready", Summary: summaryMap,
				Model: StringPointerValue(summary.Model), Fingerprint: StringPointerValue(prepared.Fingerprint),
				PromptVersion: StringPointerValue(understanding.PromptVersion),
			}}},
		})
		if err != nil || completed.Status != reducer.StatusApplied {
			return understandResult{}, errors.Join(err, fmt.Errorf("complete reducer status: %s", completed.Status))
		}
		bestSummary, err := exec.bestUnderstandingSummary(ctx, asset)
		if err != nil {
			return understandResult{}, err
		}
		summaries = append(summaries, bestSummary)
		analyzedAssetIDs = append(analyzedAssetIDs, asset.ID)
		exec.recordProgress(draftID, map[string]any{
			"type": contracts.TurnStreamSubagentProgress, "tool": "media.detect_shots",
			"asset_id": asset.ID, "note": "摘要已完成", "completed": index + 1, "total": len(request.Assets),
		})
	}
	return understandResult{
		DraftID: draftID, JobID: runID, AssetIDs: request.AssetIDs, Status: "completed",
		Summaries: summaries, CacheHitAssetIDs: request.CacheHitAssetIDs, AnalyzedAssetIDs: analyzedAssetIDs,
	}, nil
}

func (exec *Executor) bestUnderstandingSummary(
	ctx context.Context,
	asset storage.Asset,
) (rushestools.MaterialUnderstandingSummary, error) {
	effective, err := storage.BestMaterialSummary(ctx, exec.database.Read(), asset.ID)
	if err != nil {
		return rushestools.MaterialUnderstandingSummary{}, err
	}
	encoded, _ := json.Marshal(effective)
	var summary understanding.Summary
	if err := json.Unmarshal(encoded, &summary); err != nil {
		return rushestools.MaterialUnderstandingSummary{}, err
	}
	return CompactUnderstandingSummary(asset, summary, 12), nil
}

func (exec *Executor) enqueueUnderstand(
	ctx context.Context,
	draftID string,
	input understandInput,
	request preparedUnderstandRequest,
	idempotencyKey string,
) (understandResult, error) {
	jobID := RandomID("job")
	firstOptions := request.Assets[0].Options
	fingerprints := make(map[string]string, len(request.Assets))
	for _, prepared := range request.Assets {
		fingerprints[prepared.Asset.ID] = prepared.Fingerprint
	}
	result, err := reducer.Apply(ctx, exec.database, []contracts.Event{{
		Type: "JobEnqueued", DraftID: draftID,
		Payload: map[string]any{
			"job_id": jobID, "kind": "understand", "requested_by_draft_id": draftID,
			"idempotency_key": idempotencyKey,
			"job_payload": map[string]any{
				"asset_ids": request.AssetIDs, "focus": firstOptions.Focus,
				"depth": firstOptions.Depth, "max_steps_per_asset": input.MaxStepsPerAsset,
				"force_refresh": input.ForceRefresh, "refresh_nonce": strings.TrimSpace(input.RefreshNonce),
				"analysis_fingerprints": fingerprints,
			},
			"next_run_at": time.Now().UTC().Format(time.RFC3339Nano),
			"priority":    30, "max_retries": 2,
		},
	}}, reducer.Options{Actor: contracts.ActorAgent})
	if err != nil || result.Status != reducer.StatusApplied {
		if existing, found, lookupErr := exec.findUnderstandJob(ctx, idempotencyKey); lookupErr != nil {
			return understandResult{}, errors.Join(err, lookupErr)
		} else if found {
			return exec.existingUnderstandResult(ctx, draftID, existing, request)
		}
		return understandResult{}, errors.Join(err, fmt.Errorf("understand enqueue reducer status: %s", result.Status))
	}
	return queuedUnderstandResult(draftID, jobID, request), nil
}

type understandJobRef struct {
	ID     string
	Status string
}

func (exec *Executor) findUnderstandJob(
	ctx context.Context,
	idempotencyKey string,
) (understandJobRef, bool, error) {
	query := "SELECT job_id, status FROM jobs WHERE kind='understand' AND idempotency_key=? LIMIT 1"
	var job understandJobRef
	err := exec.database.Read().QueryRowContext(ctx, query, idempotencyKey).Scan(&job.ID, &job.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return understandJobRef{}, false, nil
	}
	if err != nil {
		return understandJobRef{}, false, err
	}
	return job, true, nil
}

func (exec *Executor) existingUnderstandResult(
	ctx context.Context,
	draftID string,
	job understandJobRef,
	request preparedUnderstandRequest,
) (understandResult, error) {
	switch job.Status {
	case "pending", "running":
		return queuedUnderstandResult(draftID, job.ID, request), nil
	case "succeeded":
		summaries := make([]rushestools.MaterialUnderstandingSummary, 0, len(request.Assets))
		for _, prepared := range request.Assets {
			raw, err := exec.materialSummaryForUnderstandJob(
				ctx, job.ID, prepared.Asset.ID, prepared.Fingerprint,
			)
			if err != nil {
				return understandResult{}, fmt.Errorf(
					"understand job %s 已成功但素材 %s 缺少持久化摘要: %w", job.ID, prepared.Asset.ID, err,
				)
			}
			encoded, _ := json.Marshal(raw)
			var summary understanding.Summary
			if err := json.Unmarshal(encoded, &summary); err != nil {
				return understandResult{}, err
			}
			summaries = append(summaries, CompactUnderstandingSummary(prepared.Asset, summary, 12))
		}
		return understandResult{
			DraftID: draftID, JobID: job.ID, AssetIDs: request.AssetIDs, Status: "completed",
			Summaries: summaries, CacheHitAssetIDs: request.CacheHitAssetIDs,
			UsageNote: "同参数素材理解任务已完成，结果来自持久化摘要；无需重复调用。",
		}, nil
	case "failed", "cancelled":
		return understandResult{
			DraftID: draftID, JobID: job.ID, AssetIDs: request.AssetIDs, Status: job.Status,
			CacheHitAssetIDs: request.CacheHitAssetIDs,
			UsageNote:        "同参数素材理解任务已到终态；请先读取失败信息，确需新任务时须调整素材或理解参数。",
		}, nil
	default:
		return understandResult{}, fmt.Errorf("understand job %s 状态无效: %s", job.ID, job.Status)
	}
}

func understandIdempotencyKey(
	draftID string,
	request preparedUnderstandRequest,
	forceRefresh bool,
	refreshNonce string,
) (string, error) {
	type canonicalAsset struct {
		AssetID     string `json:"asset_id"`
		Fingerprint string `json:"analysis_fingerprint"`
	}
	assets := make([]canonicalAsset, 0, len(request.Assets))
	for _, prepared := range request.Assets {
		assets = append(assets, canonicalAsset{AssetID: prepared.Asset.ID, Fingerprint: prepared.Fingerprint})
	}
	canonical := struct {
		DraftID      string           `json:"draft_id"`
		Assets       []canonicalAsset `json:"assets"`
		ForceRefresh bool             `json:"force_refresh"`
		RefreshNonce string           `json:"refresh_nonce,omitempty"`
	}{DraftID: draftID, Assets: assets, ForceRefresh: forceRefresh}
	if forceRefresh {
		canonical.RefreshNonce = strings.TrimSpace(refreshNonce)
	}
	raw, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return "understand:" + draftID + ":" + hex.EncodeToString(digest[:]), nil
}

func queuedUnderstandResult(
	draftID string,
	jobID string,
	request preparedUnderstandRequest,
) understandResult {
	return understandResult{
		DraftID: draftID, JobID: jobID, AssetIDs: request.AssetIDs, Status: "queued",
		CacheHitAssetIDs: request.CacheHitAssetIDs,
		UsageNote:        "素材理解已在后台排队；任务终态会自动续跑当前请求，请勿轮询或重复调用。",
	}
}

func CompactUnderstandingSummary(
	asset storage.Asset,
	summary understanding.Summary,
	evidenceLimit int,
) rushestools.MaterialUnderstandingSummary {
	overall := TruncateRunes(strings.TrimSpace(summary.Overall), 320)
	segments := sampleUnderstandingSegments(summary.Segments, evidenceLimit)
	evidence := make([]rushestools.MaterialEvidence, 0, len(segments))
	for _, segment := range segments {
		description := TruncateRunes(strings.TrimSpace(segment.Description), 220)
		if description == overall {
			description = ""
		}
		transcript := ""
		if segment.Transcript != nil {
			transcript = TruncateRunes(strings.TrimSpace(*segment.Transcript), 160)
		}
		startFrame := segment.SourceStartFrame
		endFrame := segment.SourceEndFrame
		if startFrame < 0 || endFrame <= startFrame {
			startFrame = max(0, int(math.Floor(segment.StartSec*timeline.DefaultFPS)))
			endFrame = max(startFrame, int(math.Ceil(segment.EndSec*timeline.DefaultFPS)))
			if segment.EndSec > segment.StartSec && endFrame == startFrame {
				endFrame++
			}
		}
		evidence = append(evidence, rushestools.MaterialEvidence{
			StartSec: segment.StartSec, EndSec: segment.EndSec,
			SourceStartFrame: startFrame, SourceEndFrame: endFrame,
			Description: description, Transcript: transcript,
			Tags:    append([]string(nil), segment.Tags[:min(6, len(segment.Tags))]...),
			Quality: segment.Quality, BoundaryKind: segment.BoundaryKind,
			BoundaryScore: segment.BoundaryScore, BoundaryVerified: segment.BoundaryVerified,
			Subjects:  append([]string(nil), segment.Subjects...),
			Actions:   append([]string(nil), segment.Actions...),
			Setting:   append([]string(nil), segment.Setting...),
			ShotScale: segment.ShotScale, Composition: segment.Composition,
			Lighting:         append([]string(nil), segment.Lighting...),
			Mood:             append([]string(nil), segment.Mood...),
			EditHints:        append([]string(nil), segment.EditHints...),
			OverexposedRatio: segment.OverexposedRatio,
			SharpnessScore:   segment.SharpnessScore,
		})
	}
	return rushestools.MaterialUnderstandingSummary{
		AssetID: asset.ID, Filename: asset.Filename, Kind: asset.Kind,
		TimelineFPS: timeline.DefaultFPS, SemanticRole: summary.SemanticRole,
		Overall: overall, Evidence: evidence,
		EvidenceTotal: len(summary.Segments), EvidenceTruncated: len(evidence) < len(summary.Segments),
		AnalysisMethod:    summary.AnalysisMethod,
		CandidateCutCount: summary.CandidateCuts, VerifiedCutCount: summary.VerifiedCuts,
		Degraded: append([]string(nil), summary.Degraded[:min(4, len(summary.Degraded))]...),
		UsageNote: "boundary_kind=analysis_window 只是长镜头理解采样窗口，不代表真实切镜；" +
			"只有 boundary_kind=visual_cut 且 boundary_verified=true 才能称为已验证切镜。",
	}
}

func sampleUnderstandingSegments(
	segments []understanding.Segment,
	limit int,
) []understanding.Segment {
	if limit <= 0 || len(segments) <= limit {
		return segments
	}
	if limit == 1 {
		return []understanding.Segment{segments[len(segments)/2]}
	}
	result := make([]understanding.Segment, 0, limit)
	for index := 0; index < limit; index++ {
		segmentIndex := int(math.Round(
			float64(index) * float64(len(segments)-1) / float64(limit-1),
		))
		result = append(result, segments[segmentIndex])
	}
	return result
}

func StringPointerValue(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
