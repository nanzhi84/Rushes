package agent

import (
	"context"
	"encoding/json"
	"time"
)

var agentWaitedJobKinds = map[string]struct{}{
	"understand": {}, "render_preview": {}, "render_final": {},
}

func (service *Service) startJobObservationBridge(ctx context.Context) {
	var cursor int64
	_ = service.database.Read().QueryRowContext(ctx,
		"SELECT COALESCE(MAX(event_id),0) FROM event_log").Scan(&cursor)
	service.bridgeWG.Add(1)
	go func() {
		defer service.bridgeWG.Done()
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
	rows, err := service.database.Read().QueryContext(ctx, `
		SELECT event_id,draft_id,payload_json FROM event_log
		WHERE event_id>? AND event_type IN ('JobSucceeded','JobFailed')
		ORDER BY event_id LIMIT 100`, cursor)
	if err != nil {
		return cursor
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var eventID int64
		var draftID *string
		var payloadJSON []byte
		if err := rows.Scan(&eventID, &draftID, &payloadJSON); err != nil {
			return cursor
		}
		cursor = eventID
		var envelope struct {
			Type    string         `json:"event"`
			DraftID string         `json:"draft_id"`
			Payload map[string]any `json:"payload"`
		}
		if err := json.Unmarshal(payloadJSON, &envelope); err != nil {
			continue
		}
		kind, _ := envelope.Payload["kind"].(string)
		if _, waited := agentWaitedJobKinds[kind]; !waited {
			continue
		}
		target, _ := envelope.Payload["requested_by_draft_id"].(string)
		if target == "" {
			target = envelope.DraftID
		}
		if target == "" && draftID != nil {
			target = *draftID
		}
		jobID, _ := envelope.Payload["job_id"].(string)
		if target == "" || jobID == "" {
			continue
		}
		service.queue.EnqueueJobObservation(target, jobID, map[string]any{
			"event": envelope.Type, "payload": envelope.Payload,
		})
	}
	return cursor
}
