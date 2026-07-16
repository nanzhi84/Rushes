package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/agent"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

func (server *Server) WorkspaceEventsApiEventsGet(writer http.ResponseWriter, request *http.Request) {
	server.streamEvents(writer, request, nil, nil, "", contracts.RoutesToWorkspace)
}

func (server *Server) DraftEventsApiDraftsDraftIdEventsGet(
	writer http.ResponseWriter,
	request *http.Request,
	draftID string,
	params DraftEventsApiDraftsDraftIdEventsGetParams,
) {
	if !validTurnStreamClientID(params.TurnStreamClientId) {
		writeBadRequest(writer, "invalid_turn_stream_client_id")
		return
	}
	if _, err := storage.GetDraft(request.Context(), server.database.Read(), draftID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeNotFound(writer, "draft_not_found")
		} else {
			server.internalError(writer, err)
		}
		return
	}
	server.streamEvents(writer, request, &draftID, &draftID, params.TurnStreamClientId, func(event contracts.Event) bool {
		return contracts.RoutesToDraft(event, draftID)
	})
}

func (server *Server) streamEvents(
	writer http.ResponseWriter,
	request *http.Request,
	draftID *string,
	turnDraftID *string,
	turnClientID string,
	predicate func(contracts.Event) bool,
) {
	writer.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("Connection", "keep-alive")
	writer.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(writer)
	cursor := lastEventID(request)
	poll := time.NewTicker(50 * time.Millisecond)
	heartbeat := time.NewTicker(15 * time.Second)
	defer poll.Stop()
	defer heartbeat.Stop()
	emitted := 0
	var turnSnapshot []agent.StreamEvent
	var turnStream <-chan agent.StreamEvent
	var acknowledgeTurnSnapshot func()
	var acknowledgeTurnEvent func(agent.StreamEvent)
	var unsubscribeTurn func()
	if turnDraftID != nil {
		turnSnapshot, turnStream, acknowledgeTurnSnapshot, acknowledgeTurnEvent, unsubscribeTurn =
			server.agent.Hub().SubscribeRecoverable(*turnDraftID, turnClientID)
		defer unsubscribeTurn()
	}
	turnSnapshotPending := len(turnSnapshot) > 0
	writeTurnEvent := func(event agent.StreamEvent) bool {
		frame, err := agent.EncodeTurnStreamFrame(event)
		if err != nil {
			server.logger.Error("回合 SSE 编码失败", "error", err)
			return false
		}
		if _, err := writer.Write(frame); err != nil {
			return false
		}
		if err := controller.Flush(); err != nil {
			return false
		}
		acknowledgeTurnEvent(event)
		emitted++
		return true
	}

	for {
		rows, err := storage.ListEventsAfter(request.Context(), server.database.Read(), cursor, draftID, 256)
		if err != nil {
			server.logger.Error("SSE 查询失败", "error", err)
			return
		}
		for _, row := range rows {
			cursor = row.ID
			event, err := contracts.ParseEvent(row.PayloadJSON)
			if err != nil {
				server.logger.Error("SSE 事件反序列化失败", "event_id", row.ID, "error", err)
				continue
			}
			if !predicate(event) {
				continue
			}
			frame, err := encodeSSE(row.ID, event)
			if err != nil {
				server.logger.Error("SSE 编码失败", "error", err)
				return
			}
			if _, err := io.WriteString(writer, frame); err != nil {
				return
			}
			if err := controller.Flush(); err != nil {
				return
			}
			emitted++
			if server.sseMaxEvents > 0 && emitted >= server.sseMaxEvents {
				return
			}
		}
		// 先重放持久化领域事件，再补当前回合快照。这样 Last-Event-ID 仍只属于
		// 领域事件，同时一个草稿页只需一条 SSE 连接。
		if turnSnapshotPending {
			for index, event := range turnSnapshot {
				if !writeTurnEvent(event) {
					return
				}
				if index == len(turnSnapshot)-1 {
					acknowledgeTurnSnapshot()
				}
				if server.sseMaxEvents > 0 && emitted >= server.sseMaxEvents {
					return
				}
			}
			turnSnapshotPending = false
			turnSnapshot = nil
		}

		select {
		case <-request.Context().Done():
			return
		case event, ok := <-turnStream:
			if !ok {
				// The hub closes slow subscribers after a final recovery frame.
				// End the HTTP stream so EventSource reconnects and receives a fresh snapshot.
				return
			}
			if !writeTurnEvent(event) {
				return
			}
			if server.sseMaxEvents > 0 && emitted >= server.sseMaxEvents {
				return
			}
		case <-heartbeat.C:
			if _, err := io.WriteString(writer, ": ping\n\n"); err != nil {
				return
			}
			if err := controller.Flush(); err != nil {
				return
			}
		case <-poll.C:
		}
	}
}

func validTurnStreamClientID(clientID string) bool {
	if clientID == "" || len(clientID) > 128 {
		return false
	}
	for index := range len(clientID) {
		character := clientID[index]
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '.' || character == '_' ||
			character == ':' || character == '-' {
			continue
		}
		return false
	}
	return true
}

func encodeSSE(eventID int64, event contracts.Event) (string, error) {
	data, err := json.Marshal(map[string]any{"event_id": eventID, "event": event})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("id: %d\nevent: %s\ndata: %s\n\n", eventID, event.Type, data), nil
}

func lastEventID(request *http.Request) int64 {
	raw := request.Header.Get("Last-Event-ID")
	if raw == "" {
		raw = request.URL.Query().Get("last_event_id")
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0
	}
	return value
}
