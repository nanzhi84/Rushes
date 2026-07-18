package agent

import (
	"context"

	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

func withQueueMemoryEvidence(ctx context.Context, item QueueItem) context.Context {
	switch item.Kind {
	case QueueUserMessage:
		return agentexec.WithMemoryEvidence(ctx, storage.UserMemoryEvidenceMessage, item.ItemID)
	case QueueUIObservation:
		if agentexec.InterfaceString(item.Payload["observation_type"]) == "decision_answered" {
			return agentexec.WithMemoryEvidence(
				ctx, storage.UserMemoryEvidenceDecision, agentexec.InterfaceString(item.Payload["decision_id"]),
			)
		}
	}
	return ctx
}
