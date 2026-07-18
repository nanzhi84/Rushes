package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"reflect"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	"github.com/nanzhi84/Rushes/go/internal/understanding"
)

const (
	contextRecentEditLimit           = 20
	contextRecentEditRuneBudget      = 6000
	contextMaterialCatalogRuneBudget = 12000
	contextUserMemoryRuneBudget      = 4000
	contextResidentWaveformPoints    = 24
)

// ContextBuilder 每次模型调用前从 SQLite 重建客观上下文。它只读取当前时间线
// 和有界语义操作日志，不读取、拼接历史时间线快照。
type ContextBuilder struct {
	database *storage.DB
}

func NewContextBuilder(database *storage.DB) *ContextBuilder {
	return &ContextBuilder{database: database}
}

func (builder *ContextBuilder) Build(ctx context.Context, draftID string) (string, error) {
	snapshot, err := builder.Snapshot(ctx, draftID)
	if err != nil {
		return "", err
	}
	raw, err := snapshot.Marshal()
	if err != nil {
		return "", err
	}
	return "【当前草稿最新 WorldState】\n" + string(raw) +
		"\nsections 是当前客观状态的唯一事实源；历史回复和 recent_edit_history 不能覆盖它。" +
		"user_memory 是跨草稿的用户长期偏好；与当前用户指令冲突时以本回合指令为准，并用 memory.update 更新记忆。" +
		"assets.material_catalog 是常驻精简素材目录；详细镜头语义必须按创作意图调用 media.search_shots 检索；完整口播转写不常驻，speech_searchable=true 时按需调用 speech.inspect。" +
		"timeline 中 beat_grid.waveform 仅常驻最多 24 点摘要：point_count 是完整压缩波形点数，sample_interval_frames 是原始 RMS 窗口宽度，sample_frames 是按 timeline_fps 标尺表示的摘要点素材内帧坐标并与 samples 一一对应，loudness_min/mean/max 汇总完整波形的 0–100 原始响度；完整波形必须按创作意图调用 audio.analyze_beats 获取，摘要不包含高潮标签。" +
		"人工编辑已经保存，不要要求用户重做；需要继续剪辑时直接基于当前轨道和片段。", nil
}

func (builder *ContextBuilder) Snapshot(
	ctx context.Context,
	draftID string,
) (WorldStateSnapshot, error) {
	state, err := builder.buildSnapshotMap(ctx, draftID)
	if err != nil {
		return WorldStateSnapshot{}, err
	}
	conversation := map[string]any{"reset": false}
	if reset, ok := state["conversation_reset"].(bool); ok {
		conversation["reset"] = reset
	}
	return NewWorldStateSnapshot(map[string]any{
		"draft":               state["draft"],
		"assets":              state["assets"],
		"timeline":            state["timeline"],
		"recent_edit_history": state["recent_edit_history"],
		"conversation":        conversation,
		"user_memory":         state["user_memory"],
	}), nil
}

