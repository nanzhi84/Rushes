package agentexec

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func TestAtomicTimelineToolsCreateOneVersionPerCatalogOperation(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_atomic_tools"
	agenttest.CreateAgentDraft(t, database, draftID)
	insertAtomicTimelineAsset(t, database, draftID, "talk", "video", 4, true)
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := rushestools.WithDraftID(t.Context(), draftID)

	insert := executeAtomicTimelineTool(t, exec, ctx, "timeline.insert", rushestools.TimelineInsertInput{
		"kind": "insert_clip", "asset_id": "talk",
		"source_start_frame": 0, "source_end_frame": 120,
	})
	if insert.Data["previous_timeline_id"] != "" || insert.Data["timeline_id"] != draftID+":v1" {
		t.Fatalf("first insert versions=%#v", insert.Data)
	}
	assertAtomicTimelineResult(t, insert, "insert_clip")

	latest, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil || latest.Version != 1 || len(latest.Tracks[0].Clips) != 1 ||
		len(latest.Tracks[2].Clips) != 1 || !latest.Tracks[0].Clips[0].Linked {
		t.Fatalf("first insert latest=%#v err=%v", latest, err)
	}
	primaryID := latest.Tracks[0].Clips[0].TimelineClipID

	split := executeAtomicTimelineTool(t, exec, ctx, "timeline.split", rushestools.TimelineSplitInput{
		"kind": "split_clip", "timeline_clip_id": primaryID, "split_frame": 60,
	})
	assertAtomicTimelineResult(t, split, "split_clip")

	update := executeAtomicTimelineTool(t, exec, ctx, "timeline.update", rushestools.TimelineUpdateInput{
		"kind": "set_clip_fades", "timeline_clip_id": primaryID,
		"fade_in_frames": 4, "fade_out_frames": 6,
	})
	assertAtomicTimelineResult(t, update, "set_clip_fades")

	subtitleInsert := executeAtomicTimelineTool(t, exec, ctx, "timeline.insert", rushestools.TimelineInsertInput{
		"kind": "insert_subtitle", "start_frame": 0, "end_frame": 30, "text": "原子字幕",
	})
	assertAtomicTimelineResult(t, subtitleInsert, "insert_subtitle")
	latest, err = timeline.Latest(t.Context(), database, draftID)
	if err != nil || len(latest.Tracks[5].Clips) != 1 {
		t.Fatalf("subtitle latest=%#v err=%v", latest, err)
	}
	subtitleID := latest.Tracks[5].Clips[0].TimelineClipID

	deleted := executeAtomicTimelineTool(t, exec, ctx, "timeline.delete", rushestools.TimelineDeleteInput{
		"kind": "delete_clip", "timeline_clip_id": subtitleID,
	})
	assertAtomicTimelineResult(t, deleted, "delete_clip")

	latest, err = timeline.Latest(t.Context(), database, draftID)
	if err != nil || latest.Version != 5 || len(latest.Tracks[5].Clips) != 0 {
		t.Fatalf("latest=%#v err=%v", latest, err)
	}
	var versionCount int
	if err := database.Read().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM timeline_versions WHERE draft_id=?", draftID,
	).Scan(&versionCount); err != nil || versionCount != 5 {
		t.Fatalf("version_count=%d err=%v", versionCount, err)
	}
	var singleOperationBatches int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM timeline_edit_batches
		WHERE draft_id=? AND json_array_length(operations_json)=1`, draftID,
	).Scan(&singleOperationBatches); err != nil || singleOperationBatches != 5 {
		t.Fatalf("single_operation_batches=%d err=%v", singleOperationBatches, err)
	}
}

func TestAtomicReplaceDerivesOriginalAudioWithoutModelFields(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_atomic_replace"
	agenttest.CreateAgentDraft(t, database, draftID)
	insertAtomicTimelineAsset(t, database, draftID, "talk", "video", 2, true)
	insertAtomicTimelineAsset(t, database, draftID, "talk_2", "video", 2, true)
	insertAtomicTimelineAsset(t, database, draftID, "still", "image", 2, false)
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	executeAtomicTimelineTool(t, exec, ctx, "timeline.insert", rushestools.TimelineInsertInput{
		"kind": "insert_clip", "asset_id": "talk",
		"source_start_frame": 0, "source_end_frame": 60,
	})
	before, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil {
		t.Fatal(err)
	}
	clipID := before.Tracks[0].Clips[0].TimelineClipID
	audioID := before.Tracks[2].Clips[0].TimelineClipID

	executeAtomicTimelineTool(t, exec, ctx, "timeline.update", rushestools.TimelineUpdateInput{
		"kind": "adjust_gain", "timeline_clip_id": audioID, "gain_db": -6,
	})
	replacedVideo := executeAtomicTimelineTool(t, exec, ctx, "timeline.update", rushestools.TimelineUpdateInput{
		"kind": "replace_clip", "timeline_clip_id": clipID, "asset_id": "talk_2",
	})
	assertAtomicTimelineResult(t, replacedVideo, "replace_clip")
	withNewVideo, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil || withNewVideo.Version != 3 ||
		withNewVideo.Tracks[2].Clips[0].AssetID != "talk_2" ||
		withNewVideo.Tracks[2].Clips[0].GainDB != -6 {
		t.Fatalf("derived audio lost creative settings: %#v err=%v", withNewVideo, err)
	}

	replaced := executeAtomicTimelineTool(t, exec, ctx, "timeline.update", rushestools.TimelineUpdateInput{
		"kind": "replace_clip", "timeline_clip_id": clipID, "asset_id": "still",
	})
	assertAtomicTimelineResult(t, replaced, "replace_clip")
	applied := replaced.Data["applied_operation"].(map[string]any)
	if applied["asset_kind"] != "image" {
		t.Fatalf("applied operation 未包含服务端注入类型: %#v", applied)
	}
	after, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil || after.Version != 4 || after.Tracks[0].Clips[0].AssetID != "still" ||
		after.Tracks[0].Clips[0].AssetKind != "image" || after.Tracks[0].Clips[0].Linked ||
		len(after.Tracks[2].Clips) != 0 {
		t.Fatalf("derived original audio after=%#v err=%v", after, err)
	}
}

func TestAtomicTimelineStaleTargetDoesNotCreateVersion(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_atomic_stale"
	agenttest.CreateAgentDraft(t, database, draftID)
	insertAtomicTimelineAsset(t, database, draftID, "talk", "video", 1, false)
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	executeAtomicTimelineTool(t, exec, ctx, "timeline.insert", rushestools.TimelineInsertInput{
		"kind": "insert_clip", "asset_id": "talk",
		"source_start_frame": 0, "source_end_frame": 30,
	})

	failedRaw, err := exec.ExecuteTool(ctx, "timeline.update", rushestools.TimelineUpdateInput{
		"kind": "adjust_gain", "timeline_clip_id": "clip_from_old_version", "gain_db": -3,
	})
	if err != nil {
		t.Fatal(err)
	}
	failed := failedRaw.(rushestools.ToolResult)
	if failed.Status != string(rushestools.StatusFailed) ||
		failed.Data["error_code"] != string(rushestools.ErrCodeStaleTarget) ||
		failed.Data["current_timeline_unchanged"] != true {
		t.Fatalf("failed=%#v", failed)
	}
	latest, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil || latest.Version != 1 {
		t.Fatalf("stale target wrote version: %#v err=%v", latest, err)
	}
}

func TestAtomicTimelineTrimRejectsSourceRangeBeyondAsset(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_atomic_trim_bounds"
	agenttest.CreateAgentDraft(t, database, draftID)
	insertAtomicTimelineAsset(t, database, draftID, "talk", "video", 4, false)
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	executeAtomicTimelineTool(t, exec, ctx, "timeline.insert", rushestools.TimelineInsertInput{
		"kind": "insert_clip", "asset_id": "talk",
		"source_start_frame": 0, "source_end_frame": 60,
	})
	before, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := exec.ExecuteTool(ctx, "timeline.update", rushestools.TimelineUpdateInput{
		"kind": "trim_clip", "timeline_clip_id": before.Tracks[0].Clips[0].TimelineClipID,
		"source_start_frame": 0, "source_end_frame": 100000,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := raw.(rushestools.ToolResult)
	facts, _ := result.Data["asset_facts"].(map[string]any)
	if result.Status != string(rushestools.StatusFailed) ||
		facts["duration_frames"] != 120 {
		t.Fatalf("out-of-bounds trim result=%#v", result)
	}
	after, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil || after.Version != before.Version ||
		after.Tracks[0].Clips[0].SourceEndFrame != before.Tracks[0].Clips[0].SourceEndFrame {
		t.Fatalf("out-of-bounds trim wrote timeline: before=%#v after=%#v err=%v", before, after, err)
	}
}

func TestAtomicTimelineInsertRejectsDerivedOriginalAudioTrack(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_atomic_original_audio_target"
	agenttest.CreateAgentDraft(t, database, draftID)
	insertAtomicTimelineAsset(t, database, draftID, "talk", "video", 2, true)
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	executeAtomicTimelineTool(t, exec, ctx, "timeline.insert", rushestools.TimelineInsertInput{
		"kind": "insert_clip", "asset_id": "talk",
		"source_start_frame": 0, "source_end_frame": 60,
	})

	raw, err := exec.ExecuteTool(ctx, "timeline.insert", rushestools.TimelineInsertInput{
		"kind": "insert_clip", "track_id": "original_audio", "asset_id": "talk",
		"timeline_start_frame": 0, "source_start_frame": 0, "source_end_frame": 60,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := raw.(rushestools.ToolResult)
	if result.Status != string(rushestools.StatusFailed) ||
		result.Data["current_timeline_unchanged"] != true {
		t.Fatalf("derived track insert result=%#v", result)
	}
	latest, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil || latest.Version != 1 || len(latest.Tracks[2].Clips) != 1 {
		t.Fatalf("derived track insert wrote timeline: %#v err=%v", latest, err)
	}
}

func TestAtomicTimelineEditDoesNotAnalyzeOrModifyUntouchedBGM(t *testing.T) {
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
	const draftID = "draft_atomic_untouched_bgm"
	agenttest.CreateAgentDraft(t, database, draftID)
	insertAtomicTimelineAsset(t, database, draftID, "talk", "video", 2, false)
	source := filepath.Join(database.Paths.Temporary, "atomic-bgm.wav")
	if err := os.WriteFile(source, []byte("fake source"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	assetResult, err := reducer.Apply(t.Context(), database, []contracts.Event{
		{Type: "AssetImported", Payload: map[string]any{
			"asset_id": "music", "job_id": "job_music", "storage_mode": "reference",
			"reference_path": source, "kind": "audio", "source": "local_path",
			"filename": "music.wav", "hash": "atomic_bgm_hash", "size": 11,
			"ingest_status": "ready", "usable": true,
			"probe": map[string]any{"duration_sec": 2.0, "has_audio": true},
		}},
		{Type: "AssetLinked", DraftID: draftID, Payload: map[string]any{
			"asset_id": "music", "linked_at": now,
		}},
	}, reducer.Options{Actor: contracts.ActorUser})
	if err != nil || assetResult.Status != reducer.StatusApplied {
		t.Fatalf("music asset status=%s err=%v", assetResult.Status, err)
	}
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	document, err := timeline.ComposeInitial(draftID, 1, []timeline.Selection{{
		AssetID: "talk", AssetKind: "video", SourceStartFrame: 0, SourceEndFrame: 60,
	}})
	if err != nil {
		t.Fatal(err)
	}
	document.Tracks[4].Clips = []timeline.Clip{{
		TimelineClipID: "bgm_existing", TrackID: "bgm", AssetID: "music", AssetKind: "audio",
		Role: "bgm", TimelineStartFrame: 0, TimelineEndFrame: 60,
		SourceStartFrame: 0, SourceEndFrame: 60, PlaybackRate: 1,
	}}
	if persisted, persistErr := exec.PersistTimeline(
		t.Context(), draftID, document, "fixture",
	); persistErr != nil || persisted.Status != "succeeded" {
		t.Fatalf("persisted=%#v err=%v", persisted, persistErr)
	}

	ctx := rushestools.WithDraftID(t.Context(), draftID)
	result := executeAtomicTimelineTool(t, exec, ctx, "timeline.insert", rushestools.TimelineInsertInput{
		"kind": "insert_subtitle", "start_frame": 0, "end_frame": 30, "text": "只改字幕",
	})
	if result.Data["beat_grid_attached_count"] != nil {
		t.Fatalf("atomic result leaked hidden BGM analysis: %#v", result.Data)
	}
	latest, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil || latest.Version != 2 || len(latest.Tracks[4].Clips) != 1 ||
		len(latest.Tracks[4].Clips[0].Effects) != 0 {
		t.Fatalf("untouched BGM changed: %#v err=%v", latest.Tracks[4], err)
	}
}

func insertAtomicTimelineAsset(
	t *testing.T,
	database *storage.DB,
	draftID string,
	assetID string,
	kind string,
	durationSeconds int,
	hasAudio bool,
) {
	t.Helper()
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{
		{
			Type: "AssetImported",
			Payload: map[string]any{
				"asset_id": assetID, "job_id": "job_" + assetID, "kind": kind,
				"filename": assetID + ".mp4", "usable": true,
				"probe": map[string]any{
					"duration_sec": float64(durationSeconds), "has_audio": hasAudio,
				},
			},
		},
		{
			Type: "AssetLinked", DraftID: draftID,
			Payload: map[string]any{"asset_id": assetID},
		},
	}, reducer.Options{Actor: contracts.ActorUser})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("insert asset %s status=%s err=%v", assetID, result.Status, err)
	}
}

func executeAtomicTimelineTool(
	t *testing.T,
	exec *Executor,
	ctx context.Context,
	name string,
	input any,
) rushestools.ToolResult {
	t.Helper()
	raw, err := exec.ExecuteTool(ctx, name, input)
	if err != nil {
		t.Fatalf("%s err=%v", name, err)
	}
	result := raw.(rushestools.ToolResult)
	if result.Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("%s result=%#v", name, result)
	}
	return result
}

func assertAtomicTimelineResult(t *testing.T, result rushestools.ToolResult, kind string) {
	t.Helper()
	operation, ok := result.Data["applied_operation"].(map[string]any)
	if !ok || operation["kind"] != kind {
		t.Fatalf("applied_operation=%#v want kind=%s", result.Data["applied_operation"], kind)
	}
	if result.Data["timeline_id"] == "" || result.Data["changed_targets"] == nil ||
		result.Data["validation_summary"] == nil {
		t.Fatalf("atomic result missing required fields: %#v", result.Data)
	}
}
