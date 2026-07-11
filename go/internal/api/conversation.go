package api

import (
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
	writeJSON(writer, http.StatusOK, MessagesResponse{DraftId: draftID, Messages: messages})
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
	requested := server.agent.Queue().RequestStop(draftID)
	status := "idle"
	if requested {
		status = "requested"
	}
	writeJSON(writer, http.StatusOK, TurnCancelResponse{
		DraftId: draftID, Requested: requested, Status: TurnCancelResponseStatus(status),
	})
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
