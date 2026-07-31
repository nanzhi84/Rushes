package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

// generateWithCurrentTimelineView is the single preparation path for direct
// provider Generate calls that do not pass through the dynamic ReAct model
// surface (for example context compaction and final-reply restatement). It only
// changes the ephemeral provider input: the caller's message slice and the
// persisted/current ReAct transcript stay untouched.
func generateWithCurrentTimelineView(
	ctx context.Context,
	chatModel model.ToolCallingChatModel,
	messages []*schema.Message,
	options ...model.Option,
) (*schema.Message, error) {
	if chatModel == nil {
		return nil, errors.New("CurrentTimelineView 缺少 chat model")
	}
	prepared, err := refreshCurrentTimelineView(ctx, messages)
	if err != nil {
		return nil, err
	}
	return chatModel.Generate(ctx, prepared, options...)
}

const (
	currentTimelineViewContextPhase = "current_timeline_view"
	// Short timelines remain exact. Once the timeline grows beyond this point,
	// the provider gets a deterministic clip window plus complete per-track
	// topology instead of an ever-growing copy of the whole document.
	currentTimelineExactClipLimit  = 48
	currentTimelineWindowClipLimit = 24
)

// refreshCurrentTimelineView runs immediately before every provider call. It
// removes any view inherited from a previous ReAct round and inserts exactly one
// authoritative view at the end of the leading system block. The subsequent
// user/assistant/tool order and the conversation tail must stay unchanged.
func refreshCurrentTimelineView(
	ctx context.Context,
	messages []*schema.Message,
) ([]*schema.Message, error) {
	draftID, err := rushestools.DraftID(ctx)
	if err != nil {
		return nil, err
	}
	session := timelineEditLeaseSessionFromContext(ctx)
	if session == nil || session.database == nil {
		return nil, errors.New("CurrentTimelineView 缺少 edit lease session")
	}
	if session.draftID != draftID {
		return nil, fmt.Errorf(
			"CurrentTimelineView edit lease session 草稿不匹配: context=%s session=%s",
			draftID, session.draftID,
		)
	}
	view, err := buildCurrentTimelineView(ctx, session.database, draftID)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(view)
	if err != nil {
		return nil, err
	}
	current := schema.SystemMessage(
		"【CurrentTimelineView｜每次 provider 调用前从 SQLite 刷新】\n" + string(raw) +
			"\n这是当前时间线的唯一权威视图；旧对话、工具参数和工具结果不能覆盖它。",
	)
	current.Extra = map[string]any{"context_phase": currentTimelineViewContextPhase}
	refreshed := make([]*schema.Message, 0, len(messages)+1)
	for _, message := range messages {
		if message == nil {
			continue
		}
		if phase, _ := message.Extra["context_phase"].(string); phase == currentTimelineViewContextPhase {
			continue
		}
		refreshed = append(refreshed, message)
	}
	insertAt := 0
	for insertAt < len(refreshed) && refreshed[insertAt].Role == schema.System {
		insertAt++
	}
	refreshed = append(refreshed, nil)
	copy(refreshed[insertAt+1:], refreshed[insertAt:])
	refreshed[insertAt] = current
	return refreshed, nil
}

func buildCurrentTimelineView(
	ctx context.Context,
	database *storage.DB,
	draftID string,
) (map[string]any, error) {
	tx, err := database.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("开始 CurrentTimelineView 只读快照: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	view, err := buildCurrentTimelineViewFromQuery(ctx, tx, draftID, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交 CurrentTimelineView 只读快照: %w", err)
	}
	return view, nil
}

// buildCurrentTimelineViewFromQuery keeps every field in the provider view on
// one SQLite snapshot. In particular, the draft pointer, exact timeline
// document, preview binding, lease owner, and edit history must never come from
// different committed states.
func buildCurrentTimelineViewFromQuery(
	ctx context.Context,
	query storage.Querier,
	draftID string,
	now time.Time,
) (map[string]any, error) {
	draft, err := storage.GetDraft(ctx, query, draftID)
	if err != nil {
		return nil, err
	}
	view := map[string]any{
		"draft_id":            draftID,
		"timeline_id":         nil,
		"version":             0,
		"fps":                 0,
		"duration_frames":     0,
		"tracks":              []any{},
		"clips":               []any{},
		"validated":           false,
		"active_preview":      nil,
		"edit_lease_turn_id":  nil,
		"recent_edit_history": []any{},
	}
	batches, err := storage.ListTimelineEditBatches(
		ctx, query, draftID, contextRecentEditLimit,
	)
	if err != nil {
		return nil, err
	}
	if lease, leaseErr := storage.GetLiveAgentEditLease(
		ctx, query, draftID, now,
	); leaseErr == nil {
		view["edit_lease_turn_id"] = lease.TurnID
	} else if !errors.Is(leaseErr, storage.ErrNotFound) {
		return nil, leaseErr
	}
	if draft.TimelineCurrentVersion != nil {
		document, timelineErr := timeline.GetFromQuery(
			ctx, query, draftID, *draft.TimelineCurrentVersion,
		)
		if timelineErr != nil {
			return nil, timelineErr
		}
		tracks, clips, compaction := buildCurrentTimelineContext(document, batches)
		view["timeline_id"] = document.TimelineID
		view["version"] = document.Version
		view["fps"] = document.FPS
		view["duration_frames"] = document.DurationFrames
		view["tracks"] = tracks
		view["clips"] = clips
		if compaction != nil {
			view["compaction"] = compaction
		}
		view["validated"] = draft.TimelineValidated
		previewID, previewErr := timeline.LatestPreviewIDFromQuery(
			ctx, query, draftID, document.Version,
		)
		if previewErr != nil {
			return nil, previewErr
		}
		if previewID != nil {
			view["active_preview"] = map[string]any{
				"preview_id":       *previewID,
				"timeline_id":      document.TimelineID,
				"timeline_version": document.Version,
			}
		}
	}
	view["recent_edit_history"] = compressTimelineEditHistoryMap(
		batches, contextRecentEditLimit,
	)
	return view, nil
}