func (builder *ContextBuilder) buildSnapshotMap(
	ctx context.Context,
	draftID string,
) (map[string]any, error) {
	draft, err := storage.GetDraft(ctx, builder.database.Read(), draftID)
	if err != nil {
		return nil, err
	}
	assets, err := storage.ListDraftAssets(ctx, builder.database.Read(), draftID)
	if err != nil {
		return nil, err
	}
	userMemory, err := builder.userMemoryContext(ctx, draftID)
	if err != nil {
		return nil, err
	}

	kindCounts := map[string]int{}
	readyUnderstanding := 0
	assetByID := make(map[string]storage.Asset, len(assets))
	audioRoles := make([]map[string]any, 0)
	for _, asset := range assets {
		kindCounts[asset.Kind]++
		assetByID[asset.ID] = asset
		if asset.UnderstandingStatus == "ready" {
			readyUnderstanding++
		}
		if asset.Kind == "audio" {
			duration, _ := agentexec.NumericValue(asset.Probe["duration_sec"])
			audioRoles = append(audioRoles, map[string]any{
				"asset_id":       asset.ID,
				"suggested_role": understanding.ClassifyAudioRole(asset.Filename, duration),
			})
		}
	}

	draftContext := map[string]any{
		"name":   draft.Name,
		"goal":   valueOrContext(agentexec.InterfaceString(draft.Brief["goal"]), "以当前用户指令为准"),
		"status": draft.Status,
	}
	if len(draft.Brief) > 1 {
		draftContext["brief"] = draft.Brief
	}
	if len(draft.ContentPlan) > 0 {
		draftContext["content_plan"] = draft.ContentPlan
	} else {
		draftContext["content_plan"] = map[string]any{"_hint": "尚未建立创作计划"}
	}
	if draft.PendingDecisionID != nil {
		draftContext["pending_decision_id"] = *draft.PendingDecisionID
	}
	if len(draft.RunningJobs) > 0 {
		draftContext["running_jobs"] = draft.RunningJobs
	}
	if len(draft.LastError) > 0 {
		draftContext["last_error"] = draft.LastError
	}
	snapshot := map[string]any{
		"draft": draftContext,
		"assets": map[string]any{
			"total": len(assets), "counts": kindCounts,
			"understanding_ready": readyUnderstanding, "audio_roles": audioRoles,
		},
		"timeline":            nil,
		"recent_edit_history": []any{},
		"user_memory":         userMemory,
	}
	if draft.MessagesTailRef != nil {
		snapshot["conversation_reset"] = true
	}

	usedAssetSet := map[string]struct{}{}
	if draft.TimelineCurrentVersion != nil {
		document, timelineErr := timeline.Latest(ctx, builder.database, draftID)
		if timelineErr != nil {
			return nil, timelineErr
		}
		timelineSnapshot, usedAssetIDs := buildTimelineContext(document)
		for _, assetID := range usedAssetIDs {
			usedAssetSet[assetID] = struct{}{}
		}
		timelineSnapshot["validated"] = draft.TimelineValidated
		timelineSnapshot["beat_alignment"] = agentexec.BeatAlignmentData(document)
		usedAssets := make([]map[string]any, 0, len(usedAssetIDs))
		for _, assetID := range usedAssetIDs {
			asset, exists := assetByID[assetID]
			if !exists {
				continue
			}
			duration, _ := agentexec.NumericValue(asset.Probe["duration_sec"])
			usedAssets = append(usedAssets, map[string]any{
				"asset_id": asset.ID, "filename": asset.Filename,
				"kind": asset.Kind, "duration_sec": duration,
			})
		}
		snapshot["timeline"] = timelineSnapshot
		snapshot["assets"].(map[string]any)["used_by_timeline"] = usedAssets
	}

	materialCatalog, catalogAvailable, err := builder.materialCatalogContext(ctx, assets, usedAssetSet)
	if err != nil {
		return nil, err
	}
	assetContext := snapshot["assets"].(map[string]any)
	assetContext["material_catalog"] = materialCatalog
	assetContext["material_catalog_included"] = len(materialCatalog)
	assetContext["material_catalog_available"] = catalogAvailable
	assetContext["material_catalog_truncated"] = len(materialCatalog) < catalogAvailable

	batches, err := storage.ListTimelineEditBatches(
		ctx, builder.database.Read(), draftID, contextRecentEditLimit,
	)
	if err != nil {
		return nil, err
	}
	snapshot["recent_edit_history"] = compressTimelineEditHistoryMap(batches, contextRecentEditLimit)

	// 最后一层递归清洗避免旧日志或外部输入把已废弃的版本字段重新带回模型。
	return sanitizeContextMap(snapshot), nil
}

