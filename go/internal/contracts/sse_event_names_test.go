package contracts

import (
	"encoding/json"
	"os"
	"sort"
	"testing"
)

type sseEventNamesGolden struct {
	Domain    []string `json:"domain_event_types"`
	Workspace []string `json:"workspace_event_types"`
	Draft     []string `json:"draft_event_types"`
	Turn      []string `json:"turn_stream_types"`
}

func TestDomainSSEEventNamesMatchRegistryRoutes(t *testing.T) {
	t.Parallel()

	payload, err := os.ReadFile("testdata/sse_event_names.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden sseEventNamesGolden
	if err := json.Unmarshal(payload, &golden); err != nil {
		t.Fatal(err)
	}
	all := make([]string, 0, len(EventRegistry))
	workspace := make([]string, 0, len(EventRegistry))
	draft := make([]string, 0, len(EventRegistry))
	for eventType, spec := range EventRegistry {
		all = append(all, eventType)
		event := Event{
			Type: eventType, DraftID: "draft",
			Payload: map[string]any{"requested_by_draft_id": "requested"},
		}
		if RoutesToWorkspace(event) {
			workspace = append(workspace, eventType)
		}
		if RoutesToDraft(event, "draft") && RoutesToDraft(event, "requested") {
			draft = append(draft, eventType)
		}
		if spec.Routes == 0 {
			t.Errorf("%s 缺少路由声明", eventType)
		}
	}
	assertSameStrings(t, "domain", all, golden.Domain)
	assertSameStrings(t, "workspace", workspace, golden.Workspace)
	assertSameStrings(t, "draft", draft, golden.Draft)
	if len(golden.Turn) == 0 {
		t.Fatal("turn stream 事件清单不能为空")
	}
}

func assertSameStrings(t *testing.T, label string, got, want []string) {
	t.Helper()
	got = append([]string(nil), got...)
	want = append([]string(nil), want...)
	sort.Strings(got)
	sort.Strings(want)
	if stringListJSON(got) != stringListJSON(want) {
		t.Fatalf("%s names=%v want=%v", label, got, want)
	}
}

func stringListJSON(values []string) string {
	payload, _ := json.Marshal(values)
	return string(payload)
}
