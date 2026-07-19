package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTurnQueueFIFOParallelDraftsAndCancel(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	order := []string{}
	started := make(chan string, 4)
	releaseA := make(chan struct{})
	queue := NewTurnQueue(t.Context(), func(ctx context.Context, item QueueItem) error {
		started <- item.DraftID + ":" + item.ItemID
		if item.DraftID == "a" && item.ItemID == "1" {
			select {
			case <-releaseA:
			case <-ctx.Done():
			}
		}
		mu.Lock()
		order = append(order, item.DraftID+":"+item.ItemID)
		mu.Unlock()
		return ctx.Err()
	})
	t.Cleanup(queue.Close)
	if !queue.Enqueue(QueueItem{DraftID: "a", ItemID: "1"}) ||
		!queue.Enqueue(QueueItem{DraftID: "a", ItemID: "2"}) ||
		!queue.Enqueue(QueueItem{DraftID: "b", ItemID: "1"}) {
		t.Fatal("enqueue 失败")
	}
	first := <-started
	second := <-started
	if first != "a:1" && second != "a:1" {
		t.Fatalf("a:1 未启动: %s %s", first, second)
	}
	if first != "b:1" && second != "b:1" {
		t.Fatalf("b 草稿未并行启动: %s %s", first, second)
	}
	if !queue.RequestStop("a") {
		t.Fatal("活跃 turn 应可取消")
	}
	if !queue.IsBusy("a") {
		t.Fatal("尚有排队 turn 时应报告 busy")
	}
	queue.JoinDraft("a")
	queue.JoinDraft("b")
	mu.Lock()
	joined := strings.Join(order, ",")
	mu.Unlock()
	if !strings.Contains(joined, "a:1,a:2") {
		t.Fatalf("草稿内非 FIFO: %s", joined)
	}
	if queue.RequestStop("missing") {
		t.Fatal("空闲草稿不应报告取消成功")
	}
	if queue.IsBusy("a") || queue.IsBusy("missing") {
		t.Fatal("消费完成或不存在的草稿不应报告 busy")
	}
}

func TestTurnQueueHelpersCloseAndRejectedItems(t *testing.T) {
	items := make(chan QueueItem, 3)
	queue := NewTurnQueue(t.Context(), func(_ context.Context, item QueueItem) error {
		items <- item
		return nil
	})
	if queue.Enqueue(QueueItem{}) {
		t.Fatal("空 draft_id 不应入队")
	}
	if !queue.EnqueueUserMessage("draft", "message", "hello") ||
		!queue.EnqueueJobObservation("draft", "job", map[string]any{"status": "done"}) ||
		!queue.EnqueueUIObservation("draft", "ui", "preview_viewed", map[string]any{"preview_id": "p1"}) {
		t.Fatal("三类 observation 应入队")
	}
	queue.JoinDraft("draft")
	queue.JoinDraft("missing")
	close(items)
	seen := map[QueueItemKind]QueueItem{}
	for item := range items {
		seen[item.Kind] = item
	}
	if len(seen) != 3 || seen[QueueUserMessage].Payload["content"] != "hello" ||
		seen[QueueJobObservation].Payload["job_id"] != "job" ||
		seen[QueueUIObservation].Payload["observation_type"] != "preview_viewed" {
		t.Fatalf("items=%#v", seen)
	}
	if queue.RequestStop("draft") {
		t.Fatal("空闲 worker 不应报告取消成功")
	}
	queue.Close()
	if queue.EnqueueUserMessage("draft", "after_close", "no") {
		t.Fatal("关闭后的队列不应接受新消息")
	}

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	cancelledQueue := NewTurnQueue(cancelled, nil)
	if cancelledQueue.EnqueueUserMessage("draft", "cancelled", "no") {
		t.Fatal("父上下文已取消时不应入队")
	}
	cancelledQueue.Close()

	nilRunner := NewTurnQueue(t.Context(), nil)
	if !nilRunner.EnqueueUserMessage("draft", "nil_runner", "ok") {
		t.Fatal("nil runner 队列仍应消费消息")
	}
	nilRunner.JoinDraft("draft")
	nilRunner.Close()

	var nilBarrier *DraftCancellationBarrier
	if !nilBarrier.Wait(t.Context()) {
		t.Fatal("nil barrier 应视为已完成")
	}
	if !nilBarrier.WaitForDrainOrQueueClose() {
		t.Fatal("nil barrier 后台等待应视为已完成")
	}
	nilBarrier.Release()
	nilBarrier.Abandon()
}

func TestTurnQueueCloseLinearizesBeforeLaterEnqueue(t *testing.T) {
	runs := make(chan QueueItem, 1)
	queue := NewTurnQueue(t.Context(), func(_ context.Context, item QueueItem) error {
		runs <- item
		return nil
	})
	queue.Close()

	const attempts = 32
	results := make(chan bool, attempts)
	for index := range attempts {
		go func() {
			results <- queue.EnqueueUserMessage("draft", fmt.Sprintf("late_%d", index), "no")
		}()
	}
	for range attempts {
		if <-results {
			t.Fatal("Close 返回后不得接受新 turn")
		}
	}
	select {
	case item := <-runs:
		t.Fatalf("Close 返回后 runner 不应启动: %#v", item)
	default:
	}
	queue.mu.Lock()
	workerCount := len(queue.workers)
	leaseCount := len(queue.cancelLeases)
	queue.mu.Unlock()
	if workerCount != 0 || leaseCount != 0 {
		t.Fatalf("Close 后遗留队列状态: workers=%d leases=%d", workerCount, leaseCount)
	}
	if barrier, requested := queue.BeginDraftCancellation("draft"); barrier != nil || requested {
		t.Fatalf("Close 后不得创建取消屏障: barrier=%v requested=%v", barrier, requested)
	}
}