// userMemoryContext 把工作区级用户记忆按 last_confirmed_at DESC 顺序装进预算。
// 逼近预算时不再静默少放：被折叠的记忆以 omitted_keys 显式列出（只列 key，几乎
// 不占预算），模型至少知道还有哪些长期记忆存在但未展开；同时把注入规模落进结构化
// 日志，让截断在真实会话中变得可观测。omitted_keys 参与预算裁剪，保证最终快照连同
// 该列表一起仍不超过 contextUserMemoryRuneBudget。
func (builder *ContextBuilder) userMemoryContext(
	ctx context.Context,
	draftID string,
) (map[string]any, error) {
	memories, err := storage.ListUserMemories(ctx, builder.database.Read())
	if err != nil {
		return nil, err
	}
	included := 0
	for included < len(memories) {
		encoded, err := json.Marshal(
			userMemorySnapshotSection(memories[:included+1], memories[included+1:]),
		)
		if err != nil {
			return nil, err
		}
		if utf8.RuneCount(encoded) > contextUserMemoryRuneBudget {
			break
		}
		included++
	}
	section := userMemorySnapshotSection(memories[:included], memories[included:])
	encoded, err := json.Marshal(section)
	if err != nil {
		return nil, err
	}
	slog.Info(
		"用户记忆注入 WorldState 快照",
		"draft_id", draftID,
		"total", len(memories),
		"included", included,
		"omitted", len(memories)-included,
		"section_runes", utf8.RuneCount(encoded),
		"truncated", included < len(memories),
	)
	return section, nil
}

// userMemorySnapshotSection 构造 user_memory 快照节。included 是已放入预算的记忆，
// omitted 是被折叠的记忆；仅在发生截断时附 omitted_keys（只列 key，不泄漏 statement），
// 未截断时省略该字段以保持既有形状与 golden 稳定。
func userMemorySnapshotSection(included, omitted []storage.UserMemory) map[string]any {
	entries := make([]map[string]any, 0, len(included))
	for _, memory := range included {
		entries = append(entries, map[string]any{
			"key": memory.Key, "kind": memory.Kind, "statement": memory.Statement,
		})
	}
	section := map[string]any{
		"entries":   entries,
		"total":     len(included) + len(omitted),
		"truncated": len(omitted) > 0,
	}
	if len(omitted) > 0 {
		omittedKeys := make([]string, 0, len(omitted))
		for _, memory := range omitted {
			omittedKeys = append(omittedKeys, memory.Key)
		}
		section["omitted_keys"] = omittedKeys
	}
	return section
}

