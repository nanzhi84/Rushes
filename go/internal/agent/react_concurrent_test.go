package agent

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

// g3bWireModel 是驱动复刻图的脚本模型:第 1 轮发全只读多工具消息(路由并行节点),第 2 轮发
// 读写混合消息(路由串行节点),第 3 轮收尾。它把每轮看到的工具结果 tool_call_id 顺序记下来,
// 供断言「两种路由都按原下标保序回灌」。
type g3bWireModel struct {
	mu              sync.Mutex
	round           int
	round1ToolOrder []string
	round2ToolOrder []string
}

func (scriptModel *g3bWireModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return scriptModel, nil
}

func (scriptModel *g3bWireModel) Generate(
	_ context.Context, messages []*schema.Message, _ ...model.Option,
) (*schema.Message, error) {
	scriptModel.mu.Lock()
	defer scriptModel.mu.Unlock()
	scriptModel.round++
	switch scriptModel.round {
	case 1:
		return schema.AssistantMessage("", []schema.ToolCall{
			{ID: "c_list", Function: schema.FunctionCall{Name: "asset.list_assets", Arguments: "{}"}},
			{ID: "c_inspect", Function: schema.FunctionCall{Name: "timeline.inspect", Arguments: "{}"}},
		}), nil
	case 2:
		scriptModel.round1ToolOrder = trailingToolCallIDs(messages, 2)
		return schema.AssistantMessage("", []schema.ToolCall{
			{ID: "c_inspect2", Function: schema.FunctionCall{Name: "timeline.inspect", Arguments: "{}"}},
			{ID: "c_plan", Function: schema.FunctionCall{Name: "plan.update", Arguments: `{"plan":{"pacing":"fast"}}`}},
		}), nil
	case 3:
		scriptModel.round2ToolOrder = trailingToolCallIDs(messages, 2)
		return schema.AssistantMessage("完成", nil), nil
	default:
		return nil, fmt.Errorf("脚本模型收到额外的第 %d 次调用", scriptModel.round)
	}
}

func (scriptModel *g3bWireModel) Stream(
	ctx context.Context, messages []*schema.Message, options ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	message, err := scriptModel.Generate(ctx, messages, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func trailingToolCallIDs(messages []*schema.Message, count int) []string {
	ids := []string{}
	for _, message := range messages {
		if message.Role == schema.Tool {
			ids = append(ids, message.ToolCallID)
		}
	}
	if len(ids) > count {
		ids = ids[len(ids)-count:]
	}
	return ids
}

// TestConcurrentReactAgentRoutesAndPreservesOrder 是复刻图的端到端集成测试(补 #129 审查点名的
// 覆盖缺口):经真实 Service 驱动复刻图,验证只读轮走并行、混合轮走串行,两种路由的工具结果都按原
// 下标保序回灌,多轮循环正常收尾,且每个工具都发出成对的 started/finished(并发下顺序无关)。
func TestConcurrentReactAgentRoutesAndPreservesOrder(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_g3b_wire")
	scriptModel := &g3bWireModel{}
	service, err := NewService(t.Context(), database, scriptModel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)

	var eventsMu sync.Mutex
	events := map[string]int{}
	ctx := rushestools.WithReporter(
		withTurnBudgetState(
			withToolRecoveryState(
				rushestools.WithDraftID(t.Context(), "draft_g3b_wire"),
				newToolRecoveryState()),
			newTurnBudgetState(maxToolRoundsPerTurn)),
		func(_ context.Context, name, phase string, _, _ any, _ error) {
			eventsMu.Lock()
			events[name+":"+phase]++
			eventsMu.Unlock()
		})

	response, err := service.react.Generate(ctx, []*schema.Message{
		schema.UserMessage("列素材、看时间线并记录计划。"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if response == nil || response.Content != "完成" {
		t.Fatalf("response=%#v", response)
	}

	// 两种路由都按 tool_calls 原下标保序回灌结果。
	if !reflect.DeepEqual(scriptModel.round1ToolOrder, []string{"c_list", "c_inspect"}) {
		t.Fatalf("只读并行轮结果未保序: %v", scriptModel.round1ToolOrder)
	}
	if !reflect.DeepEqual(scriptModel.round2ToolOrder, []string{"c_inspect2", "c_plan"}) {
		t.Fatalf("混合串行轮结果未保序: %v", scriptModel.round2ToolOrder)
	}

	// 并发只读轮下,两个工具各发出成对 started/finished（配对按名字,顺序无关）。
	eventsMu.Lock()
	defer eventsMu.Unlock()
	for _, pair := range []string{
		"asset.list_assets:started", "asset.list_assets:finished",
		"timeline.inspect:started", "timeline.inspect:finished",
		"plan.update:started", "plan.update:finished",
	} {
		if events[pair] < 1 {
			t.Fatalf("缺少工具事件 %s: events=%#v", pair, events)
		}
	}

	// 计划确实经串行轮的 reducer 落盘。
	draft, err := storage.GetDraft(t.Context(), database.Read(), "draft_g3b_wire")
	if err != nil {
		t.Fatal(err)
	}
	if draft.ContentPlan["pacing"] != "fast" {
		t.Fatalf("混合轮的 plan.update 未落盘: %#v", draft.ContentPlan)
	}
}
