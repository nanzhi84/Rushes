package agent

import (
	"testing"

	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func TestTimelineInspectReportsMissingTimelineWithoutFailure(t *testing.T) {
	t.Parallel()
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_inspect_empty")
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	resultRaw, err := service.ExecuteTool(
		rushestools.WithDraftID(t.Context(), "draft_inspect_empty"),
		"timeline.inspect",
		rushestools.TimelineInspectInput{},
	)
	if err != nil {
		t.Fatal(err)
	}
	result := resultRaw.(rushestools.ToolResult)
	if result.Status != "succeeded" || result.Data["timeline_exists"] != false ||
		result.Data["duration_frames"] != 0 {
		t.Fatalf("result=%#v", result)
	}
}
