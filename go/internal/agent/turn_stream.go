package agent

import (
	"encoding/json"
	"fmt"
	"sync"
)

const DefaultSubscriberQueueLimit = 1024

const turnStreamRecoveryGenerationKey = "_recovery_generation"

const (
	DefaultTurnStreamBufferEventLimit = 4096
	DefaultTurnStreamBufferByteLimit  = 1 << 20
	DefaultTurnStreamRecoveryLimit    = 64
)

const (
	TurnStreamTurnStarted             = "turn_started"
	TurnStreamTextDelta               = "text_delta"
	TurnStreamMessageCompleted        = "message_completed"
	TurnStreamToolStepStarted         = "tool_step_started"
	TurnStreamToolStepFinished        = "tool_step_finished"
	TurnStreamModelRetry              = "model_retry"
	TurnStreamSubagentProgress        = "subagent_progress"
	TurnStreamContextCompactionFailed = "context_compaction_failed"
	TurnStreamSnapshotTruncated       = "stream_snapshot_truncated"
	TurnStreamGap                     = "stream_gap"
	TurnStreamTurnEnded               = "turn_ended"
	TurnStreamTurnError               = "turn_error"
	TurnStreamMemoryUpdated           = "memory_updated"
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
	TurnStreamSnapshotTruncated,
	TurnStreamGap,
	TurnStreamTurnEnded,
	TurnStreamTurnError,
	TurnStreamMemoryUpdated,
}

func KnownTurnStreamTypes() []string {
	return append([]string(nil), knownTurnStreamTypes...)
}

type StreamEvent map[string]any

type turnStreamBuffer struct {
	events             []StreamEvent
	bytes              int
	truncated          bool
	recoveryGeneration uint64
	pendingSubscribers map[chan StreamEvent]string
	failedClients      map[string]struct{}
}

type turnStreamSubscriber struct {
	recoverable bool
	clientID    string
}

type TurnStreamHub struct {
	mu               sync.Mutex
	queueLimit       int
	bufferEventLimit int
	bufferByteLimit  int
	recoveryLimit    int
	buffers          map[string]turnStreamBuffer
	subs             map[string]map[chan StreamEvent]turnStreamSubscriber
	activeDrafts     map[string]struct{}
	failedClients    map[string]map[string]struct{}
	nextRecoveryGen  uint64
}

func NewTurnStreamHub(queueLimit int) *TurnStreamHub {
	return newTurnStreamHub(queueLimit, DefaultTurnStreamBufferEventLimit, DefaultTurnStreamBufferByteLimit)
}

func newTurnStreamHub(queueLimit, bufferEventLimit, bufferByteLimit int) *TurnStreamHub {
	if queueLimit <= 0 {
		queueLimit = DefaultSubscriberQueueLimit
	}
	if queueLimit < 2 {
		queueLimit = 2
	}
	if bufferEventLimit <= 0 {
		bufferEventLimit = DefaultTurnStreamBufferEventLimit
	}
	if bufferByteLimit <= 0 {
		bufferByteLimit = DefaultTurnStreamBufferByteLimit
	}
	return &TurnStreamHub{
		queueLimit: queueLimit, bufferEventLimit: bufferEventLimit, bufferByteLimit: bufferByteLimit,
		recoveryLimit: DefaultTurnStreamRecoveryLimit,
		buffers:       map[string]turnStreamBuffer{},
		subs:          map[string]map[chan StreamEvent]turnStreamSubscriber{},
		activeDrafts:  map[string]struct{}{},
		failedClients: map[string]map[string]struct{}{},
	}
}

func (hub *TurnStreamHub) Subscribe(draftID string) ([]StreamEvent, <-chan StreamEvent, func()) {
	snapshot, stream, _, _, unsubscribe := hub.subscribe(draftID, turnStreamSubscriber{})
	return snapshot, stream, unsubscribe
}

// SubscribeRecoverable lets an SSE consumer acknowledge snapshots and live
// frames only after they have been flushed to the client.
func (hub *TurnStreamHub) SubscribeRecoverable(
	draftID, clientID string,
) ([]StreamEvent, <-chan StreamEvent, func(), func(StreamEvent), func()) {
	if clientID == "" {
		panic("recoverable turn-stream subscriber requires client ID")
	}
	return hub.subscribe(draftID, turnStreamSubscriber{recoverable: true, clientID: clientID})
}

