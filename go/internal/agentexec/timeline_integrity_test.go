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

func mustPreservedAudioLineageContext(t *testing.T) preservedAudioLineageContext {
	t.Helper()
	context, err := newPreservedAudioLineageContext()
	if err != nil {
		t.Fatal(err)
	}
	return context
}

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
	lineageContext := mustPreservedAudioLineageContext(t)
	result, err := timeline.ApplyPatch(
		unlockPreservedIndependentAudio(document, preserved, operation, lineageContext), operation,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := restoreIndependentAudioTracks(&result, document, preserved, lineageContext); err != nil {
		t.Fatal(err)
	}
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
		Metadata: map[string]any{preservedAudioLineageMetadataKey: "bgm_sibling"},
	}, {
		TimelineClipID: "bgm_sibling", TrackID: "bgm", AssetID: "music_sibling", AssetKind: "audio",
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
	if err != nil {
		t.Fatal(err)
	}
	if shrunk.DurationFrames != 30 ||
		!validateWithPreservedIndependentAudio(shrunk, shrunkPreserved).Valid ||
		len(shrunk.Tracks[4].Clips) != 2 ||
		shrunk.Tracks[4].Clips[0].TimelineEndFrame != 60 ||
		shrunk.Tracks[4].Clips[0].SourceEndFrame != 60 ||
		shrunk.Tracks[4].Clips[0].Metadata[preservedAudioLineageMetadataKey] != "bgm_sibling" ||
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

func TestAtomicMoveIndependentAudioAcrossTracksRestoresReservedMetadata(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_move_independent_audio_metadata"
	agenttest.CreateAgentDraft(t, database, draftID)
	insertAtomicTimelineAsset(t, database, draftID, "music", "audio", 2, false)
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
	document.Tracks[0].Clips[0].Metadata = map[string]any{
		preservedAudioLineageMetadataKey: "bgm_moving",
	}
	for trackIndex := range document.Tracks {
		if document.Tracks[trackIndex].TrackID != "bgm" {
			continue
		}
		document.Tracks[trackIndex].Clips = []timeline.Clip{{
			TimelineClipID: "bgm_moving", TrackID: "bgm", AssetID: "music", AssetKind: "audio",
			TimelineEndFrame: 20, SourceEndFrame: 20, PlaybackRate: 1,
			Metadata: map[string]any{
				preservedAudioLineageMetadataKey: "user-bgm-value",
				"label":                          "keep-me",
			},
		}}
	}
	if persisted, persistErr := seedTimelineVersion(
		exec, t.Context(), draftID, document, "fixture", nil,
	); persistErr != nil || persisted.Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("persisted=%#v err=%v", persisted, persistErr)
	}

	ctx := rushestools.WithDraftID(t.Context(), draftID)
	moved := executeAtomicTimelineTool(t, exec, ctx, "timeline.update", rushestools.TimelineUpdateInput{
		"kind": "move_clip", "timeline_clip_id": "bgm_moving",
		"target_track_id": "voiceover", "target_frame": 10, "mode": "overwrite",
	})
	if moved.Data["timeline_id"] != draftID+":v2" {
		t.Fatalf("cross-track move result=%#v", moved)
	}
	latest, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil {
		t.Fatal(err)
	}
	var movedClip timeline.Clip
	for _, track := range latest.Tracks {
		for _, clip := range track.Clips {
			if clip.TimelineClipID == "bgm_moving" {
				movedClip = clip
			}
		}
	}
	if movedClip.TrackID != "voiceover" ||
		movedClip.Metadata[preservedAudioLineageMetadataKey] != "user-bgm-value" ||
		movedClip.Metadata["label"] != "keep-me" {
		t.Fatalf("temporary lineage leaked or user metadata was overwritten: %#v", movedClip)
	}
	if latest.Tracks[0].Clips[0].Metadata[preservedAudioLineageMetadataKey] != "bgm_moving" {
		t.Fatalf("non-audio legacy metadata was altered: %#v", latest.Tracks[0].Clips[0])
	}
	checkedRaw, err := exec.ExecuteTool(ctx, "timeline.check", rushestools.TimelineCheckInput{
		TimelineID: latest.TimelineID,
	})
	if err != nil || checkedRaw.(rushestools.ToolResult).Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("cross-track timeline check=%#v err=%v", checkedRaw, err)
	}
}

func TestAtomicMoveVoiceoverIntoIndependentAudioPreservesLegacyMetadataCollision(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_move_voiceover_metadata_collision"
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
	for trackIndex := range document.Tracks {
		switch document.Tracks[trackIndex].TrackID {
		case "bgm":
			document.Tracks[trackIndex].Clips = []timeline.Clip{{
				TimelineClipID: "bgm_existing", TrackID: "bgm", AssetID: "music", AssetKind: "audio",
				TimelineEndFrame: 20, SourceEndFrame: 20, PlaybackRate: 1,
			}}
		case "voiceover":
			document.Tracks[trackIndex].Clips = []timeline.Clip{{
				TimelineClipID: "voice_moving", TrackID: "voiceover", AssetID: "voice", AssetKind: "audio",
				TimelineEndFrame: 20, SourceEndFrame: 20, PlaybackRate: 1,
				Metadata: map[string]any{
					preservedAudioLineageMetadataKey: "bgm_existing",
					"label":                          "voice-user-metadata",
				},
			}}
		}
	}
	if persisted, persistErr := seedTimelineVersion(
		exec, t.Context(), draftID, document, "fixture", nil,
	); persistErr != nil || persisted.Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("persisted=%#v err=%v", persisted, persistErr)
	}

	ctx := rushestools.WithDraftID(t.Context(), draftID)
	executeAtomicTimelineTool(t, exec, ctx, "timeline.update", rushestools.TimelineUpdateInput{
		"kind": "move_clip", "timeline_clip_id": "voice_moving",
		"target_track_id": "bgm", "target_frame": 30, "mode": "overwrite",
	})
	latest, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil {
		t.Fatal(err)
	}
	var moved timeline.Clip
	for _, track := range latest.Tracks {
		for _, clip := range track.Clips {
			if clip.TimelineClipID == "voice_moving" {
				moved = clip
			}
		}
	}
	if moved.TrackID != "bgm" ||
		moved.Metadata[preservedAudioLineageMetadataKey] != "bgm_existing" ||
		moved.Metadata["label"] != "voice-user-metadata" {
		t.Fatalf("voiceover legacy metadata was mistaken for a temporary marker: %#v", moved)
	}
}

