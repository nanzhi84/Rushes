package agentexec

import (
	"testing"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/timeline"
)

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
