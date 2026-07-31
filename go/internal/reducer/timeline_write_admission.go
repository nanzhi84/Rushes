package reducer

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

func validateTimelineWriteAdmission(
	ctx context.Context,
	tx *sql.Tx,
	events []contracts.Event,
	actor contracts.Actor,
	admission *TimelineWriteAdmission,
) error {
	drafts := timelinePointerWriteDrafts(events)
	if len(drafts) == 0 {
		return nil
	}
	origin := ""
	now := time.Now().UTC()
	if admission != nil {
		origin = strings.TrimSpace(admission.Origin)
		if !admission.Now.IsZero() {
			now = admission.Now.UTC()
		}
	}
	// An Agent timeline pointer write is valid only as the continuation of the
	// exact persisted edit lease held by its turn. There is no fixture/legacy
	// fail-open path: missing admission must fail even when no lease row exists.
	if actor == contracts.ActorAgent &&
		(admission == nil || origin != "agent" ||
			strings.TrimSpace(admission.TurnID) == "" ||
			strings.TrimSpace(admission.LeaseToken) == "") {
		return storage.ErrAgentEditLeaseLost
	}
	if origin == "agent" && actor != contracts.ActorAgent {
		return storage.ErrAgentEditLeaseLost
	}
	if origin == "" {
		if actor == contracts.ActorUser {
			origin = "manual"
		} else {
			// Non-Agent infrastructure actors retain their historical classification;
			// production timeline edits are only manual user or admitted Agent writes.
			origin = "legacy"
		}
	}
	for draftID := range drafts {
		lease, err := storage.GetLiveAgentEditLease(ctx, tx, draftID, now)
		if errors.Is(err, storage.ErrNotFound) {
			if origin == "agent" {
				return storage.ErrAgentEditLeaseLost
			}
			continue
		}
		if err != nil {
			return err
		}
		if origin != "agent" {
			RecordManualTimelineWriteRejectedWhileAgent()
			return storage.ErrTimelineLockedByAgent
		}
		if admission == nil || lease.TurnID != admission.TurnID ||
			lease.LeaseToken != admission.LeaseToken {
			return storage.ErrAgentEditLeaseLost
		}
	}
	return nil
}

func timelinePointerWriteDrafts(events []contracts.Event) map[string]struct{} {
	result := map[string]struct{}{}
	for _, event := range events {
		switch event.Type {
		case "TimelineVersionCreated":
			if event.DraftID != "" {
				result[event.DraftID] = struct{}{}
			}
		case "TimelineVersionRestored":
			mode, _ := event.Payload["mode"].(string)
			if event.DraftID != "" && (mode == "timeline" || mode == "both") {
				result[event.DraftID] = struct{}{}
			}
		}
	}
	return result
}