func (hub *TurnStreamHub) subscribe(
	draftID string,
	subscriber turnStreamSubscriber,
) ([]StreamEvent, <-chan StreamEvent, func(), func(StreamEvent), func()) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	stream := make(chan StreamEvent, hub.queueLimit)
	if hub.subs[draftID] == nil {
		hub.subs[draftID] = map[chan StreamEvent]turnStreamSubscriber{}
	}
	hub.subs[draftID][stream] = subscriber
	buffer := hub.buffers[draftID]
	snapshot := cloneEvents(buffer.events)
	acknowledge := func() { hub.acknowledgeSnapshot(draftID, subscriber.clientID, buffer.recoveryGeneration) }
	acknowledgeEvent := func(event StreamEvent) { hub.acknowledgeEvent(draftID, stream, event) }
	unsubscribe := func() { hub.unsubscribe(draftID, stream) }
	return snapshot, stream, acknowledge, acknowledgeEvent, unsubscribe
}

func (hub *TurnStreamHub) Record(draftID string, event StreamEvent) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	frozen := cloneEvent(event)
	typeName, ok := frozen["type"].(string)
	if !ok || !knownTurnStreamType(typeName) {
		panic(fmt.Sprintf("未注册 turn stream 事件 %q", typeName))
	}
	if typeName == TurnStreamTurnStarted {
		delete(hub.buffers, draftID)
		delete(hub.failedClients, draftID)
		hub.activeDrafts[draftID] = struct{}{}
	}
	terminal := typeName == TurnStreamTurnEnded || typeName == TurnStreamTurnError
	pendingSubscribers := map[chan StreamEvent]string{}
	if terminal {
		for stream, subscriber := range hub.subs[draftID] {
			if subscriber.recoverable {
				pendingSubscribers[stream] = subscriber.clientID
			}
		}
		previouslyGapped := len(hub.failedClients[draftID]) > 0
		if previouslyGapped || len(pendingSubscribers) > 0 {
			hub.nextRecoveryGen++
			frozen[turnStreamRecoveryGenerationKey] = hub.nextRecoveryGen
		}
	}
	buffer := hub.buffers[draftID]
	buffer.events = append(buffer.events, frozen)
	buffer.bytes += encodedEventSize(frozen)
	hub.truncateBuffer(&buffer)
	hub.buffers[draftID] = buffer
	for stream, subscriber := range hub.subs[draftID] {
		select {
		case stream <- cloneEvent(frozen):
		default:
			recoveryEvents := []StreamEvent{{"type": TurnStreamGap}}
			if terminal {
				recoveryEvents = append(recoveryEvents, cloneEvent(frozen))
			}
			// The failed send proves the bounded queue is full. Evict enough stale
			// frames to guarantee the gap, and the terminal frame when applicable,
			// before closing the channel and forcing the SSE connection to restart.
			for range recoveryEvents {
				select {
				case <-stream:
				default:
				}
			}
			for _, recoveryEvent := range recoveryEvents {
				stream <- recoveryEvent
			}
			if subscriber.recoverable {
				hub.addFailedClient(draftID, subscriber.clientID)
			}
			delete(pendingSubscribers, stream)
			delete(hub.subs[draftID], stream)
			close(stream)
		}
	}
	if len(hub.subs[draftID]) == 0 {
		delete(hub.subs, draftID)
	}
	if terminal {
		delete(hub.activeDrafts, draftID)
		generation, _ := frozen[turnStreamRecoveryGenerationKey].(uint64)
		if generation != 0 {
			hub.buffers[draftID] = hub.newRecoveryBuffer(
				frozen, generation, pendingSubscribers, cloneClientSet(hub.failedClients[draftID]),
			)
			hub.enforceRecoveryLimit()
		} else {
			delete(hub.buffers, draftID)
		}
	}
}

