package agentexec

import (
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func TestRenderRejectsInvalidCurrentTimelineWithoutCreatingJob(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_invalid_render"
	agenttest.CreateAgentDraft(t, database, draftID)
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	document, err := timeline.ComposeInitial(draftID, 1, []timeline.Selection{{
		AssetID: "fixture", AssetKind: "video", SourceStartFrame: 0, SourceEndFrame: 30, Role: "a_roll",
	}})
	if err != nil {
		t.Fatal(err)
	}
	document.DurationFrames = 0
	persisted, err := exec.PersistTimeline(t.Context(), draftID, document, "invalid_render_fixture")
	if err != nil || persisted.Status != string(rushestools.StatusValidationFailed) {
		t.Fatalf("persisted=%#v err=%v", persisted, err)
	}
	var failuresBefore int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM event_log
		WHERE draft_id=? AND event_type='TimelineValidationFailed'`, draftID,
	).Scan(&failuresBefore); err != nil {
		t.Fatal(err)
	}

	raw, err := exec.ExecuteTool(
		rushestools.WithDraftID(t.Context(), draftID),
		"render.preview",
		rushestools.RenderPreviewInput{},
	)
	if err != nil {
		t.Fatal(err)
	}
	result := raw.(rushestools.ToolResult)
	if result.Status != string(rushestools.StatusValidationFailed) ||
		result.Data["reason"] != "validation_failed" ||
		result.Data["current_timeline_unchanged"] != true ||
		result.Data["validation_report"] == nil ||
		result.Data["recovery"] == nil {
		t.Fatalf("render result=%#v", result)
	}
	var jobs, failuresAfter int
	if err := database.Read().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM jobs WHERE draft_id=?", draftID,
	).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM event_log
		WHERE draft_id=? AND event_type='TimelineValidationFailed'`, draftID,
	).Scan(&failuresAfter); err != nil {
		t.Fatal(err)
	}
	if jobs != 0 || failuresAfter != failuresBefore+1 {
		t.Fatalf("jobs=%d validation failures=%d->%d", jobs, failuresBefore, failuresAfter)
	}
	latest, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil || latest.Version != 1 {
		t.Fatalf("latest=%#v err=%v", latest, err)
	}
}
