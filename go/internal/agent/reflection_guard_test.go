package agent

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

// restateStubModel 是可控的一次性重述桩:Generate 返回配置好的回复并计数,用于断言只有
// 命中反思泄漏时才发生重述调用。
type restateStubModel struct {
	mu    sync.Mutex
	reply string
	err   error
	calls int
}

func (stub *restateStubModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return stub, nil
}

func (stub *restateStubModel) Generate(
	context.Context, []*schema.Message, ...model.Option,
) (*schema.Message, error) {
	stub.mu.Lock()
	stub.calls++
	stub.mu.Unlock()
	if stub.err != nil {
		return nil, stub.err
	}
	return schema.AssistantMessage(stub.reply, nil), nil
}

func (stub *restateStubModel) Stream(
	context.Context, []*schema.Message, ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	return schema.StreamReaderFromArray([]*schema.Message{schema.AssistantMessage(stub.reply, nil)}), nil
}

func (stub *restateStubModel) callCount() int {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	return stub.calls
}

func TestFinalReplyReflectionLeakDetection(t *testing.T) {
	t.Parallel()
	leaking := []string{
		"我把开头删了。但等等，这里还有一个问题：源帧对不上。",
		"让我再确认一下时间线对齐。",
		"Done. wait, that's not right.",
	}
	clean := []string{
		"已把开头三秒删掉,并对齐了字幕。",
		"导出完成,成片时长 45 秒。",
	}
	for _, reply := range leaking {
		if !finalReplyHasReflectionLeak(reply) {
			t.Fatalf("应判为反思泄漏: %q", reply)
		}
	}
	for _, reply := range clean {
		if finalReplyHasReflectionLeak(reply) {
			t.Fatalf("应判为干净: %q", reply)
		}
	}
}

func TestQualityCheckedFinalReplyRestatesOnlyOnLeak(t *testing.T) {
	t.Parallel()
	newService := func(stub model.ToolCallingChatModel) *Service {
		database, err := storage.Open(t.Context(), t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = database.Close() })
		service, err := NewService(t.Context(), database, stub)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(service.Close)
		return service
	}
	ctx := t.Context()

	// 正常回复:零模型调用(零额外延迟),原样返回。
	clean := &restateStubModel{reply: "无关"}
	service := newService(clean)
	if out, restated := service.qualityCheckedFinalReply(ctx, "d", "m", "已删掉开头三秒。"); out != "已删掉开头三秒。" || restated {
		t.Fatalf("clean out=%q restated=%v", out, restated)
	}
	if clean.callCount() != 0 {
		t.Fatalf("正常回复不应触发重述模型调用: %d", clean.callCount())
	}

	// 泄漏 + 干净重述:采用重述,restated=true,恰好 1 次调用。
	fixed := &restateStubModel{reply: "已把开头三秒删掉,并对齐字幕。"}
	service = newService(fixed)
	out, restated := service.qualityCheckedFinalReply(ctx, "d", "m", "删了开头。但等等,字幕没对齐?")
	if out != "已把开头三秒删掉,并对齐字幕。" || !restated {
		t.Fatalf("leak out=%q restated=%v", out, restated)
	}
	if fixed.callCount() != 1 {
		t.Fatalf("应恰好 1 次重述调用: %d", fixed.callCount())
	}

	// 泄漏 + 重述仍泄漏:原样放行(不采用)。
	stillLeak := &restateStubModel{reply: "让我再确认一下。"}
	service = newService(stillLeak)
	original := "删了开头。但等等。"
	if out, restated := service.qualityCheckedFinalReply(ctx, "d", "m", original); out != original || restated {
		t.Fatalf("still-leak out=%q restated=%v", out, restated)
	}

	// 泄漏 + 模型出错:原样放行。
	failing := &restateStubModel{err: errors.New("boom")}
	service = newService(failing)
	if out, restated := service.qualityCheckedFinalReply(ctx, "d", "m", original); out != original || restated {
		t.Fatalf("error out=%q restated=%v", out, restated)
	}
}
