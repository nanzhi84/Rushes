package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/agent"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

func (server *Server) EnqueueMessageApiDraftsDraftIdMessagesPost(
	writer http.ResponseWriter,
	request *http.Request,
	draftID string,
) {
	if _, err := storage.GetDraft(request.Context(), server.database.Read(), draftID); errors.Is(err, storage.ErrNotFound) {
		writeNotFound(writer, "draft_not_found")
		return
	} else if err != nil {
		server.internalError(writer, err)
		return
	}
	var payload MessageCreateRequest
	if err := decodeJSON(request, &payload); err != nil {
		writeBadRequest(writer, "invalid_json")
		return
	}
	content := strings.TrimSpace(payload.Content)
	if content == "" {
		writeBadRequest(writer, "empty_message")
		return
	}
	messageID := newID("msg")
	if payload.MessageId != nil && *payload.MessageId != "" {
		messageID = *payload.MessageId
	}
	result, err := reducer.Apply(request.Context(), server.database, nil, reducer.Options{
		Actor: contracts.ActorUser,
		ResultRows: reducer.ResultRows{Message: &reducer.MessageRow{
			ID: messageID, DraftID: draftID, Role: "user", Kind: "user", Content: content,
		}},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		server.internalError(writer, errors.Join(err, fmt.Errorf("reducer status: %s", result.Status)))
		return
	}
	if !server.agent.Queue().EnqueueUserMessage(draftID, messageID, content) {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]any{
			"detail": map[string]string{"reason": "turn_queue_closed"},
		})
		return
	}
	writeJSON(writer, http.StatusAccepted, MessageQueuedResponse{
		DraftId: draftID, MessageId: messageID,
		Status: MessageQueuedResponseStatus("queued"), Kind: MessageQueuedResponseKind("user_message"),
	})
}

func (server *Server) ClearDraftConversationApiDraftsDraftIdConversationClearPost(
	writer http.ResponseWriter,
	request *http.Request,
	draftID string,
) {
	draft, err := storage.GetDraft(request.Context(), server.database.Read(), draftID)
	if errors.Is(err, storage.ErrNotFound) {
		writeNotFound(writer, "draft_not_found")
		return
	}
	if err != nil {
		server.internalError(writer, err)
		return
	}
	if server.agent.Queue().IsBusy(draftID) {
		writeJSON(writer, http.StatusConflict, map[string]any{
			"detail": map[string]string{"reason": "turn_active"},
		})
		return
	}
	messageID := newID("context")
	result, err := reducer.Apply(request.Context(), server.database, []contracts.Event{{
		Type: "ConversationContextCleared", DraftID: draftID,
		Payload: map[string]any{"message_id": messageID},
	}}, reducer.Options{
		Actor:       contracts.ActorUser,
		BaseVersion: &draft.StateVersion,
		ResultRows: reducer.ResultRows{Message: &reducer.MessageRow{
			ID: messageID, DraftID: draftID, Role: "system_observation", Kind: "context_reset",
			Content: "对话上下文已清空；素材、素材理解、时间线和预览均已保留。",
		}},
	})
	if err != nil {
		server.internalError(writer, err)
		return
	}
	if result.Status != reducer.StatusApplied {
		writeReducerResult(writer, result)
		return
	}
	writeJSON(writer, http.StatusOK, ConversationClearResponse{
		DraftId: draftID, MessageId: messageID, EventIds: reducerEventIDs(result),
		Preserved: []string{"assets", "material_understanding", "timeline", "preview"},
		Status:    ConversationClearResponseStatus("cleared"),
	})
}

