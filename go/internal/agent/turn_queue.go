package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var errTurnCancelledByUser = fmt.Errorf("用户取消 turn: %w", context.Canceled)

const cancelAndJoinDraftTimeout = 500 * time.Millisecond

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
	onConsumed func(error)
}

type TurnRunner func(context.Context, QueueItem) error

type TurnQueue struct {
	ctx          context.Context
	cancel       context.CancelFunc
	runner       TurnRunner
	mu           sync.Mutex
	workers      map[string]*draftWorker
	cancelLeases map[string]int
	closed       bool
	closeDone    chan struct{}
}

type draftWorker struct {
	queue        chan QueueItem
	mu           sync.Mutex
	currentStop  context.CancelCauseFunc
	canceling    bool
	retired      bool
	retire       chan struct{}
	pending      sync.WaitGroup
	producers    sync.WaitGroup
	pendingCount int
	drained      chan struct{}
}

// DraftCancellationBarrier keeps later turns out of a draft until every turn
// accepted before the cancellation and its follow-up cleanup have finished.
type DraftCancellationBarrier struct {
	queue   *TurnQueue
	draftID string
	worker  *draftWorker
	done    <-chan struct{}
	release sync.Once
}

// Wait reports whether all turns accepted before the barrier finished before
// ctx was cancelled.
func (barrier *DraftCancellationBarrier) Wait(ctx context.Context) bool {
	if barrier == nil {
		return true
	}
	select {
	case <-barrier.done:
		return true
	case <-ctx.Done():
		return false
	}
}

// WaitForDrainOrQueueClose is used by background cleanup after a bounded
// request timeout. It avoids leaking a waiter if the service shuts down while
// a provider keeps ignoring cancellation.
func (barrier *DraftCancellationBarrier) WaitForDrainOrQueueClose() bool {
	if barrier == nil {
		return true
	}
	select {
	case <-barrier.done:
		return true
	case <-barrier.queue.ctx.Done():
		return false
	}
}

// Release reopens the draft for new turns. Callers must keep the barrier until
// any state produced by the cancelled turns has been cleaned up.
func (barrier *DraftCancellationBarrier) Release() {
	if barrier == nil {
		return
	}
	barrier.release.Do(func() {
		barrier.queue.releaseDraftCancellation(barrier, false)
	})
}

// Abandon seals a worker whose runner did not stop before the cancellation
// deadline. Later turns use a fresh worker and cannot queue behind the stuck
// execution generation.
func (barrier *DraftCancellationBarrier) Abandon() {
	if barrier == nil {
		return
	}
	barrier.release.Do(func() {
		barrier.queue.releaseDraftCancellation(barrier, true)
	})
}

func NewTurnQueue(parent context.Context, runner TurnRunner) *TurnQueue {
	ctx, cancel := context.WithCancel(parent)
	return &TurnQueue{
		ctx: ctx, cancel: cancel, runner: runner,
		workers: map[string]*draftWorker{}, cancelLeases: map[string]int{},
		closeDone: make(chan struct{}),
	}
}

func newDraftWorker() *draftWorker {
	drained := make(chan struct{})
	close(drained)
	return &draftWorker{
		queue: make(chan QueueItem, 256), retire: make(chan struct{}), drained: drained,
	}
}

func (queue *TurnQueue) Enqueue(item QueueItem) bool {
	if item.DraftID == "" {
		return false
	}
	if item.EnqueuedAt.IsZero() {
		item.EnqueuedAt = time.Now().UTC()
	}
	queue.mu.Lock()
	if queue.closed || queue.ctx.Err() != nil || queue.cancelLeases[item.DraftID] > 0 {
		queue.mu.Unlock()
		return false
	}
	worker := queue.workers[item.DraftID]
	if worker == nil {
		worker = newDraftWorker()
		queue.workers[item.DraftID] = worker
		go queue.runWorker(worker)
	}
	worker.mu.Lock()
	if worker.canceling || worker.retired {
		worker.mu.Unlock()
		queue.mu.Unlock()
		return false
	}
	if worker.retire == nil {
		worker.retire = make(chan struct{})
	}
	if worker.pendingCount == 0 {
		worker.drained = make(chan struct{})
	}
	worker.pending.Add(1)
	worker.producers.Add(1)
	worker.pendingCount++
	worker.mu.Unlock()
	queue.mu.Unlock()
	select {
	case worker.queue <- item:
		worker.producers.Done()
		return true
	case <-worker.retire:
		worker.producers.Done()
		if item.onConsumed != nil {
			item.onConsumed(errTurnCancelledByUser)
		}
		queue.finishPending(worker)
		return false
	case <-queue.ctx.Done():
		worker.producers.Done()
		if item.onConsumed != nil {
			item.onConsumed(context.Canceled)
		}
		queue.finishPending(worker)
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
	worker.currentStop(errTurnCancelledByUser)
	return true
}

func (queue *TurnQueue) IsBusy(draftID string) bool {
	queue.mu.Lock()
	worker := queue.workers[draftID]
	queue.mu.Unlock()
	if worker == nil {
		return false
	}
	worker.mu.Lock()
	active := worker.currentStop != nil
	worker.mu.Unlock()
	return active || len(worker.queue) > 0
}

func (queue *TurnQueue) JoinDraft(draftID string) {
	queue.mu.Lock()
	worker := queue.workers[draftID]
	queue.mu.Unlock()
	if worker != nil {
		worker.pending.Wait()
	}
}

// BeginDraftCancellation closes the enqueue window and cancels every item
// accepted before the barrier. The caller owns the returned barrier and must
// release it after all state produced by those items has been cleaned up.
func (queue *TurnQueue) BeginDraftCancellation(draftID string) (*DraftCancellationBarrier, bool) {
	if draftID == "" {
		return nil, false
	}
	queue.mu.Lock()
	if queue.closed || queue.ctx.Err() != nil {
		queue.mu.Unlock()
		return nil, false
	}
	worker := queue.workers[draftID]
	queue.cancelLeases[draftID]++
	requested := false
	done := closedChannel()
	if worker != nil {
		worker.mu.Lock()
		requested = worker.pendingCount > 0
		worker.canceling = true
		done = worker.drained
		if worker.currentStop != nil {
			worker.currentStop(errTurnCancelledByUser)
		}
		worker.mu.Unlock()
	}
	queue.mu.Unlock()
	return &DraftCancellationBarrier{
		queue: queue, draftID: draftID, worker: worker, done: done,
	}, requested
}

func closedChannel() <-chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}