func (hub *TurnStreamHub) enforceRecoveryLimit() {
	for {
		recoveryCount := 0
		oldestDraftID := ""
		oldestGeneration := ^uint64(0)
		for draftID, buffer := range hub.buffers {
			if buffer.recoveryGeneration == 0 || len(buffer.pendingSubscribers) > 0 {
				continue
			}
			recoveryCount++
			if buffer.recoveryGeneration < oldestGeneration {
				oldestDraftID = draftID
				oldestGeneration = buffer.recoveryGeneration
			}
		}
		if recoveryCount <= hub.recoveryLimit {
			return
		}
		delete(hub.buffers, oldestDraftID)
		delete(hub.failedClients, oldestDraftID)
	}
}

func (hub *TurnStreamHub) newRecoveryBuffer(
	terminal StreamEvent,
	generation uint64,
	pendingSubscribers map[chan StreamEvent]string,
	failedClients map[string]struct{},
) turnStreamBuffer {
	gap := StreamEvent{"type": TurnStreamGap}
	buffer := turnStreamBuffer{
		events:             []StreamEvent{gap, terminal},
		bytes:              encodedEventSize(gap) + encodedEventSize(terminal),
		truncated:          true,
		recoveryGeneration: generation,
		pendingSubscribers: pendingSubscribers,
		failedClients:      failedClients,
	}
	if !hub.bufferOverLimit(buffer) {
		return buffer
	}
	marker := StreamEvent{"type": TurnStreamSnapshotTruncated}
	compactTerminal := StreamEvent{"type": terminal["type"]}
	buffer.events = []StreamEvent{marker, compactTerminal}
	buffer.bytes = encodedEventSize(marker) + encodedEventSize(compactTerminal)
	if !hub.bufferOverLimit(buffer) {
		return buffer
	}
	// Even with an unusually tiny configured limit, prefer the terminal frame
	// over optional recovery metadata so the client cannot remain stuck running.
	buffer.events = []StreamEvent{compactTerminal}
	buffer.bytes = encodedEventSize(compactTerminal)
	return buffer
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
	return cloneEvents(hub.buffers[draftID].events)
}

func (hub *TurnStreamHub) truncateBuffer(buffer *turnStreamBuffer) {
	dropped := false
	for hub.bufferOverLimit(*buffer) {
		index := oldestTextDelta(buffer.events)
		if index < 0 {
			hub.replaceBufferWithTruncationMarker(buffer)
			return
		}
		buffer.bytes -= encodedEventSize(buffer.events[index])
		buffer.events = removeStreamEvent(buffer.events, index)
		dropped = true
	}
	if !dropped || buffer.truncated {
		return
	}
	marker := StreamEvent{"type": TurnStreamSnapshotTruncated}
	buffer.events = append([]StreamEvent{marker}, buffer.events...)
	buffer.bytes += encodedEventSize(marker)
	buffer.truncated = true
	for hub.bufferOverLimit(*buffer) {
		index := oldestTextDelta(buffer.events)
		if index < 0 {
			hub.replaceBufferWithTruncationMarker(buffer)
			return
		}
		buffer.bytes -= encodedEventSize(buffer.events[index])
		buffer.events = removeStreamEvent(buffer.events, index)
	}
}

func (hub *TurnStreamHub) bufferOverLimit(buffer turnStreamBuffer) bool {
	return len(buffer.events) > hub.bufferEventLimit || buffer.bytes > hub.bufferByteLimit
}

func (hub *TurnStreamHub) replaceBufferWithTruncationMarker(buffer *turnStreamBuffer) {
	marker := StreamEvent{"type": TurnStreamSnapshotTruncated}
	buffer.events = []StreamEvent{marker}
	buffer.bytes = encodedEventSize(marker)
	buffer.truncated = true
}

func oldestTextDelta(events []StreamEvent) int {
	for index, event := range events {
		if event["type"] == TurnStreamTextDelta {
			return index
		}
	}
	return -1
}

func removeStreamEvent(events []StreamEvent, index int) []StreamEvent {
	copy(events[index:], events[index+1:])
	events[len(events)-1] = nil
	return events[:len(events)-1]
}