func TestTurnQueueCanEnqueueMatchesClosedAndCancellationFences(t *testing.T) {
	t.Parallel()
	queue := NewTurnQueue(t.Context(), nil)
	if queue.CanEnqueue("") || !queue.CanEnqueue("draft") {
		t.Fatal("empty draft must fail and an open draft must pass preflight")
	}
	barrier, _ := queue.BeginDraftCancellation("draft")
	if barrier == nil || queue.CanEnqueue("draft") {
		t.Fatal("draft cancellation lease must close the enqueue preflight")
	}
	barrier.Release()
	if !queue.CanEnqueue("draft") {
		t.Fatal("released draft cancellation lease must reopen preflight")
	}
	queue.Close()
	if queue.CanEnqueue("draft") {
		t.Fatal("closed queue must reject preflight")
	}
}

func TestDraftCancellationAbandonDrainsProducerBlockedBeforeSend(t *testing.T) {
	queue := NewTurnQueue(t.Context(), nil)
	worker := newDraftWorker()
	queue.workers["draft"] = worker
	consumed := make(chan string, cap(worker.queue)+1)

	for index := range cap(worker.queue) {
		itemID := fmt.Sprintf("queued_%d", index)
		if !queue.Enqueue(QueueItem{
			DraftID: "draft", ItemID: itemID,
			onConsumed: func(error) { consumed <- itemID },
		}) {
			t.Fatalf("填充队列失败: index=%d", index)
		}
	}
	blockedResult := make(chan bool, 1)
	go func() {
		blockedResult <- queue.Enqueue(QueueItem{
			DraftID: "draft", ItemID: "blocked_producer",
			onConsumed: func(error) { consumed <- "blocked_producer" },
		})
	}()
	deadline := time.Now().Add(time.Second)
	for {
		worker.mu.Lock()
		pending := worker.pendingCount
		worker.mu.Unlock()
		if pending == cap(worker.queue)+1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("producer 未进入 send 前窗口: pending=%d", pending)
		}
		time.Sleep(time.Millisecond)
	}

	barrier, requested := queue.BeginDraftCancellation("draft")
	if barrier == nil || !requested {
		t.Fatalf("barrier=%v requested=%v", barrier, requested)
	}
	barrier.Abandon()
	if <-blockedResult {
		t.Fatal("封存旧 worker 后阻塞的 producer 不应入队成功")
	}
	if !barrier.Wait(t.Context()) {
		t.Fatal("封存应清理全部已接受任务")
	}
	for range cap(worker.queue) + 1 {
		<-consumed
	}
	worker.mu.Lock()
	pending := worker.pendingCount
	worker.mu.Unlock()
	queue.mu.Lock()
	workerCount := len(queue.workers)
	leaseCount := len(queue.cancelLeases)
	queue.mu.Unlock()
	if pending != 0 || len(worker.queue) != 0 || workerCount != 0 || leaseCount != 0 {
		t.Fatalf("封存清理不完整: pending=%d queued=%d workers=%d leases=%d",
			pending, len(worker.queue), workerCount, leaseCount)
	}
	queue.Close()
}

func TestCancelAndJoinDraftCancelsAcceptedItemBeforeWorkerStarts(t *testing.T) {
	runErr := make(chan error, 1)
	queue := NewTurnQueue(t.Context(), func(ctx context.Context, _ QueueItem) error {
		err := ctx.Err()
		runErr <- err
		return err
	})
	worker := &draftWorker{queue: make(chan QueueItem, 1)}
	queue.workers["draft"] = worker
	if !queue.EnqueueUserMessage("draft", "message", "hello") {
		t.Fatal("消息应被接受")
	}

	result := make(chan bool, 1)
	go func() { result <- queue.CancelAndJoinDraft("draft") }()
	deadline := time.Now().Add(time.Second)
	for {
		worker.mu.Lock()
		canceling := worker.canceling
		worker.mu.Unlock()
		if canceling {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("取消屏障未建立")
		}
		time.Sleep(time.Millisecond)
	}
	if queue.EnqueueUserMessage("draft", "late", "late") {
		t.Fatal("取消屏障期间不应接受新消息")
	}
	go queue.runWorker(worker)
	if !<-result {
		t.Fatal("已接受但未启动的消息应报告取消成功")
	}
	if err := <-runErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("runner context error=%v", err)
	}
	if queue.IsBusy("draft") {
		t.Fatal("取消等待返回后队列应为空闲")
	}
	queue.Close()
}