func (queue *TurnQueue) releaseDraftCancellation(barrier *DraftCancellationBarrier, abandon bool) {
	queue.mu.Lock()
	worker := barrier.worker
	retired := false
	if abandon && worker != nil && queue.workers[barrier.draftID] == worker {
		delete(queue.workers, barrier.draftID)
		queue.sealWorker(worker)
		retired = true
	}
	queue.mu.Unlock()
	if retired {
		worker.producers.Wait()
		queue.drainRetiredWorker(worker)
	}

	queue.mu.Lock()
	leases := queue.cancelLeases[barrier.draftID] - 1
	if leases <= 0 {
		delete(queue.cancelLeases, barrier.draftID)
		if !abandon && worker != nil && queue.workers[barrier.draftID] == worker {
			worker.mu.Lock()
			worker.canceling = false
			worker.mu.Unlock()
		}
	} else {
		queue.cancelLeases[barrier.draftID] = leases
	}
	queue.mu.Unlock()
}

func (queue *TurnQueue) sealWorker(worker *draftWorker) {
	worker.mu.Lock()
	if !worker.retired {
		worker.retired = true
		if worker.retire == nil {
			worker.retire = make(chan struct{})
		}
		close(worker.retire)
	}
	worker.mu.Unlock()
}

func (queue *TurnQueue) drainRetiredWorker(worker *draftWorker) {
	for {
		select {
		case item := <-worker.queue:
			if item.onConsumed == nil {
				queue.finishPending(worker)
				continue
			}
			go func() {
				item.onConsumed(errTurnCancelledByUser)
				queue.finishPending(worker)
			}()
		default:
			return
		}
	}
}

// CancelAndJoinDraft is the synchronous helper used outside the HTTP
// cancellation path. The API keeps the barrier through job cleanup instead.
func (queue *TurnQueue) CancelAndJoinDraft(draftID string) bool {
	barrier, requested := queue.BeginDraftCancellation(draftID)
	if barrier == nil {
		return false
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), cancelAndJoinDraftTimeout)
	finished := barrier.Wait(waitCtx)
	cancel()
	if finished {
		barrier.Release()
	} else {
		barrier.Abandon()
	}
	return requested
}

func (queue *TurnQueue) Close() {
	queue.mu.Lock()
	if queue.closed {
		done := queue.closeDone
		queue.mu.Unlock()
		queue.waitForClose(done)
		return
	}
	queue.closed = true
	queue.cancel()
	workers := make([]*draftWorker, 0, len(queue.workers))
	for _, worker := range queue.workers {
		workers = append(workers, worker)
	}
	queue.workers = map[string]*draftWorker{}
	queue.cancelLeases = map[string]int{}
	queue.mu.Unlock()
	for _, worker := range workers {
		queue.sealWorker(worker)
	}
	go func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cancelAndJoinDraftTimeout)
		defer cancel()
		defer close(queue.closeDone)
		for _, worker := range workers {
			producersDone := make(chan struct{})
			go func() {
				worker.producers.Wait()
				close(producersDone)
			}()
			select {
			case <-producersDone:
			case <-shutdownCtx.Done():
				return
			}
			queue.drainRetiredWorker(worker)
		}
		for _, worker := range workers {
			select {
			case <-worker.drained:
			case <-shutdownCtx.Done():
				return
			}
		}
	}()
	queue.waitForClose(queue.closeDone)
}

func (queue *TurnQueue) waitForClose(done <-chan struct{}) {
	<-done
}

func (queue *TurnQueue) runWorker(worker *draftWorker) {
	for {
		select {
		case <-queue.ctx.Done():
			for {
				select {
				case item := <-worker.queue:
					if item.onConsumed != nil {
						item.onConsumed(context.Canceled)
					}
					queue.finishPending(worker)
				default:
					return
				}
			}
		case <-worker.retire:
			return
		case item := <-worker.queue:
			turnCtx, stop := context.WithCancelCause(queue.ctx)
			worker.mu.Lock()
			retired := worker.retired
			worker.currentStop = stop
			if worker.canceling || retired {
				stop(errTurnCancelledByUser)
			}
			worker.mu.Unlock()
			var runErr error
			if queue.runner != nil && !retired {
				runErr = queue.runner(turnCtx, item)
			}
			if cause := context.Cause(turnCtx); errors.Is(cause, errTurnCancelledByUser) {
				runErr = cause
			}
			if item.onConsumed != nil {
				item.onConsumed(runErr)
			}
			stop(nil)
			worker.mu.Lock()
			worker.currentStop = nil
			worker.mu.Unlock()
			queue.finishPending(worker)
		}
	}
}

func (queue *TurnQueue) finishPending(worker *draftWorker) {
	worker.mu.Lock()
	worker.pendingCount--
	if worker.pendingCount == 0 {
		close(worker.drained)
	}
	worker.mu.Unlock()
	worker.pending.Done()
}