func encodedEventSize(event StreamEvent) int {
	payload, err := json.Marshal(publicStreamEvent(event))
	if err != nil {
		// Keep the existing boundary: the SSE writer owns payload encoding
		// failures. Invalid events still count toward the event-count limit.
		return 0
	}
	return len(payload)
}

func (hub *TurnStreamHub) addFailedClient(draftID, clientID string) {
	if clientID == "" {
		return
	}
	if hub.failedClients[draftID] == nil {
		hub.failedClients[draftID] = map[string]struct{}{}
	}
	hub.failedClients[draftID][clientID] = struct{}{}
}

func (hub *TurnStreamHub) clearFailedClient(draftID, clientID string) {
	delete(hub.failedClients[draftID], clientID)
	if len(hub.failedClients[draftID]) == 0 {
		delete(hub.failedClients, draftID)
	}
}

func (hub *TurnStreamHub) unsubscribe(draftID string, stream chan StreamEvent) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	subscribers := hub.subs[draftID]
	if subscribers == nil {
		return
	}
	subscriber, exists := subscribers[stream]
	if exists {
		if _, active := hub.activeDrafts[draftID]; active && subscriber.recoverable {
			hub.addFailedClient(draftID, subscriber.clientID)
		}
		delete(subscribers, stream)
		close(stream)
	}
	buffer := hub.buffers[draftID]
	if clientID, pending := buffer.pendingSubscribers[stream]; pending {
		delete(buffer.pendingSubscribers, stream)
		if buffer.failedClients == nil {
			buffer.failedClients = map[string]struct{}{}
		}
		buffer.failedClients[clientID] = struct{}{}
		hub.addFailedClient(draftID, clientID)
		hub.buffers[draftID] = buffer
		if len(buffer.pendingSubscribers) == 0 {
			hub.enforceRecoveryLimit()
		}
	}
	if len(subscribers) == 0 {
		delete(hub.subs, draftID)
	}
}

func (hub *TurnStreamHub) acknowledgeSnapshot(draftID, clientID string, generation uint64) {
	if generation == 0 {
		return
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	buffer := hub.buffers[draftID]
	if buffer.recoveryGeneration != generation {
		return
	}
	delete(buffer.failedClients, clientID)
	hub.clearFailedClient(draftID, clientID)
	if len(buffer.pendingSubscribers) == 0 && len(buffer.failedClients) == 0 {
		delete(hub.buffers, draftID)
		delete(hub.failedClients, draftID)
		return
	}
	hub.buffers[draftID] = buffer
	if len(buffer.pendingSubscribers) == 0 {
		hub.enforceRecoveryLimit()
	}
}

func (hub *TurnStreamHub) acknowledgeEvent(draftID string, stream chan StreamEvent, event StreamEvent) {
	generation, _ := event[turnStreamRecoveryGenerationKey].(uint64)
	if generation == 0 {
		return
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	buffer := hub.buffers[draftID]
	if buffer.recoveryGeneration != generation {
		return
	}
	clientID, pending := buffer.pendingSubscribers[stream]
	if !pending {
		return
	}
	delete(buffer.pendingSubscribers, stream)
	delete(buffer.failedClients, clientID)
	hub.clearFailedClient(draftID, clientID)
	if len(buffer.pendingSubscribers) == 0 && len(buffer.failedClients) == 0 {
		delete(hub.buffers, draftID)
		delete(hub.failedClients, draftID)
		return
	}
	hub.buffers[draftID] = buffer
	if len(buffer.pendingSubscribers) == 0 {
		hub.enforceRecoveryLimit()
	}
}

func cloneClientSet(source map[string]struct{}) map[string]struct{} {
	result := make(map[string]struct{}, len(source))
	for clientID := range source {
		result[clientID] = struct{}{}
	}
	return result
}

func EncodeTurnStreamFrame(event StreamEvent) ([]byte, error) {
	payload, err := json.Marshal(publicStreamEvent(event))
	if err != nil {
		return nil, err
	}
	return []byte(fmt.Sprintf("event: turn_stream\ndata: %s\n\n", payload)), nil
}

func publicStreamEvent(event StreamEvent) StreamEvent {
	result := cloneEvent(event)
	delete(result, turnStreamRecoveryGenerationKey)
	return result
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
