package agent

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestModelStreamPreviewSessionNilAndCanceledPaths(t *testing.T) {
	ctx := t.Context()
	if withModelStreamPreviewSession(ctx, nil) != ctx {
		t.Fatal("nil preview session must preserve the caller context")
	}
	var empty *modelStreamPreviewSession
	if empty.begin() != "" || empty.candidate() != "" {
		t.Fatal("nil preview session must not create message state")
	}
	empty.delta("message", "delta")
	empty.finalize()

	const draftID = "draft_preview_canceled"
	service := &Service{hub: NewTurnStreamHub(0)}
	session := newModelStreamPreviewSession(service, draftID, "message_final")
	session.delta("", "ignored")
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	session.complete(canceled, "preview_canceled", schema.AssistantMessage("不会保留", nil))
	session.complete(ctx, "preview_empty", nil)
	events := service.Hub().Snapshot(draftID)
	if len(events) != 2 || events[0]["type"] != TurnStreamMessageDiscarded ||
		events[1]["type"] != TurnStreamMessageDiscarded {
		t.Fatalf("canceled/empty previews must be discarded: %#v", events)
	}
	if got := (&previewPersistenceError{status: "rejected"}).Error(); got != "model preview reducer status: rejected" {
		t.Fatalf("error=%q", got)
	}
}

func TestModelStreamPreviewSessionDiscardsSupersededFinalCandidate(t *testing.T) {
	const draftID = "draft_preview_candidate_replaced"
	service := &Service{hub: NewTurnStreamHub(0)}
	session := newModelStreamPreviewSession(service, draftID, "message_final")

	session.complete(t.Context(), "preview_old", schema.AssistantMessage("旧的成功声明", nil))
	session.complete(t.Context(), "preview_new", schema.AssistantMessage("新的终态候选", nil))

	if got := session.candidate(); got != "preview_new" {
		t.Fatalf("candidate=%q want=preview_new", got)
	}
	events := service.Hub().Snapshot(draftID)
	if len(events) != 1 || events[0]["type"] != TurnStreamMessageDiscarded ||
		events[0]["message_id"] != "preview_old" {
		t.Fatalf("被覆盖候选未立即撤销：%#v", events)
	}

	session.discardPending()
	events = service.Hub().Snapshot(draftID)
	if len(events) != 2 || events[1]["type"] != TurnStreamMessageDiscarded ||
		events[1]["message_id"] != "preview_new" {
		t.Fatalf("当前候选未在失败收口时撤销：%#v", events)
	}
}