func TestDraftCancellationBarrierReportsEveryCoveredTurn(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	queue := NewTurnQueue(t.Context(), func(ctx context.Context, _ QueueItem) error {
		select {
		case <-started:
		default:
			close(started)
		}
		<-ctx.Done()
		return ctx.Err()
	})
	t.Cleanup(queue.Close)
	for index := range 3 {
		if !queue.EnqueueUserMessage("draft_batch_cancel", fmt.Sprintf("message_%d", index), "取消") {
			t.Fatalf("enqueue %d failed", index)
		}
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("runner did not start")
	}
	barrier, requested := queue.BeginDraftCancellation("draft_batch_cancel")
	if !requested || barrier == nil || barrier.CoveredTurns() != 3 {
		t.Fatalf("requested=%v barrier=%v covered=%d", requested, barrier, barrier.CoveredTurns())
	}
	if !barrier.Wait(t.Context()) {
		t.Fatal("cancelled turns did not drain")
	}
	barrier.Release()
}

func TestCancellationExcludesCurrentTurnAfterAtomicDurableTerminalCommit(t *testing.T) {
	t.Parallel()
	commitStarted := make(chan struct{})
	allowCommit := make(chan struct{})
	var queue *TurnQueue
	queue = NewTurnQueue(t.Context(), func(_ context.Context, item QueueItem) error {
		_, err := queue.CommitCurrentDurableTerminal(item, func() (bool, error) {
			close(commitStarted)
			<-allowCommit
			return true, nil
		})
		return err
	})
	t.Cleanup(queue.Close)
	if !queue.EnqueueUserMessage("draft_terminal_race", "message_done", "已经完成") {
		t.Fatal("enqueue failed")
	}
	select {
	case <-commitStarted:
	case <-time.After(time.Second):
		t.Fatal("terminal commit did not start")
	}

	type cancellationResult struct {
		barrier   *DraftCancellationBarrier
		requested bool
	}
	result := make(chan cancellationResult, 1)
	go func() {
		barrier, requested := queue.BeginDraftCancellation("draft_terminal_race")
		result <- cancellationResult{barrier: barrier, requested: requested}
	}()
	select {
	case <-result:
		t.Fatal("cancellation must serialize with the terminal commit")
	case <-time.After(20 * time.Millisecond):
	}
	close(allowCommit)
	cancelled := <-result
	if cancelled.barrier == nil || cancelled.requested || cancelled.barrier.CoveredTurns() != 0 {
		t.Fatalf("barrier=%v requested=%v covered=%d",
			cancelled.barrier, cancelled.requested, cancelled.barrier.CoveredTurns())
	}
	if !cancelled.barrier.Wait(t.Context()) {
		t.Fatal("durable current turn did not drain")
	}
	cancelled.barrier.Release()
}

func TestDurableTerminalHelpersHandleMissingBarrierAndWorker(t *testing.T) {
	t.Parallel()
	var barrier *DraftCancellationBarrier
	if covered := barrier.CoveredTurns(); covered != 0 {
		t.Fatalf("nil barrier covered=%d", covered)
	}
	queue := NewTurnQueue(t.Context(), nil)
	t.Cleanup(queue.Close)
	commitCalls := 0
	applied, err := queue.CommitCurrentDurableTerminal(QueueItem{
		DraftID: "missing_draft", Kind: QueueUserMessage, ItemID: "missing_item",
	}, func() (bool, error) {
		commitCalls++
		return true, nil
	})
	if err != nil || !applied || commitCalls != 1 {
		t.Fatalf("applied=%v calls=%d err=%v", applied, commitCalls, err)
	}
}

func TestCancelAndJoinDraftAbandonsRunnerThatIgnoresCancellation(t *testing.T) {
	started := make(chan struct{})
	releaseRunner := make(chan struct{})
	queue := NewTurnQueue(t.Context(), func(_ context.Context, item QueueItem) error {
		if item.ItemID == "blocked" {
			close(started)
			<-releaseRunner
		}
		return nil
	})
	defer func() {
		close(releaseRunner)
		queue.Close()
	}()
	if !queue.EnqueueUserMessage("draft", "blocked", "hello") {
		t.Fatal("消息应被接受")
	}
	<-started

	start := time.Now()
	if !queue.CancelAndJoinDraft("draft") {
		t.Fatal("运行中的消息应报告取消成功")
	}
	if elapsed := time.Since(start); elapsed > 2*cancelAndJoinDraftTimeout {
		t.Fatalf("取消等待无界: elapsed=%s", elapsed)
	}
	if !queue.EnqueueUserMessage("draft", "fresh", "hello") {
		t.Fatal("超时封存旧 worker 后应接受新消息")
	}
}

