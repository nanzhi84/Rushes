package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
)

const agentJobBridgeConsumerID = "agent"

const jobObservationDispatchLimit = 100

// IsAgentWaitedJobKind is shared with the turn-cancel endpoint so the API and
// bridge cannot drift on which asynchronous jobs belong to an Agent turn.
func IsAgentWaitedJobKind(kind string) bool {
	spec, exists := contracts.LookupJobKind(kind)
	return exists && spec.AgentWaited
}

type bridgeObservation struct {
	eventID    int64
	draftID    string
	jobID      string
	event      map[string]any
	claimToken string
}

func (service *Service) startJobObservationBridge(ctx context.Context) {
	var cursor int64
	if err := service.database.Read().QueryRowContext(ctx, `
		SELECT last_event_id FROM agent_job_bridge_state WHERE consumer_id=?`,
		agentJobBridgeConsumerID,
	).Scan(&cursor); err != nil {
		slog.Error("读取 Agent job bridge 游标失败", "error", err)
		return
	}
	service.bridgeWG.Add(1)
	go func() {
		defer service.bridgeWG.Done()
		// Run once before the first tick so terminal events written while the
		// service was down are recovered immediately after restart.
		cursor = service.bridgeIteration(ctx, cursor)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cursor = service.bridgeIteration(ctx, cursor)
			}
		}
	}()
}

func (service *Service) bridgeIteration(ctx context.Context, cursor int64) int64 {
	// Dispatch exactly one bounded page per iteration. Deferring preserves retry
	// behavior on ingestion failures and includes newly persisted observations on
	// successful ingestion without allowing a second page in the same tick.
	defer service.dispatchPendingJobObservations(ctx)
	rows, err := service.database.Read().QueryContext(ctx, `
		SELECT event_id,draft_id,payload_json FROM event_log
		WHERE event_id>? AND event_type IN ('JobSucceeded','JobFailed','JobCancelled')
		ORDER BY event_id LIMIT 100`, cursor)
	if err != nil {
		return cursor
	}
	defer func() { _ = rows.Close() }()
	observations := make([]bridgeObservation, 0)
	lastEventID := cursor
	for rows.Next() {
		var eventID int64
		var draftID *string
		var payloadJSON []byte
		if err := rows.Scan(&eventID, &draftID, &payloadJSON); err != nil {
			return cursor
		}
		lastEventID = eventID
		var envelope map[string]any
		if err := json.Unmarshal(payloadJSON, &envelope); err != nil {
			continue
		}
		payload, _ := envelope["payload"].(map[string]any)
		kind, _ := payload["kind"].(string)
		if !IsAgentWaitedJobKind(kind) {
			continue
		}
		target, _ := payload["requested_by_draft_id"].(string)
		if target == "" {
			target, _ = envelope["draft_id"].(string)
		}
		if target == "" && draftID != nil {
			target = *draftID
		}
		jobID, _ := payload["job_id"].(string)
		if target == "" || jobID == "" {
			continue
		}
		observations = append(observations, bridgeObservation{
			eventID: eventID, draftID: target, jobID: jobID, event: envelope,
			claimToken: agentexec.RandomID("bridge"),
		})
	}
	if err := rows.Err(); err != nil {
		return cursor
	}
	if lastEventID == cursor {
		return cursor
	}
	resultRows := reducer.ResultRows{
		AgentJobBridgeCursor: &reducer.AgentJobBridgeCursorRow{
			ConsumerID: agentJobBridgeConsumerID, LastEventID: lastEventID,
		},
		AgentJobObservations: make([]reducer.AgentJobObservationRow, 0, len(observations)),
	}
	for _, observation := range observations {
		resultRows.AgentJobObservations = append(resultRows.AgentJobObservations,
			reducer.AgentJobObservationRow{
				JobID: observation.jobID, EventID: observation.eventID,
				DraftID: observation.draftID, Event: observation.event,
				ClaimToken: observation.claimToken,
			})
	}
	result, err := reducer.Apply(ctx, service.database, nil, reducer.Options{
		Actor: contracts.ActorAgent, ResultRows: resultRows,
	})
	if err != nil || result.Status != reducer.StatusApplied {
		return cursor
	}
	return lastEventID
}

