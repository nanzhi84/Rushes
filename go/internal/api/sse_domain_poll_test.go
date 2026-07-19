package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingSSEWriter 是线程安全的流式 ResponseWriter:统计已转发的回合流帧,供测试在
// handler 于后台 goroutine 运行时安全读取。
type countingSSEWriter struct {
	mu        sync.Mutex
	header    http.Header
	delivered int
}

func (writer *countingSSEWriter) Header() http.Header {
	if writer.header == nil {
		writer.header = http.Header{}
	}
	return writer.header
}

func (writer *countingSSEWriter) WriteHeader(int) {}

func (writer *countingSSEWriter) Write(payload []byte) (int, error) {
	writer.mu.Lock()
	if strings.Contains(string(payload), "event: turn_stream") {
		writer.delivered++
	}
	writer.mu.Unlock()
	return len(payload), nil
}

func (writer *countingSSEWriter) Flush() {}

func (writer *countingSSEWriter) deliveredCount() int {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.delivered
}

// H9:流式期间领域事件查询频率与回合流帧数解耦。一批 text_delta 走 turnStream 分支即时转发,
// 不应各自触发一次 ListEventsAfter;领域轮询只随 50ms poll tick 走。
func TestSSEDomainPollingDecoupledFromTurnStreamDeltas(t *testing.T) {
	t.Parallel()
	server, handler := testServer(t, t.TempDir(), 0)
	createDraftThroughAPI(t, handler, "draft_decouple")

	var domainPolls int64
	server.sseDomainPollObserver = func() { atomic.AddInt64(&domainPolls, 1) }

	writer := &countingSSEWriter{}
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet,
		"/api/drafts/draft_decouple/events?token="+testToken+"&turn_stream_client_id=c1", nil).WithContext(ctx)
	request.Host = "127.0.0.1:8000"
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(writer, request)
	}()

	waitFor(t, func() bool { return atomic.LoadInt64(&domainPolls) >= 1 }, "SSE 未进入循环(无首次领域轮询)")

	const burst = 40
	for index := 0; index < burst; index++ {
		server.agent.Hub().Record("draft_decouple", map[string]any{
			"type": "text_delta", "message_id": "m", "delta": "x",
		})
	}
	waitFor(t, func() bool { return writer.deliveredCount() >= burst }, "回合流帧未全部转发")

	pollsAfterBurst := atomic.LoadInt64(&domainPolls)
	cancel()
	<-done

	if pollsAfterBurst >= burst {
		t.Fatalf("领域轮询未与回合流帧解耦:%d 个 delta 触发了 %d 次领域查询(应远小于 %d)",
			burst, pollsAfterBurst, burst)
	}
}

func waitFor(t *testing.T, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal(message)
}