func (server *Server) ListDraftMessagesApiDraftsDraftIdMessagesGet(
	writer http.ResponseWriter,
	request *http.Request,
	draftID string,
	params ListDraftMessagesApiDraftsDraftIdMessagesGetParams,
) {
	if _, err := storage.GetDraft(request.Context(), server.database.Read(), draftID); errors.Is(err, storage.ErrNotFound) {
		writeNotFound(writer, "draft_not_found")
		return
	} else if err != nil {
		server.internalError(writer, err)
		return
	}
	limit := 200
	if params.Limit != nil {
		limit = *params.Limit
	}
	rows, err := storage.ListMessages(request.Context(), server.database.Read(), draftID, limit)
	if err != nil {
		server.internalError(writer, err)
		return
	}
	messages := make([]MessageRecord, 0, len(rows))
	for _, row := range rows {
		messages = append(messages, MessageRecord{
			MessageId: row.ID, Role: row.Role, Kind: row.Kind,
			Content: row.Content, CreatedAt: row.CreatedAt,
		})
	}
	rewoundCount, err := storage.CountRewoundMessages(request.Context(), server.database.Read(), draftID)
	if err != nil {
		server.internalError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, MessagesResponse{
		DraftId: draftID, Messages: messages, RewoundMessageCount: rewoundCount,
	})
}

func (server *Server) CancelCurrentTurnApiDraftsDraftIdTurnCancelPost(
	writer http.ResponseWriter,
	request *http.Request,
	draftID string,
) {
	if _, err := storage.GetDraft(request.Context(), server.database.Read(), draftID); errors.Is(err, storage.ErrNotFound) {
		writeNotFound(writer, "draft_not_found")
		return
	} else if err != nil {
		server.internalError(writer, err)
		return
	}
	barrier, stopped := server.agent.Queue().BeginDraftCancellation(draftID)
	cleanupCtx, cancelCleanup := turnCancellationContext(request.Context())
	defer cancelCleanup()
	jobBoundary, err := server.turnCancellationJobBoundary(cleanupCtx)
	if err != nil {
		barrier.Abandon()
		server.internalError(writer, err)
		return
	}
	if err := server.suppressTurnJobObservations(cleanupCtx, draftID, jobBoundary); err != nil {
		barrier.Abandon()
		server.internalError(writer, err)
		return
	}
	cancelledJobs, err := server.cancelTurnJobs(cleanupCtx, draftID, jobBoundary)
	if err != nil {
		barrier.Abandon()
		server.internalError(writer, err)
		return
	}
	waitCtx, cancelWait := context.WithTimeout(context.Background(), 500*time.Millisecond)
	finished := barrier.Wait(waitCtx)
	cancelWait()
	if finished {
		trailingJobs, trailingErr := server.cancelTurnJobs(cleanupCtx, draftID, jobBoundary)
		barrier.Release()
		if trailingErr != nil {
			server.internalError(writer, trailingErr)
			return
		}
		cancelledJobs += trailingJobs
	} else {
		trailingJobs, trailingErr := server.cancelTurnJobs(cleanupCtx, draftID, jobBoundary)
		barrier.Abandon()
		if trailingErr != nil {
			server.internalError(writer, trailingErr)
			return
		}
		cancelledJobs += trailingJobs
	}
	requested := stopped || cancelledJobs > 0
	status := "idle"
	if requested {
		status = "requested"
	}
	writeJSON(writer, http.StatusOK, TurnCancelResponse{
		DraftId: draftID, Requested: requested, Status: TurnCancelResponseStatus(status),
	})
}

func turnCancellationContext(requestCtx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(requestCtx), 5*time.Second)
}

func (server *Server) turnCancellationJobBoundary(ctx context.Context) (int64, error) {
	var boundary int64
	err := server.database.Read().QueryRowContext(ctx, "SELECT COALESCE(MAX(rowid),0) FROM jobs").Scan(&boundary)
	return boundary, err
}