func TestAtomicUnlinkDoesNotMutateLockedIndependentAudioPartner(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_locked_free_audio_unlink"
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
	for trackIndex := range document.Tracks {
		switch document.Tracks[trackIndex].TrackID {
		case "voiceover":
			document.Tracks[trackIndex].Clips = []timeline.Clip{{
				TimelineClipID: "voice_linked", TrackID: "voiceover", AssetID: "shared", AssetKind: "audio",
				TimelineEndFrame: 20, SourceEndFrame: 20, PlaybackRate: 1,
				Linked: true, ParentBlockID: "free_audio_group",
			}}
		case "bgm":
			document.Tracks[trackIndex].Locked = true
			document.Tracks[trackIndex].Clips = []timeline.Clip{{
				TimelineClipID: "bgm_linked", TrackID: "bgm", AssetID: "shared", AssetKind: "audio",
				TimelineEndFrame: 20, SourceEndFrame: 20, PlaybackRate: 1,
				Linked: true, ParentBlockID: "free_audio_group",
			}}
		}
	}
	if report := timeline.Validate(document); !report.Valid {
		t.Fatalf("fixture invalid: %#v", report)
	}
	if persisted, persistErr := seedTimelineVersion(
		exec, t.Context(), draftID, document, "fixture", nil,
	); persistErr != nil || persisted.Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("persisted=%#v err=%v", persisted, persistErr)
	}

	raw, err := exec.ExecuteTool(
		rushestools.WithDraftID(t.Context(), draftID),
		"timeline.update",
		rushestools.TimelineUpdateInput{
			"kind": "set_clip_linked", "timeline_clip_id": "voice_linked", "linked": false,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	failed := raw.(rushestools.ToolResult)
	if failed.Status != string(rushestools.StatusFailed) ||
		failed.Data["semantic_error_kind"] != timeline.SemanticTrackLocked {
		t.Fatalf("locked linked partner mutation result=%#v", failed)
	}
	latest, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil || latest.Version != 1 || !latest.Tracks[3].Clips[0].Linked ||
		!latest.Tracks[4].Clips[0].Linked ||
		latest.Tracks[3].Clips[0].ParentBlockID != "free_audio_group" ||
		latest.Tracks[4].Clips[0].ParentBlockID != "free_audio_group" {
		t.Fatalf("failed unlink persisted or mutated the locked group: latest=%#v err=%v", latest, err)
	}
}

func TestAtomicUnlinkStaleParentDoesNotMutateLockedIndependentAudio(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_locked_stale_audio_unlink"
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
	for trackIndex := range document.Tracks {
		switch document.Tracks[trackIndex].TrackID {
		case "voiceover":
			document.Tracks[trackIndex].Clips = []timeline.Clip{{
				TimelineClipID: "voice_stale", TrackID: "voiceover", AssetID: "shared", AssetKind: "audio",
				TimelineEndFrame: 20, SourceEndFrame: 20, PlaybackRate: 1,
				ParentBlockID: "free_audio_group",
			}}
		case "bgm":
			document.Tracks[trackIndex].Locked = true
			document.Tracks[trackIndex].Clips = []timeline.Clip{{
				TimelineClipID: "bgm_linked", TrackID: "bgm", AssetID: "shared", AssetKind: "audio",
				TimelineEndFrame: 20, SourceEndFrame: 20, PlaybackRate: 1,
				Linked: true, ParentBlockID: "free_audio_group",
			}}
		}
	}
	if report := timeline.Validate(document); !report.Valid {
		t.Fatalf("fixture invalid: %#v", report)
	}
	if persisted, persistErr := seedTimelineVersion(
		exec, t.Context(), draftID, document, "fixture", nil,
	); persistErr != nil || persisted.Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("persisted=%#v err=%v", persisted, persistErr)
	}

	result := executeAtomicTimelineTool(
		t,
		exec,
		rushestools.WithDraftID(t.Context(), draftID),
		"timeline.update",
		rushestools.TimelineUpdateInput{
			"kind": "set_clip_linked", "timeline_clip_id": "voice_stale", "linked": false,
		},
	)
	if result.Data["timeline_id"] != draftID+":v2" {
		t.Fatalf("stale-parent unlink result=%#v", result)
	}
	latest, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil || latest.Tracks[3].Clips[0].Linked ||
		latest.Tracks[3].Clips[0].ParentBlockID != "" ||
		!latest.Tracks[4].Clips[0].Linked ||
		latest.Tracks[4].Clips[0].ParentBlockID != "free_audio_group" {
		t.Fatalf("stale-parent unlink mutated locked BGM: latest=%#v err=%v", latest, err)
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

func TestTrustedOverhangCustomSplitUsesTemporaryClipLineage(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_custom_audio_split_lineage"
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
			TimelineClipID: "bgm_target", TrackID: "bgm", AssetID: "music", AssetKind: "audio",
			TimelineEndFrame: 90, SourceEndFrame: 90, PlaybackRate: 1,
		},
		{
			TimelineClipID: "bgm_duplicate", TrackID: "bgm", AssetID: "music", AssetKind: "audio",
			TimelineEndFrame: 90, SourceEndFrame: 90, PlaybackRate: 1,
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
	current, base, err := exec.timelineMutationSnapshot(t.Context(), draftID)
	if err != nil {
		t.Fatal(err)
	}
	trusted, err := exec.preservedIndependentAudioFromStoredProof(t.Context(), draftID, current)
	if err != nil {
		t.Fatal(err)
	}
	operation := map[string]any{
		"kind": "split_clip", "timeline_clip_id": "bgm_target", "split_frame": 30,
		"new_timeline_clip_id": "custom_tail",
	}
	preserved := preserveIndependentAudioForOperation(current, operation)
	lineageContext := mustPreservedAudioLineageContext(t)
	result, err := timeline.ApplyPatch(
		unlockPreservedIndependentAudio(current, preserved, operation, lineageContext), operation,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := restoreIndependentAudioTracks(&result, current, preserved, lineageContext); err != nil {
		t.Fatal(err)
	}
	proof := deriveIndependentAudioValidationProof(
		current, result, trusted, preserved,
		validateWithPreservedIndependentAudio(current, trusted).Valid,
		lineageContext,
	)
	stripPreservedAudioLineage(&result, current, lineageContext)
	stripPreservedAudioLineageFromTracks(proof, current, lineageContext)
	if report := validateWithPreservedIndependentAudio(result, proof); !report.Valid {
		t.Fatalf("custom split proof=%#v report=%#v", proof, report)
	}
	split, err := exec.persistTimelineFromSnapshotWithPreservedAudio(
		t.Context(), draftID, result, "timeline.split", operation, base, proof,
	)
	if err != nil || split.Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("custom split persistence=%#v err=%v", split, err)
	}
	latest, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil || latest.Version != 3 {
		t.Fatalf("latest=%#v err=%v", latest, err)
	}
	assertNoPreservedAudioLineage(t, latest)
	assertStoredOverhangCheckAndRender(t, exec, draftID, latest, 3)
}

func TestTrustedAssetlessOverhangRangeSplitUsesTemporaryClipLineage(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_assetless_audio_split_lineage"
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
	document.Tracks[4].Clips = []timeline.Clip{{
		TimelineClipID: "assetless_bgm", TrackID: "bgm", AssetKind: "audio", Role: "bgm",
		TimelineStartFrame: 30, TimelineEndFrame: 90, PlaybackRate: 1,
	}}
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
		t.Fatalf("assetless range delete result=%#v", deleted)
	}
	latest, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil || latest.Version != 3 || latest.DurationFrames != 50 {
		t.Fatalf("latest=%#v err=%v", latest, err)
	}
	assertNoPreservedAudioLineage(t, latest)
	assertStoredOverhangCheckAndRender(t, exec, draftID, latest, 3)
}

func assertNoPreservedAudioLineage(t *testing.T, document timeline.Document) {
	t.Helper()
	for _, track := range document.Tracks {
		for _, clip := range track.Clips {
			if _, leaked := clip.Metadata[preservedAudioLineageMetadataKey]; leaked {
				t.Fatalf("temporary audio lineage leaked into persisted clip: %#v", clip)
			}
		}
	}
}

func TestValidationProofCombinesTrustedAndPreservedOverhangPerClip(t *testing.T) {
	current, err := agenttest.ComposeTimeline("draft_combined_audio_proof", 1, []agenttest.TimelineSelection{{
		AssetID: "visual", AssetKind: "video", SourceEndFrame: 60,
	}})
	if err != nil {
		t.Fatal(err)
	}
	current.Tracks[4].Clips = []timeline.Clip{
		{
			TimelineClipID: "bgm_trusted_tail", TrackID: "bgm", AssetID: "trusted", AssetKind: "audio",
			TimelineEndFrame: 90, SourceEndFrame: 90, PlaybackRate: 1,
		},
		{
			TimelineClipID: "bgm_preserved_tail", TrackID: "bgm", AssetID: "preserved", AssetKind: "audio",
			TimelineEndFrame: 60, SourceEndFrame: 60, PlaybackRate: 1,
		},
	}
	result := current
	result.Version = 2
	result.TimelineID = result.DraftID + ":v2"
	result.DurationFrames = 30
	result.Tracks = append([]timeline.Track(nil), current.Tracks...)
	result.Tracks[0] = copyTimelineTrack(current.Tracks[0])
	result.Tracks[0].Clips[0].TimelineEndFrame = 30
	result.Tracks[0].Clips[0].SourceEndFrame = 30
	result.Tracks[4] = copyTimelineTrack(current.Tracks[4])
	result.Tracks[4].Clips[0].TimelineEndFrame = 60
	result.Tracks[4].Clips[0].SourceEndFrame = 60

	preservedTrack := copyTimelineTrack(current.Tracks[4])
	preservedTrack.Clips = []timeline.Clip{current.Tracks[4].Clips[1]}
	proof := deriveIndependentAudioValidationProof(
		current,
		result,
		map[string]timeline.Track{"bgm": copyTimelineTrack(current.Tracks[4])},
		map[string]timeline.Track{"bgm": preservedTrack},
		true,
		preservedAudioLineageContext{},
	)
	if _, exists := proof["bgm"]; !exists {
		t.Fatalf("trusted and preserved overhang must compose per clip: %#v", proof)
	}
	if report := validateWithPreservedIndependentAudio(result, proof); !report.Valid {
		t.Fatalf("combined per-clip proof report=%#v", report)
	}
}

func TestValidationProofRejectsUnpreservedLinkedOverhangWithUnchangedTiming(t *testing.T) {
	current, err := agenttest.ComposeTimeline("draft_unpreserved_linked_overhang", 1, []agenttest.TimelineSelection{{
		AssetID: "visual", AssetKind: "video", SourceEndFrame: 60,
	}})
	if err != nil {
		t.Fatal(err)
	}
	current.Tracks[4].Clips = []timeline.Clip{{
		TimelineClipID: "bgm_linked", TrackID: "bgm", AssetID: "music", AssetKind: "audio",
		TimelineEndFrame: 60, SourceEndFrame: 60, PlaybackRate: 1,
		Linked: true, ParentBlockID: "audio_only_group",
	}}
	result := current
	result.DurationFrames = 30
	result.Tracks = append([]timeline.Track(nil), current.Tracks...)
	result.Tracks[0] = copyTimelineTrack(current.Tracks[0])
	result.Tracks[0].Clips[0].TimelineEndFrame = 30
	result.Tracks[0].Clips[0].SourceEndFrame = 30
	proof := deriveIndependentAudioValidationProof(
		current, result, nil, nil, true, preservedAudioLineageContext{},
	)
	if _, allowed := proof["bgm"]; allowed {
		t.Fatalf("unchanged timing must not authorize an unpreserved linked overhang: %#v", proof)
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

func TestIndependentAudioGuardRespectsLinkedGroupEditsAndLocks(t *testing.T) {
	operations := []struct {
		name      string
		operation map[string]any
		assertBGM func(*testing.T, timeline.Track)
	}{
		{
			name: "trim",
			operation: map[string]any{
				"kind": "trim_clip", "timeline_clip_id": "visual",
				"source_start_frame": 10, "source_end_frame": 50,
			},
			assertBGM: func(t *testing.T, track timeline.Track) {
				t.Helper()
				if len(track.Clips) != 1 || track.Clips[0].SourceStartFrame != 10 ||
					track.Clips[0].SourceEndFrame != 50 || track.Clips[0].TimelineEndFrame != 40 {
					t.Fatalf("linked BGM was not trimmed atomically: %#v", track)
				}
			},
		},
		{
			name: "split",
			operation: map[string]any{
				"kind": "split_clip", "timeline_clip_id": "visual", "split_frame": 30,
			},
			assertBGM: func(t *testing.T, track timeline.Track) {
				t.Helper()
				if len(track.Clips) != 2 || track.Clips[0].TimelineEndFrame != 30 ||
					track.Clips[1].TimelineStartFrame != 30 {
					t.Fatalf("linked BGM was not split atomically: %#v", track)
				}
			},
		},
	}
	for _, test := range operations {
		t.Run(test.name, func(t *testing.T) {
			document := linkedIndependentAudioDocument()
			preserved := preserveIndependentAudioForOperation(document, test.operation)
			lineageContext := mustPreservedAudioLineageContext(t)
			for _, trackID := range independentAudioTrackIDs {
				if _, exists := preserved[trackID]; exists {
					t.Fatalf("linked %s must not be preserved: %#v", trackID, preserved)
				}
			}

			result, err := timeline.ApplyPatch(
				unlockPreservedIndependentAudio(document, preserved, test.operation, lineageContext), test.operation,
			)
			if err != nil {
				t.Fatal(err)
			}
			test.assertBGM(t, result.Tracks[4])
			if test.name == "trim" && result.Tracks[6].Clips[0].SourceStartFrame != 10 {
				t.Fatalf("linked SFX was not trimmed atomically: %#v", result.Tracks[6])
			}
			if test.name == "split" && len(result.Tracks[6].Clips) != 2 {
				t.Fatalf("linked SFX was not split atomically: %#v", result.Tracks[6])
			}

			for _, lockedTrackIndex := range []int{4, 6} {
				locked := linkedIndependentAudioDocument()
				locked.Tracks[lockedTrackIndex].Clips = append(
					locked.Tracks[lockedTrackIndex].Clips,
					timeline.Clip{
						TimelineClipID: "independent", TrackID: locked.Tracks[lockedTrackIndex].TrackID,
						AssetID: "independent", AssetKind: "audio", TimelineStartFrame: 45,
						TimelineEndFrame: 60, SourceEndFrame: 15, PlaybackRate: 1,
					},
				)
				locked.Tracks[lockedTrackIndex].Locked = true
				lockedPreserved := preserveIndependentAudioForOperation(locked, test.operation)
				if len(lockedPreserved[locked.Tracks[lockedTrackIndex].TrackID].Clips) != 1 {
					t.Fatalf("unrelated audio was not protected on mixed linked track: %#v", lockedPreserved)
				}
				_, err := timeline.ApplyPatch(
					unlockPreservedIndependentAudio(locked, lockedPreserved, test.operation, lineageContext), test.operation,
				)
				semantic, ok := err.(*timeline.SemanticError)
				if !ok || semantic.Kind != timeline.SemanticTrackLocked ||
					semantic.TrackID != locked.Tracks[lockedTrackIndex].TrackID {
					t.Fatalf("locked linked track %s err=%#v", locked.Tracks[lockedTrackIndex].TrackID, err)
				}
			}
		})
	}
}

func TestAtomicLinkedTrimPreservesUnrelatedAudioOnMixedTracks(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_linked_independent_audio"
	agenttest.CreateAgentDraft(t, database, draftID)
	insertAtomicTimelineAsset(t, database, draftID, "video", "video", 2, false)
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	document := linkedIndependentAudioDocument()
	for _, trackIndex := range []int{4, 6} {
		track := &document.Tracks[trackIndex]
		track.Clips = append(track.Clips, timeline.Clip{
			TimelineClipID: track.TrackID + "_independent", TrackID: track.TrackID,
			AssetID: track.TrackID + "_tail", AssetKind: "audio",
			TimelineStartFrame: 45, TimelineEndFrame: 60,
			SourceEndFrame: 15, PlaybackRate: 1,
		})
	}
	if persisted, persistErr := seedTimelineVersion(
		exec, t.Context(), draftID, document, "fixture", nil,
	); persistErr != nil || persisted.Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("persisted=%#v err=%v", persisted, persistErr)
	}

	ctx := rushestools.WithDraftID(t.Context(), draftID)
	executeAtomicTimelineTool(t, exec, ctx, "timeline.update", rushestools.TimelineUpdateInput{
		"kind": "trim_clip", "timeline_clip_id": "visual",
		"source_start_frame": 0, "source_end_frame": 30,
	})
	latest, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil || latest.Version != 2 || latest.DurationFrames != 30 {
		t.Fatalf("latest=%#v err=%v", latest, err)
	}
	for _, trackIndex := range []int{4, 6} {
		track := latest.Tracks[trackIndex]
		if len(track.Clips) != 2 || track.Clips[0].TimelineEndFrame != 30 ||
			track.Clips[1].TimelineStartFrame != 45 || track.Clips[1].TimelineEndFrame != 60 {
			t.Fatalf("mixed %s track was not edited per clip: %#v", track.TrackID, track)
		}
	}
	checkedRaw, err := exec.ExecuteTool(ctx, "timeline.check", rushestools.TimelineCheckInput{
		TimelineID: latest.TimelineID,
	})
	if err != nil || checkedRaw.(rushestools.ToolResult).Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("stored mixed-track proof check=%#v err=%v", checkedRaw, err)
	}
}

func TestPartialAudioRestoreRemovesRippleDerivedClips(t *testing.T) {
	document, err := agenttest.ComposeTimeline("draft_partial_audio_restore", 1, []agenttest.TimelineSelection{
		{AssetID: "visual", AssetKind: "video", SourceStartFrame: 0, SourceEndFrame: 30},
		{AssetID: "visual", AssetKind: "video", SourceStartFrame: 30, SourceEndFrame: 60, HasAudio: true},
		{AssetID: "visual", AssetKind: "video", SourceStartFrame: 60, SourceEndFrame: 90},
	})
	if err != nil {
		t.Fatal(err)
	}
	const groupID = "linked_delete"
	document.Tracks[0].Clips[1].Linked = true
	document.Tracks[0].Clips[1].ParentBlockID = groupID
	document.Tracks[2].Clips[0].Linked = true
	document.Tracks[2].Clips[0].ParentBlockID = groupID
	document.Tracks[4].Clips = []timeline.Clip{
		{
			TimelineClipID: "bgm_linked_delete", TrackID: "bgm", AssetID: "music_linked", AssetKind: "audio",
			TimelineStartFrame: 30, TimelineEndFrame: 60,
			SourceStartFrame: 0, SourceEndFrame: 30, PlaybackRate: 1,
			Linked: true, ParentBlockID: groupID,
		},
		{
			TimelineClipID: "bgm_independent_span", TrackID: "bgm", AssetKind: "audio",
			TimelineStartFrame: 15, TimelineEndFrame: 75,
			PlaybackRate: 1,
		},
	}
	if report := timeline.Validate(document); !report.Valid {
		t.Fatalf("fixture invalid: %#v", report)
	}
	operation := map[string]any{
		"kind": "delete_clip", "timeline_clip_id": document.Tracks[0].Clips[1].TimelineClipID,
	}
	preserved := preserveIndependentAudioForOperation(document, operation)
	lineageContext := mustPreservedAudioLineageContext(t)
	if len(preserved["bgm"].Clips) != 1 || preserved["bgm"].Clips[0].TimelineClipID != "bgm_independent_span" {
		t.Fatalf("partial snapshot=%#v", preserved)
	}
	result, err := timeline.ApplyPatch(
		unlockPreservedIndependentAudio(document, preserved, operation, lineageContext), operation,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := restoreIndependentAudioTracks(&result, document, preserved, lineageContext); err != nil {
		t.Fatal(err)
	}
	if len(result.Tracks[4].Clips) != 1 ||
		result.Tracks[4].Clips[0].TimelineClipID != "bgm_independent_span" ||
		result.Tracks[4].Clips[0].TimelineStartFrame != 15 ||
		result.Tracks[4].Clips[0].TimelineEndFrame != 75 {
		t.Fatalf("ripple-derived clip survived partial restore: %#v", result.Tracks[4])
	}
	proof := deriveIndependentAudioValidationProof(
		document, result, nil, preserved, true, lineageContext,
	)
	if report := validateWithPreservedIndependentAudio(result, proof); !report.Valid {
		t.Fatalf("restored independent span proof=%#v report=%#v", proof, report)
	}
}

func TestPartialAudioRestoreKeepsUnrelatedCustomSplitID(t *testing.T) {
	document, err := agenttest.ComposeTimeline("draft_partial_audio_split_collision", 1, []agenttest.TimelineSelection{{
		AssetID: "visual", AssetKind: "video", SourceEndFrame: 120,
	}})
	if err != nil {
		t.Fatal(err)
	}
	document.Tracks[4].Clips = []timeline.Clip{
		{
			TimelineClipID: "target", TrackID: "bgm", AssetID: "shared_audio", AssetKind: "audio",
			TimelineEndFrame: 60, SourceEndFrame: 60, PlaybackRate: 1,
			Metadata: map[string]any{preservedAudioLineageMetadataKey: "music"},
		},
		{
			TimelineClipID: "music", TrackID: "bgm", AssetID: "shared_audio", AssetKind: "audio",
			TimelineStartFrame: 60, TimelineEndFrame: 120,
			SourceEndFrame: 60, PlaybackRate: 1,
			Metadata: map[string]any{preservedAudioLineageMetadataKey: map[string]any{"user": true}},
		},
	}
	document.Tracks[0].Clips[0].Metadata = map[string]any{
		preservedAudioLineageMetadataKey: "visual-user-value",
	}
	operation := map[string]any{
		"kind": "split_clip", "timeline_clip_id": "target", "split_frame": 30,
		"new_timeline_clip_id": "music_split_30",
	}
	preserved := preserveIndependentAudioForOperation(document, operation)
	lineageContext := mustPreservedAudioLineageContext(t)
	result, err := timeline.ApplyPatch(
		unlockPreservedIndependentAudio(document, preserved, operation, lineageContext), operation,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := restoreIndependentAudioTracks(&result, document, preserved, lineageContext); err != nil {
		t.Fatal(err)
	}
	stripPreservedAudioLineage(&result, document, lineageContext)
	if len(result.Tracks[4].Clips) != 3 || result.Tracks[4].Clips[1].TimelineClipID != "music_split_30" ||
		result.Tracks[4].Clips[1].AssetID != "shared_audio" {
		t.Fatalf("unrelated custom split output was removed: %#v", result.Tracks[4])
	}
	if result.Tracks[4].Clips[0].Metadata[preservedAudioLineageMetadataKey] != "music" ||
		result.Tracks[4].Clips[1].Metadata[preservedAudioLineageMetadataKey] != "music" ||
		!reflect.DeepEqual(
			result.Tracks[4].Clips[2].Metadata[preservedAudioLineageMetadataKey],
			map[string]any{"user": true},
		) || result.Tracks[0].Clips[0].Metadata[preservedAudioLineageMetadataKey] != "visual-user-value" {
		t.Fatalf("pre-existing reserved metadata was not restored: %#v", result)
	}
}

func TestPartialAudioRestoreRejectsProtectedIDCollision(t *testing.T) {
	document, err := agenttest.ComposeTimeline("draft_partial_audio_exact_collision", 1, []agenttest.TimelineSelection{{
		AssetID: "visual", AssetKind: "video", SourceEndFrame: 120,
	}})
	if err != nil {
		t.Fatal(err)
	}
	document.Tracks[4].Clips = []timeline.Clip{
		{
			TimelineClipID: "target", TrackID: "bgm", AssetID: "target_audio", AssetKind: "audio",
			TimelineEndFrame: 60, SourceEndFrame: 60, PlaybackRate: 1,
		},
		{
			TimelineClipID: "protected", TrackID: "bgm", AssetID: "protected_audio", AssetKind: "audio",
			TimelineStartFrame: 60, TimelineEndFrame: 120,
			SourceEndFrame: 60, PlaybackRate: 1,
		},
	}
	operation := map[string]any{
		"kind": "split_clip", "timeline_clip_id": "target", "split_frame": 1,
		"new_timeline_clip_id": "protected",
	}
	preserved := preserveIndependentAudioForOperation(document, operation)
	lineageContext := mustPreservedAudioLineageContext(t)
	result, err := timeline.ApplyPatch(
		unlockPreservedIndependentAudio(document, preserved, operation, lineageContext), operation,
	)
	if err != nil {
		t.Fatal(err)
	}
	err = restoreIndependentAudioTracks(&result, document, preserved, lineageContext)
	if err == nil || err.Error() != `受保护音频片段 ID 冲突: duplicate timeline_clip_id "protected"` {
		t.Fatalf("protected ID collision err=%v result=%#v", err, result.Tracks[4])
	}
	if len(result.Tracks[4].Clips) != 3 {
		t.Fatalf("collision must be rejected before either duplicate is filtered: %#v", result.Tracks[4])
	}
}

func TestLockedMixedAudioTrackUnlocksOnlyProtectedRippleFootprint(t *testing.T) {
	document, err := agenttest.ComposeTimeline("draft_locked_mixed_audio", 1, []agenttest.TimelineSelection{
		{AssetID: "visual", AssetKind: "video", SourceStartFrame: 0, SourceEndFrame: 30},
		{AssetID: "visual", AssetKind: "video", SourceStartFrame: 30, SourceEndFrame: 60},
	})
	if err != nil {
		t.Fatal(err)
	}
	document.Tracks[4].Locked = true
	document.Tracks[4].Clips = []timeline.Clip{
		{
			TimelineClipID: "bgm_linked_upstream", TrackID: "bgm", AssetID: "linked", AssetKind: "audio",
			TimelineEndFrame: 10, SourceEndFrame: 10, PlaybackRate: 1,
			Linked: true, ParentBlockID: "unrelated_upstream_group",
		},
		{
			TimelineClipID: "bgm_unlinked_tail", TrackID: "bgm", AssetID: "tail", AssetKind: "audio",
			TimelineStartFrame: 45, TimelineEndFrame: 60,
			SourceEndFrame: 15, PlaybackRate: 1,
		},
	}
	operation := map[string]any{
		"kind": "trim_clip", "timeline_clip_id": document.Tracks[0].Clips[0].TimelineClipID,
		"source_start_frame": 0, "source_end_frame": 20,
	}
	preserved := preserveIndependentAudioForOperation(document, operation)
	lineageContext := mustPreservedAudioLineageContext(t)
	result, err := timeline.ApplyPatch(
		unlockPreservedIndependentAudio(document, preserved, operation, lineageContext), operation,
	)
	if err != nil {
		t.Fatalf("protected downstream clip must not cause a lock conflict: %v", err)
	}
	if err := restoreIndependentAudioTracks(&result, document, preserved, lineageContext); err != nil {
		t.Fatal(err)
	}
	if !result.Tracks[4].Locked || len(result.Tracks[4].Clips) != 2 ||
		result.Tracks[4].Clips[0].TimelineStartFrame != 0 ||
		result.Tracks[4].Clips[1].TimelineStartFrame != 45 {
		t.Fatalf("locked mixed track was not restored clip-granularly: %#v", result.Tracks[4])
	}
}

func TestMoveClipDoesNotRestoreExplicitTargetTrackFootprint(t *testing.T) {
	document, err := agenttest.ComposeTimeline("draft_move_audio_target", 1, []agenttest.TimelineSelection{{
		AssetID: "visual", AssetKind: "video", SourceEndFrame: 120,
	}})
	if err != nil {
		t.Fatal(err)
	}
	document.Tracks[4].Clips = []timeline.Clip{
		{
			TimelineClipID: "bgm_moving", TrackID: "bgm", AssetID: "music_a", AssetKind: "audio",
			TimelineEndFrame: 20, SourceEndFrame: 20, PlaybackRate: 1,
		},
		{
			TimelineClipID: "bgm_shifted", TrackID: "bgm", AssetID: "music_b", AssetKind: "audio",
			TimelineStartFrame: 30, TimelineEndFrame: 40,
			SourceEndFrame: 10, PlaybackRate: 1,
		},
	}
	operation := map[string]any{
		"kind": "move_clip", "timeline_clip_id": "bgm_moving",
		"target_track_id": "bgm", "target_frame": 10, "mode": "insert",
	}
	if preserved := preserveIndependentAudioForOperation(document, operation); len(preserved) != 0 {
		t.Fatalf("explicit move target footprint must not be restored: %#v", preserved)
	}
	result, err := timeline.ApplyPatch(document, operation)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Tracks[4].Clips) != 2 ||
		result.Tracks[4].Clips[0].TimelineClipID != "bgm_moving" ||
		result.Tracks[4].Clips[0].TimelineStartFrame != 10 ||
		result.Tracks[4].Clips[1].TimelineClipID != "bgm_shifted" ||
		result.Tracks[4].Clips[1].TimelineStartFrame != 50 {
		t.Fatalf("move target semantics were lost: %#v", result.Tracks[4])
	}
}

func TestPrimaryRippleDoesNotProtectOrUnlockAnotherLinkedGroup(t *testing.T) {
	document, err := agenttest.ComposeTimeline("draft_other_linked_group", 1, []agenttest.TimelineSelection{
		{AssetID: "visual", AssetKind: "video", SourceStartFrame: 0, SourceEndFrame: 30, HasAudio: true},
		{AssetID: "visual", AssetKind: "video", SourceStartFrame: 30, SourceEndFrame: 60, HasAudio: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	for index, groupID := range []string{"visual_group_a", "visual_group_b"} {
		document.Tracks[0].Clips[index].Linked = true
		document.Tracks[0].Clips[index].ParentBlockID = groupID
		document.Tracks[2].Clips[index].Linked = true
		document.Tracks[2].Clips[index].ParentBlockID = groupID
	}
	document.Tracks[4].Clips = []timeline.Clip{
		{
			TimelineClipID: "bgm_other_group", TrackID: "bgm", AssetID: "music", AssetKind: "audio",
			TimelineStartFrame: 30, TimelineEndFrame: 60,
			SourceStartFrame: 0, SourceEndFrame: 30, PlaybackRate: 1,
			Linked: true, ParentBlockID: "visual_group_b",
		},
		{
			TimelineClipID: "bgm_unlinked_tail", TrackID: "bgm", AssetID: "tail", AssetKind: "audio",
			TimelineStartFrame: 45, TimelineEndFrame: 60,
			SourceStartFrame: 0, SourceEndFrame: 15, PlaybackRate: 1,
		},
	}
	operation := map[string]any{
		"kind": "trim_clip", "timeline_clip_id": document.Tracks[0].Clips[0].TimelineClipID,
		"source_start_frame": 0, "source_end_frame": 20,
	}
	preserved := preserveIndependentAudioForOperation(document, operation)
	lineageContext := mustPreservedAudioLineageContext(t)
	if len(preserved["bgm"].Clips) != 1 || preserved["bgm"].Clips[0].TimelineClipID != "bgm_unlinked_tail" {
		t.Fatalf("only unlinked audio may be protected: %#v", preserved)
	}
	result, err := timeline.ApplyPatch(
		unlockPreservedIndependentAudio(document, preserved, operation, lineageContext), operation,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := restoreIndependentAudioTracks(&result, document, preserved, lineageContext); err != nil {
		t.Fatal(err)
	}
	if len(result.Tracks[4].Clips) != 2 ||
		result.Tracks[4].Clips[0].TimelineStartFrame != 20 || result.Tracks[4].Clips[0].TimelineEndFrame != 50 ||
		result.Tracks[4].Clips[1].TimelineStartFrame != 45 || result.Tracks[4].Clips[1].TimelineEndFrame != 60 {
		t.Fatalf("other linked group did not follow primary ripple: %#v", result.Tracks[4])
	}

	reorder := map[string]any{
		"kind": "move_clip", "timeline_clip_id": document.Tracks[0].Clips[0].TimelineClipID,
		"target_track_id": "visual_base", "target_frame": 60, "mode": "insert",
	}
	reorderPreserved := preserveIndependentAudioForOperation(document, reorder)
	reordered, err := timeline.ApplyPatch(
		unlockPreservedIndependentAudio(document, reorderPreserved, reorder, lineageContext), reorder,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := restoreIndependentAudioTracks(&reordered, document, reorderPreserved, lineageContext); err != nil {
		t.Fatal(err)
	}
	if len(reordered.Tracks[4].Clips) != 2 ||
		reordered.Tracks[4].Clips[0].TimelineClipID != "bgm_other_group" ||
		reordered.Tracks[4].Clips[0].TimelineStartFrame != 0 ||
		reordered.Tracks[4].Clips[1].TimelineClipID != "bgm_unlinked_tail" ||
		reordered.Tracks[4].Clips[1].TimelineStartFrame != 45 {
		t.Fatalf("reorder froze another linked group: %#v", reordered.Tracks[4])
	}

	locked := document
	locked.Tracks = append([]timeline.Track(nil), document.Tracks...)
	locked.Tracks[4] = copyTimelineTrack(document.Tracks[4])
	locked.Tracks[4].Locked = true
	lockedPreserved := preserveIndependentAudioForOperation(locked, operation)
	_, err = timeline.ApplyPatch(
		unlockPreservedIndependentAudio(locked, lockedPreserved, operation, lineageContext), operation,
	)
	semantic, ok := err.(*timeline.SemanticError)
	if !ok || semantic.Kind != timeline.SemanticTrackLocked || semantic.TrackID != "bgm" {
		t.Fatalf("other linked group lock was bypassed: %#v", err)
	}
	equalDurationTrim := map[string]any{
		"kind": "trim_clip", "timeline_clip_id": locked.Tracks[0].Clips[0].TimelineClipID,
		"source_start_frame": 10, "source_end_frame": 40,
	}
	equalPreserved := preserveIndependentAudioForOperation(locked, equalDurationTrim)
	if _, err := timeline.ApplyPatch(
		unlockPreservedIndependentAudio(locked, equalPreserved, equalDurationTrim, lineageContext), equalDurationTrim,
	); err != nil {
		t.Fatalf("equal-duration trim must not require a downstream ripple unlock: %v", err)
	}
	equalDurationRate := map[string]any{
		"kind": "set_playback_rate", "timeline_clip_id": locked.Tracks[0].Clips[0].TimelineClipID,
		"playback_rate": 1.01,
	}
	ratePreserved := preserveIndependentAudioForOperation(locked, equalDurationRate)
	if _, err := timeline.ApplyPatch(
		unlockPreservedIndependentAudio(locked, ratePreserved, equalDurationRate, lineageContext), equalDurationRate,
	); err != nil {
		t.Fatalf("equal-duration playback rate must not require a downstream ripple unlock: %v", err)
	}
}

func linkedIndependentAudioDocument() timeline.Document {
	document := timeline.Empty("draft_linked_independent_audio", 1)
	document.DurationFrames = 60
	for trackIndex, clip := range map[int]timeline.Clip{
		0: {
			TimelineClipID: "visual", TrackID: "visual_base", AssetID: "video", AssetKind: "video",
		},
		2: {
			TimelineClipID: "original_audio", TrackID: "original_audio", AssetID: "video", AssetKind: "audio",
		},
		4: {
			TimelineClipID: "bgm_linked", TrackID: "bgm", AssetID: "music", AssetKind: "audio",
		},
		6: {
			TimelineClipID: "sfx_linked", TrackID: "sfx", AssetID: "effect", AssetKind: "audio",
		},
	} {
		clip.TimelineEndFrame = 60
		clip.SourceEndFrame = 60
		clip.PlaybackRate = 1
		clip.ParentBlockID = "linked_block"
		clip.Linked = true
		document.Tracks[trackIndex].Clips = []timeline.Clip{clip}
	}
	return document
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
