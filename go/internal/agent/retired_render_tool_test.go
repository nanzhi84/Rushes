package agent

import (
	"strings"
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func TestExecuteToolRejectsRetiredRenderAndJobPollingNames(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_retired_render_tools"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ctx := rushestools.WithDraftID(t.Context(), draftID)

	for _, test := range []struct {
		name  string
		input any
		want  string
	}{
		{name: "render.start", input: map[string]any{"kind": "final"}, want: "不属于 Agent 能力"},
		{name: "job.read", input: map[string]any{"job_id": "job_old"}, want: "不属于 Agent 能力"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := service.ExecuteTool(ctx, test.name, test.input); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("retired tool error=%v", err)
			}
		})
	}
}