func TestDraftCancellationBarrierHasBoundedWaitAndExplicitRelease(t *testing.T) {
	started := make(chan struct{}, 1)
	releaseRunner := make(chan struct{})
	var releaseOnce sync.Once
	runs := make(chan string, 2)
	queue := NewTurnQueue(t.Context(), func(_ context.Context, item QueueItem) error {
		runs <- item.ItemID
		if item.ItemID == "blocked" {
			started <- struct{}{}
			<-releaseRunner // 模拟不响应 context 取消的 provider。
		}
		return nil
	})
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releaseRunner) })
		queue.Close()
	})
	if !queue.EnqueueUserMessage("draft", "blocked", "hello") {
		t.Fatal("首个 turn 应入队")
	}
	<-started

	barrier, requested := queue.BeginDraftCancellation("draft")
	if barrier == nil || !requested {
		t.Fatalf("barrier=%v requested=%v", barrier, requested)
	}
	waitCtx, cancelWait := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancelWait()
	if barrier.Wait(waitCtx) {
		t.Fatal("忽略取消的 runner 不应让有界等待提前成功")
	}
	if queue.EnqueueUserMessage("draft", "too_early", "no") {
		t.Fatal("清理完成前不得接受新 turn")
	}

	releaseOnce.Do(func() { close(releaseRunner) })
	if !barrier.Wait(t.Context()) {
		t.Fatal("runner 退出后屏障应可完成")
	}
	if queue.EnqueueUserMessage("draft", "before_release", "no") {
		t.Fatal("等待完成但显式释放前仍应保持屏障")
	}
	barrier.Release()
	barrier.Release()
	if !queue.EnqueueUserMessage("draft", "after_release", "ok") {
		t.Fatal("释放屏障后应接受新 turn")
	}
	queue.JoinDraft("draft")
	close(runs)
	var seen []string
	for itemID := range runs {
		seen = append(seen, itemID)
	}
	if strings.Join(seen, ",") != "blocked,after_release" {
		t.Fatalf("runs=%v", seen)
	}
}

func TestTurnQueueCloseDoesNotWaitForRunnerCapturedBeforeAbandon(t *testing.T) {
	started := make(chan struct{})
	releaseRunner := make(chan struct{})
	var releaseOnce sync.Once
	queue := NewTurnQueue(t.Context(), func(context.Context, QueueItem) error {
		close(started)
		<-releaseRunner
		return nil
	})
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseRunner) }) })
	if !queue.EnqueueUserMessage("draft", "blocked", "hello") {
		t.Fatal("turn 应入队")
	}
	<-started
	barrier, requested := queue.BeginDraftCancellation("draft")
	if barrier == nil || !requested {
		t.Fatalf("barrier=%v requested=%v", barrier, requested)
	}
	closed := make(chan struct{})
	go func() {
		queue.Close()
		close(closed)
	}()
	deadline := time.Now().Add(time.Second)
	for {
		queue.mu.Lock()
		closing := queue.closed
		queue.mu.Unlock()
		if closing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Close 未进入关闭临界区")
		}
		time.Sleep(time.Millisecond)
	}
	barrier.Abandon()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close 不应等待忽略取消的旧 runner")
	}
	select {
	case <-queue.closeDone:
	default:
		t.Fatal("Close 返回前清理协调器必须退出")
	}
	releaseOnce.Do(func() { close(releaseRunner) })
}

func TestTurnQueueCloseBoundsBlockedProducerCallback(t *testing.T) {
	queue := NewTurnQueue(t.Context(), nil)
	worker := newDraftWorker()
	queue.workers["draft"] = worker
	for index := range cap(worker.queue) {
		if !queue.Enqueue(QueueItem{DraftID: "draft", ItemID: fmt.Sprintf("queued_%d", index)}) {
			t.Fatalf("填充队列失败: index=%d", index)
		}
	}

	callbackStarted := make(chan struct{})
	releaseCallback := make(chan struct{})
	producerDone := make(chan bool, 1)
	go func() {
		producerDone <- queue.Enqueue(QueueItem{
			DraftID: "draft", ItemID: "blocked_producer",
			onConsumed: func(error) {
				close(callbackStarted)
				<-releaseCallback
			},
		})
	}()

	deadline := time.Now().Add(time.Second)
	for {
		worker.mu.Lock()
		pending := worker.pendingCount
		worker.mu.Unlock()
		if pending == cap(worker.queue)+1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("producer 未进入 send 前窗口: pending=%d", pending)
		}
		time.Sleep(time.Millisecond)
	}

	closed := make(chan struct{})
	go func() {
		queue.Close()
		close(closed)
	}()
	select {
	case <-callbackStarted:
	case <-time.After(time.Second):
		t.Fatal("关闭未唤醒阻塞 producer")
	}
	select {
	case <-closed:
	case <-time.After(2 * cancelAndJoinDraftTimeout):
		t.Fatal("Close 被 producer 回调无界阻塞")
	}
	if queue.EnqueueUserMessage("draft", "after_close", "no") {
		t.Fatal("Close 返回后不得接受新 turn")
	}

	close(releaseCallback)
	if <-producerDone {
		t.Fatal("关闭唤醒的 producer 不应入队成功")
	}
	select {
	case <-queue.closeDone:
	case <-time.After(time.Second):
		t.Fatal("回调返回后关闭清理应完成")
	}
}

func TestDraftCancellationBarrierCoversIdleDraft(t *testing.T) {
	queue := NewTurnQueue(t.Context(), nil)
	t.Cleanup(queue.Close)
	for index := range 100 {
		draftID := fmt.Sprintf("idle_%d", index)
		barrier, requested := queue.BeginDraftCancellation(draftID)
		if barrier == nil || requested {
			t.Fatalf("barrier=%v requested=%v", barrier, requested)
		}
		if !barrier.Wait(t.Context()) {
			t.Fatal("空闲草稿屏障应立即完成")
		}
		if queue.EnqueueUserMessage(draftID, "during_cleanup", "no") {
			t.Fatal("空闲草稿的清理窗口也必须阻止新 turn")
		}
		barrier.Release()
	}
	queue.mu.Lock()
	workerCount := len(queue.workers)
	leaseCount := len(queue.cancelLeases)
	queue.mu.Unlock()
	if workerCount != 0 || leaseCount != 0 {
		t.Fatalf("空闲取消遗留状态: workers=%d leases=%d", workerCount, leaseCount)
	}
	if !queue.EnqueueUserMessage("idle", "after_cleanup", "ok") {
		t.Fatal("清理后应恢复入队")
	}
	queue.JoinDraft("idle")
}

