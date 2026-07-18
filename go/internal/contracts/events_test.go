package contracts

import (
	"encoding/json"
	"testing"
)

func TestEventRegistryCoreSetAndVersionModes(t *testing.T) {
	t.Parallel()

	if got := len(EventRegistry); got != 28 {
		t.Fatalf("事件数=%d，核心与前端生命周期事件应为 28", got)
	}
	strict := map[string]bool{
		"DecisionCreated": true, "DecisionAnswered": true,
		"ConversationContextCleared": true,
		"TimelineVersionCreated":     true, "TimelineVersionRestored": true,
		"TimelineValidated":        true,
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
	mode, _ := workspace.VersionMode()
	if mode != VersionMerge {
		t.Fatalf("workspace decision mode=%s", mode)
	}
	declared, _ := workspace.Spec()
	if declared.Mode != VersionStrict || declared.WorkspaceScopeMode != VersionMerge {
		t.Fatalf("workspace decision declaration=%#v", declared)
	}
}

func TestEventRegistryDeclaresEveryRoute(t *testing.T) {
	t.Parallel()

	for eventType, spec := range EventRegistry {
		if spec.Routes == 0 {
			t.Errorf("%s 缺少 SSE 路由声明", eventType)
		}
		event := Event{
			Type: eventType, Actor: ActorAgent, DraftID: "draft",
			Payload: map[string]any{"requested_by_draft_id": "requested"},
		}
		workspace := spec.Routes.Includes(RouteWorkspace)
		if got := RoutesToWorkspace(event); got != workspace {
			t.Errorf("%s workspace route=%t want=%t", eventType, got, workspace)
		}
		draft := spec.Routes.Includes(RouteDraft)
		if got := RoutesToDraft(event, "draft"); got != draft {
			t.Errorf("%s direct draft route=%t want=%t", eventType, got, draft)
		}
		if got := RoutesToDraft(event, "requested"); got != draft {
			t.Errorf("%s requested draft route=%t want=%t", eventType, got, draft)
		}
	}
	unknown := Event{Type: "FutureEvent", DraftID: "draft", Payload: map[string]any{"requested_by_draft_id": "requested"}}
	if RoutesToWorkspace(unknown) || RoutesToDraft(unknown, "draft") || RoutesToDraft(unknown, "requested") {
		t.Fatal("未注册事件不应进入任何 SSE 路由")
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

func TestTimelineVersionRestoredValidation(t *testing.T) {
	t.Parallel()
	valid := []Event{
		{
			Type: "TimelineVersionRestored", Actor: ActorUser, DraftID: "draft",
			Payload: map[string]any{
				"checkpoint_id": "checkpoint", "restore_checkpoint_id": "restore",
				"mode": "timeline", "timeline_version": 2,
			},
		},
		{
			Type: "TimelineVersionRestored", Actor: ActorUser, DraftID: "draft",
			Payload: map[string]any{
				"checkpoint_id": "checkpoint", "restore_checkpoint_id": "restore",
				"mode": "conversation",
			},
		},
	}
	for _, event := range valid {
		if err := event.Validate(); err != nil {
			t.Fatalf("valid restore rejected: %v", err)
		}
	}
	for _, payload := range []map[string]any{
		{"mode": "timeline", "timeline_version": 2, "restore_checkpoint_id": "restore"},
		{"checkpoint_id": "checkpoint", "mode": "invalid", "restore_checkpoint_id": "restore"},
		{"checkpoint_id": "checkpoint", "mode": "timeline", "restore_checkpoint_id": "restore"},
	} {
		event := Event{
			Type: "TimelineVersionRestored", Actor: ActorUser, DraftID: "draft", Payload: payload,
		}
		if err := event.Validate(); err == nil {
			t.Fatalf("invalid restore unexpectedly accepted: %#v", payload)
		}
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

func TestMaterialUnderstandingMergeKeysKeepLegacyEventsAndIsolateJobs(t *testing.T) {
	t.Parallel()

	for _, eventType := range []string{
		"MaterialUnderstandingStarted",
		"MaterialUnderstandingCompleted",
		"MaterialUnderstandingFailed",
	} {
		legacyJSON := []byte(`{"event":"` + eventType + `","actor":"job","payload":{"asset_id":"asset-1"}}`)
		legacy, err := ParseEvent(legacyJSON)
		if err != nil {
			t.Fatalf("旧事件 %s 不应因缺少 job_id 而无法解析: %v", eventType, err)
		}
		key, err := legacy.MergeKey()
		if err != nil || key != "asset_id=asset-1" {
			t.Fatalf("旧事件 %s key=%q err=%v", eventType, key, err)
		}

		first := legacy
		first.Payload = map[string]any{"asset_id": "asset-1", "job_id": "job-1"}
		second := legacy
		second.Payload = map[string]any{"asset_id": "asset-1", "job_id": "job-2"}
		firstKey, firstErr := first.MergeKey()
		secondKey, secondErr := second.MergeKey()
		if firstErr != nil || secondErr != nil || firstKey == secondKey {
			t.Fatalf("%s 的不同 job 必须使用不同 merge key: first=%q second=%q err=%v/%v",
				eventType, firstKey, secondKey, firstErr, secondErr)
		}
		if firstKey != "asset_id=asset-1\x1fjob_id=job-1" ||
			secondKey != "asset_id=asset-1\x1fjob_id=job-2" {
			t.Fatalf("%s 新 merge key 不稳定: first=%q second=%q", eventType, firstKey, secondKey)
		}

		firstAttempt := legacy
		firstAttempt.Payload = map[string]any{"asset_id": "asset-1", "job_id": "job-retry", "attempt": 0}
		secondAttempt := legacy
		secondAttempt.Payload = map[string]any{"asset_id": "asset-1", "job_id": "job-retry", "attempt": 1}
		firstAttemptKey, firstAttemptErr := firstAttempt.MergeKey()
		secondAttemptKey, secondAttemptErr := secondAttempt.MergeKey()
		if firstAttemptErr != nil || secondAttemptErr != nil || firstAttemptKey == secondAttemptKey {
			t.Fatalf("%s 的不同 attempt 必须使用不同 merge key: first=%q second=%q err=%v/%v",
				eventType, firstAttemptKey, secondAttemptKey, firstAttemptErr, secondAttemptErr)
		}
		if firstAttemptKey != "asset_id=asset-1\x1fjob_id=job-retry\x1fattempt=0" ||
			secondAttemptKey != "asset_id=asset-1\x1fjob_id=job-retry\x1fattempt=1" {
			t.Fatalf("%s attempt merge key 不稳定: first=%q second=%q",
				eventType, firstAttemptKey, secondAttemptKey)
		}
	}
}
