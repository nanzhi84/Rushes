package agentexec

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func TestPrepareTimelineBatchReordersFullPrimaryReplacementAndPreservesAudio(t *testing.T) {
	document, err := timeline.ComposeInitial("draft_batch_integrity", 1, []timeline.Selection{
		{AssetID: "old_a", AssetKind: "video", SourceEndFrame: 30},
		{AssetID: "old_b", AssetKind: "video", SourceEndFrame: 30},
	})
	if err != nil {
		t.Fatal(err)
	}
	document.Tracks[4].Clips = []timeline.Clip{{
		TimelineClipID: "bgm_keep", TrackID: "bgm", AssetID: "music", AssetKind: "audio",
		TimelineStartFrame: 0, TimelineEndFrame: 60, SourceEndFrame: 60, PlaybackRate: 1,
		Effects: []map[string]any{{"kind": "beat_grid", "beat_frames": []int{10, 20, 30, 40, 50}}},
	}}
	document.Tracks[6].Clips = []timeline.Clip{{
		TimelineClipID: "sfx_keep", TrackID: "sfx", AssetID: "hit", AssetKind: "audio",
		TimelineStartFrame: 40, TimelineEndFrame: 50, SourceEndFrame: 10, PlaybackRate: 1,
	}}
	operations := []map[string]any{
		{"kind": "delete_clip", "timeline_clip_id": "clip_v1_001"},
		{"kind": "delete_clip", "timeline_clip_id": "clip_v1_002"},
		{"kind": "insert_clip", "track_id": "visual_base", "asset_id": "new_a", "asset_kind": "video", "source_start_frame": 0, "source_end_frame": 20},
		{"kind": "insert_clip", "track_id": "visual_base", "asset_id": "new_b", "asset_kind": "video", "source_start_frame": 0, "source_end_frame": 40},
	}

	planned, preserved := PrepareTimelineBatch(document, operations)
	if StringValue(planned[0]["kind"]) != "insert_clip" || StringValue(planned[1]["kind"]) != "insert_clip" {
		t.Fatalf("full replacement was not reordered safely: %#v", planned)
	}
	result := document
	for _, operation := range planned {
		result, err = timeline.ApplyPatch(result, operation)
		if err != nil {
			t.Fatalf("operation=%#v err=%v", operation, err)
		}
	}
	if err := RestoreIndependentAudioTracks(&result, preserved); err != nil {
		t.Fatal(err)
	}
	if report := timeline.Validate(result); !report.Valid {
		t.Fatalf("report=%#v document=%#v", report, result)
	}
	if result.DurationFrames != 60 || len(result.Tracks[0].Clips) != 2 ||
		result.Tracks[0].Clips[0].AssetID != "new_a" || result.Tracks[0].Clips[1].AssetID != "new_b" {
		t.Fatalf("primary=%#v duration=%d", result.Tracks[0].Clips, result.DurationFrames)
	}
	if !reflect.DeepEqual(result.Tracks[4], document.Tracks[4]) ||
		!reflect.DeepEqual(result.Tracks[6], document.Tracks[6]) {
		t.Fatalf("independent audio changed: bgm=%#v sfx=%#v", result.Tracks[4], result.Tracks[6])
	}
}