func TestTurnStreamHubSnapshotAllTypesAndSlowSubscriber(t *testing.T) {
	t.Parallel()
	hub := NewTurnStreamHub(2)
	allTypes := []string{
		"turn_started", "text_delta", "message_completed", "tool_step_started",
		"tool_step_finished", "subagent_progress", "model_retry", "turn_ended", "turn_error",
	}
	for _, typeName := range allTypes[:3] {
		hub.Record("draft", StreamEvent{"type": typeName})
	}
	snapshot, live, unsubscribe := hub.Subscribe("draft")
	defer unsubscribe()
	if len(snapshot) != 3 || snapshot[0]["type"] != "turn_started" {
		t.Fatalf("snapshot=%#v", snapshot)
	}
	frame, err := EncodeTurnStreamFrame(StreamEvent{"type": "text_delta", "delta": "你"})
	if err != nil || string(frame) != "event: turn_stream\ndata: {\"delta\":\"你\",\"type\":\"text_delta\"}\n\n" {
		t.Fatalf("frame=%q err=%v", frame, err)
	}
	completedFrame, err := EncodeTurnStreamFrame(StreamEvent{
		"type": "message_completed", "message_id": "msg_1", "kind": "reply", "content": "完成",
	})
	if err != nil {
		t.Fatal(err)
	}
	golden, err := os.ReadFile("testdata/turn_stream.golden")
	if err != nil {
		t.Fatal(err)
	}
	if string(completedFrame) != string(golden) {
		t.Fatalf("turn-stream 漂移\n--- expected ---\n%s--- actual ---\n%s", golden, completedFrame)
	}
	retryFrame, err := EncodeTurnStreamFrame(StreamEvent{
		"type": "model_retry", "attempt": 2, "max_retries": 5,
		"reason": "模型响应超时", "next_delay_ms": int64(500),
	})
	if err != nil || !strings.Contains(string(retryFrame), `"type":"model_retry"`) ||
		!strings.Contains(string(retryFrame), `"attempt":2`) ||
		!strings.Contains(string(retryFrame), `"max_retries":5`) {
		t.Fatalf("retry frame=%q err=%v", retryFrame, err)
	}
	for _, typeName := range allTypes[3:6] {
		hub.Record("draft", StreamEvent{"type": typeName})
	}
	select {
	case event, open := <-live:
		if !open || event["type"] != "tool_step_finished" {
			t.Fatalf("慢订阅者剩余事件=%#v open=%v", event, open)
		}
	case <-time.After(time.Second):
		t.Fatal("未收到实时事件")
	}
	select {
	case event, open := <-live:
		if !open || event["type"] != TurnStreamGap {
			t.Fatalf("gap=%#v open=%v", event, open)
		}
	case <-time.After(time.Second):
		t.Fatal("慢订阅者未收到 stream_gap")
	}
	if _, open := <-live; open {
		t.Fatal("stream_gap 后慢订阅 channel 应关闭")
	}
	_, terminal, stop := hub.Subscribe("terminal")
	hub.Record("terminal", StreamEvent{"type": "turn_started"})
	hub.Record("terminal", StreamEvent{"type": "turn_ended"})
	if len(hub.Snapshot("terminal")) != 0 {
		t.Fatal("终态后快照未清空")
	}
	stop()
	closed := false
	for range 3 {
		if _, ok := <-terminal; !ok {
			closed = true
			break
		}
	}
	if !closed {
		t.Fatal("unsubscribe 后 channel 应关闭")
	}
	stop()
	hub.Record("without-subscriber", StreamEvent{"type": "text_delta"})
	if _, err := EncodeTurnStreamFrame(StreamEvent{"bad": make(chan int)}); err == nil {
		t.Fatal("不可 JSON 编码的 turn-stream 事件应失败")
	}
}