// materialCatalogContext keeps a compact directory resident in every model turn.
// Detailed per-shot evidence stays in SQLite and is fetched through
// media.search_shots, avoiding both context bloat and an uninformed planner.
func (builder *ContextBuilder) materialCatalogContext(
	ctx context.Context,
	assets []storage.Asset,
	usedAssetIDs map[string]struct{},
) ([]map[string]any, int, error) {
	type candidate struct {
		linkedIndex int
		priority    int
		item        map[string]any
		base        map[string]any
	}
	candidates := make([]candidate, 0, len(assets))
	for linkedIndex, asset := range assets {
		durationSec, _ := agentexec.NumericValue(asset.Probe["duration_sec"])
		relDir := ""
		if asset.RelDir != nil {
			relDir = *asset.RelDir
		}
		base := map[string]any{
			"asset_id": asset.ID, "filename": asset.Filename, "kind": asset.Kind,
			"duration_frames":      int(durationSec*timeline.DefaultFPS + 0.5),
			"understanding_status": asset.UnderstandingStatus,
		}
		if relDir != "" {
			base["rel_dir"] = relDir
		}
		switch asset.Kind {
		case "audio":
			base["suggested_role"] = understanding.ClassifyAudioRole(asset.Filename, durationSec)
		case "video":
			if role := understanding.SuggestVisualRole(asset.Filename, relDir, ""); role != "" {
				base["suggested_visual_role"] = role
			}
		}
		item := cloneContextMap(base)
		hasTranscript := false
		raw, summaryErr := storage.BestMaterialSummary(ctx, builder.database.Read(), asset.ID)
		if summaryErr == nil {
			encoded, _ := json.Marshal(raw)
			var summary understanding.Summary
			if json.Unmarshal(encoded, &summary) == nil {
				item["overall"] = truncateRunes(strings.TrimSpace(summary.Overall), 128)
				item["shot_count"] = len(summary.Segments)
				item["searchable"] = asset.Kind == "video" && len(summary.Segments) > 0
				item["semantic_tags"] = catalogSemanticTags(summary.Segments, 10)
				if role := understanding.SuggestVisualRole(
					asset.Filename, relDir, summary.SemanticRole,
				); role != "" {
					item["semantic_role"] = role
				}
				if summary.AnalysisDepth != "" {
					item["analysis_depth"] = summary.AnalysisDepth
				}
			}
		} else if !errors.Is(summaryErr, storage.ErrNotFound) {
			return nil, 0, summaryErr
		}
		if transcript, transcriptErr := storage.LatestTranscript(
			ctx, builder.database.Read(), asset.ID,
		); transcriptErr == nil {
			hasTranscript = true
			item["speech_searchable"] = true
			item["utterance_count"] = len(transcript.Utterances)
			item["transcript_provider"] = transcript.ProviderID
		} else if !errors.Is(transcriptErr, storage.ErrNotFound) {
			return nil, 0, transcriptErr
		}
		priority := 3
		if _, used := usedAssetIDs[asset.ID]; used {
			priority = 0
		} else if hasTranscript || asset.Kind == "audio" && base["suggested_role"] == "bgm" {
			priority = 1
		} else if asset.UnderstandingStatus == "ready" {
			priority = 2
		}
		candidates = append(candidates, candidate{
			linkedIndex: linkedIndex, priority: priority, item: item, base: base,
		})
	}
	sort.SliceStable(candidates, func(left, right int) bool {
		if candidates[left].priority == candidates[right].priority {
			return candidates[left].linkedIndex < candidates[right].linkedIndex
		}
		return candidates[left].priority < candidates[right].priority
	})
	selected := make([]candidate, 0, len(candidates))
	usedRunes := 2
	for _, entry := range candidates {
		item := entry.item
		encoded, err := json.Marshal(item)
		if err != nil {
			return nil, 0, err
		}
		itemRunes := utf8.RuneCount(encoded)
		if len(selected) > 0 {
			itemRunes++
		}
		if usedRunes+itemRunes > contextMaterialCatalogRuneBudget {
			item = entry.base
			encoded, _ = json.Marshal(item)
			itemRunes = utf8.RuneCount(encoded)
			if len(selected) > 0 {
				itemRunes++
			}
		}
		if usedRunes+itemRunes > contextMaterialCatalogRuneBudget {
			continue
		}
		entry.item = item
		selected = append(selected, entry)
		usedRunes += itemRunes
	}
	sort.Slice(selected, func(left, right int) bool {
		return selected[left].linkedIndex < selected[right].linkedIndex
	})
	items := make([]map[string]any, 0, len(selected))
	for _, entry := range selected {
		items = append(items, entry.item)
	}
	return items, len(assets), nil
}

func catalogSemanticTags(segments []understanding.Segment, limit int) []string {
	values := make([]string, 0, limit)
	seen := map[string]struct{}{}
	for _, segment := range segments {
		groups := [][]string{segment.Tags, segment.Subjects, segment.Actions, segment.Setting, segment.Mood}
		for _, group := range groups {
			for _, value := range group {
				value = strings.TrimSpace(value)
				if value == "" {
					continue
				}
				if _, duplicate := seen[value]; duplicate {
					continue
				}
				seen[value] = struct{}{}
				values = append(values, value)
				if len(values) >= limit {
					return values
				}
			}
		}
	}
	return values
}

