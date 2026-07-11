package agent

import (
	"encoding/json"
	"fmt"
	"sync"
)

const DefaultSubscriberQueueLimit = 1024

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
	hub.buffers[draftID] = append(hub.buffers[draftID], frozen)
	for subscriber := range hub.subs[draftID] {
		select {
		case subscriber <- cloneEvent(frozen):
		default:
			delete(hub.subs[draftID], subscriber)
			close(subscriber)
		}
	}
	typeName, _ := frozen["type"].(string)
	if typeName == "turn_ended" || typeName == "turn_error" {
		delete(hub.buffers, draftID)
	}
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