func TestTurnStreamHubBoundsSnapshotsByEventCountAndBytes(t *testing.T) {
	t.Parallel()
	t.Run("event count", func(t *testing.T) {
		hub := newTurnStreamHub(2, 5, DefaultTurnStreamBufferByteLimit)
		hub.Record("draft", StreamEvent{"type": TurnStreamTurnStarted})
		hub.Record("draft", StreamEvent{"type": TurnStreamTextDelta, "delta": "old"})
		hub.Record("draft", StreamEvent{"type": TurnStreamToolStepStarted, "step_id": "tool"})
		for index := range 10 {
			hub.Record("draft", StreamEvent{
				"type": TurnStreamTextDelta, "message_id": "message", "delta": fmt.Sprintf("delta-%d", index),
			})
		}
		hub.Record("draft", StreamEvent{"type": TurnStreamMessageCompleted, "message_id": "message"})

		snapshot := hub.Snapshot("draft")
		if len(snapshot) > 5 || snapshot[0]["type"] != TurnStreamSnapshotTruncated {
			t.Fatalf("snapshot=%#v", snapshot)
		}
		for _, required := range []string{
			TurnStreamTurnStarted, TurnStreamToolStepStarted, TurnStreamMessageCompleted,
		} {
			if !streamSnapshotContainsType(snapshot, required) {
				t.Fatalf("结构事件 %s 被淘汰: %#v", required, snapshot)
			}
		}
		if !streamSnapshotContainsDelta(snapshot, "delta-9") || streamSnapshotContainsDelta(snapshot, "old") {
			t.Fatalf("text delta 未按从旧到新淘汰: %#v", snapshot)
		}
	})

	t.Run("encoded bytes", func(t *testing.T) {
		started := StreamEvent{"type": TurnStreamTurnStarted}
		tool := StreamEvent{"type": TurnStreamToolStepStarted, "step_id": "tool"}
		latest := StreamEvent{"type": TurnStreamTextDelta, "message_id": "message", "delta": "latest"}
		marker := StreamEvent{"type": TurnStreamSnapshotTruncated}
		byteLimit := encodedEventSize(started) + encodedEventSize(tool) + encodedEventSize(latest) +
			encodedEventSize(marker) + 8
		hub := newTurnStreamHub(2, 100, byteLimit)
		hub.Record("draft", started)
		hub.Record("draft", StreamEvent{
			"type": TurnStreamTextDelta, "message_id": "message", "delta": strings.Repeat("x", byteLimit),
		})
		hub.Record("draft", tool)
		hub.Record("draft", latest)

		snapshot := hub.Snapshot("draft")
		if snapshot[0]["type"] != TurnStreamSnapshotTruncated ||
			!streamSnapshotContainsType(snapshot, TurnStreamToolStepStarted) ||
			!streamSnapshotContainsDelta(snapshot, "latest") {
			t.Fatalf("snapshot=%#v", snapshot)
		}
		hub.mu.Lock()
		bufferBytes := hub.buffers["draft"].bytes
		hub.mu.Unlock()
		if bufferBytes > byteLimit {
			t.Fatalf("buffer bytes=%d limit=%d", bufferBytes, byteLimit)
		}
	})

	t.Run("structure only fallback", func(t *testing.T) {
		hub := newTurnStreamHub(2, 3, DefaultTurnStreamBufferByteLimit)
		for index := range 5 {
			hub.Record("draft", StreamEvent{
				"type": TurnStreamToolStepFinished, "step_id": fmt.Sprintf("step-%d", index),
			})
		}
		snapshot := hub.Snapshot("draft")
		if len(snapshot) != 2 || snapshot[0]["type"] != TurnStreamSnapshotTruncated ||
			snapshot[1]["step_id"] != "step-4" {
			t.Fatalf("snapshot=%#v", snapshot)
		}
	})

	t.Run("oversized structure", func(t *testing.T) {
		markerBytes := encodedEventSize(StreamEvent{"type": TurnStreamSnapshotTruncated})
		hub := newTurnStreamHub(2, 10, markerBytes+8)
		hub.Record("draft", StreamEvent{
			"type": TurnStreamMessageCompleted, "content": strings.Repeat("x", markerBytes+32),
		})
		snapshot := hub.Snapshot("draft")
		if len(snapshot) != 1 || snapshot[0]["type"] != TurnStreamSnapshotTruncated {
			t.Fatalf("snapshot=%#v", snapshot)
		}
	})
}

func TestTurnStreamHubRecoversTerminalAfterOrdinaryUnsubscribe(t *testing.T) {
	t.Parallel()
	hub := NewTurnStreamHub(2)
	_, _, _, _, unsubscribe := hub.SubscribeRecoverable("draft", "client")
	hub.Record("draft", StreamEvent{"type": TurnStreamTurnStarted})
	unsubscribe()
	hub.Record("draft", StreamEvent{"type": TurnStreamTurnEnded, "outcome": "finished"})

	snapshot, _, acknowledge, _, stop := hub.SubscribeRecoverable("draft", "client")
	defer stop()
	if len(snapshot) != 2 || snapshot[0]["type"] != TurnStreamGap ||
		snapshot[1]["type"] != TurnStreamTurnEnded {
		t.Fatalf("普通断线后的终态恢复快照=%#v", snapshot)
	}
	acknowledge()
	if remaining := hub.Snapshot("draft"); len(remaining) != 0 {
		t.Fatalf("完整重放确认后仍保留恢复快照=%#v", remaining)
	}
}

func TestTurnStreamHubStaleSnapshotAckDoesNotDeleteNewRecovery(t *testing.T) {
	t.Parallel()
	hub := NewTurnStreamHub(2)
	_, _, _, _, unsubscribe := hub.SubscribeRecoverable("draft", "client")
	hub.Record("draft", StreamEvent{"type": TurnStreamTurnStarted})
	unsubscribe()
	hub.Record("draft", StreamEvent{"type": TurnStreamTurnEnded, "turn": 1})
	_, _, acknowledgeOld, _, stopOld := hub.SubscribeRecoverable("draft", "client")
	defer stopOld()
	stopOld()

	hub.Record("draft", StreamEvent{"type": TurnStreamTurnStarted})
	_, _, _, _, unsubscribeNew := hub.SubscribeRecoverable("draft", "client")
	unsubscribeNew()
	hub.Record("draft", StreamEvent{"type": TurnStreamTurnEnded, "turn": 2})
	acknowledgeOld()

	snapshot := hub.Snapshot("draft")
	if len(snapshot) != 2 || snapshot[1]["turn"] != 2 {
		t.Fatalf("旧 generation 确认误删新恢复快照=%#v", snapshot)
	}
}