func cloneContextMap(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func buildTimelineContext(document timeline.Document) (map[string]any, []string) {
	tracks := make([]map[string]any, 0, len(document.Tracks))
	used := map[string]struct{}{}
	for _, track := range document.Tracks {
		clips := make([]map[string]any, 0, len(track.Clips))
		for _, clip := range track.Clips {
			item := map[string]any{
				"timeline_clip_id":     clip.TimelineClipID,
				"timeline_start_frame": clip.TimelineStartFrame,
				"timeline_end_frame":   clip.TimelineEndFrame,
			}
			for key, value := range map[string]any{
				"asset_id": clip.AssetID, "asset_kind": clip.AssetKind, "role": clip.Role,
				"text": clip.Text, "source_start_frame": clip.SourceStartFrame,
				"source_end_frame": clip.SourceEndFrame, "playback_rate": clip.PlaybackRate,
				"gain_db": clip.GainDB, "linked": clip.Linked,
				"parent_block_id": clip.ParentBlockID,
			} {
				if !emptyContextValue(value) {
					item[key] = value
				}
			}
			if clip.AssetID != "" {
				used[clip.AssetID] = struct{}{}
			}
			if beatGrid := compactBeatGridContext(clip.Effects); beatGrid != nil {
				item["beat_grid"] = beatGrid
			}
			if anchor := compactSemanticAnchorContext(clip.Metadata); anchor != nil {
				item["semantic_anchor"] = anchor
			}
			clips = append(clips, item)
		}
		tracks = append(tracks, map[string]any{
			"track_id": track.TrackID, "track_type": track.TrackType,
			"muted": track.Muted, "solo": track.Solo, "locked": track.Locked,
			"gain_db": track.GainDB, "clips": clips,
		})
	}
	usedIDs := make([]string, 0, len(used))
	for assetID := range used {
		usedIDs = append(usedIDs, assetID)
	}
	sort.Strings(usedIDs)
	return map[string]any{
		"fps": document.FPS, "duration_frames": document.DurationFrames, "tracks": tracks,
	}, usedIDs
}

func compactSemanticAnchorContext(metadata map[string]any) map[string]any {
	if agentexec.StringValue(metadata["kind"]) != "b_roll_semantic_anchor" {
		return nil
	}
	result := map[string]any{"kind": "b_roll_semantic_anchor"}
	for _, key := range []string{
		"shot_id", "a_roll_asset_id", "a_roll_source_start_frame", "a_roll_source_end_frame",
		"start_utterance_id", "end_utterance_id", "start_word_id", "end_word_id",
		"b_roll_asset_id", "b_roll_filename",
	} {
		if value, exists := metadata[key]; exists && !emptyContextValue(value) {
			result[key] = value
		}
	}
	if text := strings.TrimSpace(agentexec.StringValue(metadata["transcript_text"])); text != "" {
		result["transcript_text"] = truncateRunes(text, 160)
	}
	if description := strings.TrimSpace(agentexec.StringValue(metadata["b_roll_description"])); description != "" {
		result["b_roll_description"] = truncateRunes(description, 160)
	}
	return result
}

func compactBeatGridContext(effects []map[string]any) map[string]any {
	for _, effect := range effects {
		if agentexec.StringValue(effect["kind"]) != "beat_grid" {
			continue
		}
		result := map[string]any{
			"bpm":               effect["bpm"],
			"analysis_method":   effect["analysis_method"],
			"beat_count":        len(agentexec.EffectFrameValues(effect["beat_frames"])),
			"strong_beat_count": len(agentexec.EffectFrameValues(effect["strong_beat_frames"])),
			"downbeat_count":    len(agentexec.EffectFrameValues(effect["downbeat_frames"])),
		}
		if waveform := compactWaveformContext(effect["waveform"]); waveform != nil {
			result["waveform"] = waveform
		}
		return result
	}
	return nil
}

func compactWaveformContext(value any) map[string]any {
	waveform, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	interval, intervalOK := agentexec.NumericValue(waveform["sample_interval_frames"])
	samples := agentexec.EffectFrameValues(waveform["samples"])
	if !intervalOK || interval <= 0 || interval != float64(int(interval)) || len(samples) == 0 {
		return nil
	}
	if len(samples) > 256 {
		samples = samples[:256]
	}
	for _, sample := range samples {
		if sample < 0 || sample > 100 {
			return nil
		}
	}
	sampleFrames := agentexec.EffectFrameValues(waveform["sample_frames"])
	if len(sampleFrames) == 0 {
		// 兼容升级前已经持久化的 beat_grid：旧数据虽只存间隔和数组，
		// 仍可无损恢复每个 RMS 窗口的起始帧。
		sampleFrames = make([]int, len(samples))
		for index := range samples {
			sampleFrames[index] = index * int(interval)
		}
	}
	if len(sampleFrames) != len(samples) {
		return nil
	}
	for index, frame := range sampleFrames {
		if frame < 0 || (index > 0 && frame <= sampleFrames[index-1]) {
			return nil
		}
	}
	pointCount := len(samples)
	residentCount := min(pointCount, contextResidentWaveformPoints)
	residentFrames := make([]int, 0, residentCount)
	residentSamples := make([]int, 0, residentCount)
	minimum, maximum, total := samples[0], samples[0], 0
	for _, sample := range samples {
		minimum = min(minimum, sample)
		maximum = max(maximum, sample)
		total += sample
	}
	for index := 0; index < residentCount; index++ {
		sourceIndex := index
		if pointCount > residentCount {
			sourceIndex = index * (pointCount - 1) / (residentCount - 1)
		}
		residentFrames = append(residentFrames, sampleFrames[sourceIndex])
		residentSamples = append(residentSamples, samples[sourceIndex])
	}
	mean := math.Round(float64(total)/float64(pointCount)*10) / 10
	return map[string]any{
		"sample_interval_frames": int(interval),
		"point_count":            pointCount,
		"sample_frames":          residentFrames,
		"samples":                residentSamples,
		"loudness_min":           minimum,
		"loudness_mean":          mean,
		"loudness_max":           maximum,
	}
}

func compressTimelineEditBatches(
	batches []storage.TimelineEditBatch,
	limit int,
) []map[string]any {
	entries := compressTimelineEditEntries(batches, limit)
	result := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		clean := cloneContextMap(entry)
		delete(clean, "_history_key")
		result = append(result, clean)
	}
	return result
}

