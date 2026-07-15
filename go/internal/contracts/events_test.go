package contracts

import (
	"encoding/json"
	"testing"
)

func TestEventRegistryCoreSetAndVersionModes(t *testing.T) {
	t.Parallel()

	if got := len(EventRegistry); got != 26 {
		t.Fatalf("事件数=%d，核心与前端生命周期事件应为 26", got)
	}
	strict := map[string]bool{
		"DecisionCreated": true, "DecisionAnswered": true,
		"ConversationContextCleared": true,
		"TimelineVersionCreated":     true, "TimelineValidated": true,
		"TimelineValidationFailed": true,
	}
	for name, spec := range EventRegistry {
		want := VersionMerge
		if strict[name] {
			want = VersionStrict
		}
		if spec.Mode != want {
			t.Errorf("%s mode=%s want=%s", name, spec.Mode, want)
		}
	}
}

func TestEventMergeKeyAndDecisionWorkspaceMode(t *testing.T) {
	t.Parallel()

	event := Event{
		Type:    "AssetLinked",
		Actor:   ActorUser,
		DraftID: "draft-1",
		Payload: map[string]any{"asset_id": "asset-1"},
	}
	if err := event.Validate(); err != nil {
		t.Fatal(err)
	}
	key, err := event.MergeKey()
	if err != nil || key != "draft_id=draft-1\x1fasset_id=asset-1" {
		t.Fatalf("key=%q err=%v", key, err)
	}
	firstProxy := Event{
		Type: "ProxyGenerated", Actor: ActorJob,
		Payload: map[string]any{"asset_id": "asset-1", "proxy_object_hash": "hash-1"},
	}
	secondProxy := Event{
		Type: "ProxyGenerated", Actor: ActorJob,
		Payload: map[string]any{"asset_id": "asset-1", "proxy_object_hash": "hash-2"},
	}
	firstKey, firstErr := firstProxy.MergeKey()
	secondKey, secondErr := secondProxy.MergeKey()
	if firstErr != nil || secondErr != nil || firstKey == secondKey {
		t.Fatalf("重新生成的代理必须使用不同幂等键: first=%q second=%q err=%v/%v",
			firstKey, secondKey, firstErr, secondErr)
	}
	workspace := Event{
		Type:    "DecisionCreated",
		Actor:   ActorAgent,
		Payload: map[string]any{"scope_type": "workspace", "decision_id": "d"},
	}
	spec, _ := workspace.Spec()
	if spec.Mode != VersionMerge {
		t.Fatalf("workspace decision mode=%s", spec.Mode)
	}
}

func TestEventValidationSerializationAndRoutingBranches(t *testing.T) {
	t.Parallel()
	invalid := []Event{
		{Type: "Missing", Actor: ActorUser, Payload: map[string]any{}},
		{Type: "DraftCreated", Actor: Actor("robot"), DraftID: "d", Payload: map[string]any{}},
		{Type: "DraftCreated", Actor: ActorUser, DraftID: "d"},
		{Type: "DraftCreated", Actor: ActorUser, Payload: map[string]any{}},
		{Type: "AssetImported", Actor: ActorUser, Payload: map[string]any{"asset_id": "a"}},
	}
	for index, event := range invalid {
		if err := event.Validate(); err == nil {
			t.Fatalf("invalid[%d] unexpectedly valid", index)
		}
	}
	for _, actor := range []Actor{ActorUser, ActorAgent, ActorJob, ActorSystem} {
		if !actor.Valid() {
			t.Fatalf("actor %s invalid", actor)
		}
	}
	if _, err := (Event{Type: "Missing"}).MergeKey(); err == nil {
		t.Fatal("unknown event merge key should fail")
	}
	strict := Event{Type: "TimelineValidated", Actor: ActorAgent, DraftID: "d", Payload: map[string]any{}}
	if key, err := strict.MergeKey(); err != nil || key != "" {
		t.Fatalf("strict merge key=%q err=%v", key, err)
	}

	original := Event{
		Type: "JobProgress", Actor: ActorJob, DraftID: "draft_a",
		Payload: map[string]any{"job_id": "j", "progress": json.Number("0.5"), "requested_by_draft_id": "draft_b"},
	}
	encoded, err := original.JSON()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseEvent(encoded)
	if err != nil || parsed.Type != original.Type {
		t.Fatalf("parsed=%#v err=%v", parsed, err)
	}
	if _, err := ParseEvent([]byte("{")); err == nil {
		t.Fatal("invalid JSON should fail")
	}
	if !RoutesToWorkspace(original) || !RoutesToDraft(original, "draft_a") || !RoutesToDraft(original, "draft_b") {
		t.Fatalf("routing failed: %#v", original)
	}
	if RoutesToWorkspace(Event{Type: "TimelineValidated"}) || RoutesToDraft(original, "draft_c") {
		t.Fatal("unrelated event routed unexpectedly")
	}
}

func TestMergeKeyNumericRepresentations(t *testing.T) {
	t.Parallel()
	values := []any{float64(1.5), float32(2.5), 3, int64(4), json.Number("5.5")}
	for index, value := range values {
		event := Event{
			Type: "JobProgress", Actor: ActorJob,
			Payload: map[string]any{"job_id": "job", "progress": value},
		}
		if err := event.Validate(); err != nil {
			t.Fatalf("value[%d]=%T: %v", index, value, err)
		}
		if key, err := event.MergeKey(); err != nil || key == "" {
			t.Fatalf("value[%d] key=%q err=%v", index, key, err)
		}
	}
}