func TestTurnStreamHubSnapshotAckDoesNotCoverLaterSubscriberFailure(t *testing.T) {
	t.Parallel()
	hub := NewTurnStreamHub(2)
	_, streamA, _, _, stopA := hub.SubscribeRecoverable("draft", "client-a")
	_, streamB, _, _, stopB := hub.SubscribeRecoverable("draft", "client-b")
	hub.Record("draft", StreamEvent{"type": TurnStreamTurnStarted})
	<-streamA
	<-streamB
	hub.Record("draft", StreamEvent{"type": TurnStreamTurnEnded})
	<-streamA
	<-streamB

	stopA()
	stopB()
	_, _, acknowledgeARecovery, _, stopARecovery := hub.SubscribeRecoverable("draft", "client-a")
	defer stopARecovery()
	acknowledgeARecovery()
	acknowledgeARecovery()
	if snapshot := hub.Snapshot("draft"); len(snapshot) == 0 {
		t.Fatal("A 的恢复确认清除了尚未恢复的 client-b terminal")
	}

	_, _, acknowledgeBRecovery, _, stopBRecovery := hub.SubscribeRecoverable("draft", "client-b")
	defer stopBRecovery()
	acknowledgeBRecovery()
	if snapshot := hub.Snapshot("draft"); len(snapshot) != 0 {
		t.Fatalf("client-b 已确认但恢复快照未清理=%#v", snapshot)
	}
}

func TestTurnStreamHubLiveTerminalAckClearsEarlierFailureForSameClient(t *testing.T) {
	t.Parallel()
	hub := NewTurnStreamHub(2)
	_, first, _, _, stopFirst := hub.SubscribeRecoverable("draft", "client")
	hub.Record("draft", StreamEvent{"type": TurnStreamTurnStarted})
	<-first
	stopFirst()

	_, reconnected, _, acknowledgeEvent, stopReconnected := hub.SubscribeRecoverable("draft", "client")
	defer stopReconnected()
	hub.Record("draft", StreamEvent{"type": TurnStreamTurnEnded})
	terminal := <-reconnected
	acknowledgeEvent(terminal)
	if snapshot := hub.Snapshot("draft"); len(snapshot) != 0 {
		t.Fatalf("同一 client 成功 flush live terminal 后仍遗留恢复快照=%#v", snapshot)
	}
}

func TestTurnStreamHubBoundsOversizedTerminalRecovery(t *testing.T) {
	t.Parallel()
	hub := newTurnStreamHub(2, 5, 256)
	_, stream, _, _, stop := hub.SubscribeRecoverable("draft", "client")
	hub.Record("draft", StreamEvent{"type": TurnStreamTurnStarted})
	<-stream
	hub.Record("draft", StreamEvent{
		"type": TurnStreamTurnError, "message": strings.Repeat("x", 1024),
	})
	<-stream
	stop()

	snapshot := hub.Snapshot("draft")
	if len(snapshot) != 2 || snapshot[0]["type"] != TurnStreamSnapshotTruncated ||
		snapshot[1]["type"] != TurnStreamTurnError {
		t.Fatalf("超大 terminal 恢复快照=%#v", snapshot)
	}
	hub.mu.Lock()
	bufferBytes := hub.buffers["draft"].bytes
	hub.mu.Unlock()
	if bufferBytes > 256 {
		t.Fatalf("terminal recovery bytes=%d limit=256", bufferBytes)
	}
}

func TestTurnStreamHubBoundsRecoveryDrafts(t *testing.T) {
	t.Parallel()
	hub := NewTurnStreamHub(2)
	hub.recoveryLimit = 2
	for _, draftID := range []string{"draft-1", "draft-2", "draft-3"} {
		_, stream, _, _, stop := hub.SubscribeRecoverable(draftID, "client")
		hub.Record(draftID, StreamEvent{"type": TurnStreamTurnStarted})
		<-stream
		stop()
		hub.Record(draftID, StreamEvent{"type": TurnStreamTurnEnded})
	}
	if snapshot := hub.Snapshot("draft-1"); len(snapshot) != 0 {
		t.Fatalf("最老 recovery tombstone 未被淘汰=%#v", snapshot)
	}
	for _, draftID := range []string{"draft-2", "draft-3"} {
		if snapshot := hub.Snapshot(draftID); len(snapshot) == 0 {
			t.Fatalf("较新的 recovery tombstone %s 被误删", draftID)
		}
	}
}