func compressTimelineEditHistoryMap(
	batches []storage.TimelineEditBatch,
	limit int,
) map[string]any {
	entries := compressTimelineEditEntries(batches, limit)
	result := make(map[string]any, len(entries))
	keys := make([]string, 0, len(entries))
	for index, entry := range entries {
		key, _ := entry["_history_key"].(string)
		if key == "" {
			key = timelineEditHistoryKey(int64(index+1), 0)
		}
		clean := cloneContextMap(entry)
		delete(clean, "_history_key")
		result[key] = clean
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for len(keys) > 0 {
		encoded, err := json.Marshal(result)
		if err == nil && utf8.RuneCount(encoded) <= contextRecentEditRuneBudget {
			break
		}
		delete(result, keys[0])
		keys = keys[1:]
	}
	return result
}

func compressTimelineEditEntries(
	batches []storage.TimelineEditBatch,
	limit int,
) []map[string]any {
	entries := make([]map[string]any, 0)
	coalesced := map[string]int{}
	inserted := map[string]int{}
	for batchIndex, batch := range batches {
		sequence := batch.Sequence
		if sequence <= 0 {
			sequence = int64(batchIndex + 1)
		}
		for operationIndex, rawOperation := range batch.Operations {
			operation := summarizeTimelineEditOperation(rawOperation)
			kind, _ := operation["kind"].(string)
			target := operationTarget(operation)
			// 这两类操作会原子替换整条时间线。此前的逐片段操作已经被最新
			// WorldState 吸收，继续保留只会让模型误读为仍待执行的指令。
			if kind == "recut_to_beats" || kind == "compose_initial" {
				entries = entries[:0]
				coalesced = map[string]int{}
				inserted = map[string]int{}
			}
			if kind == "delete_clip" && target != "" {
				if index, ok := inserted[target]; ok && index >= 0 && index < len(entries) {
					entries = append(entries[:index], entries[index+1:]...)
					coalesced, inserted = rebuildEditIndexes(entries)
					continue
				}
			}
			entry := map[string]any{
				"_history_key": timelineEditHistoryKey(sequence, operationIndex),
				"actor":        batch.Actor,
				"origin":       batch.Origin,
				"op":           operation,
			}
			key := coalesceOperationKey(kind, operation, target)
			if key != "" {
				if index, ok := coalesced[key]; ok {
					entries = append(entries[:index], entries[index+1:]...)
					entries = append(entries, entry)
					coalesced, inserted = rebuildEditIndexes(entries)
					continue
				}
				coalesced[key] = len(entries)
			}
			if kind == "insert_clip" && target != "" {
				inserted[target] = len(entries)
			}
			entries = append(entries, entry)
		}
	}
	if limit > 0 && len(entries) > limit {
		entries = entries[len(entries)-limit:]
	}
	return boundRecentEditHistory(entries, contextRecentEditRuneBudget)
}

func timelineEditHistoryKey(sequence int64, operationIndex int) string {
	return fmt.Sprintf("b%020d-o%020d", sequence, operationIndex)
}

func summarizeTimelineEditOperation(raw map[string]any) map[string]any {
	operation := sanitizeContextMap(raw)
	kind := agentexec.InterfaceString(operation["kind"])
	if kind != "recut_to_beats" {
		compacted, _ := compactEditHistoryValue(operation, 0).(map[string]any)
		if compacted == nil {
			return map[string]any{"kind": kind}
		}
		return compacted
	}

	result := map[string]any{"kind": kind}
	copyContextFields(result, operation,
		"bgm_asset_id", "target_duration_frames", "sfx_asset_id", "sfx_start_frame",
	)
	cutFrames := agentexec.EffectFrameValues(operation["cut_frames"])
	if len(cutFrames) > 0 {
		result["clip_count"] = len(cutFrames)
		result["first_cut_frame"] = cutFrames[0]
		result["last_cut_frame"] = cutFrames[len(cutFrames)-1]
	}
	result["video_asset_count"] = distinctContextStringCount(operation["video_asset_ids"])
	result["source_range_count"] = contextCollectionCount(operation["source_range_usage"])
	shotCount := contextCollectionCount(operation["shot_ids"])
	result["shot_count"] = shotCount
	result["uses_explicit_shots"] = shotCount > 0
	return result
}

func copyContextFields(target, source map[string]any, keys ...string) {
	for _, key := range keys {
		if value, exists := source[key]; exists && !emptyContextValue(value) {
			target[key] = compactEditHistoryValue(value, 1)
		}
	}
}

func compactEditHistoryValue(value any, depth int) any {
	if depth >= 5 {
		return map[string]any{"compacted": true, "value_type": fmt.Sprintf("%T", value)}
	}
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		result := make(map[string]any, minInt(len(keys), 24)+1)
		if kind, exists := typed["kind"]; exists {
			result["kind"] = compactEditHistoryValue(kind, depth+1)
		}
		kept := 0
		for _, key := range keys {
			if key == "kind" {
				continue
			}
			if kept >= 23 {
				break
			}
			result[key] = compactEditHistoryValue(typed[key], depth+1)
			kept++
		}
		nonKindFields := len(keys)
		if _, hasKind := typed["kind"]; hasKind {
			nonKindFields--
		}
		if omitted := nonKindFields - kept; omitted > 0 {
			result["omitted_field_count"] = omitted
		}
		return result
	case string:
		return truncateRunes(typed, 240)
	}

	reflected := reflect.ValueOf(value)
	if reflected.IsValid() && (reflected.Kind() == reflect.Slice || reflected.Kind() == reflect.Array) {
		length := reflected.Len()
		if length <= 8 {
			items := make([]any, 0, length)
			for index := 0; index < length; index++ {
				items = append(items, compactEditHistoryValue(reflected.Index(index).Interface(), depth+1))
			}
			return items
		}
		return map[string]any{
			"item_count": length,
			"first":      compactEditHistoryValue(reflected.Index(0).Interface(), depth+1),
			"last":       compactEditHistoryValue(reflected.Index(length-1).Interface(), depth+1),
		}
	}
	return value
}

