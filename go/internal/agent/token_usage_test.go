package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

type usageServiceModel struct {
	mu    sync.Mutex
	calls int
}

func (stub *usageServiceModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return stub, nil
}

func (stub *usageServiceModel) Generate(
	context.Context,
	[]*schema.Message,
	...model.Option,
) (*schema.Message, error) {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	stub.calls++
	var response *schema.Message
	if stub.calls == 1 {
		response = schema.AssistantMessage("", []schema.ToolCall{{
			ID: "usage_list", Function: schema.FunctionCall{Name: "asset.list_assets", Arguments: `{}`},
		}})
	} else {
		response = schema.AssistantMessage("已完成用量统计。", nil)
	}
	response.ResponseMeta = &schema.ResponseMeta{Usage: &schema.TokenUsage{
		PromptTokens:       stub.calls * 100,
		PromptTokenDetails: schema.PromptTokenDetails{CachedTokens: stub.calls * 40},
		CompletionTokens:   stub.calls * 10,
		TotalTokens:        stub.calls * 110,
	}}
	return response, nil
}

func (stub *usageServiceModel) Stream(
	ctx context.Context,
	messages []*schema.Message,
	options ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	response, err := stub.Generate(ctx, messages, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{response}), nil
}

func TestTurnEndedReportsAccumulatedTokenUsage(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_token_usage")
	service, err := NewService(t.Context(), database, &usageServiceModel{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	_, stream, unsubscribe := service.Hub().Subscribe("draft_token_usage")
	defer unsubscribe()
	if !service.Queue().EnqueueUserMessage("draft_token_usage", "user_token_usage", "列出素材") {
		t.Fatal("enqueue failed")
	}
	service.Queue().JoinDraft("draft_token_usage")

	var turnEnded StreamEvent
	for turnEnded == nil {
		select {
		case event := <-stream:
			if event["type"] == "turn_ended" {
				turnEnded = event
			}
		case <-time.After(3 * time.Second):
			t.Fatal("等待 turn_ended 超时")
		}
	}
	usage, _ := turnEnded["token_usage"].(map[string]any)
	if usage["model_calls"] != 2 || usage["prompt_tokens"] != 300 ||
		usage["cached_prompt_tokens"] != 120 || usage["completion_tokens"] != 30 ||
		usage["total_tokens"] != 330 {
		t.Fatalf("turn_ended=%#v", turnEnded)
	}
}

func TestMissingModelUsageIsSafelyIgnored(t *testing.T) {
	t.Parallel()
	state := newTurnBudgetState(5)
	recordModelResponseUsage(withTurnBudgetState(t.Context(), state), schema.AssistantMessage("ok", nil))
	if usage := state.usageSnapshot(); usage != nil {
		t.Fatalf("missing provider usage should remain absent: %#v", usage)
	}
}

type contextSummaryModel struct {
	content string
	err     error
}

func (stub *contextSummaryModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return stub, nil
}

func (stub *contextSummaryModel) Generate(
	context.Context,
	[]*schema.Message,
	...model.Option,
) (*schema.Message, error) {
	if stub.err != nil {
		return nil, stub.err
	}
	return schema.AssistantMessage(stub.content, nil), nil
}

func (stub *contextSummaryModel) Stream(
	context.Context,
	[]*schema.Message,
	...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	return nil, errors.New("unused")
}

func TestContextSummaryFailureIsObservableAndBounded(t *testing.T) {
	t.Parallel()
	service := &Service{
		chatModel: &contextSummaryModel{err: errors.New("provider unavailable")},
		hub:       NewTurnStreamHub(0),
	}
	summary := service.contextSummary(t.Context(), "draft_compaction_failure", strings.Repeat("历史", 5000))
	prefixRunes := utf8.RuneCountInString(deterministicContextSummary(""))
	if !strings.Contains(summary, "自动语义压缩不可用") ||
		utf8.RuneCountInString(summary) > prefixRunes+contextCompactionFallbackRuneLimit+1 {
		t.Fatalf("fallback summary runes=%d", utf8.RuneCountInString(summary))
	}
	events := service.hub.Snapshot("draft_compaction_failure")
	if len(events) != 1 || events[0]["type"] != "context_compaction_failed" ||
		events[0]["fallback"] != "deterministic_bounded_summary" {
		t.Fatalf("events=%#v", events)
	}

	service.chatModel = &contextSummaryModel{content: strings.Repeat("摘要", 5000)}
	bounded := service.contextSummary(t.Context(), "draft_compaction_success", "source")
	if utf8.RuneCountInString(bounded) > contextCompactionSummaryRuneLimit+1 {
		t.Fatalf("model summary not bounded: %d", utf8.RuneCountInString(bounded))
	}
	if !strings.Contains(contextCompactionPrompt, "content_plan 已持久保存的决定不要重复写入摘要") {
		t.Fatal("compaction prompt lost content_plan dedup guard")
	}
	if !strings.Contains(contextCompactionPrompt, "user_memories 已持久保存的偏好不要重复写入摘要") ||
		!strings.Contains(contextCompactionPrompt, "只保留尚未固化的新偏好") {
		t.Fatal("compaction prompt lost user memory dedup guard")
	}
}

func TestContextSummaryFallbackKeepsLatestCorrection(t *testing.T) {
	t.Parallel()
	latest := "[user:latest]\n用户纠正：最终必须保留最新片尾"
	source := "[上一份交接]\n" + strings.Repeat("旧交接", contextCompactionFallbackRuneLimit) + "\n\n" + latest
	summary := deterministicContextSummary(source)
	if !strings.Contains(summary, latest) {
		t.Fatalf("fallback dropped latest correction: %q", summary)
	}
}

type cancelAfterUsageModel struct {
	mu      sync.Mutex
	calls   int
	blocked chan struct{}
}

func (stub *cancelAfterUsageModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return stub, nil
}

func (stub *cancelAfterUsageModel) Generate(ctx context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	stub.mu.Lock()
	stub.calls++
	call := stub.calls
	stub.mu.Unlock()
	if call == 1 {
		response := schema.AssistantMessage("", []schema.ToolCall{{
			ID: "usage_then_cancel", Function: schema.FunctionCall{Name: "asset.list_assets", Arguments: `{}`},
		}})
		response.ResponseMeta = &schema.ResponseMeta{Usage: &schema.TokenUsage{
			PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12,
		}}
		return response, nil
	}
	close(stub.blocked)
	<-ctx.Done()
	return nil, ctx.Err()
}

func (stub *cancelAfterUsageModel) Stream(ctx context.Context, messages []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	response, err := stub.Generate(ctx, messages, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{response}), nil
}

func TestCancelledTurnReportsUsageAlreadyProduced(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_cancelled_usage")
	stub := &cancelAfterUsageModel{blocked: make(chan struct{})}
	service, err := NewService(t.Context(), database, stub)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	_, stream, unsubscribe := service.Hub().Subscribe("draft_cancelled_usage")
	defer unsubscribe()
	if !service.Queue().EnqueueUserMessage("draft_cancelled_usage", "user_cancelled_usage", "列出素材后等待") {
		t.Fatal("enqueue failed")
	}
	select {
	case <-stub.blocked:
	case <-time.After(3 * time.Second):
		t.Fatal("second model call did not start")
	}
	if !service.Queue().RequestStop("draft_cancelled_usage") {
		t.Fatal("cancel request failed")
	}
	service.Queue().JoinDraft("draft_cancelled_usage")
	for {
		select {
		case event := <-stream:
			if event["type"] != "turn_ended" {
				continue
			}
			usage, _ := event["token_usage"].(map[string]any)
			if event["outcome"] != "cancelled" || usage["model_calls"] != 1 || usage["total_tokens"] != 12 {
				t.Fatalf("event=%#v", event)
			}
			return
		case <-time.After(3 * time.Second):
			t.Fatal("waiting cancelled turn_ended timed out")
		}
	}
}

const modelToolSchemaTotalBaselineRunes = 16957

var modelToolSchemaBaselineRunes = map[string]int{
	"asset.list_assets":           435,
	"audio.analyze_beats":         493,
	"audio.analyze_speech_pauses": 798,
	"decision.answer":             566,
	"interaction.ask_user":        1079,
	"interaction.confirm_action":  387,
	"job.read":                    246,
	"media.detect_shots":          813,
	"memory.remove":               418,
	"memory.set":                  929,
	"plan.update":                 1573,
	"preview.check":               442,
	"render.start":                522,
	"shot.search":                 870,
	"speech.search":               1144,
	"speech.transcribe":           486,
	"timeline.check":              199,
	"timeline.delete":             790,
	"timeline.insert":             1114,
	"timeline.inspect":            195,
	"timeline.split":              473,
	"timeline.update":             2985,
}

func modelToolSchemaRuneLimit(baseline int) int {
	percentLimit := (baseline*110 + 99) / 100
	absoluteLimit := baseline + 499
	if percentLimit < absoluteLimit {
		return percentLimit
	}
	return absoluteLimit
}

func TestModelToolSchemaRuneBudgetCoversEveryLLMTool(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	want := 0
	for _, spec := range service.tools.Specs(true) {
		if spec.Exposure == "llm" {
			want++
		}
	}
	metrics, err := modelToolSchemaSize(t.Context(), service.tools)
	if err != nil || len(metrics.PerToolRunes) != want || want != len(modelToolSchemaBaselineRunes) {
		t.Fatalf("schema metrics=%#v count=%d want=%d err=%v", metrics, len(metrics.PerToolRunes), want, err)
	}
	if limit := modelToolSchemaRuneLimit(modelToolSchemaTotalBaselineRunes); metrics.TotalRunes > limit {
		t.Errorf("total schema runes=%d exceeds limit=%d baseline=%d", metrics.TotalRunes, limit, modelToolSchemaTotalBaselineRunes)
	} else if metrics.TotalRunes < modelToolSchemaTotalBaselineRunes {
		t.Errorf("total schema runes shrank to %d; lower the reviewed baseline %d in this change", metrics.TotalRunes, modelToolSchemaTotalBaselineRunes)
	}
	for name, runes := range metrics.PerToolRunes {
		if strings.HasPrefix(name, "timeline.") &&
			(name == "timeline.insert" || name == "timeline.delete" ||
				name == "timeline.update" || name == "timeline.split") &&
			runes > 3000 {
			t.Errorf("tool %s schema runes=%d exceeds hard per-tool limit=3000", name, runes)
		}
		baseline, exists := modelToolSchemaBaselineRunes[name]
		if !exists {
			t.Errorf("LLM tool %s has no reviewed schema baseline", name)
			continue
		}
		limit := modelToolSchemaRuneLimit(baseline)
		if runes > limit {
			t.Errorf("tool %s schema runes=%d exceeds limit=%d baseline=%d", name, runes, limit, baseline)
		} else if runes < baseline {
			t.Errorf("tool %s schema runes shrank to %d; lower the reviewed baseline %d in this change", name, runes, baseline)
		}
		if baseline+500 <= limit {
			t.Errorf("tool %s budget would allow an unreviewed 500-rune increase", name)
		}
	}
	for name := range modelToolSchemaBaselineRunes {
		if _, exists := metrics.PerToolRunes[name]; !exists {
			t.Errorf("schema baseline for removed or hidden tool %s must be reviewed", name)
		}
	}
}

type toolSurfaceBindingModel struct {
	withToolsErr error
	boundNames   []string
}

func (stub *toolSurfaceBindingModel) WithTools(infos []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	if stub.withToolsErr != nil {
		return nil, stub.withToolsErr
	}
	stub.boundNames = make([]string, 0, len(infos))
	for _, info := range infos {
		stub.boundNames = append(stub.boundNames, info.Name)
	}
	return stub, nil
}

func (*toolSurfaceBindingModel) Generate(
	context.Context,
	[]*schema.Message,
	...model.Option,
) (*schema.Message, error) {
	return schema.AssistantMessage("ok", nil), nil
}

func (stub *toolSurfaceBindingModel) Stream(
	ctx context.Context,
	messages []*schema.Message,
	options ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	message, err := stub.Generate(ctx, messages, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func TestModelToolSurfaceMetricsFollowSuccessfulGraphBinding(t *testing.T) {
	// 写入哨兵值，确保下面的无模型构造本身刷新 Catalog，而不是误读更早测试
	// 留下的进程级 gauge 值。一次性约束仅用于目录日志，不阻止 gauge 刷新。
	metricModelToolCatalogCount.Set(-1)
	metricModelToolCatalogSchemaRunes.Set(-1)

	boundCountBefore, boundCountSumBefore, _, _ := metricModelToolBoundCount.Snapshot()
	boundRunesBefore, boundRunesSumBefore, _, _ := metricModelToolBoundSchemaRunes.Snapshot()

	nilModelDatabase := agenttest.AgentTestDatabase(t)
	nilModelService, err := NewService(t.Context(), nilModelDatabase, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nilModelService.Close)
	nilModelCatalog, err := modelToolSchemaSize(t.Context(), nilModelService.tools)
	if err != nil {
		t.Fatal(err)
	}
	if got := metricModelToolCatalogCount.Value(); got != int64(len(nilModelCatalog.PerToolRunes)) {
		t.Fatalf("无模型模式 catalog count gauge=%d want=%d", got, len(nilModelCatalog.PerToolRunes))
	}
	if got := metricModelToolCatalogSchemaRunes.Value(); got != int64(nilModelCatalog.TotalRunes) {
		t.Fatalf("无模型模式 catalog schema runes gauge=%d want=%d", got, nilModelCatalog.TotalRunes)
	}
	if count, sum, _, _ := metricModelToolBoundCount.Snapshot(); count != boundCountBefore || sum != boundCountSumBefore {
		t.Fatalf("无模型模式不应记录 bound count: before=(%d,%d) after=(%d,%d)",
			boundCountBefore, boundCountSumBefore, count, sum)
	}
	if count, sum, _, _ := metricModelToolBoundSchemaRunes.Snapshot(); count != boundRunesBefore || sum != boundRunesSumBefore {
		t.Fatalf("无模型模式不应记录 bound schema runes: before=(%d,%d) after=(%d,%d)",
			boundRunesBefore, boundRunesSumBefore, count, sum)
	}

	failingDatabase := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, failingDatabase, "draft_surface_failure")
	failingModel := &toolSurfaceBindingModel{withToolsErr: errors.New("bind failed")}
	failingService, err := NewService(t.Context(), failingDatabase, failingModel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(failingService.Close)
	failingCtx := rushestools.WithDraftID(t.Context(), "draft_surface_failure")
	if _, err := failingService.react.Generate(failingCtx, []*schema.Message{
		schema.UserMessage("列出素材"),
	}); err == nil {
		t.Fatal("动态模型工具绑定失败时 Generate 应返回错误")
	}
	if count, sum, _, _ := metricModelToolBoundCount.Snapshot(); count != boundCountBefore || sum != boundCountSumBefore {
		t.Fatalf("失败建图不应记录 bound count: before=(%d,%d) after=(%d,%d)",
			boundCountBefore, boundCountSumBefore, count, sum)
	}
	if count, sum, _, _ := metricModelToolBoundSchemaRunes.Snapshot(); count != boundRunesBefore || sum != boundRunesSumBefore {
		t.Fatalf("失败建图不应记录 bound schema runes: before=(%d,%d) after=(%d,%d)",
			boundRunesBefore, boundRunesSumBefore, count, sum)
	}

	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_surface_success")
	successModel := &toolSurfaceBindingModel{}
	service, err := NewService(t.Context(), database, successModel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)

	catalog, err := modelToolSchemaSize(t.Context(), service.tools)
	if err != nil {
		t.Fatal(err)
	}
	messages := []*schema.Message{schema.UserMessage("列出素材")}
	successCtx := rushestools.WithDraftID(t.Context(), "draft_surface_success")
	specs, err := selectModelToolSurface(successCtx, service.tools, messages)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := modelToolSchemaSizeFromTools(t.Context(), implementationsForSpecs(specs))
	if err != nil {
		t.Fatal(err)
	}
	if got := metricModelToolCatalogCount.Value(); got != int64(len(catalog.PerToolRunes)) {
		t.Fatalf("catalog count gauge=%d want=%d", got, len(catalog.PerToolRunes))
	}
	if got := metricModelToolCatalogSchemaRunes.Value(); got != int64(catalog.TotalRunes) {
		t.Fatalf("catalog schema runes gauge=%d want=%d", got, catalog.TotalRunes)
	}
	if _, err := service.react.Generate(successCtx, messages); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(successModel.boundNames, bound.Names) {
		t.Fatalf("模型实际收到工具=%v want=%v", successModel.boundNames, bound.Names)
	}
	if count, sum, _, _ := metricModelToolBoundCount.Snapshot(); count != boundCountBefore+1 ||
		sum != boundCountSumBefore+int64(len(bound.PerToolRunes)) {
		t.Fatalf("bound count after=(%d,%d) want=(%d,%d)",
			count, sum, boundCountBefore+1, boundCountSumBefore+int64(len(bound.PerToolRunes)))
	}
	if count, sum, _, _ := metricModelToolBoundSchemaRunes.Snapshot(); count != boundRunesBefore+1 ||
		sum != boundRunesSumBefore+int64(bound.TotalRunes) {
		t.Fatalf("bound schema runes after=(%d,%d) want=(%d,%d)",
			count, sum, boundRunesBefore+1, boundRunesSumBefore+int64(bound.TotalRunes))
	}
}
