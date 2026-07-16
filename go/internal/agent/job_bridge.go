package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
)

const agentJobBridgeConsumerID = "agent"

const jobObservationDispatchLimit = 100

var agentWaitedJobKinds = map[string]struct{}{
	"understand": {}, "render_preview": {}, "render_final": {},
}

// IsAgentWaitedJobKind is shared with the turn-cancel endpoint so the API and
// bridge cannot drift on which asynchronous jobs belong to an Agent turn.
func IsAgentWaitedJobKind(kind string) bool {
	_, ok := agentWaitedJobKinds[kind]
	return ok
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
	service.dispatchPendingJobObservations(ctx)
	rows, err := service.database.Read().QueryContext(ctx, `
		SELECT event_id,draft_id,payload_json FROM event_log
		WHERE event_id>? AND event_type IN ('JobSucceeded','JobFailed','JobCancelled')
		ORDER BY event_id LIMIT 100`, cursor)
	if err != nil {
		return cursor
	}
	observations := make([]bridgeObservation, 0)
	lastEventID := cursor
	for rows.Next() {
		var eventID int64
		var draftID *string
		var payloadJSON []byte
		if err := rows.Scan(&eventID, &draftID, &payloadJSON); err != nil {
			_ = rows.Close()
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
			claimToken: randomID("bridge"),
		})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return cursor
	}
	_ = rows.Close()
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
	service.dispatchPendingJobObservations(ctx)
	return lastEventID
}

func (service *Service) dispatchPendingJobObservations(ctx context.Context) {
	service.bridgeDispatchMu.Lock()
	defer service.bridgeDispatchMu.Unlock()
	afterEventID := service.bridgeScanCursor
	for {
		page, lastEventID, scanned := service.pendingJobObservationPage(ctx, afterEventID)
		if scanned < 0 {
			return
		}
		if scanned == 0 && afterEventID != 0 {
			// Wrap without consuming scan budget: the first query returned no rows,
			// so this invocation still examines at most dispatchLimit records.
			service.bridgeScanCursor = 0
			afterEventID = 0
			continue
		}
		if scanned > 0 {
			service.bridgeScanCursor = lastEventID
		}
		for _, observation := range page {
			service.dispatchJobObservation(ctx, observation)
		}
		return
	}
}

func (service *Service) pendingJobObservationPage(
	ctx context.Context,
	afterEventID int64,
) ([]bridgeObservation, int64, int) {
	rows, err := service.database.Read().QueryContext(ctx, `
		SELECT event_id,job_id,draft_id,event_json,claim_token
		FROM agent_job_observations
		WHERE delivered_at IS NULL AND event_id>?
		ORDER BY event_id LIMIT ?`, afterEventID, jobObservationDispatchLimit)
	if err != nil {
		return nil, afterEventID, -1
	}
	page := make([]bridgeObservation, 0, jobObservationDispatchLimit)
	lastEventID := afterEventID
	scanned := 0
	for rows.Next() {
		var observation bridgeObservation
		var eventJSON []byte
		if err := rows.Scan(&observation.eventID, &observation.jobID, &observation.draftID,
			&eventJSON, &observation.claimToken); err != nil {
			_ = rows.Close()
			return nil, afterEventID, -1
		}
		scanned++
		lastEventID = observation.eventID
		if json.Unmarshal(eventJSON, &observation.event) != nil {
			continue
		}
		page = append(page, observation)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, afterEventID, -1
	}
	if err := rows.Close(); err != nil {
		return nil, afterEventID, -1
	}
	return page, lastEventID, scanned
}

func (service *Service) dispatchJobObservation(ctx context.Context, observation bridgeObservation) bool {
	jobID, draftID, claimToken := observation.jobID, observation.draftID, observation.claimToken
	event := observation.event
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
	if !service.queue.Enqueue(item) {
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
