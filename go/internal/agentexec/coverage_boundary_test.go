package agentexec

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
)

func TestPreviewAlreadyInspectedRequiresLatestSuccessForEveryCheck(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_preview_already_inspected"
	const previewID = "preview_complete"
	agenttest.CreateAgentDraft(t, database, draftID)
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}

	if exec.PreviewAlreadyInspected(t.Context(), draftID, nil) ||
		exec.PreviewAlreadyInspected(t.Context(), draftID, map[string]any{}) {
		t.Fatal("missing preview id must not count as inspected")
	}
	insertPreviewTraceMessage(t, database, draftID, "trace_reply", "reply", "not a tool trace")
	insertPreviewTraceMessage(t, database, draftID, "trace_malformed", "tool", "{")
	insertPreviewTraceMessage(t, database, draftID, "trace_other_tool", "tool", previewTraceJSON(t, map[string]any{
		"tool": "timeline.check", "preview_id": previewID, "preview_check": "decode", "status": "succeeded",
	}))
	insertPreviewTraceMessage(t, database, draftID, "trace_other_preview", "tool", previewTraceJSON(t, map[string]any{
		"tool": "preview.check", "preview_id": "preview_other", "preview_check": "decode", "status": "succeeded",
	}))
	insertPreviewTraceMessage(t, database, draftID, "trace_empty_check", "tool", previewTraceJSON(t, map[string]any{
		"tool": "preview.check", "preview_id": previewID, "status": "succeeded",
	}))

	checks := []string{"decode", "black", "freeze", "silence", "loudness"}
	for index, check := range checks {
		record := map[string]any{"tool": "preview.check", "status": "succeeded"}
		if index%2 == 0 {
			record["preview_id"] = previewID
			record["preview_check"] = check
		} else {
			args, marshalErr := json.Marshal(map[string]any{"preview_id": previewID, "check": check})
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			record["args_summary"] = string(args)
		}
		insertPreviewTraceMessage(
			t, database, draftID, "trace_success_"+check, "tool", previewTraceJSON(t, record),
		)
	}
	if !exec.PreviewAlreadyInspected(
		t.Context(), draftID, map[string]any{"preview_id": previewID},
	) {
		t.Fatal("five successful atomic checks should complete preview inspection")
	}

	// The scan is newest-first and only the latest trace for each check counts.
	insertPreviewTraceMessage(t, database, draftID, "trace_decode_failed_latest", "tool", previewTraceJSON(t, map[string]any{
		"tool": "preview.check", "preview_id": previewID,
		"preview_check": "decode", "status": "failed",
	}))
	if exec.PreviewAlreadyInspected(
		t.Context(), draftID, map[string]any{"artifact_id": previewID},
	) {
		t.Fatal("a newer failed check must supersede the older successful trace")
	}

	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if exec.PreviewAlreadyInspected(
		t.Context(), draftID, map[string]any{"artifact_id": previewID},
	) {
		t.Fatal("message storage failures must fail closed")
	}
}

