package agentexec

import (
	"context"
	"strings"
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
	"github.com/nanzhi84/Rushes/go/internal/understanding"
)

func TestFallbackMainlineStopsOnStructuredAtomicFailure(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_fallback_atomic_failure"
	agenttest.CreateAgentDraft(t, database, draftID)
	seedFallbackAsset(t, database, draftID, "asset_fallback_failure")
	executor := New(database, understanding.NewAnalyzer(nil), nil, nil)

	renderCalled := false
	reply, err := executor.FallbackMainline(
		rushestools.WithDraftID(t.Context(), draftID),
		draftID,
		func(_ context.Context, name string, _ any) (any, error) {
			switch name {
			case "media.detect_shots":
				return rushestools.DetectShotsResult{Status: "succeeded"}, nil
			case "timeline.insert":
				return rushestools.ToolResult{
					Status: string(rushestools.StatusValidationFailed), Observation: "素材目标已失效",
				}, nil
			case "preview.generate":
				renderCalled = true
			}
			return nil, nil
		},
	)
	if err == nil || !strings.Contains(err.Error(), "timeline.insert 未完成") || reply != "" {
		t.Fatalf("reply=%q err=%v", reply, err)
	}
	if renderCalled {
		t.Fatal("原子插入失败后不得继续排队预览")
	}
}

func TestFallbackMainlineUsesLatestExistingTimelineAndRejectsRenderDrift(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_fallback_latest"
	agenttest.CreateAgentDraft(t, database, draftID)
	seedFallbackAsset(t, database, draftID, "asset_fallback_latest")
	executor := New(database, understanding.NewAnalyzer(nil), nil, nil)
	ctx := manualTimelineMutationContext(rushestools.WithDraftID(t.Context(), draftID))
	if _, err := executor.ExecuteTool(ctx, "timeline.insert", rushestools.TimelineInsertInput{
		"kind": "insert_clip", "asset_id": "asset_fallback_latest", "role": "b_roll",
		"source_start_frame": 0, "source_end_frame": 15,
	}); err != nil {
		t.Fatal(err)
	}

	var renderTimelineID string
	reply, err := executor.FallbackMainline(
		ctx,
		draftID,
		func(callCtx context.Context, name string, input any) (any, error) {
			switch name {
			case "media.detect_shots":
				return rushestools.DetectShotsResult{Status: "succeeded"}, nil
			case "timeline.insert":
				return executor.ExecuteTool(callCtx, name, input)
			case "timeline.check":
				return executor.ExecuteTool(callCtx, name, input)
			case "preview.generate":
				renderTimelineID = input.(rushestools.PreviewGenerateInput).TimelineID
				return rushestools.ToolResult{
					Status: string(rushestools.StatusFailed), Observation: "timeline_id 已过期",
				}, nil
			default:
				t.Fatalf("unexpected tool %s", name)
				return nil, nil
			}
		},
	)
	if err == nil || !strings.Contains(err.Error(), "preview.generate 未完成") || reply != "" {
		t.Fatalf("reply=%q err=%v", reply, err)
	}
	document, latestErr := timeline.Latest(t.Context(), database, draftID)
	if latestErr != nil {
		t.Fatal(latestErr)
	}
	if document.Version != 2 || renderTimelineID != document.TimelineID {
		t.Fatalf(
			"version=%d render timeline=%q latest=%q",
			document.Version, renderTimelineID, document.TimelineID,
		)
	}
}

func seedFallbackAsset(
	t *testing.T,
	database *storage.DB,
	draftID string,
	assetID string,
) {
	t.Helper()
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{
		{Type: "AssetImported", Payload: map[string]any{
			"asset_id": assetID, "job_id": "job_" + assetID, "storage_mode": "reference",
			"reference_path": "/tmp/" + assetID + ".mp4", "kind": "video", "source": "local_path",
			"filename": assetID + ".mp4", "hash": assetID, "size": 1,
			"probe": map[string]any{"duration_sec": 1}, "ingest_status": "ready",
		}},
		{Type: "AssetLinked", DraftID: draftID, Payload: map[string]any{"asset_id": assetID}},
	}, reducer.Options{Actor: contracts.ActorUser})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("assets status=%s err=%v", result.Status, err)
	}
}
