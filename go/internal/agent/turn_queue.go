package agent

import (
	"context"
	"sync"
	"time"
)

type QueueItemKind string

const (
	QueueUserMessage    QueueItemKind = "user_message"
	QueueJobObservation QueueItemKind = "job_observation"
	QueueUIObservation  QueueItemKind = "ui_observation"
)

type QueueItem struct {
	DraftID    string
	Kind       QueueItemKind
	ItemID     string
	Payload    map[string]any
	EnqueuedAt time.Time
}

type TurnRunner func(context.Context, QueueItem) error

type TurnQueue struct {
	ctx     context.Context
	cancel  context.CancelFunc
	runner  TurnRunner
	mu      sync.Mutex
	workers map[string]*draftWorker
}

type draftWorker struct {
	queue       chan QueueItem
	mu          sync.Mutex
	currentStop context.CancelFunc
	pending     sync.WaitGroup
}

func NewTurnQueue(parent context.Context, runner TurnRunner) *TurnQueue {
	ctx, cancel := context.WithCancel(parent)
	return &TurnQueue{ctx: ctx, cancel: cancel, runner: runner, workers: map[string]*draftWorker{}}
}

func (queue *TurnQueue) Enqueue(item QueueItem) bool {
	if item.DraftID == "" {
		return false
	}
	if queue.ctx.Err() != nil {
		return false
	}
	if item.EnqueuedAt.IsZero() {
		item.EnqueuedAt = time.Now().UTC()
	}
	queue.mu.Lock()
	worker := queue.workers[item.DraftID]
	if worker == nil {
		worker = &draftWorker{queue: make(chan QueueItem, 256)}
		queue.workers[item.DraftID] = worker
		go queue.runWorker(worker)
	}
	worker.pending.Add(1)
	queue.mu.Unlock()
	select {
	case worker.queue <- item:
		return true
	case <-queue.ctx.Done():
		worker.pending.Done()
		return false
	}
}

func (queue *TurnQueue) EnqueueUserMessage(draftID, messageID, content string) bool {
	return queue.Enqueue(QueueItem{
		DraftID: draftID, Kind: QueueUserMessage, ItemID: messageID,
		Payload: map[string]any{"message_id": messageID, "content": content},
	})
}

func (queue *TurnQueue) EnqueueJobObservation(draftID, jobID string, event map[string]any) bool {
	return queue.Enqueue(QueueItem{
		DraftID: draftID, Kind: QueueJobObservation, ItemID: jobID,
		Payload: map[string]any{"job_id": jobID, "event": event},
	})
}

func (queue *TurnQueue) EnqueueUIObservation(draftID, itemID, observationType string, payload map[string]any) bool {
	values := map[string]any{"observation_type": observationType}
	for key, value := range payload {
		values[key] = value
	}
	return queue.Enqueue(QueueItem{
		DraftID: draftID, Kind: QueueUIObservation, ItemID: itemID, Payload: values,
	})
}

func (queue *TurnQueue) RequestStop(draftID string) bool {
	queue.mu.Lock()
	worker := queue.workers[draftID]
	queue.mu.Unlock()
	if worker == nil {
		return false
	}
	worker.mu.Lock()
	defer worker.mu.Unlock()
	if worker.currentStop == nil {
		return false
	}
	worker.currentStop()
	return true
}

func (queue *TurnQueue) JoinDraft(draftID string) {
	queue.mu.Lock()
	worker := queue.workers[draftID]
	queue.mu.Unlock()
	if worker != nil {
		worker.pending.Wait()
	}
}

func (queue *TurnQueue) Close() {
	queue.cancel()
	queue.mu.Lock()
	workers := make([]*draftWorker, 0, len(queue.workers))
	for _, worker := range queue.workers {
		workers = append(workers, worker)
	}
	queue.mu.Unlock()
	for _, worker := range workers {
		worker.pending.Wait()
	}
}

func (queue *TurnQueue) runWorker(worker *draftWorker) {
	for {
		select {
		case <-queue.ctx.Done():
			for {
				select {
				case <-worker.queue:
					worker.pending.Done()
				default:
					return
				}
			}
		case item := <-worker.queue:
			turnCtx, stop := context.WithCancel(queue.ctx)
			worker.mu.Lock()
			worker.currentStop = stop
			worker.mu.Unlock()
			if queue.runner != nil {
				_ = queue.runner(turnCtx, item)
			}
			stop()
			worker.mu.Lock()
			worker.currentStop = nil
			worker.mu.Unlock()
			worker.pending.Done()
		}
	}
}
