package api

import (
	"os"
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
)

func TestDomainSSEFrameGolden(t *testing.T) {
	t.Parallel()
	event := contracts.Event{
		Type: "DraftCreated", Actor: contracts.ActorUser, DraftID: "draft_1",
		Payload: map[string]any{"name": "演示"}, CreatedAt: "2026-07-10T00:00:00Z",
	}
	actual, err := encodeSSE(42, event)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := os.ReadFile("testdata/domain_sse.golden")
	if err != nil {
		t.Fatal(err)
	}
	if actual != string(expected) {
		t.Fatalf("domain SSE 漂移\n--- expected ---\n%s--- actual ---\n%s", expected, actual)
	}
}

func TestTimelineVersionRestoredSSEFrameGolden(t *testing.T) {
	t.Parallel()
	event := contracts.Event{
		Type: "TimelineVersionRestored", Actor: contracts.ActorUser, DraftID: "draft_1",
		Payload: map[string]any{
			"checkpoint_id": "rewind_1", "restore_checkpoint_id": "rewind_2", "mode": "both",
			"source_version": 2, "timeline_version": 4,
		},
		CreatedAt: "2026-07-16T00:00:00Z",
	}
	actual, err := encodeSSE(43, event)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := os.ReadFile("testdata/timeline_restored_sse.golden")
	if err != nil {
		t.Fatal(err)
	}
	if actual != string(expected)+"\n" {
		t.Fatalf("TimelineVersionRestored SSE 漂移\n--- expected ---\n%s--- actual ---\n%s", expected, actual)
	}
}

func TestLastEventIDHeaderQueryAndInvalidFallback(t *testing.T) {
	t.Parallel()
	request := apiRequest(t, "GET", "/api/events?last_event_id=7", nil)
	if got := lastEventID(request); got != 7 {
		t.Fatalf("query cursor=%d", got)
	}
	request.Header.Set("Last-Event-ID", "9")
	if got := lastEventID(request); got != 9 {
		t.Fatalf("header cursor=%d", got)
	}
	request.Header.Set("Last-Event-ID", "-1")
	if got := lastEventID(request); got != 0 {
		t.Fatalf("invalid cursor=%d", got)
	}
}
