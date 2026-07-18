package agent

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func TestPlanUpdatePersistsAndAppearsInNextWorldStatePatch(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_plan_context"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	manager := NewContextManager(database)
	first, err := manager.Build(t.Context(), draftID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := storage.GetDraft(t.Context(), database.Read(), draftID)
	if err != nil {
		t.Fatal(err)
	}
	var eventsBefore int
	if err := database.Read().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM event_log").Scan(&eventsBefore); err != nil {
		t.Fatal(err)
	}

	result := executePlanUpdate(t, service, draftID, rushestools.PlanUpdateInput{Plan: map[string]any{
		"intent": "强调产品质感",
		"decisions": map[string]any{
			"pace": "fast", "keep_original_audio": true,
		},
	}})
	if result.Status != "succeeded" || result.Data["mode"] != "merge" {
		t.Fatalf("result=%#v", result)
	}
	stored, err := storage.GetDraft(t.Context(), database.Read(), draftID)
	if err != nil {
		t.Fatal(err)
	}
	decisions, _ := stored.ContentPlan["decisions"].(map[string]any)
	if stored.ContentPlan["intent"] != "强调产品质感" ||
		decisions["pace"] != "fast" || decisions["keep_original_audio"] != true {
		t.Fatalf("stored content plan=%#v", stored.ContentPlan)
	}
	if stored.StateVersion != before.StateVersion || stored.UpdatedAt != before.UpdatedAt {
		t.Fatalf("plan update changed domain version: before=%#v after=%#v", before, stored)
	}
	var eventsAfter int
	if err := database.Read().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM event_log").Scan(&eventsAfter); err != nil {
		t.Fatal(err)
	}
	if eventsAfter != eventsBefore {
		t.Fatalf("event_log count=%d want=%d", eventsAfter, eventsBefore)
	}

	second, err := manager.Build(t.Context(), draftID)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Manifest.HasWorldStatePatch || second.Manifest.WindowID != first.Manifest.WindowID ||
		second.Manifest.WindowNumber != first.Manifest.WindowNumber {
		t.Fatalf("first=%#v second=%#v", first.Manifest, second.Manifest)
	}
	draftSection, _ := second.Snapshot.Sections["draft"].(map[string]any)
	contentPlan, _ := draftSection["content_plan"].(map[string]any)
	if contentPlan["intent"] != "强调产品质感" {
		t.Fatalf("snapshot draft=%#v", draftSection)
	}
	foundUpdate := false
	for _, message := range second.Messages {
		if message.Role == schema.System && message.Extra["context_phase"] == "world_state_update" {
			foundUpdate = strings.Contains(message.Content, `"content_plan"`)
		}
	}
	if !foundUpdate {
		t.Fatalf("WorldState update 未携带 content_plan: %#v", second.Messages)
	}

	base, err := WorldStateSnapshotFromMap(second.Checkpoint.BaseSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	patch, err := base.MergePatchTo(second.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	sectionsPatch, _ := patch["sections"].(map[string]any)
	draftPatch, _ := sectionsPatch["draft"].(map[string]any)
	if _, exists := draftPatch["content_plan"]; !exists {
		t.Fatalf("patch=%#v", patch)
	}
	baseMap, _ := base.Map()
	currentMap, _ := second.Snapshot.Map()
	if reconstructed := applyMergePatch(baseMap, patch); !reflect.DeepEqual(reconstructed, currentMap) {
		t.Fatalf("content_plan patch 无法重建当前状态\npatch=%#v\nwant=%#v\ngot=%#v", patch, currentMap, reconstructed)
	}
}

func TestPlanUpdateRFC7396MergeDeleteAndReset(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_plan_merge"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ctx := rushestools.WithDraftID(t.Context(), draftID)

	initial := map[string]any{
		"a": 1, "b": 2,
		"nested": map[string]any{"keep": true, "replace": "old"},
		"shots":  []any{"one", "two"},
	}
	if raw, err := service.ExecuteTool(ctx, "plan.update", rushestools.PlanUpdateInput{Plan: initial}); err != nil || raw.(rushestools.ToolResult).Status != "succeeded" {
		t.Fatalf("initial=%#v err=%v", raw, err)
	}
	patch := map[string]any{
		"b": 3, "c": 4,
		"nested": map[string]any{"replace": "new", "added": true},
		"shots":  []any{"three"},
	}
	if raw, err := service.ExecuteTool(ctx, "plan.update", rushestools.PlanUpdateInput{Plan: patch}); err != nil || raw.(rushestools.ToolResult).Status != "succeeded" {
		t.Fatalf("merge=%#v err=%v", raw, err)
	}
	merged, err := storage.GetDraft(t.Context(), database.Read(), draftID)
	if err != nil {
		t.Fatal(err)
	}
	nested := merged.ContentPlan["nested"].(map[string]any)
	shots := merged.ContentPlan["shots"].([]any)
	if merged.ContentPlan["a"] != float64(1) || merged.ContentPlan["b"] != float64(3) ||
		merged.ContentPlan["c"] != float64(4) || nested["keep"] != true ||
		nested["replace"] != "new" || nested["added"] != true ||
		len(shots) != 1 || shots[0] != "three" {
		t.Fatalf("merged=%#v", merged.ContentPlan)
	}

	if raw, err := service.ExecuteTool(ctx, "plan.update", rushestools.PlanUpdateInput{Plan: map[string]any{
		"b": nil, "nested": map[string]any{"replace": nil},
	}}); err != nil || raw.(rushestools.ToolResult).Status != "succeeded" {
		t.Fatalf("delete=%#v err=%v", raw, err)
	}
	deleted, err := storage.GetDraft(t.Context(), database.Read(), draftID)
	if err != nil {
		t.Fatal(err)
	}
	nested = deleted.ContentPlan["nested"].(map[string]any)
	if _, exists := deleted.ContentPlan["b"]; exists {
		t.Fatalf("RFC7396 null 未删除 b: %#v", deleted.ContentPlan)
	}
	if _, exists := nested["replace"]; exists || nested["keep"] != true {
		t.Fatalf("RFC7396 nested delete=%#v", nested)
	}

	manager := NewContextManager(database)
	beforeReset, err := manager.Build(t.Context(), draftID)
	if err != nil {
		t.Fatal(err)
	}
	reset := true
	result := executePlanUpdate(t, service, draftID, rushestools.PlanUpdateInput{
		Plan: map[string]any{
			"only": "replacement", "drop": nil,
			"nested": map[string]any{"drop": nil},
			"array":  []any{nil},
		},
		Reset: &reset,
	})
	if result.Status != "succeeded" || result.Data["mode"] != "reset" {
		t.Fatalf("reset result=%#v", result)
	}
	replaced, err := storage.GetDraft(t.Context(), database.Read(), draftID)
	replacedNested, _ := replaced.ContentPlan["nested"].(map[string]any)
	replacedArray, _ := replaced.ContentPlan["array"].([]any)
	if err != nil || len(replaced.ContentPlan) != 3 || replaced.ContentPlan["only"] != "replacement" ||
		len(replacedNested) != 0 || len(replacedArray) != 1 || replacedArray[0] != nil {
		t.Fatalf("reset plan=%#v err=%v", replaced.ContentPlan, err)
	}
	afterReset, err := manager.Build(t.Context(), draftID)
	if err != nil || !afterReset.Manifest.HasWorldStatePatch ||
		afterReset.Manifest.WindowID != beforeReset.Manifest.WindowID {
		t.Fatalf("reset context before=%#v after=%#v err=%v", beforeReset.Manifest, afterReset.Manifest, err)
	}
	base, err := WorldStateSnapshotFromMap(afterReset.Checkpoint.BaseSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	resetPatch, err := base.MergePatchTo(afterReset.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	baseMap, _ := base.Map()
	currentMap, _ := afterReset.Snapshot.Map()
	if reconstructed := applyMergePatch(baseMap, resetPatch); !reflect.DeepEqual(reconstructed, currentMap) {
		t.Fatalf("reset plan patch 无法重建当前状态\npatch=%#v\nwant=%#v\ngot=%#v", resetPatch, currentMap, reconstructed)
	}
	result = executePlanUpdate(t, service, draftID, rushestools.PlanUpdateInput{
		Plan: map[string]any{}, Reset: &reset,
	})
	cleared, err := storage.GetDraft(t.Context(), database.Read(), draftID)
	if result.Status != "succeeded" || err != nil || len(cleared.ContentPlan) != 0 {
		t.Fatalf("empty reset result=%#v plan=%#v err=%v", result, cleared.ContentPlan, err)
	}
	if initial["b"] != 2 || patch["b"] != 3 {
		t.Fatalf("plan.update mutated inputs initial=%#v patch=%#v", initial, patch)
	}
}

func TestPlanUpdateRetriesConcurrentMergesWithoutLostUpdates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		patches [2]map[string]any
		assert  func(*testing.T, map[string]any)
	}{
		{
			name: "different top-level keys",
			patches: [2]map[string]any{
				{"alpha": "one"},
				{"beta": "two"},
			},
			assert: func(t *testing.T, plan map[string]any) {
				t.Helper()
				if plan["alpha"] != "one" || plan["beta"] != "two" {
					t.Fatalf("concurrent different-key plan=%#v", plan)
				}
			},
		},
		{
			name: "same object key",
			patches: [2]map[string]any{
				{"story": map[string]any{"pace": "fast"}},
				{"story": map[string]any{"tone": "warm"}},
			},
			assert: func(t *testing.T, plan map[string]any) {
				t.Helper()
				story, _ := plan["story"].(map[string]any)
				if story["pace"] != "fast" || story["tone"] != "warm" {
					t.Fatalf("concurrent same-key plan=%#v", plan)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			database := agenttest.AgentTestDatabase(t)
			const draftID = "draft_plan_concurrent"
			agenttest.CreateAgentDraft(t, database, draftID)
			service, err := NewService(t.Context(), database, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(service.Close)

			ready := make(chan struct{}, 2)
			release := make(chan struct{})
			type callResult struct {
				result rushestools.ToolResult
				err    error
			}
			results := make(chan callResult, len(test.patches))
			ctx := t.Context()
			for _, patch := range test.patches {
				patch := patch
				go func() {
					result, err := service.executor.ToolPlanUpdateWithBeforeApply(
						ctx, draftID, rushestools.PlanUpdateInput{Plan: patch},
						func(attempt int) error {
							if attempt == 1 {
								ready <- struct{}{}
								<-release
							}
							return nil
						},
					)
					results <- callResult{result: result, err: err}
				}()
			}
			for index := 0; index < len(test.patches); index++ {
				select {
				case <-ready:
				case <-time.After(5 * time.Second):
					close(release)
					t.Fatal("concurrent plan.update did not reach first-attempt barrier")
				}
			}
			close(release)
			for index := 0; index < len(test.patches); index++ {
				select {
				case call := <-results:
					if call.err != nil || call.result.Status != "succeeded" {
						t.Fatalf("concurrent result=%#v err=%v", call.result, call.err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("concurrent plan.update did not finish")
				}
			}
			stored, err := storage.GetDraft(t.Context(), database.Read(), draftID)
			if err != nil {
				t.Fatal(err)
			}
			test.assert(t, stored.ContentPlan)
		})
	}
}

func TestPlanUpdateReturnsStructuredFailureAfterThreeConflicts(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_plan_conflicts"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)

	attempts := 0
	result, err := service.executor.ToolPlanUpdateWithBeforeApply(
		t.Context(), draftID,
		rushestools.PlanUpdateInput{Plan: map[string]any{"tool": "must-not-stick"}},
		func(attempt int) error {
			attempts++
			external, err := json.Marshal(map[string]any{"external_attempt": attempt})
			if err != nil {
				return err
			}
			_, err = database.Write().ExecContext(t.Context(),
				"UPDATE drafts SET content_plan_json=? WHERE draft_id=?", string(external), draftID,
			)
			return err
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if attempts != agentexec.PlanUpdateMaxAttempts || result.Status != "failed" ||
		result.Data["reason"] != "plan_conflict" || result.Data["error_code"] != "plan_conflict" ||
		result.Data["current_plan_unchanged"] != false ||
		!strings.Contains(result.Data["recovery"].(string), "WorldState") {
		t.Fatalf("attempts=%d result=%#v", attempts, result)
	}
	stored, err := storage.GetDraft(t.Context(), database.Read(), draftID)
	if err != nil || stored.ContentPlan["external_attempt"] != float64(agentexec.PlanUpdateMaxAttempts) {
		t.Fatalf("stored plan=%#v err=%v", stored.ContentPlan, err)
	}
	if _, exists := stored.ContentPlan["tool"]; exists {
		t.Fatalf("conflicted tool write leaked into plan: %#v", stored.ContentPlan)
	}
}

func TestPlanUpdateRejectsInvalidPlansWithoutChangingStoredContent(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_plan_guards"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	executePlanUpdate(t, service, draftID, rushestools.PlanUpdateInput{
		Plan: map[string]any{"stable": "keep"},
	})

	invalid := []struct {
		name       string
		plan       map[string]any
		wantReason string
	}{
		{name: "missing", plan: nil, wantReason: "plan_required"},
		{name: "non json", plan: map[string]any{"bad": make(chan int)}, wantReason: "plan_not_json"},
		{name: "timeline version", plan: map[string]any{"timeline_version": 1}, wantReason: "reserved_key"},
		{name: "timeline revision in contract", plan: map[string]any{
			"contract": map[string]any{"custom": map[string]any{"timeline_revision": 1}},
		}, wantReason: "reserved_key"},
		{name: "version in contract array", plan: map[string]any{
			"contract": map[string]any{"items": []any{map[string]any{"version": 1}}},
		}, wantReason: "reserved_key"},
		{name: "timeline id deep in contract", plan: map[string]any{
			"contract": map[string]any{"custom": map[string]any{"item": map[string]any{"timeline_id": "bad"}}},
		}, wantReason: "reserved_key"},
		{name: "draft id", plan: map[string]any{"draft_id": draftID}, wantReason: "reserved_key"},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			result := executePlanUpdate(t, service, draftID, rushestools.PlanUpdateInput{Plan: test.plan})
			if result.Status != "failed" || result.Data["reason"] != test.wantReason ||
				result.Data["error_code"] != test.wantReason {
				t.Fatalf("result=%#v", result)
			}
			if test.wantReason == "reserved_key" && !strings.Contains(result.Observation, "保留键") {
				t.Fatalf("保留键失败缺少说明: %#v", result)
			}
			stored, err := storage.GetDraft(t.Context(), database.Read(), draftID)
			if err != nil || len(stored.ContentPlan) != 1 || stored.ContentPlan["stable"] != "keep" {
				t.Fatalf("guard changed plan=%#v err=%v", stored.ContentPlan, err)
			}
		})
	}
	allowed := map[string]any{
		"section": map[string]any{"timeline_revision": 1},
		"items":   []any{map[string]any{"version": 1}},
		"details": map[string]any{"item": map[string]any{"timeline_id": "business-label"}},
	}
	result := executePlanUpdate(t, service, draftID, rushestools.PlanUpdateInput{Plan: allowed})
	stored, err := storage.GetDraft(t.Context(), database.Read(), draftID)
	if result.Status != "succeeded" || err != nil || stored.ContentPlan["section"] == nil ||
		stored.ContentPlan["items"] == nil || stored.ContentPlan["details"] == nil {
		t.Fatalf("business nested reserved names should be allowed: result=%#v plan=%#v err=%v", result, stored.ContentPlan, err)
	}
	snapshot, err := NewContextBuilder(database).Snapshot(t.Context(), draftID)
	if err != nil {
		t.Fatal(err)
	}
	worldPlan := snapshot.Sections["draft"].(map[string]any)["content_plan"].(map[string]any)
	section := worldPlan["section"].(map[string]any)
	items := worldPlan["items"].([]any)
	details := worldPlan["details"].(map[string]any)["item"].(map[string]any)
	if section["timeline_revision"] != float64(1) ||
		items[0].(map[string]any)["version"] != float64(1) || details["timeline_id"] != "business-label" {
		t.Fatalf("allowed business keys were stripped from WorldState: %#v", worldPlan)
	}
	if testRetrySafe(t)("plan.update") {
		t.Fatal("plan.update 是写工具，不得自动重放")
	}
}

func TestPlanUpdateRequiresResetToRepairStoredReservedKeys(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_plan_legacy_reserved"
	agenttest.CreateAgentDraft(t, database, draftID)
	if _, err := database.Write().ExecContext(t.Context(),
		`UPDATE drafts SET content_plan_json='{"version":1,"legacy":true}' WHERE draft_id=?`,
		draftID,
	); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	result := executePlanUpdate(t, service, draftID, rushestools.PlanUpdateInput{
		Plan: map[string]any{"new": true},
	})
	if result.Status != "failed" || result.Data["reason"] != "stored_reserved_key" ||
		result.Data["reserved_key"] != "version" ||
		!strings.Contains(result.Observation, "reset=true") {
		t.Fatalf("stored reserved result=%#v", result)
	}
	reset := true
	result = executePlanUpdate(t, service, draftID, rushestools.PlanUpdateInput{
		Plan: map[string]any{"clean": true}, Reset: &reset,
	})
	stored, err := storage.GetDraft(t.Context(), database.Read(), draftID)
	if result.Status != "succeeded" || err != nil || len(stored.ContentPlan) != 1 ||
		stored.ContentPlan["clean"] != true {
		t.Fatalf("reset repair result=%#v plan=%#v err=%v", result, stored.ContentPlan, err)
	}
}

func TestPlanUpdateAllowsExactlyEightThousandRunesAndRejectsMore(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_plan_limit"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	reset := true
	emptyEncoded, err := json.Marshal(map[string]any{"notes": ""})
	if err != nil {
		t.Fatal(err)
	}
	overhead := utf8.RuneCount(emptyEncoded)
	exact := map[string]any{"notes": strings.Repeat("界", agentexec.ContentPlanRuneLimit-overhead)}
	result := executePlanUpdate(t, service, draftID, rushestools.PlanUpdateInput{Plan: exact, Reset: &reset})
	if result.Status != "succeeded" || result.Data["plan_runes"] != agentexec.ContentPlanRuneLimit {
		t.Fatalf("exact limit result=%#v overhead=%d", result, overhead)
	}
	before, err := storage.GetDraft(t.Context(), database.Read(), draftID)
	if err != nil {
		t.Fatal(err)
	}
	over := map[string]any{"notes": strings.Repeat("界", agentexec.ContentPlanRuneLimit-overhead+1)}
	result = executePlanUpdate(t, service, draftID, rushestools.PlanUpdateInput{Plan: over, Reset: &reset})
	if result.Status != "failed" || result.Data["reason"] != "plan_too_large" ||
		result.Data["current_plan_unchanged"] != true ||
		!strings.Contains(result.Observation, "超出 8000 字上限") ||
		!strings.Contains(result.Observation, "只记纲要") {
		t.Fatalf("over limit result=%#v", result)
	}
	after, err := storage.GetDraft(t.Context(), database.Read(), draftID)
	if err != nil || !reflect.DeepEqual(after.ContentPlan, before.ContentPlan) {
		t.Fatalf("oversize changed plan before=%#v after=%#v err=%v", before.ContentPlan, after.ContentPlan, err)
	}
}

func TestMergeContentPlanHandlesScalarToObjectWithoutMutatingInputs(t *testing.T) {
	t.Parallel()
	target := map[string]any{
		"section": "old", "nested": map[string]any{"keep": true},
	}
	patch := map[string]any{
		"section": map[string]any{"new": true},
		"nested":  map[string]any{"remove": nil},
	}
	targetBefore, _ := agentexec.CanonicalContentPlan(target)
	patchBefore, _ := agentexec.CanonicalContentPlan(patch)
	merged := agentexec.MergeContentPlan(target, patch)
	section := merged["section"].(map[string]any)
	if section["new"] != true || merged["nested"].(map[string]any)["keep"] != true {
		t.Fatalf("merged=%#v", merged)
	}
	if !reflect.DeepEqual(target, targetBefore) || !reflect.DeepEqual(patch, patchBefore) {
		t.Fatalf("merge mutated inputs target=%#v patch=%#v", target, patch)
	}
}

func executePlanUpdate(
	t *testing.T,
	service *Service,
	draftID string,
	input rushestools.PlanUpdateInput,
) rushestools.ToolResult {
	t.Helper()
	raw, err := service.ExecuteTool(
		rushestools.WithDraftID(t.Context(), draftID), "plan.update", input,
	)
	if err != nil {
		t.Fatal(err)
	}
	result, ok := raw.(rushestools.ToolResult)
	if !ok {
		t.Fatalf("plan.update output type=%T", raw)
	}
	return result
}
