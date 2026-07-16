package agent

import (
	"encoding/json"
	"os"
	"sort"
	"testing"
)

func TestTurnStreamTypesMatchSharedGolden(t *testing.T) {
	t.Parallel()

	payload, err := os.ReadFile("../contracts/testdata/sse_event_names.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Turn []string `json:"turn_stream_types"`
	}
	if err := json.Unmarshal(payload, &golden); err != nil {
		t.Fatal(err)
	}
	got := KnownTurnStreamTypes()
	sort.Strings(got)
	sort.Strings(golden.Turn)
	if stringList(got) != stringList(golden.Turn) {
		t.Fatalf("turn stream types=%v want=%v", got, golden.Turn)
	}
}

func TestTurnStreamHubRejectsUnregisteredType(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Fatal("未注册 turn stream type 应 fail fast")
		}
	}()
	NewTurnStreamHub(1).Record("draft", StreamEvent{"type": "future_stream_event"})
}

func stringList(values []string) string {
	payload, _ := json.Marshal(values)
	return string(payload)
}
