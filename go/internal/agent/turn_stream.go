package agent

import (
	"encoding/json"
	"fmt"
	"sync"
)

const DefaultSubscriberQueueLimit = 1024

const (
	TurnStreamTurnStarted             = "turn_started"
	TurnStreamTextDelta               = "text_delta"
	TurnStreamMessageCompleted        = "message_completed"
	TurnStreamToolStepStarted         = "tool_step_started"
	TurnStreamToolStepFinished        = "tool_step_finished"
	TurnStreamModelRetry              = "model_retry"
	TurnStreamSubagentProgress        = "subagent_progress"
	TurnStreamContextCompactionFailed = "context_compaction_failed"
	TurnStreamTurnEnded               = "turn_ended"
	TurnStreamTurnError               = "turn_error"
)

var knownTurnStreamTypes = []string{
	TurnStreamTurnStarted,
	TurnStreamTextDelta,
	TurnStreamMessageCompleted,
	TurnStreamToolStepStarted,
	TurnStreamToolStepFinished,
	TurnStreamModelRetry,
	TurnStreamSubagentProgress,
	TurnStreamContextCompactionFailed,
	TurnStreamTurnEnded,
	TurnStreamTurnError,
}

func KnownTurnStreamTypes() []string {
	return append([]string(nil), knownTurnStreamTypes...)
}

type StreamEvent map[string]any

type TurnStreamHub struct {
	mu         sync.Mutex
	queueLimit int
	buffers    map[string][]StreamEvent
	subs       map[string]map[chan StreamEvent]struct{}
}

func NewTurnStreamHub(queueLimit int) *TurnStreamHub {
	if queueLimit <= 0 {
		queueLimit = DefaultSubscriberQueueLimit
	}
	return &TurnStreamHub{
		queueLimit: queueLimit, buffers: map[string][]StreamEvent{},
		subs: map[string]map[chan StreamEvent]struct{}{},
	}
}

func (hub *TurnStreamHub) Subscribe(draftID string) ([]StreamEvent, <-chan StreamEvent, func()) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	stream := make(chan StreamEvent, hub.queueLimit)
	if hub.subs[draftID] == nil {
		hub.subs[draftID] = map[chan StreamEvent]struct{}{}
	}
	hub.subs[draftID][stream] = struct{}{}
	snapshot := cloneEvents(hub.buffers[draftID])
	unsubscribe := func() { hub.unsubscribe(draftID, stream) }
	return snapshot, stream, unsubscribe
}

func (hub *TurnStreamHub) Record(draftID string, event StreamEvent) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	frozen := cloneEvent(event)
	typeName, ok := frozen["type"].(string)
	if !ok || !knownTurnStreamType(typeName) {
		panic(fmt.Sprintf("未注册 turn stream 事件 %q", typeName))
	}
	hub.buffers[draftID] = append(hub.buffers[draftID], frozen)
	for subscriber := range hub.subs[draftID] {
		select {
		case subscriber <- cloneEvent(frozen):
		default:
			delete(hub.subs[draftID], subscriber)
			close(subscriber)
		}
	}
	if typeName == TurnStreamTurnEnded || typeName == TurnStreamTurnError {
		delete(hub.buffers, draftID)
	}
}

func knownTurnStreamType(typeName string) bool {
	for _, known := range knownTurnStreamTypes {
		if typeName == known {
			return true
		}
	}
	return false
}

func (hub *TurnStreamHub) Snapshot(draftID string) []StreamEvent {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	return cloneEvents(hub.buffers[draftID])
}

func (hub *TurnStreamHub) unsubscribe(draftID string, stream chan StreamEvent) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	subscribers := hub.subs[draftID]
	if subscribers == nil {
		return
	}
	if _, exists := subscribers[stream]; exists {
		delete(subscribers, stream)
		close(stream)
	}
	if len(subscribers) == 0 {
		delete(hub.subs, draftID)
	}
}

func EncodeTurnStreamFrame(event StreamEvent) ([]byte, error) {
	payload, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	return []byte(fmt.Sprintf("event: turn_stream\ndata: %s\n\n", payload)), nil
}

func cloneEvents(events []StreamEvent) []StreamEvent {
	result := make([]StreamEvent, 0, len(events))
	for _, event := range events {
		result = append(result, cloneEvent(event))
	}
	return result
}

func cloneEvent(event StreamEvent) StreamEvent {
	result := make(StreamEvent, len(event))
	for key, value := range event {
		result[key] = value
	}
	return result
}
