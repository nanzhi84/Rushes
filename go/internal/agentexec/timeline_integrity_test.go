package agentexec

import (
	"math"
	"reflect"
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func TestIndependentAudioPreservationAllowsPrimaryShrinkWithoutTruncatingBGM(t *testing.T) {
	document, err := agenttest.ComposeTimeline("draft_partial_delete", 1, []agenttest.TimelineSelection{
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
	operation := map[string]any{
		"kind": "delete_clip", "timeline_clip_id": "clip_v1_001",
	}
	preserved := preserveIndependentAudioForOperation(document, operation)
	result, err := timeline.ApplyPatch(document, operation)
	if err != nil {
		t.Fatal(err)
	}
	restoreIndependentAudioTracks(&result, preserved)
	if report := timeline.Validate(result); !report.Valid {
		t.Fatalf("preserved overhang must remain structurally valid: %#v", report)
	}
	if result.DurationFrames != 30 || len(result.Tracks[4].Clips) != 1 ||
		result.Tracks[4].Clips[0].TimelineEndFrame != 60 ||
		result.Tracks[4].Clips[0].SourceEndFrame != 60 {
		t.Fatalf("partial replacement truncated untouched BGM: %#v", result)
	}
}

func TestAtomicPrimaryDeletionPreservesIndependentAudioAcrossLaterRegrowth(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_preserve_audio_overhang"
	agenttest.CreateAgentDraft(t, database, draftID)
	insertAtomicTimelineAsset(t, database, draftID, "old_a", "video", 1, false)
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	document, err := agenttest.ComposeTimeline(draftID, 1, []agenttest.TimelineSelection{
		{AssetID: "old_a", AssetKind: "video", SourceEndFrame: 30},
		{AssetID: "old_b", AssetKind: "video", SourceEndFrame: 30},
	})
	if err != nil {
		t.Fatal(err)
	}
	document.Tracks[4].Clips = []timeline.Clip{{
		TimelineClipID: "bgm_full", TrackID: "bgm", AssetID: "music", AssetKind: "audio",
		TimelineEndFrame: 60, SourceEndFrame: 60, PlaybackRate: 1,
	}}
	document.Tracks[6].Clips = []timeline.Clip{{
		TimelineClipID: "sfx_tail", TrackID: "sfx", AssetID: "effect", AssetKind: "audio",
		TimelineStartFrame: 45, TimelineEndFrame: 60,
		SourceStartFrame: 0, SourceEndFrame: 15, PlaybackRate: 1,
	}}
	if persisted, persistErr := seedTimelineVersion(
		exec, t.Context(), draftID, document, "fixture", nil,
	); persistErr != nil || persisted.Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("persisted=%#v err=%v", persisted, persistErr)
	}

	ctx := rushestools.WithDraftID(t.Context(), draftID)
	deleted := executeAtomicTimelineTool(t, exec, ctx, "timeline.delete", rushestools.TimelineDeleteInput{
		"kind": "delete_clip", "timeline_clip_id": "clip_v1_001",
	})
	for _, target := range deleted.Data["changed_targets"].([]map[string]any) {
		if ContainsString(independentAudioTrackIDs, StringValue(target["track_id"])) {
			t.Fatalf("untouched independent audio must not be reported as changed: %#v", deleted.Data)
		}
	}
	shrunk, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil || shrunk.DurationFrames != 30 || !timeline.Validate(shrunk).Valid ||
		shrunk.Tracks[4].Clips[0].TimelineEndFrame != 60 ||
		shrunk.Tracks[4].Clips[0].SourceEndFrame != 60 ||
		shrunk.Tracks[6].Clips[0].TimelineStartFrame != 45 ||
		shrunk.Tracks[6].Clips[0].TimelineEndFrame != 60 {
		t.Fatalf("shrunk=%#v report=%#v err=%v", shrunk, timeline.Validate(shrunk), err)
	}

	executeAtomicTimelineTool(t, exec, ctx, "timeline.insert", rushestools.TimelineInsertInput{
		"kind": "insert_clip", "asset_id": "old_a",
		"source_start_frame": 0, "source_end_frame": 30,
	})
	regrown, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil || regrown.DurationFrames != 60 || !timeline.Validate(regrown).Valid ||
		regrown.Tracks[4].Clips[0].TimelineEndFrame != 60 ||
		regrown.Tracks[4].Clips[0].SourceEndFrame != 60 ||
		regrown.Tracks[6].Clips[0].TimelineStartFrame != 45 ||
		regrown.Tracks[6].Clips[0].TimelineEndFrame != 60 {
		t.Fatalf("regrown=%#v report=%#v err=%v", regrown, timeline.Validate(regrown), err)
	}
}

func TestIndependentAudioGuardAllowsExplicitAudioEdit(t *testing.T) {
	document, err := agenttest.ComposeTimeline("draft_audio_edit", 1, []agenttest.TimelineSelection{{
		AssetID: "video", AssetKind: "video", SourceEndFrame: 60,
	}})
	if err != nil {
		t.Fatal(err)
	}
	document.Tracks[4].Clips = []timeline.Clip{{
		TimelineClipID: "bgm_edit", TrackID: "bgm", AssetID: "music", AssetKind: "audio",
		TimelineEndFrame: 60, SourceEndFrame: 60,
	}}
	preserved := preserveIndependentAudioForOperation(document, map[string]any{
		"kind": "adjust_gain", "timeline_clip_id": "bgm_edit", "gain_db": -8,
	})
	if _, exists := preserved["bgm"]; exists {
		t.Fatalf("explicitly edited BGM must not be restored: %#v", preserved)
	}
}

func TestTimelineIntegrityHelpersNormalizeEvidenceAndTouchedTracks(t *testing.T) {
	t.Parallel()
	floatFrames := EffectFrameValues([]float64{
		0, 30, math.NaN(), math.Inf(1), -1, 1.5,
	})
	if !reflect.DeepEqual(floatFrames, []int{0, 30}) {
		t.Fatalf("float frames=%v", floatFrames)
	}
	clip := timeline.Clip{
		TimelineStartFrame: 10,
		TimelineEndFrame:   20,
		SourceStartFrame:   100,
		SourceEndFrame:     110,
	}
	if got := mapEffectFramesToTimeline(clip, []float64{99, 100, 110, 111}); !reflect.DeepEqual(got, []int{10, 20}) {
		t.Fatalf("mapped frames=%v", got)
	}
	if got := sortedUniqueInts([]int{3, 1, 3, 2, 2}); !reflect.DeepEqual(got, []int{1, 2, 3}) {
		t.Fatalf("sorted unique=%v", got)
	}

	document := timeline.Empty("draft_touched_tracks", 1)
	document.Tracks[0].Clips = []timeline.Clip{{
		TimelineClipID: "existing", TrackID: "visual_base",
	}}
	touched := touchedTrackIDsForOperation(document, map[string]any{
		"kind": "move_clip", "timeline_clip_id": "existing",
		"target_track_id": "visual_overlay",
	})
	for _, trackID := range []string{"visual_base", "visual_overlay"} {
		if _, exists := touched[trackID]; !exists {
			t.Errorf("touched tracks=%v missing %s", touched, trackID)
		}
	}
	insertTouched := touchedTrackIDsForOperation(document, map[string]any{
		"kind": "insert_clip", "track_id": "sfx",
	})
	if _, exists := insertTouched["sfx"]; !exists {
		t.Errorf("insert touched tracks=%v missing sfx", insertTouched)
	}
	for _, kind := range []string{"delete_range", "delete_source_range"} {
		preserved := preserveIndependentAudioForOperation(document, map[string]any{"kind": kind})
		if len(preserved) != 0 {
			t.Errorf("%s must explicitly edit independent audio, preserved=%v", kind, preserved)
		}
	}
}

func TestAtomicEditDerivesOriginalAudioFromAssetProbe(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_derive_original_audio")
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{
		{Type: "AssetImported", Payload: map[string]any{
			"asset_id": "talk", "job_id": "job_talk", "kind": "video", "filename": "talk.mp4",
			"usable": true, "probe": map[string]any{"duration_sec": 4.0, "has_audio": true},
		}},
		{Type: "AssetLinked", DraftID: "draft_derive_original_audio", Payload: map[string]any{"asset_id": "talk"}},
	}, reducer.Options{Actor: contracts.ActorUser})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("asset result=%#v err=%v", result, err)
	}
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	document, err := agenttest.ComposeTimeline("draft_derive_original_audio", 1, []agenttest.TimelineSelection{
		{AssetID: "talk", AssetKind: "video", SourceStartFrame: 0, SourceEndFrame: 60, Role: "a_roll", HasAudio: true},
		{AssetID: "talk", AssetKind: "video", SourceStartFrame: 60, SourceEndFrame: 120, Role: "a_roll", HasAudio: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	document.Tracks[2].Clips[0].TimelineStartFrame = 10
	document.Tracks[2].Clips[0].TimelineEndFrame = 70
	if persisted, persistErr := seedTimelineVersion(exec, t.Context(), "draft_derive_original_audio", document, "drifted_fixture", nil); persistErr != nil || persisted.Status != "validation_failed" {
		t.Fatalf("persisted=%#v err=%v", persisted, persistErr)
	}

	ctx := rushestools.WithDraftID(t.Context(), "draft_derive_original_audio")
	raw, err := exec.ExecuteTool(ctx, "timeline.update", rushestools.TimelineUpdateInput{
		"kind": "replace_clip", "timeline_clip_id": "clip_v1_001",
		"asset_id": "talk", "role": "a_roll",
	})
	if err != nil || raw.(rushestools.ToolResult).Status != "succeeded" {
		t.Fatalf("sync=%#v err=%v", raw, err)
	}
	latest, err := timeline.Latest(t.Context(), database, "draft_derive_original_audio")
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
	document, err := agenttest.ComposeTimeline("draft_alignment", 1, []agenttest.TimelineSelection{
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
