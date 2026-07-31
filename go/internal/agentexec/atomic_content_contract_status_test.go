package agentexec

import (
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func TestAtomicEditReportsCommittedWhenOnlyContentContractIsStillOpen(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_atomic_contract_gap"
	agenttest.CreateAgentDraft(t, database, draftID)
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := exec.toolPlanUpdate(t.Context(), draftID, rushestools.PlanUpdateInput{
		Plan: map[string]any{"goal": "补足到 120 帧"},
		Contract: &rushestools.ContentPlanContract{
			TargetDurationFrames: 120,
		},
	})
	if err != nil || updated.Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("plan=%#v err=%v", updated, err)
	}
	document, err := agenttest.ComposeTimeline(draftID, 1, []agenttest.TimelineSelection{{
		AssetID: "visual", AssetKind: "video", SourceEndFrame: 60,
	}})
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := seedTimelineVersion(exec, t.Context(), draftID, document, "contract_gap_fixture", nil)
	if err != nil || persisted.Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("fixture=%#v err=%v", persisted, err)
	}

	raw, err := exec.ExecuteTool(
		manualTimelineMutationContext(rushestools.WithDraftID(t.Context(), draftID)),
		"timeline.update",
		rushestools.TimelineUpdateInput{
			"kind": "set_track_state", "track_id": "bgm", "muted": true,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	result := raw.(rushestools.ToolResult)
	if result.Status != string(rushestools.StatusSucceeded) ||
		result.Data["previous_timeline_id"] != draftID+":v1" ||
		result.Data["timeline_id"] != draftID+":v2" ||
		result.Data["contract_failures"] == nil {
		t.Fatalf("atomic result=%#v", result)
	}
	validation := result.Data["validation_summary"].(map[string]any)
	if validation["structural_valid"] != true || validation["content_contract_valid"] != false {
		t.Fatalf("validation=%#v", validation)
	}
	latest, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil || latest.TimelineID != draftID+":v2" {
		t.Fatalf("latest=%#v err=%v", latest, err)
	}
	draft, err := storage.GetDraft(t.Context(), database.Read(), draftID)
	if err != nil || draft.TimelineValidated {
		t.Fatalf("合同未完成不得标记 validated: draft=%#v err=%v", draft, err)
	}
	checkedRaw, err := exec.ExecuteTool(
		rushestools.WithDraftID(t.Context(), draftID),
		"timeline.check",
		rushestools.TimelineCheckInput{TimelineID: latest.TimelineID},
	)
	if err != nil || checkedRaw.(rushestools.ToolResult).Status != string(rushestools.StatusValidationFailed) {
		t.Fatalf("check=%#v err=%v", checkedRaw, err)
	}
}
