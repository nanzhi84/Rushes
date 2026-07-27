package agentexec

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

func TestAtomicTimelineEditReportsRippleCoordinateEffect(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_atomic_coordinate_effect"
	agenttest.CreateAgentDraft(t, database, draftID)
	insertAtomicTimelineAsset(t, database, draftID, "talk", "video", 4, false)
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := rushestools.WithDraftID(t.Context(), draftID)

	first := executeAtomicTimelineTool(t, exec, ctx, "timeline.insert", rushestools.TimelineInsertInput{
		"kind": "insert_clip", "asset_id": "talk",
		"source_start_frame": 0, "source_end_frame": 60,
	})
	assertCoordinateEffect(t, first, 0, 60, 0, false)
	second := executeAtomicTimelineTool(t, exec, ctx, "timeline.insert", rushestools.TimelineInsertInput{
		"kind": "insert_clip", "asset_id": "talk",
		"source_start_frame": 60, "source_end_frame": 120,
	})
	assertCoordinateEffect(t, second, 60, 120, 0, false)

	latest, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil {
		t.Fatal(err)
	}
	firstClipID := latest.Tracks[0].Clips[0].TimelineClipID
	trimmed := executeAtomicTimelineTool(t, exec, ctx, "timeline.update", rushestools.TimelineUpdateInput{
		"kind": "trim_clip", "timeline_clip_id": firstClipID,
		"source_start_frame": 0, "source_end_frame": 30,
	})
	assertCoordinateEffect(t, trimmed, 120, 90, -30, true)

	latest, err = timeline.Latest(t.Context(), database, draftID)
	if err != nil || latest.DurationFrames != 90 ||
		latest.Tracks[0].Clips[0].TimelineEndFrame != 30 ||
		latest.Tracks[0].Clips[1].TimelineStartFrame != 30 ||
		latest.Tracks[0].Clips[1].TimelineEndFrame != 90 {
		t.Fatalf("latest=%#v err=%v", latest, err)
	}
	staleInspection, err := exec.toolInspectTimeline(t.Context(), draftID, rushestools.TimelineInspectInput{
		TimelineID: first.Data["timeline_id"].(string),
	})
	if err != nil || staleInspection.Data["is_current"] != false {
		t.Fatalf("stale inspection=%#v err=%v", staleInspection, err)
	}
	currentInspection, err := exec.toolInspectTimeline(
		t.Context(), draftID, rushestools.TimelineInspectInput{},
	)
	if err != nil || currentInspection.Data["timeline_id"] != latest.TimelineID ||
		currentInspection.Data["is_current"] != true {
		t.Fatalf("current inspection=%#v err=%v", currentInspection, err)
	}
}

func TestTimelineInspectMarksAbsentCurrentState(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_absent_timeline_current_observation"
	agenttest.CreateAgentDraft(t, database, draftID)
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}

	inspection, err := exec.toolInspectTimeline(
		t.Context(), draftID, rushestools.TimelineInspectInput{},
	)
	if err != nil || inspection.Status != string(rushestools.StatusSucceeded) ||
		inspection.Data["timeline_exists"] != false || inspection.Data["is_current"] != true {
		t.Fatalf("absent timeline inspection=%#v err=%v", inspection, err)
	}
}