func TestDerivedAndUniqueSourceAncestorResolution(t *testing.T) {
	base := timeline.Clip{
		TimelineClipID: "clip_base", TrackID: "bgm", AssetID: "asset_music",
		AssetKind: "audio", Role: "bgm", PlaybackRate: 1,
		SourceStartFrame: 10, SourceEndFrame: 210,
	}
	previousByID := map[string]timeline.Clip{base.TimelineClipID: base}
	for _, derivedID := range []string{"clip_base_after_30", "clip_base_split_90"} {
		derived := base
		derived.TimelineClipID = derivedID
		derived.SourceStartFrame = 30
		derived.SourceEndFrame = 120
		ancestor, found := deterministicDerivedClipAncestor(derived, previousByID)
		if !found || ancestor.TimelineClipID != base.TimelineClipID {
			t.Fatalf("derived=%s ancestor=%#v found=%v", derivedID, ancestor, found)
		}
	}

	for _, invalid := range []timeline.Clip{
		func() timeline.Clip {
			clip := base
			clip.TimelineClipID = "clip_base_after_"
			return clip
		}(),
		func() timeline.Clip {
			clip := base
			clip.TimelineClipID = "clip_base_split_not_a_frame"
			return clip
		}(),
		func() timeline.Clip {
			clip := base
			clip.TimelineClipID = "clip_base_after_30"
			clip.SourceEndFrame = base.SourceEndFrame + 1
			return clip
		}(),
	} {
		if ancestor, found := deterministicDerivedClipAncestor(invalid, previousByID); found {
			t.Fatalf("invalid derived clip resolved ancestor=%#v", ancestor)
		}
	}

	descendant := base
	descendant.TimelineClipID = "clip_unknown"
	descendant.SourceStartFrame = 40
	descendant.SourceEndFrame = 80
	if ancestor, found := uniqueSourceAncestor(descendant, nil); found {
		t.Fatalf("empty history ancestor=%#v", ancestor)
	}
	unrelated := base
	unrelated.AssetID = "asset_other"
	ancestor, found := uniqueSourceAncestor(descendant, []timeline.Clip{unrelated, base})
	if !found || ancestor.TimelineClipID != base.TimelineClipID {
		t.Fatalf("unique ancestor=%#v found=%v", ancestor, found)
	}
	second := base
	second.TimelineClipID = "clip_second_source_parent"
	if ancestor, found := uniqueSourceAncestor(descendant, []timeline.Clip{base, second}); found {
		t.Fatalf("ambiguous source ancestry must fail closed: %#v", ancestor)
	}

	lineageContext := preservedAudioLineageContext{prefix: "fixture-lineage"}
	markedMissing := descendant
	markedMissing.Metadata = map[string]any{
		preservedAudioLineageMetadataKey: lineageContext.markerValue("missing_parent"),
	}
	if ancestor, found := independentAudioClipAncestor(
		markedMissing, previousByID, []timeline.Clip{base}, lineageContext,
	); found {
		t.Fatalf("missing explicit lineage parent resolved ancestor=%#v", ancestor)
	}
	derived := descendant
	derived.TimelineClipID = "clip_base_after_30"
	if ancestor, found := independentAudioClipAncestor(
		derived, previousByID, []timeline.Clip{base}, preservedAudioLineageContext{},
	); !found || ancestor.TimelineClipID != base.TimelineClipID {
		t.Fatalf("deterministic fallback ancestor=%#v found=%v", ancestor, found)
	}
	if ancestor, found := independentAudioClipAncestor(
		descendant, previousByID, []timeline.Clip{base}, preservedAudioLineageContext{},
	); !found || ancestor.TimelineClipID != base.TimelineClipID {
		t.Fatalf("unique-source fallback ancestor=%#v found=%v", ancestor, found)
	}
}

func TestBeginIndexedToolCallLocksOnlyTheDeclaredResource(t *testing.T) {
	state := NewTurnInteractionState()
	ctx := WithTurnInteractionState(t.Context(), state)
	MarkDecisionCreatedThisTurn(ctx, "decision_indexed", true)

	releaseWrite, decisionID := state.BeginIndexedToolCall(
		"speech", []string{"asset_a"}, true, false,
	)
	if decisionID != "decision_indexed" {
		releaseWrite()
		t.Fatalf("decision=%q", decisionID)
	}

	sameResource := make(chan func(), 1)
	go func() {
		release, _ := state.BeginIndexedToolCall("speech", []string{"asset_a"}, false, false)
		sameResource <- release
	}()
	select {
	case release := <-sameResource:
		release()
		releaseWrite()
		t.Fatal("same-resource reader bypassed detector write lock")
	case <-time.After(20 * time.Millisecond):
	}

	differentResource := make(chan func(), 1)
	go func() {
		release, _ := state.BeginIndexedToolCall("speech", []string{"asset_b"}, false, false)
		differentResource <- release
	}()
	select {
	case release := <-differentResource:
		release()
	case <-time.After(time.Second):
		releaseWrite()
		t.Fatal("different-resource reader was unnecessarily blocked")
	}

	releaseWrite()
	select {
	case release := <-sameResource:
		release()
	case <-time.After(time.Second):
		t.Fatal("same-resource reader remained blocked after release")
	}
}

func insertPreviewTraceMessage(
	t *testing.T,
	database *storage.DB,
	draftID, messageID, kind, content string,
) {
	t.Helper()
	result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: contracts.ActorAgent,
		ResultRows: reducer.ResultRows{Message: &reducer.MessageRow{
			ID: messageID, DraftID: draftID, Role: "system", Kind: kind, Content: content,
		}},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("insert trace status=%s err=%v", result.Status, err)
	}
}

func previewTraceJSON(t *testing.T, record map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