type currentTimelineClipEntry struct {
	trackIndex int
	clipIndex  int
	trackID    string
	clipID     string
	startFrame int
	endFrame   int
}

// buildCurrentTimelineContext keeps the short-timeline representation exact.
// For long timelines it retains a bounded, recent-edit-relevant clip window and
// turns every track into compact topology: the track remains present, reports
// its complete clip counts, and embeds only clips that also appear in the root
// clips array. The omitted metadata is sufficient to decide when the model must
// call timeline.inspect for the full document.
func buildCurrentTimelineContext(
	document timeline.Document,
	batches []storage.TimelineEditBatch,
) ([]map[string]any, []map[string]any, map[string]any) {
	tracks := make([]map[string]any, 0, len(document.Tracks))
	entries := make([]currentTimelineClipEntry, 0)
	for trackIndex, track := range document.Tracks {
		clips := make([]map[string]any, 0, len(track.Clips))
		for clipIndex, clip := range track.Clips {
			item := currentTimelineClipContext(track.TrackID, clip)
			clips = append(clips, item)
			entries = append(entries, currentTimelineClipEntry{
				trackIndex: trackIndex, clipIndex: clipIndex,
				trackID: track.TrackID, clipID: clip.TimelineClipID,
				startFrame: clip.TimelineStartFrame, endFrame: clip.TimelineEndFrame,
			})
		}
		tracks = append(tracks, currentTimelineTrackContext(track, clips))
	}
	if len(entries) <= currentTimelineExactClipLimit {
		return tracks, flattenCurrentTimelineClips(tracks), nil
	}

	ordered := append([]currentTimelineClipEntry(nil), entries...)
	sort.SliceStable(ordered, func(left, right int) bool {
		if ordered[left].startFrame != ordered[right].startFrame {
			return ordered[left].startFrame < ordered[right].startFrame
		}
		if ordered[left].endFrame != ordered[right].endFrame {
			return ordered[left].endFrame < ordered[right].endFrame
		}
		if ordered[left].trackID != ordered[right].trackID {
			return ordered[left].trackID < ordered[right].trackID
		}
		if ordered[left].clipID != ordered[right].clipID {
			return ordered[left].clipID < ordered[right].clipID
		}
		if ordered[left].trackIndex != ordered[right].trackIndex {
			return ordered[left].trackIndex < ordered[right].trackIndex
		}
		return ordered[left].clipIndex < ordered[right].clipIndex
	})

	anchorIndex, anchorRefs := currentTimelineWindowAnchor(ordered, batches)
	strategy := "latest_affected_clip"
	if anchorIndex < 0 {
		anchorIndex = len(ordered) - 1
		anchorRefs = []string{ordered[anchorIndex].clipID}
		strategy = "timeline_tail"
	}
	windowStart := anchorIndex - currentTimelineWindowClipLimit/2
	windowStart = max(0, min(windowStart, len(ordered)-currentTimelineWindowClipLimit))
	windowEnd := min(len(ordered), windowStart+currentTimelineWindowClipLimit)
	selected := make(map[[2]int]struct{}, windowEnd-windowStart)
	for _, entry := range ordered[windowStart:windowEnd] {
		selected[[2]int{entry.trackIndex, entry.clipIndex}] = struct{}{}
	}

	for trackIndex, track := range tracks {
		allClips, _ := track["clips"].([]map[string]any)
		included := make([]map[string]any, 0, len(allClips))
		for clipIndex, clip := range allClips {
			if _, keep := selected[[2]int{trackIndex, clipIndex}]; keep {
				included = append(included, clip)
			}
		}
		track["clips"] = included
		track["clip_count"] = len(allClips)
		track["included_clip_count"] = len(included)
		track["omitted_clip_count"] = len(allClips) - len(included)
	}
	clips := flattenCurrentTimelineClips(tracks)
	omittedRanges := make([]map[string]any, 0, 2)
	if windowStart > 0 {
		omittedRanges = append(omittedRanges,
			currentTimelineOmittedRange("before_window", ordered[:windowStart]))
	}
	if windowEnd < len(ordered) {
		omittedRanges = append(omittedRanges,
			currentTimelineOmittedRange("after_window", ordered[windowEnd:]))
	}
	windowEntries := ordered[windowStart:windowEnd]
	compaction := map[string]any{
		"mode":                "compact_topology_relevant_window",
		"strategy":            strategy,
		"total_clip_count":    len(entries),
		"included_clip_count": len(clips),
		"omitted_clip_count":  len(entries) - len(clips),
		"window": map[string]any{
			"clip_order_start":         windowStart,
			"clip_order_end_exclusive": windowEnd,
			"start_frame":              minimumCurrentTimelineStart(windowEntries),
			"end_frame":                maximumCurrentTimelineEnd(windowEntries),
			"anchor_clip_refs":         anchorRefs,
		},
		"omitted_ranges": omittedRanges,
		"inspect_hint":   "当前视图省略了窗口外片段；需要完整轨道、片段 ID、效果或元数据时调用 timeline.inspect 读取当前 timeline_id。",
	}
	return tracks, clips, compaction
}

