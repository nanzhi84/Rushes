package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

// concurrencyProbe 记录并发峰值:每个工具进入时 inFlight+1 并抬高 maxSeen,退出时 -1。
// maxSeen 是并发的确定性证据,不受调度抖动影响,补足 wall-clock 断言。
type concurrencyProbe struct {
	inFlight atomic.Int32
	maxSeen  atomic.Int32
}

func (probe *concurrencyProbe) tool(name string, delay time.Duration) tool.BaseTool {
	impl, err := utils.InferTool(name, name, func(_ context.Context, _ struct{}) (string, error) {
		current := probe.inFlight.Add(1)
		for {
			seen := probe.maxSeen.Load()
			if current <= seen || probe.maxSeen.CompareAndSwap(seen, current) {
				break
			}
		}
		time.Sleep(delay)
		probe.inFlight.Add(-1)
		return name, nil
	})
	if err != nil {
		panic(err)
	}
	return impl
}

func routerMessage(names ...string) *schema.Message {
	calls := make([]schema.ToolCall, len(names))
	for index, name := range names {
		calls[index] = schema.ToolCall{
			ID:       "call_" + name,
			Function: schema.FunctionCall{Name: name, Arguments: "{}"},
		}
	}
	return &schema.Message{Role: schema.Assistant, ToolCalls: calls}
}

func newRouterForTest(t *testing.T, effect map[string]rushestools.Effect, tools ...tool.BaseTool) *toolRouter {
	t.Helper()
	router, err := newToolRouter(t.Context(), compose.ToolsNodeConfig{Tools: tools},
		func(name string) (rushestools.Effect, bool) { value, ok := effect[name]; return value, ok })
	if err != nil {
		t.Fatal(err)
	}
	return router
}

func TestToolRouterRunsReadOnlyMessagesInParallel(t *testing.T) {
	t.Parallel()
	probe := &concurrencyProbe{}
	const delay = 80 * time.Millisecond
	router := newRouterForTest(t,
		map[string]rushestools.Effect{"read.a": rushestools.EffectReadOnly, "read.b": rushestools.EffectReadOnly},
		probe.tool("read.a", delay), probe.tool("read.b", delay))

	start := time.Now()
	results, err := router.Invoke(t.Context(), routerMessage("read.a", "read.b"))
	elapsed := time.Since(start)
	if err != nil || len(results) != 2 {
		t.Fatalf("results=%#v err=%v", results, err)
	}
	if results[0].ToolCallID != "call_read.a" || results[1].ToolCallID != "call_read.b" {
		t.Fatalf("并发结果未按原下标保序: %s,%s", results[0].ToolCallID, results[1].ToolCallID)
	}
	if probe.maxSeen.Load() != 2 {
		t.Fatalf("只读消息未并发执行: 并发峰值=%d", probe.maxSeen.Load())
	}
	if elapsed >= 2*delay {
		t.Fatalf("wall-clock 未低于串行和: elapsed=%v serial≈%v", elapsed, 2*delay)
	}
}

func TestToolRouterSerializesMessagesWithWrites(t *testing.T) {
	t.Parallel()
	probe := &concurrencyProbe{}
	const delay = 40 * time.Millisecond
	router := newRouterForTest(t,
		map[string]rushestools.Effect{"read.a": rushestools.EffectReadOnly, "write.b": rushestools.EffectReversible},
		probe.tool("read.a", delay), probe.tool("write.b", delay))

	results, err := router.Invoke(t.Context(), routerMessage("read.a", "write.b"))
	if err != nil || len(results) != 2 {
		t.Fatalf("results=%#v err=%v", results, err)
	}
	if results[0].ToolCallID != "call_read.a" || results[1].ToolCallID != "call_write.b" {
		t.Fatal("串行结果未保序")
	}
	if probe.maxSeen.Load() != 1 {
		t.Fatalf("含写消息不得并发: 并发峰值=%d", probe.maxSeen.Load())
	}
}

func TestToolRouterClassification(t *testing.T) {
	t.Parallel()
	effect := map[string]rushestools.Effect{
		"read.a": rushestools.EffectReadOnly, "read.b": rushestools.EffectReadOnly,
		"write.c": rushestools.EffectReversible,
	}
	router := newRouterForTest(t, effect,
		newRouterNoopTool("read.a"), newRouterNoopTool("read.b"), newRouterNoopTool("write.c"))
	cases := []struct {
		names    []string
		parallel bool
	}{
		{[]string{"read.a", "read.b"}, true},
		{[]string{"read.a", "write.c"}, false},
		{[]string{"write.c"}, false},
		{[]string{"unknown.x"}, false},
		{nil, false},
	}
	for _, test := range cases {
		if got := router.allReadOnly(routerMessage(test.names...)); got != test.parallel {
			t.Fatalf("allReadOnly(%v)=%v 期望 %v", test.names, got, test.parallel)
		}
	}
}

func newRouterNoopTool(name string) tool.BaseTool {
	impl, err := utils.InferTool(name, name, func(_ context.Context, _ struct{}) (string, error) {
		return name, nil
	})
	if err != nil {
		panic(err)
	}
	return impl
}