func (server *Server) suppressTurnJobObservations(
	ctx context.Context,
	draftID string,
	boundary int64,
) error {
	rows, err := server.database.Read().QueryContext(ctx, `
		SELECT job_id,kind FROM jobs
		WHERE rowid<=? AND COALESCE(requested_by_draft_id,draft_id)=?
		ORDER BY rowid`, boundary, draftID)
	if err != nil {
		return err
	}
	suppressions := make([]reducer.AgentJobObservationSuppressionRow, 0)
	for rows.Next() {
		var jobID, kind string
		if err := rows.Scan(&jobID, &kind); err != nil {
			_ = rows.Close()
			return err
		}
		if agent.IsAgentWaitedJobKind(kind) {
			suppressions = append(suppressions, reducer.AgentJobObservationSuppressionRow{JobID: jobID})
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(suppressions) == 0 {
		return nil
	}
	result, err := reducer.Apply(ctx, server.database, nil, reducer.Options{
		Actor:      contracts.ActorUser,
		ResultRows: reducer.ResultRows{AgentJobObservationSuppressions: suppressions},
	})
	if err != nil {
		return err
	}
	if result.Status != reducer.StatusApplied {
		return fmt.Errorf("抑制 turn job observation reducer status: %s", result.Status)
	}
	return nil
}

func (server *Server) cancelTurnJobs(ctx context.Context, draftID string, boundary int64) (int, error) {
	rows, err := server.database.Read().QueryContext(ctx, `
		SELECT job_id,kind,draft_id,requested_by_draft_id,asset_id
		FROM jobs
		WHERE rowid<=?
		  AND COALESCE(requested_by_draft_id,draft_id)=?
		  AND status IN ('pending','running')
		ORDER BY rowid`, boundary, draftID)
	if err != nil {
		return 0, err
	}
	type cancellableJob struct {
		id, kind                      string
		draftID, requestedBy, assetID sql.NullString
	}
	jobs := make([]cancellableJob, 0)
	for rows.Next() {
		var job cancellableJob
		if err := rows.Scan(&job.id, &job.kind, &job.draftID, &job.requestedBy, &job.assetID); err != nil {
			_ = rows.Close()
			return 0, err
		}
		if agent.IsAgentWaitedJobKind(job.kind) {
			jobs = append(jobs, job)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	_ = rows.Close()
	cancelled := 0
	for _, job := range jobs {
		eventDraftID := ""
		if job.draftID.Valid {
			eventDraftID = job.draftID.String
		}
		payload := map[string]any{
			"job_id": job.id, "kind": job.kind, "reason": "turn_cancelled",
		}
		if job.requestedBy.Valid {
			payload["requested_by_draft_id"] = job.requestedBy.String
		}
		if job.assetID.Valid {
			payload["asset_id"] = job.assetID.String
		}
		result, applyErr := reducer.Apply(ctx, server.database, []contracts.Event{{
			Type: "JobCancelled", DraftID: eventDraftID, Payload: payload,
		}}, reducer.Options{Actor: contracts.ActorUser})
		if errors.Is(applyErr, reducer.ErrJobNotCancellable) {
			continue
		}
		if applyErr != nil {
			return cancelled, applyErr
		}
		if result.Status != reducer.StatusApplied {
			return cancelled, fmt.Errorf("取消 turn job reducer status: %s", result.Status)
		}
		cancelled++
	}
	return cancelled, nil
}

func (server *Server) DraftTurnStreamApiDraftsDraftIdTurnStreamGet(
	writer http.ResponseWriter,
	request *http.Request,
	draftID string,
) {
	if _, err := storage.GetDraft(request.Context(), server.database.Read(), draftID); errors.Is(err, storage.ErrNotFound) {
		writeNotFound(writer, "draft_not_found")
		return
	} else if err != nil {
		server.internalError(writer, err)
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("Connection", "keep-alive")
	writer.WriteHeader(http.StatusOK)
	controller := http.NewResponseController(writer)
	_ = controller.Flush()
	snapshot, stream, unsubscribe := server.agent.Hub().Subscribe(draftID)
	defer unsubscribe()
	sent := 0
	writeEvent := func(event agent.StreamEvent) bool {
		frame, err := agent.EncodeTurnStreamFrame(event)
		if err != nil {
			return false
		}
		if _, err := writer.Write(frame); err != nil {
			return false
		}
		if err := controller.Flush(); err != nil {
			return false
		}
		sent++
		return true
	}
	for _, event := range snapshot {
		if !writeEvent(event) || server.sseMaxEvents > 0 && sent >= server.sseMaxEvents {
			return
		}
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-request.Context().Done():
			return
		case event, ok := <-stream:
			if !ok || !writeEvent(event) {
				return
			}
			if server.sseMaxEvents > 0 && sent >= server.sseMaxEvents {
				return
			}
		case <-heartbeat.C:
			if _, err := writer.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			if err := controller.Flush(); err != nil {
				return
			}
		}
	}
}
