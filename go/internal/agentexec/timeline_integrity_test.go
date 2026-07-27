package agentexec

import (
	"encoding/json"
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
	result, err := timeline.ApplyPatch(unlockPreservedIndependentAudio(document, preserved), operation)
	if err != nil {
		t.Fatal(err)
	}
	restoreIndependentAudioTracks(&result, preserved)
	if report := validateWithPreservedIndependentAudio(result, preserved); !report.Valid {
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
	document.Tracks[4].Locked = true
	document.Tracks[6].Clips = []timeline.Clip{{
		TimelineClipID: "sfx_tail", TrackID: "sfx", AssetID: "effect", AssetKind: "audio",
		TimelineStartFrame: 45, TimelineEndFrame: 60,
		SourceStartFrame: 0, SourceEndFrame: 15, PlaybackRate: 1,
	}}
	document.Tracks[6].Locked = true
	if persisted, persistErr := seedTimelineVersion(
		exec, t.Context(), draftID, document, "fixture", nil,
	); persistErr != nil || persisted.Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("persisted=%#v err=%v", persisted, persistErr)
	}

	ctx := rushestools.WithDraftID(t.Context(), draftID)
	deleted := executeAtomicTimelineTool(t, exec, ctx, "timeline.delete", rushestools.TimelineDeleteInput{
		"kind": "delete_clip", "timeline_clip_id": "clip_v1_001",
	})
	validation, _ := deleted.Data["validation_summary"].(map[string]any)
	if deleted.Data["previous_timeline_id"] != draftID+":v1" ||
		deleted.Data["timeline_id"] != draftID+":v2" || validation["valid"] != true {
		t.Fatalf("delete persistence contract=%#v", deleted.Data)
	}
	for _, target := range deleted.Data["changed_targets"].([]map[string]any) {
		if ContainsString(independentAudioTrackIDs, StringValue(target["track_id"])) {
			t.Fatalf("untouched independent audio must not be reported as changed: %#v", deleted.Data)
		}
	}
	shrunk, err := timeline.Latest(t.Context(), database, draftID)
	shrunkPreserved := preserveIndependentAudioForOperation(shrunk, map[string]any{
		"kind": "insert_clip", "track_id": "visual_base",
	})
	if err != nil || shrunk.DurationFrames != 30 ||
		!validateWithPreservedIndependentAudio(shrunk, shrunkPreserved).Valid ||
		shrunk.Tracks[4].Clips[0].TimelineEndFrame != 60 ||
		shrunk.Tracks[4].Clips[0].SourceEndFrame != 60 ||
		!shrunk.Tracks[4].Locked ||
		shrunk.Tracks[6].Clips[0].TimelineStartFrame != 45 ||
		shrunk.Tracks[6].Clips[0].TimelineEndFrame != 60 ||
		!shrunk.Tracks[6].Locked {
		t.Fatalf("shrunk=%#v report=%#v err=%v", shrunk, timeline.Validate(shrunk), err)
	}
	checkedRaw, err := exec.ExecuteTool(ctx, "timeline.check", rushestools.TimelineCheckInput{
		TimelineID: shrunk.TimelineID,
	})
	if err != nil {
		t.Fatal(err)
	}
	checked := checkedRaw.(rushestools.ToolResult)
	checkedValidation, _ := checked.Data["validation_report"].(map[string]any)
	if checked.Status != string(rushestools.StatusSucceeded) || checkedValidation["valid"] != true {
		t.Fatalf("stored preserved overhang must remain checkable: %#v", checked)
	}
	renderRaw, err := exec.ExecuteTool(ctx, "render.start", rushestools.RenderStartInput{
		Kind: "preview", TimelineID: shrunk.TimelineID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if render := renderRaw.(rushestools.ToolResult); render.Status != "queued" || render.Data["timeline_version"] != 2 {
		t.Fatalf("stored preserved overhang must remain renderable: %#v", render)
	}
	var storedReport string
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT validation_report_json FROM timeline_versions WHERE draft_id=? AND version=2`,
		draftID,
	).Scan(&storedReport); err != nil {
		t.Fatal(err)
	}
	var persistedProof struct {
		Proofs []independentAudioPreservationProof `json:"preservation_proofs"`
	}
	if err := json.Unmarshal([]byte(storedReport), &persistedProof); err != nil || len(persistedProof.Proofs) != 2 {
		t.Fatalf("portable preservation proof=%#v err=%v report=%s", persistedProof, err, storedReport)
	}

	const copiedDraftID = "draft_preserve_audio_overhang_copy"
	copyResult, err := reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "DraftCopied", DraftID: copiedDraftID,
		Payload: map[string]any{"source_draft_id": draftID, "name": "proof copy"},
	}}, reducer.Options{Actor: contracts.ActorUser})
	if err != nil || copyResult.Status != reducer.StatusApplied {
		t.Fatalf("copy result=%#v err=%v", copyResult, err)
	}
	copied, err := timeline.Latest(t.Context(), database, copiedDraftID)
	if err != nil {
		t.Fatal(err)
	}
	assertStoredOverhangCheckAndRender(t, exec, copiedDraftID, copied, 2)

	const checkpointID = "checkpoint:preserved-audio"
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO rewind_checkpoints(
			checkpoint_id,draft_id,trigger_kind,timeline_version,summary,created_at
		) VALUES(?,?,'timeline_write',2,'preserved audio','now')`,
		checkpointID, copiedDraftID,
	); err != nil {
		t.Fatal(err)
	}
	var copiedStateVersion int
	if err := database.Read().QueryRowContext(t.Context(),
		"SELECT state_version FROM drafts WHERE draft_id=?", copiedDraftID,
	).Scan(&copiedStateVersion); err != nil {
		t.Fatal(err)
	}
	restoreResult, err := reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "TimelineVersionRestored", DraftID: copiedDraftID,
		Payload: map[string]any{
			"checkpoint_id": checkpointID, "mode": "timeline", "timeline_version": 3,
			"restore_checkpoint_id": "checkpoint:preserved-audio:restored",
		},
	}}, reducer.Options{Actor: contracts.ActorUser, BaseVersion: &copiedStateVersion})
	if err != nil || restoreResult.Status != reducer.StatusApplied {
		t.Fatalf("restore result=%#v err=%v", restoreResult, err)
	}
	restored, err := timeline.Latest(t.Context(), database, copiedDraftID)
	if err != nil {
		t.Fatal(err)
	}
	assertStoredOverhangCheckAndRender(t, exec, copiedDraftID, restored, 3)

	const restoredCheckpointID = "checkpoint:preserved-audio:restored-twice"
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO rewind_checkpoints(
			checkpoint_id,draft_id,trigger_kind,timeline_version,summary,created_at
		) VALUES(?,?,'timeline_write',3,'preserved audio twice','now')`,
		restoredCheckpointID, copiedDraftID,
	); err != nil {
		t.Fatal(err)
	}
	if err := database.Read().QueryRowContext(t.Context(),
		"SELECT state_version FROM drafts WHERE draft_id=?", copiedDraftID,
	).Scan(&copiedStateVersion); err != nil {
		t.Fatal(err)
	}
	restoreAgain, err := reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "TimelineVersionRestored", DraftID: copiedDraftID,
		Payload: map[string]any{
			"checkpoint_id": restoredCheckpointID, "mode": "timeline", "timeline_version": 4,
			"restore_checkpoint_id": "checkpoint:preserved-audio:restored-again",
		},
	}}, reducer.Options{Actor: contracts.ActorUser, BaseVersion: &copiedStateVersion})
	if err != nil || restoreAgain.Status != reducer.StatusApplied {
		t.Fatalf("second restore result=%#v err=%v", restoreAgain, err)
	}
	restoredAgain, err := timeline.Latest(t.Context(), database, copiedDraftID)
	if err != nil {
		t.Fatal(err)
	}
	assertStoredOverhangCheckAndRender(t, exec, copiedDraftID, restoredAgain, 4)

	const restoredCopyID = "draft_preserve_audio_overhang_restored_copy"
	restoredCopy, err := reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "DraftCopied", DraftID: restoredCopyID,
		Payload: map[string]any{"source_draft_id": copiedDraftID, "name": "restored proof copy"},
	}}, reducer.Options{Actor: contracts.ActorUser})
	if err != nil || restoredCopy.Status != reducer.StatusApplied {
		t.Fatalf("restored copy result=%#v err=%v", restoredCopy, err)
	}
	restoredCopyTimeline, err := timeline.Latest(t.Context(), database, restoredCopyID)
	if err != nil {
		t.Fatal(err)
	}
	assertStoredOverhangCheckAndRender(t, exec, restoredCopyID, restoredCopyTimeline, 4)

	regrow := executeAtomicTimelineTool(t, exec, ctx, "timeline.insert", rushestools.TimelineInsertInput{
		"kind": "insert_clip", "asset_id": "old_a",
		"source_start_frame": 0, "source_end_frame": 30,
	})
	if regrow.Data["previous_timeline_id"] != draftID+":v2" || regrow.Data["timeline_id"] != draftID+":v3" {
		t.Fatalf("regrow persistence contract=%#v", regrow.Data)
	}
	regrown, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil || regrown.DurationFrames != 60 || !timeline.Validate(regrown).Valid ||
		regrown.Tracks[4].Clips[0].TimelineEndFrame != 60 ||
		regrown.Tracks[4].Clips[0].SourceEndFrame != 60 ||
		regrown.Tracks[6].Clips[0].TimelineStartFrame != 45 ||
		regrown.Tracks[6].Clips[0].TimelineEndFrame != 60 {
		t.Fatalf("regrown=%#v report=%#v err=%v", regrown, timeline.Validate(regrown), err)
	}

	raw, err := exec.ExecuteTool(ctx, "timeline.update", rushestools.TimelineUpdateInput{
		"kind": "adjust_gain", "timeline_clip_id": "bgm_full", "gain_db": -6,
	})
	if err != nil {
		t.Fatal(err)
	}
	failed := raw.(rushestools.ToolResult)
	if failed.Status != string(rushestools.StatusFailed) ||
		failed.Data["semantic_error_kind"] != timeline.SemanticTrackLocked {
		t.Fatalf("explicit edit of locked BGM must remain blocked: %#v", failed)
	}
}

func assertStoredOverhangCheckAndRender(
	t *testing.T,
	exec *Executor,
	draftID string,
	document timeline.Document,
	version int,
) {
	t.Helper()
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	checkedRaw, err := exec.ExecuteTool(ctx, "timeline.check", rushestools.TimelineCheckInput{
		TimelineID: document.TimelineID,
	})
	if err != nil {
		t.Fatal(err)
	}
	checked := checkedRaw.(rushestools.ToolResult)
	validation, _ := checked.Data["validation_report"].(map[string]any)
	if checked.Status != string(rushestools.StatusSucceeded) || validation["valid"] != true {
		t.Fatalf("portable stored proof must remain checkable: %#v", checked)
	}
	renderRaw, err := exec.ExecuteTool(ctx, "render.start", rushestools.RenderStartInput{
		Kind: "preview", TimelineID: document.TimelineID,
	})
	if err != nil {
		t.Fatal(err)
	}
	render := renderRaw.(rushestools.ToolResult)
	if render.Status != "queued" || render.Data["timeline_version"] != version {
		t.Fatalf("portable stored proof must remain renderable: %#v", render)
	}
}

func TestAtomicExplicitAudioExtensionDoesNotPersist(t *testing.T) {
	for _, test := range []struct {
		name      string
		operation rushestools.TimelineUpdateInput
	}{
		{
			name: "trim_clip",
			operation: rushestools.TimelineUpdateInput{
				"kind": "trim_clip", "timeline_clip_id": "bgm_edit",
				"source_start_frame": 0, "source_end_frame": 90,
			},
		},
		{
			name: "set_playback_rate",
			operation: rushestools.TimelineUpdateInput{
				"kind": "set_playback_rate", "timeline_clip_id": "bgm_edit",
				"playback_rate": 0.5,
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			database := agenttest.AgentTestDatabase(t)
			draftID := "draft_explicit_audio_overhang_" + test.name
			agenttest.CreateAgentDraft(t, database, draftID)
			insertAtomicTimelineAsset(t, database, draftID, "music", "audio", 3, false)
			exec, err := newTestExecutor(t.Context(), database, nil)
			if err != nil {
				t.Fatal(err)
			}
			document, err := agenttest.ComposeTimeline(draftID, 1, []agenttest.TimelineSelection{{
				AssetID: "visual", AssetKind: "video", SourceEndFrame: 60,
			}})
			if err != nil {
				t.Fatal(err)
			}
			document.Tracks[4].Clips = []timeline.Clip{{
				TimelineClipID: "bgm_edit", TrackID: "bgm", AssetID: "music", AssetKind: "audio",
				TimelineEndFrame: 60, SourceEndFrame: 60, PlaybackRate: 1,
			}}
			if persisted, persistErr := seedTimelineVersion(
				exec, t.Context(), draftID, document, "fixture", nil,
			); persistErr != nil || persisted.Status != string(rushestools.StatusSucceeded) {
				t.Fatalf("persisted=%#v err=%v", persisted, persistErr)
			}

			raw, err := exec.ExecuteTool(
				rushestools.WithDraftID(t.Context(), draftID),
				"timeline.update",
				test.operation,
			)
			if err != nil {
				t.Fatal(err)
			}
			result := raw.(rushestools.ToolResult)
			if result.Status != string(rushestools.StatusFailed) {
				t.Fatalf("explicit audio extension result=%#v", result)
			}
			latest, err := timeline.Latest(t.Context(), database, draftID)
			if err != nil || latest.Version != 1 ||
				latest.Tracks[4].Clips[0].TimelineEndFrame != 60 {
				t.Fatalf("latest=%#v err=%v", latest, err)
			}
		})
	}
}

func TestTrustedOverhangDoesNotAuthorizeAnotherClipToExtend(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_per_clip_audio_overhang"
	agenttest.CreateAgentDraft(t, database, draftID)
	insertAtomicTimelineAsset(t, database, draftID, "music_b", "audio", 2, false)
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	document, err := agenttest.ComposeTimeline(draftID, 1, []agenttest.TimelineSelection{
		{AssetID: "visual", AssetKind: "video", SourceStartFrame: 0, SourceEndFrame: 30},
		{AssetID: "visual", AssetKind: "video", SourceStartFrame: 30, SourceEndFrame: 60},
		{AssetID: "visual", AssetKind: "video", SourceStartFrame: 60, SourceEndFrame: 90},
	})
	if err != nil {
		t.Fatal(err)
	}
	document.Tracks[4].Clips = []timeline.Clip{
		{
			TimelineClipID: "bgm_existing_tail", TrackID: "bgm",
			AssetID: "music_a", AssetKind: "audio",
			TimelineEndFrame: 90, SourceEndFrame: 90, PlaybackRate: 1,
		},
		{
			TimelineClipID: "bgm_in_bounds", TrackID: "bgm",
			AssetID: "music_b", AssetKind: "audio",
			TimelineEndFrame: 60, SourceEndFrame: 60, PlaybackRate: 1,
		},
	}
	if persisted, persistErr := seedTimelineVersion(
		exec, t.Context(), draftID, document, "fixture", nil,
	); persistErr != nil || persisted.Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("persisted=%#v err=%v", persisted, persistErr)
	}
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	executeAtomicTimelineTool(t, exec, ctx, "timeline.delete", rushestools.TimelineDeleteInput{
		"kind": "delete_clip", "timeline_clip_id": "clip_v1_003",
	})

	raw, err := exec.ExecuteTool(ctx, "timeline.update", rushestools.TimelineUpdateInput{
		"kind": "set_playback_rate", "timeline_clip_id": "bgm_in_bounds", "playback_rate": 0.75,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result := raw.(rushestools.ToolResult); result.Status != string(rushestools.StatusFailed) {
		t.Fatalf("one clip's trusted tail must not authorize another clip to extend: %#v", result)
	}
	latest, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil || latest.Version != 2 || latest.Tracks[4].Clips[1].TimelineEndFrame != 60 {
		t.Fatalf("per-clip extension wrote a version: %#v err=%v", latest, err)
	}
}

func TestTrustedOverhangRangeSplitUsesDeterministicClipLineage(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_repeated_source_audio_split"
	agenttest.CreateAgentDraft(t, database, draftID)
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	document, err := agenttest.ComposeTimeline(draftID, 1, []agenttest.TimelineSelection{
		{AssetID: "visual", AssetKind: "video", SourceStartFrame: 0, SourceEndFrame: 30},
		{AssetID: "visual", AssetKind: "video", SourceStartFrame: 30, SourceEndFrame: 60},
		{AssetID: "visual", AssetKind: "video", SourceStartFrame: 60, SourceEndFrame: 90},
	})
	if err != nil {
		t.Fatal(err)
	}
	document.Tracks[4].Clips = []timeline.Clip{
		{
			TimelineClipID: "bgm_tail", TrackID: "bgm", AssetID: "music", AssetKind: "audio",
			TimelineStartFrame: 30, TimelineEndFrame: 90,
			SourceStartFrame: 0, SourceEndFrame: 60, PlaybackRate: 1,
		},
		{
			TimelineClipID: "bgm_repeat", TrackID: "bgm", AssetID: "music", AssetKind: "audio",
			TimelineStartFrame: 0, TimelineEndFrame: 60,
			SourceStartFrame: 0, SourceEndFrame: 60, PlaybackRate: 1,
		},
	}
	if persisted, persistErr := seedTimelineVersion(
		exec, t.Context(), draftID, document, "fixture", nil,
	); persistErr != nil || persisted.Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("persisted=%#v err=%v", persisted, persistErr)
	}
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	executeAtomicTimelineTool(t, exec, ctx, "timeline.delete", rushestools.TimelineDeleteInput{
		"kind": "delete_clip", "timeline_clip_id": "clip_v1_003",
	})

	deleted := executeAtomicTimelineTool(t, exec, ctx, "timeline.delete", rushestools.TimelineDeleteInput{
		"kind": "delete_range", "start_frame": 40, "end_frame": 50,
	})
	if deleted.Data["timeline_id"] != draftID+":v3" {
		t.Fatalf("repeated-source split result=%#v", deleted)
	}
	latest, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil || latest.Version != 3 || latest.DurationFrames != 50 {
		t.Fatalf("latest=%#v err=%v", latest, err)
	}
	var tail timeline.Clip
	for _, clip := range latest.Tracks[4].Clips {
		if clip.TimelineClipID == "bgm_tail_after_50" {
			tail = clip
			break
		}
	}
	if tail.TimelineClipID == "" || tail.TimelineStartFrame != 40 || tail.TimelineEndFrame != 80 ||
		tail.SourceStartFrame != 20 || tail.SourceEndFrame != 60 {
		t.Fatalf("deterministic tail lineage=%#v clips=%#v", tail, latest.Tracks[4].Clips)
	}
	checkedRaw, err := exec.ExecuteTool(ctx, "timeline.check", rushestools.TimelineCheckInput{
		TimelineID: latest.TimelineID,
	})
	if err != nil || checkedRaw.(rushestools.ToolResult).Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("repeated-source split check=%#v err=%v", checkedRaw, err)
	}
}

func TestDirectPersistenceDoesNotValidateIndependentAudioOverhang(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_direct_overhang"
	agenttest.CreateAgentDraft(t, database, draftID)
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	document, err := agenttest.ComposeTimeline(draftID, 1, []agenttest.TimelineSelection{{
		AssetID: "visual", AssetKind: "video", SourceEndFrame: 60,
	}})
	if err != nil {
		t.Fatal(err)
	}
	document.Tracks[4].Clips = []timeline.Clip{{
		TimelineClipID: "bgm_overhang", TrackID: "bgm", AssetID: "music", AssetKind: "audio",
		TimelineEndFrame: 90, SourceEndFrame: 90, PlaybackRate: 1,
	}}
	result, err := seedTimelineVersion(exec, t.Context(), draftID, document, "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "validation_failed" {
		t.Fatalf("direct persistence must keep strict validation: %#v", result)
	}
	raw, err := exec.ExecuteTool(
		rushestools.WithDraftID(t.Context(), draftID),
		"timeline.check",
		rushestools.TimelineCheckInput{TimelineID: document.TimelineID},
	)
	if err != nil {
		t.Fatal(err)
	}
	if checked := raw.(rushestools.ToolResult); checked.Status != "validation_failed" {
		t.Fatalf("directly persisted overhang must remain invalid: %#v", checked)
	}
	raw, err = exec.ExecuteTool(
		rushestools.WithDraftID(t.Context(), draftID),
		"timeline.update",
		rushestools.TimelineUpdateInput{
			"kind": "set_track_state", "track_id": "visual_base", "muted": true,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if edited := raw.(rushestools.ToolResult); edited.Status != string(rushestools.StatusFailed) {
		t.Fatalf("unrelated edit must not launder invalid overhang: %#v", edited)
	}
	latest, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil || latest.Version != 1 {
		t.Fatalf("invalid overhang laundering wrote a version: %#v err=%v", latest, err)
	}
}

func TestTrustedAudioOverhangAllowsSafeEditsAndNonIncreasingRipple(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_trusted_audio_overhang_edits"
	agenttest.CreateAgentDraft(t, database, draftID)
	insertAtomicTimelineAsset(t, database, draftID, "music", "audio", 3, false)
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	document, err := agenttest.ComposeTimeline(draftID, 1, []agenttest.TimelineSelection{
		{AssetID: "talk", AssetKind: "video", SourceStartFrame: 0, SourceEndFrame: 30},
		{AssetID: "talk", AssetKind: "video", SourceStartFrame: 30, SourceEndFrame: 60},
	})
	if err != nil {
		t.Fatal(err)
	}
	document.Tracks[4].Clips = []timeline.Clip{{
		TimelineClipID: "bgm_safe", TrackID: "bgm", AssetID: "music", AssetKind: "audio",
		TimelineEndFrame: 60, SourceEndFrame: 60, PlaybackRate: 1,
	}}
	if persisted, persistErr := seedTimelineVersion(
		exec, t.Context(), draftID, document, "fixture", nil,
	); persistErr != nil || persisted.Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("persisted=%#v err=%v", persisted, persistErr)
	}
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	executeAtomicTimelineTool(t, exec, ctx, "timeline.delete", rushestools.TimelineDeleteInput{
		"kind": "delete_clip", "timeline_clip_id": "clip_v1_001",
	})

	for _, operation := range []rushestools.TimelineUpdateInput{
		{"kind": "adjust_gain", "timeline_clip_id": "bgm_safe", "gain_db": -6},
		{"kind": "set_clip_fades", "timeline_clip_id": "bgm_safe", "fade_in_frames": 3, "fade_out_frames": 3},
		{"kind": "set_track_state", "track_id": "bgm", "muted": true},
		{"kind": "set_track_ducking", "track_id": "bgm", "enabled": true, "duck_db": -9,
			"trigger_tracks": []string{"voiceover"}},
	} {
		executeAtomicTimelineTool(t, exec, ctx, "timeline.update", operation)
	}
	executeAtomicTimelineTool(t, exec, ctx, "timeline.delete", rushestools.TimelineDeleteInput{
		"kind": "delete_source_range", "asset_id": "talk",
		"source_start_frame": 30, "source_end_frame": 40,
	})
	executeAtomicTimelineTool(t, exec, ctx, "timeline.delete", rushestools.TimelineDeleteInput{
		"kind": "delete_range", "start_frame": 0, "end_frame": 5,
	})

	latest, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil || latest.Version != 8 || latest.DurationFrames != 15 ||
		len(latest.Tracks[4].Clips) != 1 ||
		latest.Tracks[4].Clips[0].TimelineEndFrame != 45 ||
		latest.Tracks[4].Clips[0].GainDB != -6 ||
		latest.Tracks[4].Clips[0].FadeInFrames != 3 ||
		latest.Tracks[4].Clips[0].FadeOutFrames != 3 ||
		!latest.Tracks[4].Muted || latest.Tracks[4].Ducking == nil {
		t.Fatalf("latest=%#v err=%v", latest, err)
	}
	checkedRaw, err := exec.ExecuteTool(ctx, "timeline.check", rushestools.TimelineCheckInput{
		TimelineID: latest.TimelineID,
	})
	if err != nil || checkedRaw.(rushestools.ToolResult).Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("safe overhang sequence check=%#v err=%v", checkedRaw, err)
	}

	raw, err := exec.ExecuteTool(ctx, "timeline.update", rushestools.TimelineUpdateInput{
		"kind": "set_playback_rate", "timeline_clip_id": "bgm_safe", "playback_rate": 0.5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if extended := raw.(rushestools.ToolResult); extended.Status != string(rushestools.StatusFailed) {
		t.Fatalf("timing edit that increases trusted overhang must fail: %#v", extended)
	}
	afterFailure, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil || afterFailure.Version != 8 {
		t.Fatalf("increasing overhang wrote a version: %#v err=%v", afterFailure, err)
	}
}

func TestAtomicRangeDeletionExplicitlyEditsIndependentAudioAndRespectsLocks(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_explicit_audio_ripple"
	agenttest.CreateAgentDraft(t, database, draftID)
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	document, err := agenttest.ComposeTimeline(draftID, 1, []agenttest.TimelineSelection{
		{AssetID: "talk", AssetKind: "video", SourceStartFrame: 0, SourceEndFrame: 30},
		{AssetID: "talk", AssetKind: "video", SourceStartFrame: 30, SourceEndFrame: 60},
		{AssetID: "talk", AssetKind: "video", SourceStartFrame: 60, SourceEndFrame: 90},
	})
	if err != nil {
		t.Fatal(err)
	}
	document.Tracks[4].Clips = []timeline.Clip{{
		TimelineClipID: "bgm_ripple", TrackID: "bgm", AssetID: "music", AssetKind: "audio",
		TimelineEndFrame: 90, SourceEndFrame: 90, PlaybackRate: 1,
	}}
	document.Tracks[6].Clips = []timeline.Clip{{
		TimelineClipID: "sfx_ripple", TrackID: "sfx", AssetID: "effect", AssetKind: "audio",
		TimelineStartFrame: 60, TimelineEndFrame: 90,
		SourceEndFrame: 30, PlaybackRate: 1,
	}}
	if persisted, persistErr := seedTimelineVersion(
		exec, t.Context(), draftID, document, "fixture", nil,
	); persistErr != nil || persisted.Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("persisted=%#v err=%v", persisted, persistErr)
	}
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	deleted := executeAtomicTimelineTool(t, exec, ctx, "timeline.delete", rushestools.TimelineDeleteInput{
		"kind": "delete_range", "start_frame": 30, "end_frame": 60,
	})
	changedTracks := map[string]bool{}
	for _, target := range deleted.Data["changed_targets"].([]map[string]any) {
		changedTracks[StringValue(target["track_id"])] = true
	}
	if !changedTracks["bgm"] || !changedTracks["sfx"] {
		t.Fatalf("range delete must report independent audio changes: %#v", deleted.Data)
	}
	rippled, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil || rippled.Version != 2 || rippled.DurationFrames != 60 ||
		len(rippled.Tracks[4].Clips) != 2 ||
		rippled.Tracks[4].Clips[1].SourceStartFrame != 60 ||
		rippled.Tracks[6].Clips[0].TimelineStartFrame != 30 {
		t.Fatalf("rippled=%#v err=%v", rippled, err)
	}
	executeAtomicTimelineTool(t, exec, ctx, "timeline.update", rushestools.TimelineUpdateInput{
		"kind": "set_track_state", "track_id": "bgm", "locked": true,
	})
	raw, err := exec.ExecuteTool(ctx, "timeline.delete", rushestools.TimelineDeleteInput{
		"kind": "delete_source_range", "asset_id": "talk",
		"source_start_frame": 0, "source_end_frame": 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	failed := raw.(rushestools.ToolResult)
	if failed.Status != string(rushestools.StatusFailed) ||
		failed.Data["semantic_error_kind"] != timeline.SemanticTrackLocked {
		t.Fatalf("explicit source range delete must respect BGM lock: %#v", failed)
	}
	latest, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil || latest.Version != 3 || latest.DurationFrames != 60 {
		t.Fatalf("failed locked range delete wrote a version: %#v err=%v", latest, err)
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
