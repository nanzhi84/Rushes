package agent

import (
	"context"
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
}

func TestTurnStreamHubSnapshotEightTypesAndSlowSubscriber(t *testing.T) {
	t.Parallel()
	hub := NewTurnStreamHub(2)
	eight := []string{
		"turn_started", "text_delta", "message_completed", "tool_step_started",
		"tool_step_finished", "subagent_progress", "turn_ended", "turn_error",
	}
	for _, typeName := range eight[:3] {
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
	for _, typeName := range eight[3:6] {
		hub.Record("draft", StreamEvent{"type": typeName})
	}
	select {
	case _, open := <-live:
		if !open {
			t.Fatal("第一条实时事件不应关闭")
		}
	case <-time.After(time.Second):
		t.Fatal("未收到实时事件")
	}
	// queue limit=2；第三条到来时慢订阅者被踢并关闭。
	for range 3 {
		select {
		case <-live:
		case <-time.After(time.Second):
			return
		}
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