func TestTimelineInspectSnapshotStaysCoherentAcrossConcurrentCurrentTransitions(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_concurrent_timeline_inspection"
	agenttest.CreateAgentDraft(t, database, draftID)
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []int{1, 2} {
		document, composeErr := agenttest.ComposeTimeline(
			draftID, version, []agenttest.TimelineSelection{{
				AssetID: "asset", AssetKind: "video",
				SourceStartFrame: 0, SourceEndFrame: version * 30,
			}},
		)
		if composeErr != nil {
			t.Fatal(composeErr)
		}
		if _, persistErr := seedTimelineVersion(
			exec, t.Context(), draftID, document, "concurrent_inspection_fixture", nil,
		); persistErr != nil {
			t.Fatal(persistErr)
		}
	}

	transitions := []struct {
		name            string
		from, to        any
		allowedTimeline map[string]bool
		allowAbsent     bool
	}{
		{
			name: "commit_new_version", from: 1, to: 2,
			allowedTimeline: map[string]bool{draftID + ":v1": true, draftID + ":v2": true},
		},
		{
			name: "rewind", from: 2, to: 1,
			allowedTimeline: map[string]bool{draftID + ":v1": true, draftID + ":v2": true},
		},
		{
			name: "absent_to_v1", from: nil, to: 1,
			allowedTimeline: map[string]bool{draftID + ":v1": true}, allowAbsent: true,
		},
	}
	for _, transition := range transitions {
		t.Run(transition.name, func(t *testing.T) {
			if _, err := database.Write().ExecContext(t.Context(),
				"UPDATE drafts SET timeline_current_version=? WHERE draft_id=?",
				transition.from, draftID,
			); err != nil {
				t.Fatal(err)
			}
			start := make(chan struct{})
			writeDone := make(chan error, 1)
			inspectDone := make(chan struct {
				result rushestools.ToolResult
				err    error
			}, 1)
			go func() {
				<-start
				_, updateErr := database.Write().ExecContext(t.Context(),
					"UPDATE drafts SET timeline_current_version=? WHERE draft_id=?",
					transition.to, draftID,
				)
				writeDone <- updateErr
			}()
			go func() {
				<-start
				result, inspectErr := exec.toolInspectTimeline(
					t.Context(), draftID, rushestools.TimelineInspectInput{},
				)
				inspectDone <- struct {
					result rushestools.ToolResult
					err    error
				}{result: result, err: inspectErr}
			}()
			close(start)
			if err := <-writeDone; err != nil {
				t.Fatal(err)
			}
			inspection := <-inspectDone
			if inspection.err != nil || inspection.result.Data["is_current"] != true {
				t.Fatalf("inspection=%#v err=%v", inspection.result, inspection.err)
			}
			if inspection.result.Data["timeline_exists"] == false {
				if !transition.allowAbsent {
					t.Fatalf("unexpected absent inspection=%#v", inspection.result)
				}
				return
			}
			gotID, _ := inspection.result.Data["timeline_id"].(string)
			if !transition.allowedTimeline[gotID] {
				t.Fatalf("timeline_id=%q inspection=%#v", gotID, inspection.result)
			}
		})
	}
}

func TestAtomicCoordinateEffectCoversExistingNonPrimaryClips(t *testing.T) {
	t.Parallel()
	before := timeline.Document{
		DurationFrames: 120,
		Tracks: []timeline.Track{{
			TrackID: "visual_overlay",
			Clips: []timeline.Clip{{
				TimelineClipID:     "overlay_1",
				TimelineStartFrame: 10,
				TimelineEndFrame:   40,
			}},
		}},
	}
	after := before
	after.Tracks = append([]timeline.Track(nil), before.Tracks...)
	after.Tracks[0].Clips = append([]timeline.Clip(nil), before.Tracks[0].Clips...)
	after.Tracks[0].Clips[0].TimelineStartFrame = 20
	after.Tracks[0].Clips[0].TimelineEndFrame = 50

	effect := atomicTimelineCoordinateEffect(before, after)
	if effect["scope"] != "existing_timeline_clips" ||
		effect["observation_required"] != true ||
		effect["ripple_delta_frames"] != 0 {
		t.Fatalf("non-primary coordinate_effect=%#v", effect)
	}
}