func boundRecentEditHistory(entries []map[string]any, budget int) []map[string]any {
	if budget <= 0 || len(entries) == 0 {
		return nil
	}
	selected := make([]map[string]any, 0, len(entries))
	for index := len(entries) - 1; index >= 0; index-- {
		candidate := append([]map[string]any{entries[index]}, selected...)
		encoded, err := json.Marshal(candidate)
		if err != nil || utf8.RuneCount(encoded) > budget {
			continue
		}
		selected = candidate
	}
	if len(selected) > 0 {
		return selected
	}
	minimal := minimalEditHistoryEntry(entries[len(entries)-1])
	encoded, err := json.Marshal([]map[string]any{minimal})
	if err == nil && utf8.RuneCount(encoded) <= budget {
		return []map[string]any{minimal}
	}
	return nil
}

func minimalEditHistoryEntry(entry map[string]any) map[string]any {
	result := map[string]any{
		"actor": entry["actor"], "origin": entry["origin"],
	}
	if key, ok := entry["_history_key"].(string); ok {
		result["_history_key"] = key
	}
	operation, _ := entry["op"].(map[string]any)
	minimalOperation := map[string]any{"kind": operation["kind"]}
	if target := operationTarget(operation); target != "" {
		minimalOperation["target"] = truncateRunes(target, 120)
	}
	result["op"] = minimalOperation
	return result
}