func (service *Service) dispatchPendingJobObservations(ctx context.Context) {
	service.bridgeDispatchMu.Lock()
	defer service.bridgeDispatchMu.Unlock()
	page, err := service.pendingJobObservations(ctx)
	if err != nil {
		return
	}
	for _, observation := range page {
		service.dispatchJobObservation(ctx, observation)
	}
}

func (service *Service) pendingJobObservations(ctx context.Context) ([]bridgeObservation, error) {
	rows, err := service.database.Read().QueryContext(ctx, `
		SELECT event_id,job_id,draft_id,event_json,claim_token
		FROM agent_job_observations
		WHERE delivered_at IS NULL
		ORDER BY event_id LIMIT ?`, jobObservationDispatchLimit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	page := make([]bridgeObservation, 0, jobObservationDispatchLimit)
	for rows.Next() {
		var observation bridgeObservation
		var eventJSON []byte
		if err := rows.Scan(&observation.eventID, &observation.jobID, &observation.draftID,
			&eventJSON, &observation.claimToken); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(eventJSON, &observation.event)
		page = append(page, observation)
	}
	return page, rows.Err()
}

func (service *Service) dispatchJobObservation(ctx context.Context, observation bridgeObservation) bool {
	return service.enqueueJobObservation(ctx, observation, false)
}

func (service *Service) enqueueJobObservation(
	ctx context.Context,
	observation bridgeObservation,
	onlyIfIdle bool,
) bool {
	jobID, draftID, claimToken := observation.jobID, observation.draftID, observation.claimToken
	event := observation.event
	if event == nil {
		slog.Error("隔离损坏的 Agent job observation", "job_id", jobID, "event_id", observation.eventID)
		service.markJobObservationDelivered(ctx, jobID, claimToken)
		return true
	}
	payload, _ := event["payload"].(map[string]any)
	reason, _ := payload["reason"].(string)
	if (event["event"] == "JobCancelled" && reason == "turn_cancelled") ||
		service.jobObservationSuppressed(ctx, jobID) {
		service.markJobObservationDelivered(ctx, jobID, claimToken)
		return true
	}
	service.bridgeMu.Lock()
	if service.bridgeInflight == nil {
		service.bridgeInflight = map[string]struct{}{}
	}
	if _, exists := service.bridgeInflight[jobID]; exists {
		service.bridgeMu.Unlock()
		return false
	}
	service.bridgeInflight[jobID] = struct{}{}
	service.bridgeMu.Unlock()
	item := QueueItem{
		DraftID: draftID, Kind: QueueJobObservation, ItemID: jobID,
		Payload: map[string]any{"job_id": jobID, "event": event, "claim_token": claimToken},
	}
	item.onConsumed = func(consumeErr error) {
		if errors.Is(consumeErr, errTurnCancelledByUser) {
			deliveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			service.markJobObservationDelivered(deliveryCtx, jobID, claimToken)
			cancel()
		}
		service.releaseJobObservation(jobID)
	}
	if !service.queue.enqueue(item, onlyIfIdle) {
		service.releaseJobObservation(jobID)
		return false
	}
	return true
}

func (service *Service) jobObservationSuppressed(ctx context.Context, jobID string) bool {
	var suppressed int
	if err := service.database.Read().QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM agent_job_observation_suppressions WHERE job_id=?
		)`, jobID).Scan(&suppressed); err != nil {
		slog.Error("读取 Agent job observation 抑制状态失败", "job_id", jobID, "error", err)
		return false
	}
	return suppressed == 1
}

func (service *Service) markJobObservationDelivered(ctx context.Context, jobID, claimToken string) {
	result, err := reducer.Apply(ctx, service.database, nil, reducer.Options{
		Actor: contracts.ActorAgent,
		ResultRows: reducer.ResultRows{AgentJobObservationDelivery: &reducer.AgentJobObservationDeliveryRow{
			JobID: jobID, ClaimToken: claimToken,
		}},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		slog.Error("标记 Agent job observation 已交付失败", "job_id", jobID, "error", err)
	}
}

func (service *Service) releaseJobObservation(jobID string) {
	service.bridgeMu.Lock()
	delete(service.bridgeInflight, jobID)
	service.bridgeMu.Unlock()
}