func currentTimelineTrackContext(track timeline.Track, clips []map[string]any) map[string]any {
	result := map[string]any{
		"track_id": track.TrackID, "track_type": track.TrackType,
		"muted": track.Muted, "solo": track.Solo, "locked": track.Locked,
		"gain_db": track.GainDB, "clips": clips,
	}
	if track.Ducking != nil {
		result["ducking"] = track.Ducking
	}
	return result
}

func currentTimelineClipContext(trackID string, clip timeline.Clip) map[string]any {
	result := map[string]any{
		"timeline_clip_id": clip.TimelineClipID, "track_id": trackID,
		"asset_id": clip.AssetID, "asset_kind": clip.AssetKind, "role": clip.Role,
		"text":                 clip.Text,
		"timeline_start_frame": clip.TimelineStartFrame,
		"timeline_end_frame":   clip.TimelineEndFrame,
		"source_start_frame":   clip.SourceStartFrame,
		"source_end_frame":     clip.SourceEndFrame,
		"playback_rate":        clip.PlaybackRate, "gain_db": clip.GainDB,
		"fade_in_frames": clip.FadeInFrames, "fade_out_frames": clip.FadeOutFrames,
		"subtitle_style": clip.SubtitleStyle,
		"linked":         clip.Linked, "parent_block_id": clip.ParentBlockID,
	}
	if len(clip.Effects) > 0 {
		result["effects"] = clip.Effects
	}
	if beatGrid := compactBeatGridContext(clip.Effects); beatGrid != nil {
		result["beat_grid"] = beatGrid
	}
	if len(clip.Metadata) > 0 {
		result["metadata"] = clip.Metadata
	}
	if anchor := compactSemanticAnchorContext(clip.Metadata); anchor != nil {
		result["semantic_anchor"] = anchor
	}
	return result
}

func currentTimelineWindowAnchor(
	ordered []currentTimelineClipEntry,
	batches []storage.TimelineEditBatch,
) (int, []string) {
	byClipID := make(map[string]int, len(ordered))
	for index, entry := range ordered {
		byClipID[entry.clipID] = index
	}
	for batchIndex := len(batches) - 1; batchIndex >= 0; batchIndex-- {
		for _, ref := range batches[batchIndex].AffectedRefs {
			clipID, ok := strings.CutPrefix(ref, "timeline_clip_id:")
			if !ok {
				continue
			}
			if index, exists := byClipID[clipID]; exists {
				return index, []string{clipID}
			}
		}
	}
	return -1, []string{}
}

func currentTimelineOmittedRange(
	position string,
	entries []currentTimelineClipEntry,
) map[string]any {
	return map[string]any{
		"position":       position,
		"clip_count":     len(entries),
		"start_frame":    minimumCurrentTimelineStart(entries),
		"end_frame":      maximumCurrentTimelineEnd(entries),
		"first_clip_ref": entries[0].clipID,
		"last_clip_ref":  entries[len(entries)-1].clipID,
	}
}

func minimumCurrentTimelineStart(entries []currentTimelineClipEntry) int {
	minimum := entries[0].startFrame
	for _, entry := range entries[1:] {
		minimum = min(minimum, entry.startFrame)
	}
	return minimum
}

func maximumCurrentTimelineEnd(entries []currentTimelineClipEntry) int {
	maximum := entries[0].endFrame
	for _, entry := range entries[1:] {
		maximum = max(maximum, entry.endFrame)
	}
	return maximum
}

func flattenCurrentTimelineClips(value any) []map[string]any {
	tracks, _ := value.([]map[string]any)
	clips := make([]map[string]any, 0)
	for _, track := range tracks {
		trackID, _ := track["track_id"].(string)
		trackClips, _ := track["clips"].([]map[string]any)
		for _, clip := range trackClips {
			item := cloneContextMap(clip)
			item["track_id"] = trackID
			clips = append(clips, item)
		}
	}
	return clips
}