func TestIndependentAudioGuardRejectsPartialPrimaryDeletion(t *testing.T) {
	document, err := timeline.ComposeInitial("draft_partial_delete", 1, []timeline.Selection{
		{AssetID: "old_a", AssetKind: "video", SourceEndFrame: 30},
		{AssetID: "old_b", AssetKind: "video", SourceEndFrame: 30},
	})
	if err != nil {
		t.Fatal(err)
	}
	document.Tracks[4].Clips = []timeline.Clip{{
		TimelineClipID: "bgm_full", TrackID: "bgm", AssetID: "music", AssetKind: "audio",
		TimelineEndFrame: 60, SourceEndFrame: 60,
	}}
	planned, preserved := PrepareTimelineBatch(document, []map[string]any{{
		"kind": "delete_clip", "timeline_clip_id": "clip_v1_001",
	}})
	result, err := timeline.ApplyPatch(document, planned[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := RestoreIndependentAudioTracks(&result, preserved); err == nil {
		t.Fatalf("partial replacement must not silently truncate BGM: %#v", result)
	}
}

func TestPrepareTimelineBatchAllowsExplicitAudioEdit(t *testing.T) {
	document, err := timeline.ComposeInitial("draft_audio_edit", 1, []timeline.Selection{{
		AssetID: "video", AssetKind: "video", SourceEndFrame: 60,
	}})
	if err != nil {
		t.Fatal(err)
	}
	document.Tracks[4].Clips = []timeline.Clip{{
		TimelineClipID: "bgm_edit", TrackID: "bgm", AssetID: "music", AssetKind: "audio",
		TimelineEndFrame: 60, SourceEndFrame: 60,
	}}
	_, preserved := PrepareTimelineBatch(document, []map[string]any{{
		"kind": "adjust_gain", "timeline_clip_id": "bgm_edit", "gain_db": -8,
	}})
	if _, exists := preserved["bgm"]; exists {
		t.Fatalf("explicitly edited BGM must not be restored: %#v", preserved)
	}
}

func TestApplyPatchesAtomicallyReplacesPrimaryWithoutChangingBGMOrSFX(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_atomic_primary")
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	document, err := timeline.ComposeInitial("draft_atomic_primary", 1, []timeline.Selection{
		{AssetID: "old_a", AssetKind: "video", SourceEndFrame: 30},
		{AssetID: "old_b", AssetKind: "video", SourceEndFrame: 30},
	})
	if err != nil {
		t.Fatal(err)
	}
	document.Tracks[4].Clips = []timeline.Clip{{
		TimelineClipID: "bgm_keep", TrackID: "bgm", AssetID: "music", AssetKind: "audio",
		TimelineEndFrame: 60, SourceEndFrame: 60, PlaybackRate: 1,
		Effects: []map[string]any{{"kind": "beat_grid", "beat_frames": []int{10, 20, 30, 40, 50}}},
	}}
	document.Tracks[6].Clips = []timeline.Clip{{
		TimelineClipID: "sfx_keep", TrackID: "sfx", AssetID: "hit", AssetKind: "audio",
		TimelineStartFrame: 40, TimelineEndFrame: 50, SourceEndFrame: 10, PlaybackRate: 1,
	}}
	if persisted, persistErr := exec.PersistTimeline(t.Context(), "draft_atomic_primary", document, "fixture"); persistErr != nil || persisted.Status != "succeeded" {
		t.Fatalf("persisted=%#v err=%v", persisted, persistErr)
	}
	ctx := rushestools.WithDraftID(t.Context(), "draft_atomic_primary")
	output, err := exec.ExecuteTool(ctx, "timeline.apply_patches", rushestools.TimelinePatchBatchInput{Ops: []rushestools.TimelineOp{
		{"kind": "delete_clip", "timeline_clip_id": "clip_v1_001"},
		{"kind": "delete_clip", "timeline_clip_id": "clip_v1_002"},
		{"kind": "insert_clip", "track_id": "visual_base", "asset_id": "new_a", "asset_kind": "video", "source_start_frame": 0, "source_end_frame": 20},
		{"kind": "insert_clip", "track_id": "visual_base", "asset_id": "new_b", "asset_kind": "video", "source_start_frame": 0, "source_end_frame": 40},
	}})
	if err != nil || output.(rushestools.ToolResult).Status != "succeeded" {
		t.Fatalf("output=%#v err=%v", output, err)
	}
	latest, err := timeline.Latest(t.Context(), database, "draft_atomic_primary")
	if err != nil || len(latest.Tracks[4].Clips) != 1 || len(latest.Tracks[6].Clips) != 1 ||
		latest.Tracks[4].Clips[0].TimelineClipID != "bgm_keep" ||
		latest.Tracks[4].Clips[0].TimelineEndFrame != 60 ||
		!HasBeatGrid(latest.Tracks[4].Clips[0].Effects) ||
		latest.Tracks[6].Clips[0].TimelineClipID != "sfx_keep" ||
		latest.Tracks[6].Clips[0].TimelineStartFrame != 40 {
		t.Fatalf("latest=%#v err=%v", latest, err)
	}
	failed, err := exec.ExecuteTool(ctx, "timeline.apply_patches", rushestools.TimelinePatchBatchInput{Ops: []rushestools.TimelineOp{{
		"kind": "delete_clip", "timeline_clip_id": latest.Tracks[0].Clips[0].TimelineClipID,
	}}})
	if err != nil || failed.(rushestools.ToolResult).Status != "failed" {
		t.Fatalf("partial delete=%#v err=%v", failed, err)
	}
	unchanged, err := timeline.Latest(t.Context(), database, "draft_atomic_primary")
	if err != nil || unchanged.Version != latest.Version || len(unchanged.Tracks[4].Clips) != 1 ||
		unchanged.Tracks[4].Clips[0].TimelineEndFrame != 60 ||
		!HasBeatGrid(unchanged.Tracks[4].Clips[0].Effects) {
		t.Fatalf("failed batch changed timeline: %#v err=%v", unchanged, err)
	}
}

func TestSyncOriginalAudioRepairsDriftedTimelineFromAssetProbe(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_sync_original_audio")
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{
		{Type: "AssetImported", Payload: map[string]any{
			"asset_id": "talk", "job_id": "job_talk", "kind": "video", "filename": "talk.mp4",
			"usable": true, "probe": map[string]any{"duration_sec": 4.0, "has_audio": true},
		}},
		{Type: "AssetLinked", DraftID: "draft_sync_original_audio", Payload: map[string]any{"asset_id": "talk"}},
	}, reducer.Options{Actor: contracts.ActorUser})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("asset result=%#v err=%v", result, err)
	}
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	document, err := timeline.ComposeInitial("draft_sync_original_audio", 1, []timeline.Selection{
		{AssetID: "talk", AssetKind: "video", SourceStartFrame: 0, SourceEndFrame: 60, Role: "a_roll", HasAudio: true},
		{AssetID: "talk", AssetKind: "video", SourceStartFrame: 60, SourceEndFrame: 120, Role: "a_roll", HasAudio: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	document.Tracks[2].Clips[0].TimelineStartFrame = 10
	document.Tracks[2].Clips[0].TimelineEndFrame = 70
	if persisted, persistErr := exec.PersistTimeline(t.Context(), "draft_sync_original_audio", document, "drifted_fixture"); persistErr != nil || persisted.Status != "validation_failed" {
		t.Fatalf("persisted=%#v err=%v", persisted, persistErr)
	}

	ctx := rushestools.WithDraftID(t.Context(), "draft_sync_original_audio")
	raw, err := exec.ExecuteTool(ctx, "timeline.apply_patches", rushestools.TimelinePatchBatchInput{
		Ops: []rushestools.TimelineOp{{"kind": "sync_original_audio"}},
	})
	if err != nil || raw.(rushestools.ToolResult).Status != "succeeded" {
		t.Fatalf("sync=%#v err=%v", raw, err)
	}
	latest, err := timeline.Latest(t.Context(), database, "draft_sync_original_audio")
	if err != nil || !timeline.Validate(latest).Valid || len(latest.Tracks[2].Clips) != 2 {
		t.Fatalf("latest=%#v report=%#v err=%v", latest, timeline.Validate(latest), err)
	}
	for index := range latest.Tracks[0].Clips {
		visual := latest.Tracks[0].Clips[index]
		audio := latest.Tracks[2].Clips[index]
		if visual.TimelineStartFrame != audio.TimelineStartFrame || visual.TimelineEndFrame != audio.TimelineEndFrame ||
			visual.SourceStartFrame != audio.SourceStartFrame || visual.SourceEndFrame != audio.SourceEndFrame {
			t.Fatalf("visual=%#v audio=%#v", visual, audio)
		}
	}
}

func TestBeatAlignmentDataDistinguishesStructureFromBeatSync(t *testing.T) {
	document, err := timeline.ComposeInitial("draft_alignment", 1, []timeline.Selection{
		{AssetID: "video_a", AssetKind: "video", SourceEndFrame: 30},
		{AssetID: "video_b", AssetKind: "video", SourceEndFrame: 30},
		{AssetID: "video_c", AssetKind: "video", SourceEndFrame: 30},
	})
	if err != nil {
		t.Fatal(err)
	}
	missing := BeatAlignmentData(document)
	if missing["beat_grid_present"] != false || missing["cut_count"] != 2 {
		t.Fatalf("missing=%#v", missing)
	}
	document.Tracks[4].Clips = []timeline.Clip{{
		TimelineClipID: "bgm_grid", TrackID: "bgm", AssetID: "music", AssetKind: "audio",
		TimelineEndFrame: 90, SourceEndFrame: 90, PlaybackRate: 1,
		Effects: []map[string]any{{
			"kind": "beat_grid", "beat_frames": []any{30.0, 45.0, 60.0},
			"strong_beat_frames": []int{60}, "downbeat_frames": []int{30},
		}},
	}}
	aligned := BeatAlignmentData(document)
	if aligned["beat_grid_present"] != true || aligned["on_beat_cut_count"] != 2 ||
		aligned["on_accent_cut_count"] != 2 || aligned["all_cuts_on_beat_grid"] != true {
		t.Fatalf("aligned=%#v", aligned)
	}
	document.Tracks[4].Clips[0].Effects = nil
	document.Tracks[4].Clips[0].Metadata = map[string]any{
		"beat_grid": map[string]any{
			"bpm": 120, "beat_frames": []any{30.0, 45.0, 60.0},
			"strong_beat_frames": []int{60}, "downbeat_frames": []int{30},
			"analysis_method": "fixture",
		},
	}
	metadataAligned := BeatAlignmentData(document)
	if metadataAligned["beat_grid_present"] != true ||
		metadataAligned["on_beat_cut_count"] != 2 ||
		metadataAligned["on_accent_cut_count"] != 2 ||
		metadataAligned["all_cuts_on_beat_grid"] != true {
		t.Fatalf("metadata aligned=%#v", metadataAligned)
	}
}

func TestBeatAlignmentDataUsesToleranceAndExcludesContinuousSourceSplits(t *testing.T) {
	document := timeline.Empty("draft_alignment_tolerance", 1)
	document.FPS = 30
	document.DurationFrames = 90
	document.Tracks[0].Clips = []timeline.Clip{
		{TrackID: "visual_base", AssetID: "video_a", TimelineStartFrame: 0, TimelineEndFrame: 31, SourceStartFrame: 0, SourceEndFrame: 31},
		{TrackID: "visual_base", AssetID: "video_b", TimelineStartFrame: 31, TimelineEndFrame: 60, SourceStartFrame: 0, SourceEndFrame: 29},
		{TrackID: "visual_base", AssetID: "video_b", TimelineStartFrame: 60, TimelineEndFrame: 62, SourceStartFrame: 29, SourceEndFrame: 31},
		{TrackID: "visual_base", AssetID: "video_c", TimelineStartFrame: 62, TimelineEndFrame: 90, SourceStartFrame: 0, SourceEndFrame: 28},
	}
	document.Tracks[4].Clips = []timeline.Clip{{
		TrackID: "bgm", AssetID: "music", TimelineEndFrame: 90, SourceEndFrame: 90, PlaybackRate: 1,
		Effects: []map[string]any{{"kind": "beat_grid", "beat_frames": []int{30, 60}}},
	}}

	alignment := BeatAlignmentData(document)
	if alignment["cut_count"] != 2 || alignment["on_beat_cut_count"] != 1 ||
		alignment["alignment_ratio"] != 0.5 {
		t.Fatalf("alignment=%#v", alignment)
	}
	offBeat, ok := alignment["off_beat_cut_frames"].([]int)
	if !ok || len(offBeat) != 1 || offBeat[0] != 62 {
		t.Fatalf("off-beat cuts=%#v alignment=%#v", offBeat, alignment)
	}
}

func TestGenericBGMInsertAutomaticallyAttachesBeatGrid(t *testing.T) {
	fakeBin := t.TempDir()
	for name, body := range map[string]string{
		"aubiotrack": "#!/bin/sh\nprintf '0.333333\\n0.666667\\n1.000000\\n1.333333\\n1.666667\\n2.000000\\n'\n",
		"aubioonset": "#!/bin/sh\nprintf '0.333333\\n1.000000\\n1.666667\\n'\n",
	} {
		if err := os.WriteFile(filepath.Join(fakeBin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_generic_bgm")
	source := filepath.Join(database.Paths.Temporary, "generic-bgm.wav")
	if err := os.WriteFile(source, []byte("fake source"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{
		{Type: "AssetImported", Payload: map[string]any{
			"asset_id": "generic_bgm", "job_id": "job_generic_bgm", "storage_mode": "reference",
			"reference_path": source, "kind": "audio", "source": "local_path", "filename": "music.wav",
			"hash": "generic_bgm_hash", "size": 11, "ingest_status": "ready", "usable": true,
			"probe": map[string]any{"duration_sec": 2.0, "has_audio": true},
		}},
		{Type: "AssetLinked", DraftID: "draft_generic_bgm", Payload: map[string]any{
			"asset_id": "generic_bgm", "linked_at": now,
		}},
	}, reducer.Options{Actor: contracts.ActorUser})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("assets status=%s err=%v", result.Status, err)
	}
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	document, err := timeline.ComposeInitial("draft_generic_bgm", 1, []timeline.Selection{{
		AssetID: "video", AssetKind: "video", SourceEndFrame: 60,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if persisted, persistErr := exec.PersistTimeline(t.Context(), "draft_generic_bgm", document, "fixture"); persistErr != nil || persisted.Status != "succeeded" {
		t.Fatalf("persisted=%#v err=%v", persisted, persistErr)
	}
	ctx := rushestools.WithDraftID(t.Context(), "draft_generic_bgm")
	output, err := exec.ExecuteTool(ctx, "timeline.apply_patches", rushestools.TimelinePatchBatchInput{Ops: []rushestools.TimelineOp{{
		"kind": "insert_clip", "track_id": "bgm", "asset_id": "generic_bgm",
		"role": "bgm", "timeline_start_frame": 0, "source_start_frame": 0, "source_end_frame": 60,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	toolResult := output.(rushestools.ToolResult)
	if toolResult.Status != "succeeded" || toolResult.Data["beat_grid_attached_count"] != 1 {
		t.Fatalf("tool result=%#v", toolResult)
	}
	latest, err := timeline.Latest(t.Context(), database, "draft_generic_bgm")
	if err != nil || len(latest.Tracks[4].Clips) != 1 || latest.Tracks[4].Clips[0].AssetKind != "audio" ||
		!HasBeatGrid(latest.Tracks[4].Clips[0].Effects) {
		t.Fatalf("latest=%#v err=%v", latest, err)
	}
}