func contextCollectionCount(value any) int {
	reflected := reflect.ValueOf(value)
	if !reflected.IsValid() || (reflected.Kind() != reflect.Slice && reflected.Kind() != reflect.Array) {
		return 0
	}
	return reflected.Len()
}

func distinctContextStringCount(value any) int {
	reflected := reflect.ValueOf(value)
	if !reflected.IsValid() || (reflected.Kind() != reflect.Slice && reflected.Kind() != reflect.Array) {
		return 0
	}
	seen := map[string]struct{}{}
	for index := 0; index < reflected.Len(); index++ {
		item := reflected.Index(index).Interface()
		if text, ok := item.(string); ok && text != "" {
			seen[text] = struct{}{}
		}
	}
	return len(seen)
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func rebuildEditIndexes(entries []map[string]any) (map[string]int, map[string]int) {
	coalesced := map[string]int{}
	inserted := map[string]int{}
	for index, entry := range entries {
		op, _ := entry["op"].(map[string]any)
		kind, _ := op["kind"].(string)
		target := operationTarget(op)
		if key := coalesceOperationKey(kind, op, target); key != "" {
			coalesced[key] = index
		}
		if kind == "insert_clip" && target != "" {
			inserted[target] = index
		}
	}
	return coalesced, inserted
}

func coalesceOperationKey(kind string, operation map[string]any, target string) string {
	switch kind {
	case "recut_to_beats", "compose_initial":
		return kind
	case "move_clip", "adjust_gain", "set_clip_fades", "set_clip_linked", "edit_subtitle_text", "set_playback_rate":
		return kind + ":" + target
	case "trim_clip", "trim_clip_edge":
		return kind + ":" + target + ":" + fmt.Sprint(operation["edge"])
	case "set_track_state":
		return kind + ":" + fmt.Sprint(operation["track_id"])
	default:
		return ""
	}
}

func operationTarget(operation map[string]any) string {
	for _, key := range []string{"timeline_clip_id", "clip_id", "track_id"} {
		if value, ok := operation[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

func sanitizeContextMap(input map[string]any) map[string]any {
	result := make(map[string]any, len(input))
	for key, value := range input {
		if isReservedContextKey(key) {
			continue
		}
		if key == "content_plan" {
			result[key] = sanitizeContentPlanForContext(value)
			continue
		}
		result[key] = sanitizeContextValue(value)
	}
	return result
}

func sanitizeContentPlanForContext(value any) any {
	plan, ok := value.(map[string]any)
	if !ok {
		return sanitizeContextValue(value)
	}
	result := make(map[string]any, len(plan))
	for key, item := range plan {
		if isReservedContextKey(key) {
			continue
		}
		if key == "contract" {
			result[key] = sanitizeContextValue(item)
			continue
		}
		result[key] = cloneContentPlanValue(item)
	}
	return result
}

func isReservedContextKey(key string) bool {
	switch key {
	case "timeline_version", "timeline_revision", "version", "timeline_id", "draft_id":
		return true
	default:
		return false
	}
}

func sanitizeContextValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return sanitizeContextMap(typed)
	case []map[string]any:
		result := make([]map[string]any, 0, len(typed))
		for _, item := range typed {
			result = append(result, sanitizeContextMap(item))
		}
		return result
	case []any:
		result := make([]any, 0, len(typed))
		for _, item := range typed {
			result = append(result, sanitizeContextValue(item))
		}
		return result
	default:
		return typed
	}
}

func emptyContextValue(value any) bool {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed) == ""
	case int:
		return typed == 0
	case float64:
		return typed == 0
	case bool:
		return !typed
	default:
		return value == nil
	}
}

func valueOrContext(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}