func TestPersistTimelineStaleSnapshotReturnsStructuredConflict(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_atomic_stale_snapshot"
	agenttest.CreateAgentDraft(t, database, draftID)
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := agenttest.ComposeTimeline(draftID, 1, []agenttest.TimelineSelection{{
		AssetID: "asset_stale", AssetKind: "video",
		SourceStartFrame: 0, SourceEndFrame: 120,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result, persistErr := seedTimelineVersion(exec,
		t.Context(), draftID, initial, "stale_fixture", nil,
	); persistErr != nil || result.Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("initial result=%#v err=%v", result, persistErr)
	}
	snapshot, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil {
		t.Fatal(err)
	}
	draftSnapshot, err := storage.GetDraft(t.Context(), database.Read(), draftID)
	if err != nil {
		t.Fatal(err)
	}
	manual, err := timeline.ApplyPatch(snapshot, map[string]any{
		"kind": "insert_subtitle", "start_frame": 0, "end_frame": 30, "text": "manual",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result, persistErr := seedTimelineVersion(exec,
		t.Context(), draftID, manual, "manual_race", nil,
	); persistErr != nil || result.Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("manual result=%#v err=%v", result, persistErr)
	}
	staleAttempt, err := timeline.ApplyPatch(snapshot, map[string]any{
		"kind": "insert_subtitle", "start_frame": 30, "end_frame": 60, "text": "stale",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := exec.persistTimelineFromSnapshot(
		t.Context(),
		draftID,
		staleAttempt,
		"stale_race",
		nil,
		timelineMutationBase{
			stateVersion:    draftSnapshot.StateVersion,
			timelineVersion: snapshot.Version,
			timelineID:      snapshot.TimelineID,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != string(rushestools.StatusFailed) ||
		result.Data["error_code"] != string(rushestools.ErrCodeStaleTarget) ||
		result.Data["current_timeline_unchanged"] != true ||
		result.Data["previous_timeline_id"] != draftID+":v1" ||
		result.Data["attempted_timeline_id"] != draftID+":v2" ||
		result.Data["timeline_id"] != draftID+":v2" ||
		result.Data["expected_state_version"] != draftSnapshot.StateVersion ||
		result.Data["expected_timeline_version"] != snapshot.Version ||
		result.Data["actual_state_version"] != draftSnapshot.StateVersion+1 {
		t.Fatalf("conflict result=%#v", result)
	}
	latest, err := timeline.Latest(t.Context(), database, draftID)
	var subtitles []timeline.Clip
	for _, track := range latest.Tracks {
		if track.TrackID == "subtitles" {
			subtitles = track.Clips
			break
		}
	}
	if err != nil || latest.Version != 2 ||
		len(subtitles) != 1 || subtitles[0].Text != "manual" {
		t.Fatalf("latest=%#v err=%v", latest, err)
	}
	var timelineRows, staleBatches, createdEvents, validatedEvents int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT
			(SELECT COUNT(*) FROM timeline_versions WHERE draft_id=?),
			(SELECT COUNT(*) FROM timeline_edit_batches
				WHERE draft_id=? AND edit_batch_id LIKE 'stale_race:%'),
			(SELECT COUNT(*) FROM event_log
				WHERE draft_id=? AND event_type='TimelineVersionCreated'),
			(SELECT COUNT(*) FROM event_log
				WHERE draft_id=? AND event_type='TimelineValidated')`,
		draftID, draftID, draftID, draftID,
	).Scan(&timelineRows, &staleBatches, &createdEvents, &validatedEvents); err != nil {
		t.Fatal(err)
	}
	if timelineRows != 2 || staleBatches != 0 ||
		createdEvents != 2 || validatedEvents != 2 {
		t.Fatalf(
			"rows=%d stale_batches=%d created=%d validated=%d",
			timelineRows, staleBatches, createdEvents, validatedEvents,
		)
	}
}

func TestPersistTimelineCompetingInitialVersionsHaveSingleWinner(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_atomic_initial_race"
	agenttest.CreateAgentDraft(t, database, draftID)
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	empty, base, err := exec.timelineMutationSnapshot(t.Context(), draftID)
	if err != nil || empty.Version != 0 || base.timelineVersion != 0 ||
		base.timelineID != "" {
		t.Fatalf("empty=%#v base=%#v err=%v", empty, base, err)
	}
	winner, err := agenttest.ComposeTimeline(draftID, 1, []agenttest.TimelineSelection{{
		AssetID: "asset_winner", AssetKind: "video",
		SourceStartFrame: 0, SourceEndFrame: 60,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result, persistErr := seedTimelineVersion(exec,
		t.Context(), draftID, winner, "initial_winner", nil,
	); persistErr != nil || result.Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("winner result=%#v err=%v", result, persistErr)
	}
	loser, err := agenttest.ComposeTimeline(draftID, 1, []agenttest.TimelineSelection{{
		AssetID: "asset_loser", AssetKind: "video",
		SourceStartFrame: 0, SourceEndFrame: 90,
	}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := exec.persistTimelineFromSnapshot(
		t.Context(), draftID, loser, "initial_loser", nil, base,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != string(rushestools.StatusFailed) ||
		result.Data["error_code"] != string(rushestools.ErrCodeStaleTarget) ||
		result.Data["expected_timeline_version"] != 0 ||
		result.Data["timeline_id"] != draftID+":v1" {
		t.Fatalf("loser result=%#v", result)
	}
	latest, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil || latest.Version != 1 ||
		latest.Tracks[0].Clips[0].AssetID != "asset_winner" {
		t.Fatalf("latest=%#v err=%v", latest, err)
	}
	var versions, loserBatches int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT
			(SELECT COUNT(*) FROM timeline_versions WHERE draft_id=?),
			(SELECT COUNT(*) FROM timeline_edit_batches
				WHERE draft_id=? AND edit_batch_id LIKE 'initial_loser:%')`,
		draftID, draftID,
	).Scan(&versions, &loserBatches); err != nil {
		t.Fatal(err)
	}
	if versions != 1 || loserBatches != 0 {
		t.Fatalf("versions=%d loser_batches=%d", versions, loserBatches)
	}
}

func TestAtomicDeleteSourceRangeMisalignedRateDoesNotPersist(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_atomic_source_rate"
	agenttest.CreateAgentDraft(t, database, draftID)
	insertAtomicTimelineAsset(t, database, draftID, "talk_rate", "video", 4, false)
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	executeAtomicTimelineTool(t, exec, ctx, "timeline.insert", rushestools.TimelineInsertInput{
		"kind": "insert_clip", "asset_id": "talk_rate",
		"source_start_frame": 0, "source_end_frame": 20,
	})
	latest, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil {
		t.Fatal(err)
	}
	executeAtomicTimelineTool(t, exec, ctx, "timeline.update", rushestools.TimelineUpdateInput{
		"kind":             "set_playback_rate",
		"timeline_clip_id": latest.Tracks[0].Clips[0].TimelineClipID,
		"playback_rate":    2.0,
	})
	failedRaw, err := exec.ExecuteTool(ctx, "timeline.delete", rushestools.TimelineDeleteInput{
		"kind": "delete_source_range", "asset_id": "talk_rate",
		"source_start_frame": 0, "source_end_frame": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	failed := failedRaw.(rushestools.ToolResult)
	if failed.Status != string(rushestools.StatusFailed) ||
		!strings.Contains(InterfaceString(failed.Data["reason"]), "精确映射") {
		t.Fatalf("failed=%#v", failed)
	}
	var versionCount, editBatchCount int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT
			(SELECT COUNT(*) FROM timeline_versions WHERE draft_id=?),
			(SELECT COUNT(*) FROM timeline_edit_batches WHERE draft_id=?)`,
		draftID, draftID,
	).Scan(&versionCount, &editBatchCount); err != nil {
		t.Fatal(err)
	}
	latest, err = timeline.Latest(t.Context(), database, draftID)
	if err != nil || latest.Version != 2 || versionCount != 2 || editBatchCount != 2 {
		t.Fatalf(
			"失败写入了版本/编辑批次 latest=%#v versions=%d batches=%d err=%v",
			latest, versionCount, editBatchCount, err,
		)
	}
	aligned := executeAtomicTimelineTool(t, exec, ctx, "timeline.delete", rushestools.TimelineDeleteInput{
		"kind": "delete_source_range", "asset_id": "talk_rate",
		"source_start_frame": 0, "source_end_frame": 2,
	})
	assertAtomicTimelineResult(t, aligned, "delete_source_range")
	latest, err = timeline.Latest(t.Context(), database, draftID)
	if err != nil || latest.Version != 3 || latest.DurationFrames != 9 ||
		latest.Tracks[0].Clips[0].SourceStartFrame != 2 {
		t.Fatalf("aligned latest=%#v err=%v", latest, err)
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

func TestAtomicBGMInsertRejectsPartialBeatGridWithoutCreatingVersion(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_atomic_bgm_grid"
	agenttest.CreateAgentDraft(t, database, draftID)
	insertAtomicTimelineAsset(t, database, draftID, "visual", "video", 4, false)
	insertAtomicTimelineAsset(t, database, draftID, "music", "audio", 4, true)
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	executeAtomicTimelineTool(t, exec, ctx, "timeline.insert", rushestools.TimelineInsertInput{
		"kind": "insert_clip", "asset_id": "visual",
		"source_start_frame": 0, "source_end_frame": 120,
	})

	raw, err := exec.ExecuteTool(ctx, "timeline.insert", rushestools.TimelineInsertInput{
		"kind": "insert_clip", "track_id": "bgm", "asset_id": "music",
		"source_start_frame": 0, "source_end_frame": 120,
		"metadata": map[string]any{"beat_grid": map[string]any{
			"bpm": 120, "beat_frames": []any{0, 15, 30},
			"strong_beat_frames": []any{0}, "downbeat_frames": []any{0},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	failed := raw.(rushestools.ToolResult)
	missing, _ := failed.Data["missing_beat_grid_fields"].([]string)
	if failed.Status != string(rushestools.StatusFailed) ||
		len(missing) != 2 || missing[0] != "bar_phase" ||
		missing[1] != "analysis_method" {
		t.Fatalf("partial beat grid result=%#v", failed)
	}
	afterFailure, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil || afterFailure.Version != 1 || len(afterFailure.Tracks[4].Clips) != 0 {
		t.Fatalf("partial beat grid wrote timeline: %#v err=%v", afterFailure, err)
	}

	for _, testCase := range []struct {
		name         string
		beatFrames   any
		strongFrames any
		downbeats    any
		invalidField string
	}{
		{
			name: "wrong_type", beatFrames: "oops",
			strongFrames: []any{0}, downbeats: []any{0}, invalidField: "beat_frames",
		},
		{
			name: "null", beatFrames: nil,
			strongFrames: []any{0}, downbeats: []any{0}, invalidField: "beat_frames",
		},
		{
			name: "empty", beatFrames: []any{},
			strongFrames: []any{0}, downbeats: []any{0}, invalidField: "beat_frames",
		},
		{
			name: "fractional", beatFrames: []any{0, 15.5},
			strongFrames: []any{0}, downbeats: []any{0}, invalidField: "beat_frames",
		},
		{
			name: "negative_strong", beatFrames: []any{0, 15},
			strongFrames: []any{-1}, downbeats: []any{0}, invalidField: "strong_beat_frames",
		},
		{
			name: "invalid_downbeat_type", beatFrames: []any{0, 15},
			strongFrames: []any{0}, downbeats: "0", invalidField: "downbeat_frames",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			raw, executeErr := exec.ExecuteTool(ctx, "timeline.insert", rushestools.TimelineInsertInput{
				"kind": "insert_clip", "track_id": "bgm", "asset_id": "music",
				"source_start_frame": 0, "source_end_frame": 120,
				"metadata": map[string]any{"beat_grid": map[string]any{
					"bpm": 120, "beat_frames": testCase.beatFrames,
					"strong_beat_frames": testCase.strongFrames,
					"downbeat_frames":    testCase.downbeats,
					"bar_phase":          0, "analysis_method": "fixture",
				}},
			})
			if executeErr != nil {
				t.Fatal(executeErr)
			}
			failed := raw.(rushestools.ToolResult)
			invalid, _ := failed.Data["missing_beat_grid_fields"].([]string)
			if failed.Status != string(rushestools.StatusFailed) ||
				!ContainsString(invalid, testCase.invalidField) {
				t.Fatalf("invalid beat grid result=%#v", failed)
			}
			afterInvalid, latestErr := timeline.Latest(t.Context(), database, draftID)
			if latestErr != nil || afterInvalid.Version != 1 ||
				len(afterInvalid.Tracks[4].Clips) != 0 {
				t.Fatalf("invalid beat grid wrote timeline: %#v err=%v", afterInvalid, latestErr)
			}
		})
	}

	succeeded := executeAtomicTimelineTool(t, exec, ctx, "timeline.insert", rushestools.TimelineInsertInput{
		"kind": "insert_clip", "track_id": "bgm", "asset_id": "music",
		"source_start_frame": 0, "source_end_frame": 120,
		"metadata": map[string]any{"beat_grid": map[string]any{
			"bpm": 120, "beat_frames": []any{0, 15, 30},
			"strong_beat_frames": []any{0}, "downbeat_frames": []any{0},
			"bar_phase": 0, "analysis_method": "fixture",
		}},
	})
	assertAtomicTimelineResult(t, succeeded, "insert_clip")
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
	document, err := agenttest.ComposeTimeline(draftID, 1, []agenttest.TimelineSelection{{
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
	if persisted, persistErr := seedTimelineVersion(exec,
		t.Context(), draftID, document, "fixture", nil); persistErr != nil || persisted.Status != "succeeded" {
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
		result.Data["coordinate_effect"] == nil ||
		result.Data["validation_summary"] == nil {
		t.Fatalf("atomic result missing required fields: %#v", result.Data)
	}
}

func assertCoordinateEffect(
	t *testing.T,
	result rushestools.ToolResult,
	before, after, rippleDelta int,
	observationRequired bool,
) {
	t.Helper()
	effect, ok := result.Data["coordinate_effect"].(map[string]any)
	if !ok || effect["scope"] != "existing_timeline_clips" ||
		effect["duration_frames_before"] != before ||
		effect["duration_frames_after"] != after ||
		effect["ripple_delta_frames"] != rippleDelta ||
		effect["observation_required"] != observationRequired {
		t.Fatalf(
			"coordinate_effect=%#v want before=%d after=%d ripple=%d observation=%v",
			effect, before, after, rippleDelta, observationRequired,
		)
	}
}