func TestTurnStreamHubRecoveryLimitDoesNotEvictPendingDelivery(t *testing.T) {
	t.Parallel()
	hub := NewTurnStreamHub(2)
	hub.recoveryLimit = 1
	_, pendingStream, _, _, stopPending := hub.SubscribeRecoverable("pending", "pending-client")
	hub.Record("pending", StreamEvent{"type": TurnStreamTurnStarted})
	<-pendingStream
	hub.Record("pending", StreamEvent{"type": TurnStreamTurnEnded})

	for _, draftID := range []string{"orphan-1", "orphan-2"} {
		_, stream, _, _, stop := hub.SubscribeRecoverable(draftID, draftID+"-client")
		hub.Record(draftID, StreamEvent{"type": TurnStreamTurnStarted})
		<-stream
		stop()
		hub.Record(draftID, StreamEvent{"type": TurnStreamTurnEnded})
	}
	if snapshot := hub.Snapshot("pending"); len(snapshot) == 0 {
		t.Fatal("recovery 上限淘汰了仍在等待 live flush 的 terminal")
	}
	stopPending()
	hub.mu.Lock()
	orphanCount := 0
	for _, buffer := range hub.buffers {
		if buffer.recoveryGeneration != 0 && len(buffer.pendingSubscribers) == 0 {
			orphanCount++
		}
	}
	hub.mu.Unlock()
	if orphanCount > 1 {
		t.Fatalf("pending 转 orphan 后 recovery 数量=%d limit=1", orphanCount)
	}
	if snapshot := hub.Snapshot("orphan-1"); len(snapshot) != 0 {
		t.Fatalf("最老 orphan recovery 未被淘汰=%#v", snapshot)
	}
	if snapshot := hub.Snapshot("orphan-2"); len(snapshot) == 0 {
		t.Fatal("最新 orphan recovery 被误删")
	}
}

func TestTurnStreamHubBoundsLiveAckToOrphanRecovery(t *testing.T) {
	t.Parallel()
	hub := NewTurnStreamHub(2)
	hub.recoveryLimit = 1
	_, failedStream, _, _, stopFailed := hub.SubscribeRecoverable("live-ack", "failed-client")
	hub.Record("live-ack", StreamEvent{"type": TurnStreamTurnStarted})
	<-failedStream
	stopFailed()
	_, liveStream, _, acknowledgeLive, stopLive := hub.SubscribeRecoverable("live-ack", "live-client")
	defer stopLive()
	hub.Record("live-ack", StreamEvent{"type": TurnStreamTurnEnded})
	terminal := <-liveStream

	for _, draftID := range []string{"orphan-1", "orphan-2"} {
		_, stream, _, _, stop := hub.SubscribeRecoverable(draftID, draftID+"-client")
		hub.Record(draftID, StreamEvent{"type": TurnStreamTurnStarted})
		<-stream
		stop()
		hub.Record(draftID, StreamEvent{"type": TurnStreamTurnEnded})
	}
	acknowledgeLive(terminal)

	hub.mu.Lock()
	orphanCount := 0
	for _, buffer := range hub.buffers {
		if buffer.recoveryGeneration != 0 && len(buffer.pendingSubscribers) == 0 {
			orphanCount++
		}
	}
	hub.mu.Unlock()
	if orphanCount > 1 {
		t.Fatalf("live ACK 转 orphan 后 recovery 数量=%d limit=1", orphanCount)
	}
	if snapshot := hub.Snapshot("live-ack"); len(snapshot) != 0 {
		t.Fatalf("最老的 live ACK orphan 未被淘汰=%#v", snapshot)
	}
	if snapshot := hub.Snapshot("orphan-2"); len(snapshot) == 0 {
		t.Fatal("最新 orphan recovery 被误删")
	}
}

func streamSnapshotContainsType(snapshot []StreamEvent, typeName string) bool {
	for _, event := range snapshot {
		if event["type"] == typeName {
			return true
		}
	}
	return false
}

func streamSnapshotContainsDelta(snapshot []StreamEvent, delta string) bool {
	for _, event := range snapshot {
		if event["type"] == TurnStreamTextDelta && event["delta"] == delta {
			return true
		}
	}
	return false
}

func TestTurnQueueEnqueueUserMessageIfIdleReservesAtMostOne(t *testing.T) {
	t.Parallel()
	started := make(chan string, 4)
	release := make(chan struct{})
	var mu sync.Mutex
	var ran []string
	queue := NewTurnQueue(t.Context(), func(ctx context.Context, item QueueItem) error {
		started <- item.ItemID
		select {
		case <-release:
		case <-ctx.Done():
		}
		mu.Lock()
		ran = append(ran, item.ItemID)
		mu.Unlock()
		return ctx.Err()
	})
	t.Cleanup(queue.Close)

	// 空闲草稿补入队成功,回合开始并阻塞在 runner。
	if !queue.EnqueueUserMessageIfIdle("draft", "m1", "内容") {
		t.Fatal("空闲草稿应可补入队")
	}
	if got := <-started; got != "m1" {
		t.Fatalf("m1 未启动: %s", got)
	}
	// 回合运行中(pendingCount=1)再补入队被空闲守卫拒绝,不产生第二个回合。
	if queue.EnqueueUserMessageIfIdle("draft", "m2", "内容") {
		t.Fatal("非空闲草稿不应补入队")
	}
	if pending := queue.PendingCount("draft"); pending != 1 {
		t.Fatalf("pending=%d", pending)
	}
	close(release)
	queue.JoinDraft("draft")
	mu.Lock()
	joined := strings.Join(ran, ",")
	mu.Unlock()
	if joined != "m1" {
		t.Fatalf("只应运行 m1: %s", joined)
	}
	if pending := queue.PendingCount("draft"); pending != 0 {
		t.Fatalf("排空后 pending=%d", pending)
	}
	// 空闲草稿 PendingCount 为 0。
	if pending := queue.PendingCount("missing"); pending != 0 {
		t.Fatalf("不存在草稿 pending=%d", pending)
	}
}
