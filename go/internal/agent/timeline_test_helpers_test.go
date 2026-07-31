package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func withTestTurnLeaseSession(
	t *testing.T,
	service *Service,
	ctx context.Context,
	draftID string,
) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancelCause(ctx)
	turnID := "turn-test-" + agentexec.RandomID("lease")
	messageID := "message-test-" + turnID
	session := newTimelineEditLeaseSession(service.database, draftID, turnID, cancel)
	t.Cleanup(func() {
		session.close()
		cancel(nil)
	})
	ctx = rushestools.WithDraftID(ctx, draftID)
	ctx = rushestools.WithTurnIdentity(ctx, turnID, messageID)
	if err := service.startAgentTurnRun(ctx, turnID, QueueItem{
		DraftID: draftID, ItemID: messageID, Kind: QueueUserMessage,
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = service.finishAgentTurnRun(context.Background(), turnID, "cancelled")
	})
	ctx = withTimelineEditLeaseSession(ctx, session)
	return rushestools.WithTimelineWriteAdmission(ctx, turnID, session.token, session.markLost)
}

func fmtJSON(value any) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func timelineTrackClips(document timeline.Document, trackID string) []timeline.Clip {
	for _, track := range document.Tracks {
		if track.TrackID == trackID {
			return track.Clips
		}
	}
	return nil
}

func seedTimelineVersion(
	service *Service,
	ctx context.Context,
	draftID string,
	document timeline.Document,
	operation string,
	editOperation map[string]any,
) (rushestools.ToolResult, error) {
	draft, err := storage.GetDraft(ctx, service.database.Read(), draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	documentMap, err := timeline.ToMap(document)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	report := timeline.Validate(document)
	reportMap := map[string]any{
		"valid": report.Valid, "checks": report.Checks, "issues": report.Issues,
	}
	validationType := "TimelineValidated"
	if !report.Valid {
		validationType = "TimelineValidationFailed"
	}
	actor := contracts.ActorAgent
	origin := rushestools.TimelineMutationOrigin(ctx)
	if origin == "manual" {
		actor = contracts.ActorUser
	}
	if origin == "" {
		origin = "agent"
	}
	var writeAdmission *reducer.TimelineWriteAdmission
	if origin == "manual" {
		writeAdmission = &reducer.TimelineWriteAdmission{Origin: "manual"}
	} else {
		turnID := "turn-test-seed-" + agentexec.RandomID("lease")
		leaseToken := agentexec.RandomID("lease")
		if session := timelineEditLeaseSessionFromContext(ctx); session != nil {
			if session.draftID != draftID {
				return rushestools.ToolResult{}, fmt.Errorf(
					"timeline fixture lease draft mismatch: session=%s seed=%s",
					session.draftID, draftID,
				)
			}
			if err := session.ensure(ctx); err != nil {
				return rushestools.ToolResult{}, err
			}
			turnID = session.turnID
			leaseToken = session.token
		} else {
			result, acquireErr := applyAgentEditLeaseMutation(ctx, service.database,
				reducer.AgentEditLeaseMutation{
					Operation: reducer.AgentEditLeaseAcquire,
					DraftID:   draftID, TurnID: turnID, LeaseToken: leaseToken,
					Now: time.Now().UTC(), TTL: agentEditLeaseTTL,
				},
			)
			if acquireErr != nil {
				return rushestools.ToolResult{}, acquireErr
			}
			if result.AgentEditLease == nil || result.AgentEditLease.Lease == nil {
				return rushestools.ToolResult{}, errors.New("timeline fixture lease acquire 未返回持久化租约")
			}
			defer func() {
				_, _ = applyAgentEditLeaseMutation(context.WithoutCancel(ctx), service.database,
					reducer.AgentEditLeaseMutation{
						Operation: reducer.AgentEditLeaseRelease,
						DraftID:   draftID, TurnID: turnID, LeaseToken: leaseToken,
						Now: time.Now().UTC(),
					},
				)
			}()
		}
		writeAdmission = &reducer.TimelineWriteAdmission{
			Origin: "agent", TurnID: turnID, LeaseToken: leaseToken,
		}
	}
	editOperations := []map[string]any{}
	if editOperation != nil {
		editOperations = append(editOperations, editOperation)
	}
	payload := map[string]any{
		"timeline_id": document.TimelineID, "timeline_version": document.Version,
		"patch_id":      operation + ":" + agentexec.RandomID("patch"),
		"document_json": documentMap, "edit_origin": origin,
		"edit_operations": editOperations,
	}
	if document.Version > 1 {
		payload["parent_version"] = document.Version - 1
	}
	result, err := reducer.Apply(ctx, service.database, []contracts.Event{
		{
			Type: "TimelineVersionCreated", DraftID: draftID,
			Payload: payload,
		},
		{
			Type: validationType, DraftID: draftID,
			Payload: map[string]any{
				"timeline_version":  document.Version,
				"validation_report": reportMap,
			},
		},
	}, reducer.Options{
		Actor: actor, BaseVersion: &draft.StateVersion,
		TimelineWriteAdmission: writeAdmission,
	})
	if err != nil || result.Status != reducer.StatusApplied {
		return rushestools.ToolResult{}, errors.Join(
			err, fmt.Errorf("timeline fixture reducer status: %s", result.Status),
		)
	}
	status := string(rushestools.StatusSucceeded)
	if !report.Valid {
		status = string(rushestools.StatusValidationFailed)
	}
	return rushestools.ToolResult{
		Status: status, Observation: timeline.Inspect(document),
		Data: map[string]any{"validation_report": reportMap},
	}, nil
}
