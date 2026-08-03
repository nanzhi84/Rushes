package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/media"
	"github.com/nanzhi84/Rushes/go/internal/providers"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

const (
	liveToolStabilityTarget = 0.99
	liveWorkflowMinimumRuns = 5
	liveWorkflowTimelineFPS = 30
)

var retiredLLMToolNames = []string{
	"media.search_shots",
	"memory.update",
	"render.final_mp4",
	"render.inspect_preview",
	"render.preview",
	"render.status",
	"speech.inspect",
	"timeline.apply_patch",
	"timeline.compose_initial",
	"timeline.apply_patches",
	"timeline.recut_to_beats",
	"timeline.edit_talking_head",
	"timeline.validate",
	"understand.materials",
}

type liveToolEvalCase struct {
	Name              string
	Prompt            string
	Expected          []string
	Snapshot          WorldStateSnapshot
	ValidateArguments func(string) error
}

type liveToolEvalFailure struct {
	Suite    string `json:"suite"`
	Case     string `json:"case"`
	Run      int    `json:"run"`
	Expected string `json:"expected"`
	Actual   string `json:"actual,omitempty"`
	Error    string `json:"error,omitempty"`
}

type liveToolEvalMetric struct {
	Succeeded int     `json:"succeeded"`
	Total     int     `json:"total"`
	Rate      float64 `json:"rate"`
}

type liveToolEvalReport struct {
	GeneratedAt     string                        `json:"generated_at"`
	Model           string                        `json:"model"`
	CatalogLoad     liveToolEvalMetric            `json:"catalog_tool_load"`
	CatalogCases    map[string]liveToolEvalMetric `json:"catalog_tool_load_cases,omitempty"`
	CatalogCompare  *liveCatalogComparison        `json:"catalog_prompt_comparison,omitempty"`
	Schema          liveToolEvalMetric            `json:"schema"`
	SchemaCases     map[string]liveToolEvalMetric `json:"schema_cases,omitempty"`
	Routing         liveToolEvalMetric            `json:"routing"`
	RoutingCases    map[string]liveToolEvalMetric `json:"routing_cases,omitempty"`
	RoutingVariants map[string]liveToolEvalMetric `json:"routing_variants,omitempty"`
	PerKind         map[string]liveToolEvalMetric `json:"timeline_op_per_kind,omitempty"`
	Budget          liveToolEvalMetric            `json:"budget_zero_tool_calls"`
	Workflows       map[string]liveToolEvalMetric `json:"workflows,omitempty"`
	WorkflowRuns    []liveWorkflowRunReport       `json:"workflow_runs,omitempty"`
	Failures        []liveToolEvalFailure         `json:"failures,omitempty"`
}

type liveCatalogComparison struct {
	FullSchemaRunes                 int     `json:"full_13_schema_runes"`
	CatalogPromptRunes              int     `json:"catalog_prompt_runes"`
	InitialBoundSchemaRunes         int     `json:"initial_tool_load_schema_runes"`
	AverageLoadedBoundSchemaRunes   float64 `json:"average_loaded_bound_schema_runes"`
	AverageFullPromptTokens         float64 `json:"average_full_13_prompt_tokens"`
	AverageInitialPromptTokens      float64 `json:"average_initial_prompt_tokens"`
	AverageLoadedPromptTokens       float64 `json:"average_loaded_prompt_tokens"`
	AverageFullProviderLatencyMS    float64 `json:"average_full_13_provider_latency_ms"`
	AverageInitialProviderLatencyMS float64 `json:"average_initial_provider_latency_ms"`
	AverageLoadedProviderLatencyMS  float64 `json:"average_loaded_provider_latency_ms"`
	AdditionalProviderRoundTrips    int     `json:"additional_provider_round_trips"`
	ObservedAttempts                int     `json:"observed_attempts"`
	FullSamples                     int     `json:"full_13_samples"`
	LoadedSamples                   int     `json:"loaded_samples"`
}

type liveCatalogComparisonAccumulator struct {
	liveCatalogComparison
	loadedBoundSchemaRunesTotal int
	loadedSchemaSamples         int
	fullPromptTokensTotal       int
	fullPromptTokenSamples      int
	initialPromptTokensTotal    int
	initialPromptTokenSamples   int
	loadedPromptTokensTotal     int
	loadedPromptTokenSamples    int
	initialLatencyMSTotal       int64
	fullLatencyMSTotal          int64
	loadedLatencyMSTotal        int64
}

func TestLiveCatalogToolLoadStability(t *testing.T) {
	service, chat, modelName := newLiveToolEvalHarness(t)
	report := liveToolEvalReport{
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339Nano),
		Model:        modelName,
		CatalogCases: map[string]liveToolEvalMetric{},
	}
	runLiveCatalogToolLoadEvaluation(t, &report, service, chat)
	writeLiveToolEvalReport(t, report)
	failed := []string{}
	for _, evalCase := range liveCatalogToolLoadCases() {
		metric := report.CatalogCases[evalCase.Name]
		t.Logf(
			"CATALOG_TOOL_LOAD_RESULT model=%s case=%s expected=%s succeeded=%d/%d rate=%.2f%%",
			modelName, evalCase.Name, evalCase.Expected[0], metric.Succeeded, metric.Total,
			metric.Rate*100,
		)
		if metric.Rate < liveToolStabilityTarget {
			failed = append(failed, evalCase.Name)
		}
	}
	if len(failed) > 0 {
		encoded, _ := json.Marshal(report.Failures)
		t.Fatalf(
			"真实 Catalog/tool.load 稳定性低于 %.0f%%（cases=%s）: %s",
			liveToolStabilityTarget*100, strings.Join(failed, ","), encoded,
		)
	}
}

func runLiveCatalogToolLoadEvaluation(
	t *testing.T,
	report *liveToolEvalReport,
	service *Service,
	chat model.ToolCallingChatModel,
) {
	t.Helper()
	const draftID = "draft_live_catalog_tool_load"
	agenttest.CreateAgentDraft(t, service.database, draftID)
	comparison := newLiveCatalogComparisonAccumulator(t, service)
	runs := liveEvalRuns()
	for _, evalCase := range liveCatalogToolLoadCases() {
		expected := evalCase.Expected[0]
		metric := liveToolEvalMetric{}
		for run := 1; run <= runs; run++ {
			metric.Total++
			report.CatalogLoad.Total++
			actual, evalErr := liveCatalogToolLoadAttempt(
				t.Context(), service, chat, draftID, evalCase, comparison,
			)
			if evalErr == nil {
				metric.Succeeded++
				report.CatalogLoad.Succeeded++
				continue
			}
			report.Failures = append(report.Failures, liveToolEvalFailure{
				Suite: "catalog_tool_load", Case: evalCase.Name, Run: run,
				Expected: expected, Actual: actual, Error: evalErr.Error(),
			})
		}
		metric.Rate = ratio(metric.Succeeded, metric.Total)
		report.CatalogCases[evalCase.Name] = metric
	}
	report.CatalogLoad.Rate = ratio(report.CatalogLoad.Succeeded, report.CatalogLoad.Total)
	report.CatalogCompare = comparison.result()
}

func liveCatalogToolLoadAttempt(
	parent context.Context,
	service *Service,
	chat model.ToolCallingChatModel,
	draftID string,
	evalCase liveToolEvalCase,
	comparison *liveCatalogComparisonAccumulator,
) (string, error) {
	expected := evalCase.Expected[0]
	loadSpec, exists := service.tools.Spec("tool.load")
	if !exists || loadSpec.Exposure != rushestools.ExposureMeta {
		return "", errors.New("Registry 缺少 tool.load meta action")
	}
	loadInfo, err := loadSpec.Implementation.Info(parent)
	if err != nil {
		return "", err
	}
	loadOnly, err := chat.WithTools([]*schema.ToolInfo{loadInfo})
	if err != nil {
		return "", err
	}
	catalogPrompt, err := modelActionCatalogPrompt(service.tools)
	if err != nil {
		return "", err
	}
	snapshot, err := json.Marshal(evalCase.Snapshot)
	if err != nil {
		return "", err
	}
	baselineMessages := []*schema.Message{
		schema.SystemMessage(coreSystemPrompt),
		schema.SystemMessage("【WorldState 参考快照】\n" + string(snapshot)),
	}
	if playbook := taskPlaybookMessage(evalCase.Snapshot); playbook != nil {
		baselineMessages = append(baselineMessages, playbook)
	}
	baselineMessages = append(baselineMessages, schema.UserMessage(evalCase.Prompt))
	fullSpecs := liveAllModelActionSpecs(service.tools)
	fullBound, err := bindLiveWorkflowTools(parent, chat, fullSpecs)
	if err != nil {
		return "", err
	}
	fullStartedAt := time.Now()
	fullResponse, err := liveGenerateResponse(parent, fullBound, baselineMessages,
		model.WithToolChoice(schema.ToolChoiceForced, expected))
	comparison.observeFull(fullResponse, time.Since(fullStartedAt))
	if err != nil {
		return "", err
	}
	if len(fullResponse.ToolCalls) != 1 || fullResponse.ToolCalls[0].Function.Name != expected {
		return toolCallNames(fullResponse), fmt.Errorf(
			"全量 13 schema 基线 action 调用错误: actual=%s expected=%s",
			toolCallNames(fullResponse), expected,
		)
	}
	messages := append([]*schema.Message{baselineMessages[0], schema.SystemMessage(catalogPrompt)}, baselineMessages[1:]...)
	initialStartedAt := time.Now()
	response, err := liveGenerateResponse(parent, loadOnly, messages,
		model.WithToolChoice(schema.ToolChoiceForced, "tool.load"))
	comparison.observeInitial(response, time.Since(initialStartedAt))
	if err != nil {
		return "", err
	}
	if len(response.ToolCalls) != 1 || response.ToolCalls[0].Function.Name != "tool.load" {
		return toolCallNames(response), fmt.Errorf("首次应只调用 tool.load，实际=%s", toolCallNames(response))
	}
	call := response.ToolCalls[0]
	argumentObject := map[string]any{}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &argumentObject); err != nil {
		return call.Function.Arguments, fmt.Errorf("tool.load 参数不是 JSON 对象: %w", err)
	}
	decoded, err := service.tools.DecodeInput("tool.load", argumentObject)
	if err != nil {
		return call.Function.Arguments, fmt.Errorf("tool.load 参数不符合固定 schema: %w", err)
	}
	input, ok := decoded.(rushestools.ToolLoadInput)
	if !ok {
		return call.Function.Arguments, fmt.Errorf("tool.load 解码类型=%T", decoded)
	}
	if !reflect.DeepEqual(input.ToolNames, []string{expected}) {
		return strings.Join(input.ToolNames, ","), fmt.Errorf(
			"Catalog action 选择错误: loaded=%v expected=[%s]", input.ToolNames, expected,
		)
	}
	ctx := rushestools.WithDraftID(withToolDisclosureSession(parent), draftID)
	rawResult, err := service.ExecuteTool(ctx, "tool.load", input)
	if err != nil {
		return strings.Join(input.ToolNames, ","), err
	}
	result, ok := rawResult.(rushestools.ToolLoadResult)
	if !ok || result.Status != string(rushestools.StatusSucceeded) ||
		!reflect.DeepEqual(result.LoadedNames, []string{expected}) || len(result.NotLoadable) != 0 {
		return strings.Join(input.ToolNames, ","), fmt.Errorf("tool.load 回执异常: %#v", rawResult)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return expected, err
	}
	messages = append(messages, response, schema.ToolMessage(
		string(encoded), call.ID, schema.WithToolName("tool.load"),
	))
	loadedSpecs, err := loadedModelActionSpecs(ctx, service.tools, messages)
	if err != nil || len(loadedSpecs) != 1 || loadedSpecs[0].Name != expected {
		return expected, fmt.Errorf("transcript 未确定性披露期望 schema: specs=%v err=%v", liveWorkflowSpecNames(loadedSpecs), err)
	}
	actionInfo, err := loadedSpecs[0].Implementation.Info(parent)
	if err != nil {
		return expected, err
	}
	loaded, err := chat.WithTools([]*schema.ToolInfo{loadInfo, actionInfo})
	if err != nil {
		return expected, err
	}
	boundMetrics, err := modelToolSchemaSizeFromTools(parent, []tool.BaseTool{
		loadSpec.Implementation, loadedSpecs[0].Implementation,
	})
	if err != nil {
		return expected, err
	}
	comparison.observeLoadedSchema(boundMetrics.TotalRunes)
	actionStartedAt := time.Now()
	actionResponse, err := liveGenerateResponse(parent, loaded, messages,
		model.WithToolChoice(schema.ToolChoiceForced, expected))
	comparison.observeLoaded(actionResponse, time.Since(actionStartedAt))
	if err != nil {
		return expected, err
	}
	if len(actionResponse.ToolCalls) != 1 || actionResponse.ToolCalls[0].Function.Name != expected {
		return toolCallNames(actionResponse), fmt.Errorf(
			"加载 schema 后 action 调用错误: actual=%s expected=%s",
			toolCallNames(actionResponse), expected,
		)
	}
	if err := validateLiveToolArguments(loadedSpecs[0], actionResponse.ToolCalls[0].Function.Arguments); err != nil {
		return expected, fmt.Errorf("加载 schema 后参数无效: %w", err)
	}
	if evalCase.ValidateArguments != nil {
		if err := evalCase.ValidateArguments(actionResponse.ToolCalls[0].Function.Arguments); err != nil {
			return expected, err
		}
	}
	return expected, nil
}

func newLiveCatalogComparisonAccumulator(
	t *testing.T,
	service *Service,
) *liveCatalogComparisonAccumulator {
	t.Helper()
	full, err := modelToolSchemaSize(t.Context(), service.tools)
	if err != nil {
		t.Fatal(err)
	}
	loadSpec, exists := service.tools.Spec("tool.load")
	if !exists {
		t.Fatal("tool.load missing")
	}
	initial, err := modelToolSchemaSizeFromTools(t.Context(), []tool.BaseTool{loadSpec.Implementation})
	if err != nil {
		t.Fatal(err)
	}
	catalogPrompt, err := modelActionCatalogPrompt(service.tools)
	if err != nil {
		t.Fatal(err)
	}
	return &liveCatalogComparisonAccumulator{liveCatalogComparison: liveCatalogComparison{
		FullSchemaRunes: full.TotalRunes, CatalogPromptRunes: utf8.RuneCountInString(catalogPrompt),
		InitialBoundSchemaRunes: initial.TotalRunes, AdditionalProviderRoundTrips: 1,
	}}
}

func (comparison *liveCatalogComparisonAccumulator) observeInitial(
	response *schema.Message,
	latency time.Duration,
) {
	comparison.ObservedAttempts++
	comparison.initialLatencyMSTotal += latency.Milliseconds()
	if usage := messageTokenUsage(response); usage != nil {
		comparison.initialPromptTokensTotal += usage.PromptTokens
		comparison.initialPromptTokenSamples++
	}
}

func (comparison *liveCatalogComparisonAccumulator) observeFull(
	response *schema.Message,
	latency time.Duration,
) {
	comparison.FullSamples++
	comparison.fullLatencyMSTotal += latency.Milliseconds()
	if usage := messageTokenUsage(response); usage != nil {
		comparison.fullPromptTokensTotal += usage.PromptTokens
		comparison.fullPromptTokenSamples++
	}
}

func (comparison *liveCatalogComparisonAccumulator) observeLoadedSchema(runes int) {
	comparison.loadedBoundSchemaRunesTotal += runes
	comparison.loadedSchemaSamples++
}

func (comparison *liveCatalogComparisonAccumulator) observeLoaded(
	response *schema.Message,
	latency time.Duration,
) {
	comparison.LoadedSamples++
	comparison.loadedLatencyMSTotal += latency.Milliseconds()
	if usage := messageTokenUsage(response); usage != nil {
		comparison.loadedPromptTokensTotal += usage.PromptTokens
		comparison.loadedPromptTokenSamples++
	}
}

func (comparison *liveCatalogComparisonAccumulator) result() *liveCatalogComparison {
	result := comparison.liveCatalogComparison
	if comparison.loadedSchemaSamples > 0 {
		result.AverageLoadedBoundSchemaRunes = float64(comparison.loadedBoundSchemaRunesTotal) /
			float64(comparison.loadedSchemaSamples)
	}
	if result.ObservedAttempts > 0 {
		result.AverageInitialProviderLatencyMS = float64(comparison.initialLatencyMSTotal) /
			float64(result.ObservedAttempts)
	}
	if result.FullSamples > 0 {
		result.AverageFullProviderLatencyMS = float64(comparison.fullLatencyMSTotal) /
			float64(result.FullSamples)
	}
	if result.LoadedSamples > 0 {
		result.AverageLoadedProviderLatencyMS = float64(comparison.loadedLatencyMSTotal) /
			float64(result.LoadedSamples)
	}
	if comparison.fullPromptTokenSamples > 0 {
		result.AverageFullPromptTokens = float64(comparison.fullPromptTokensTotal) /
			float64(comparison.fullPromptTokenSamples)
	}
	if comparison.initialPromptTokenSamples > 0 {
		result.AverageInitialPromptTokens = float64(comparison.initialPromptTokensTotal) /
			float64(comparison.initialPromptTokenSamples)
	}
	if comparison.loadedPromptTokenSamples > 0 {
		result.AverageLoadedPromptTokens = float64(comparison.loadedPromptTokensTotal) /
			float64(comparison.loadedPromptTokenSamples)
	}
	return &result
}

func liveAllModelActionSpecs(registry *rushestools.Registry) []rushestools.Spec {
	result := make([]rushestools.Spec, 0, len(registry.ModelActionNames()))
	for _, spec := range registry.Specs(true) {
		if spec.Exposure == rushestools.ExposureLLM {
			result = append(result, spec)
		}
	}
	return result
}

func TestLiveCatalogComparisonUsesIndependentSampleCounts(t *testing.T) {
	comparison := &liveCatalogComparisonAccumulator{
		liveCatalogComparison:       liveCatalogComparison{ObservedAttempts: 4, FullSamples: 4, LoadedSamples: 2},
		loadedBoundSchemaRunesTotal: 600, loadedSchemaSamples: 2,
		fullLatencyMSTotal:    400,
		initialLatencyMSTotal: 200,
		loadedLatencyMSTotal:  80,
	}
	result := comparison.result()
	if result.AverageLoadedBoundSchemaRunes != 300 ||
		result.AverageFullProviderLatencyMS != 100 ||
		result.AverageInitialProviderLatencyMS != 50 ||
		result.AverageLoadedProviderLatencyMS != 40 {
		t.Fatalf("independent sample averages=%#v", result)
	}
}

func liveGenerateResponse(
	parent context.Context,
	chat model.ToolCallingChatModel,
	messages []*schema.Message,
	options ...model.Option,
) (*schema.Message, error) {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		ctx, cancel := context.WithTimeout(parent, 90*time.Second)
		response, err := chat.Generate(ctx, messages, options...)
		cancel()
		if err == nil && response != nil {
			return response, nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = errors.New("模型返回 nil")
		}
		if attempt < 3 {
			time.Sleep(time.Duration(attempt) * 200 * time.Millisecond)
		}
	}
	return nil, lastErr
}

func toolCallNames(response *schema.Message) string {
	if response == nil || len(response.ToolCalls) == 0 {
		return ""
	}
	names := make([]string, 0, len(response.ToolCalls))
	for _, call := range response.ToolCalls {
		names = append(names, call.Function.Name)
	}
	return strings.Join(names, ",")
}

type liveRoutingVariant struct {
	Name            string
	Case            liveToolEvalCase
	IncludePlaybook bool
}

type liveWorkflowFixture struct {
	DraftID     string
	PrimaryID   string
	SecondaryID string
	AudioID     string
}

type scriptedWorkflowStep struct {
	Name              string
	ExpectedTool      string
	ExpectedArguments map[string]any
}

type liveWorkflowSuite struct {
	Name             string
	Goal             string
	MaxSteps         int
	AllowedTools     []string
	RequiredEvidence []string
}

type liveWorkflowStepReport struct {
	Step       string                   `json:"step"`
	Actual     string                   `json:"actual,omitempty"`
	Arguments  string                   `json:"arguments,omitempty"`
	BoundTools []string                 `json:"bound_tools,omitempty"`
	Calls      []liveWorkflowCallReport `json:"calls,omitempty"`
	Succeeded  bool                     `json:"succeeded"`
	Error      string                   `json:"error,omitempty"`
}

type liveWorkflowCallReport struct {
	Actual    string `json:"actual"`
	Arguments string `json:"arguments,omitempty"`
	Succeeded bool   `json:"succeeded"`
}

type liveWorkflowRunReport struct {
	Suite              string                   `json:"suite"`
	Run                int                      `json:"run"`
	DraftID            string                   `json:"draft_id,omitempty"`
	Succeeded          bool                     `json:"succeeded"`
	FinalStateValid    bool                     `json:"final_state_valid"`
	EvidenceChainValid bool                     `json:"evidence_chain_valid"`
	EvidenceChainError string                   `json:"evidence_chain_error,omitempty"`
	RequiredEvidence   []string                 `json:"required_evidence"`
	ObservedEvidence   []string                 `json:"observed_evidence"`
	MissingEvidence    []string                 `json:"missing_evidence"`
	LegacyBoundTools   []string                 `json:"legacy_bound_tools"`
	LegacyToolCalls    []string                 `json:"legacy_tool_calls"`
	Steps              []liveWorkflowStepReport `json:"steps"`
	Error              string                   `json:"error,omitempty"`
}

func TestLiveUserMemoryModelContract(t *testing.T) {
	if os.Getenv("RUSHES_LIVE_TOOL_EVAL") != "1" {
		t.Skip("设置 RUSHES_LIVE_TOOL_EVAL=1 才运行真实用户记忆模型合同评测")
	}
	key := strings.TrimSpace(os.Getenv("RUSHES_DASHSCOPE_API_KEY"))
	if key == "" {
		t.Fatal("真实用户记忆评测缺少 RUSHES_DASHSCOPE_API_KEY")
	}
	modelName := strings.TrimSpace(os.Getenv("RUSHES_QWEN_CHAT_MODEL"))
	if modelName == "" {
		modelName = providers.DefaultChatModel
	}
	tiers, err := providers.NewQwenTiers(t.Context(), providers.QwenTierConfig{
		APIKey: key, BaseURL: os.Getenv("RUSHES_DASHSCOPE_BASE_URL"), ChatModel: modelName,
	})
	if err != nil {
		t.Fatal(err)
	}
	database := agenttest.AgentTestDatabase(t)
	service, err := NewServiceWithModels(t.Context(), database, tiers.Chat, tiers.Vision)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	toolInfos, specs, err := userMemoryEvalToolContracts(t.Context(), service)
	if err != nil {
		t.Fatal(err)
	}

	runs := liveEvalRuns()
	metrics := map[string]liveToolEvalMetric{}
	failures := []liveToolEvalFailure{}
	for _, evalCase := range loadUserMemoryModelEvalCases(t) {
		infos := make([]*schema.ToolInfo, 0, len(evalCase.AvailableTools))
		for _, name := range evalCase.AvailableTools {
			info := toolInfos[name]
			if info == nil {
				t.Fatalf("评测工具未注册: %s", name)
			}
			infos = append(infos, info)
		}
		bound, err := tiers.Chat.WithTools(infos)
		if err != nil {
			t.Fatal(err)
		}
		metric := liveToolEvalMetric{}
		for run := 1; run <= runs; run++ {
			metric.Total++
			response, responseErr := liveGenerateUserMemoryResponse(t.Context(), bound, evalCase)
			if responseErr == nil {
				responseErr = validateUserMemoryModelResponse(evalCase, response, specs)
			}
			if responseErr == nil {
				metric.Succeeded++
				continue
			}
			failures = append(failures, liveToolEvalFailure{
				Suite: "user_memory_model_contract", Case: evalCase.Name, Run: run,
				Expected: userMemoryExpectedBehavior(evalCase),
				Actual:   userMemoryResponseSummary(response),
				Error:    responseErr.Error(),
			})
		}
		metric.Rate = ratio(metric.Succeeded, metric.Total)
		metrics[evalCase.Name] = metric
	}

	for _, evalCase := range loadUserMemoryModelEvalCases(t) {
		metric := metrics[evalCase.Name]
		t.Logf(
			"USER_MEMORY_MODEL_RESULT model=%s case=%s succeeded=%d/%d rate=%.2f%%",
			modelName, evalCase.Name, metric.Succeeded, metric.Total, metric.Rate*100,
		)
		if metric.Rate < liveToolStabilityTarget {
			encoded, _ := json.Marshal(failures)
			t.Fatalf("真实用户记忆模型合同低于 %.0f%%: %s", liveToolStabilityTarget*100, encoded)
		}
	}
}

func TestLiveToolCallingStability(t *testing.T) {
	service, chat, modelName := newLiveToolEvalHarness(t)

	toolInfos := map[string]*schema.ToolInfo{}
	specs := map[string]rushestools.Spec{}
	allInfos := make([]*schema.ToolInfo, 0)
	for _, spec := range service.tools.Specs(true) {
		if spec.Exposure != rushestools.ExposureLLM {
			continue
		}
		info, infoErr := spec.Implementation.Info(t.Context())
		if infoErr != nil {
			t.Fatal(infoErr)
		}
		toolInfos[spec.Name] = info
		specs[spec.Name] = spec
		allInfos = append(allInfos, info)
	}

	report := liveToolEvalReport{GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano), Model: modelName}
	runs := liveEvalRuns()
	report.SchemaCases = map[string]liveToolEvalMetric{}
	for _, evalCase := range liveSchemaCases() {
		info := toolInfos[evalCase.Expected[0]]
		if info == nil {
			t.Fatalf("评测工具未注册: %s", evalCase.Expected[0])
		}
		bound, bindErr := chat.WithTools([]*schema.ToolInfo{info})
		if bindErr != nil {
			t.Fatal(bindErr)
		}
		metric := liveToolEvalMetric{}
		for run := 1; run <= runs; run++ {
			report.Schema.Total++
			metric.Total++
			call, callErr := liveGenerateToolCall(
				t.Context(), bound, evalCase.Prompt, evalCase.Snapshot,
				true, evalCase.Expected[0],
			)
			if callErr == nil {
				callErr = validateLiveToolArguments(specs[evalCase.Expected[0]], call.Function.Arguments)
			}
			if callErr == nil && evalCase.ValidateArguments != nil {
				callErr = evalCase.ValidateArguments(call.Function.Arguments)
			}
			if callErr == nil && call.Function.Name == evalCase.Expected[0] {
				report.Schema.Succeeded++
				metric.Succeeded++
				continue
			}
			report.Failures = append(report.Failures, liveToolEvalFailure{
				Suite: "schema", Case: evalCase.Name, Run: run,
				Expected: evalCase.Expected[0], Actual: call.Function.Name,
				Error: errorText(callErr),
			})
		}
		metric.Rate = ratio(metric.Succeeded, metric.Total)
		report.SchemaCases[evalCase.Name] = metric
	}

	boundAll, err := chat.WithTools(allInfos)
	if err != nil {
		t.Fatal(err)
	}
	report.RoutingCases = map[string]liveToolEvalMetric{}
	for _, evalCase := range liveRoutingCases() {
		metric := liveToolEvalMetric{}
		for run := 1; run <= runs; run++ {
			report.Routing.Total++
			metric.Total++
			call, callErr := liveGenerateToolCall(
				t.Context(), boundAll, evalCase.Prompt, evalCase.Snapshot, false, "",
			)
			if callErr == nil && containsToolName(evalCase.Expected, call.Function.Name) {
				callErr = validateLiveToolArguments(specs[call.Function.Name], call.Function.Arguments)
			}
			if callErr == nil && containsToolName(evalCase.Expected, call.Function.Name) {
				report.Routing.Succeeded++
				metric.Succeeded++
				continue
			}
			report.Failures = append(report.Failures, liveToolEvalFailure{
				Suite: "routing", Case: evalCase.Name, Run: run,
				Expected: strings.Join(evalCase.Expected, "|"), Actual: call.Function.Name,
				Error: errorText(callErr),
			})
		}
		metric.Rate = ratio(metric.Succeeded, metric.Total)
		report.RoutingCases[evalCase.Name] = metric
	}

	report.RoutingVariants = map[string]liveToolEvalMetric{}
	for _, variant := range liveRoutingAblationCases() {
		metric := liveToolEvalMetric{}
		for run := 1; run <= runs; run++ {
			metric.Total++
			call, callErr := liveGenerateToolCallWithPlaybook(
				t.Context(), boundAll, variant.Case.Prompt, variant.Case.Snapshot,
				false, "", variant.IncludePlaybook,
			)
			if callErr == nil && containsToolName(variant.Case.Expected, call.Function.Name) {
				callErr = validateLiveToolArguments(specs[call.Function.Name], call.Function.Arguments)
			}
			if callErr == nil && containsToolName(variant.Case.Expected, call.Function.Name) {
				metric.Succeeded++
				continue
			}
			report.Failures = append(report.Failures, liveToolEvalFailure{
				Suite: "routing_variant:" + variant.Name, Case: variant.Case.Name, Run: run,
				Expected: strings.Join(variant.Case.Expected, "|"), Actual: call.Function.Name,
				Error: errorText(callErr),
			})
		}
		metric.Rate = ratio(metric.Succeeded, metric.Total)
		report.RoutingVariants[variant.Name+":"+variant.Case.Name] = metric
	}

	report.PerKind = map[string]liveToolEvalMetric{}
	for _, opSpec := range timeline.Catalog {
		toolName, exposed := rushestools.TimelineAtomicToolForKind(opSpec.Kind)
		if !exposed {
			continue
		}
		boundAtomic, bindErr := chat.WithTools([]*schema.ToolInfo{toolInfos[toolName]})
		if bindErr != nil {
			t.Fatal(bindErr)
		}
		metric := liveToolEvalMetric{}
		example, _ := json.Marshal(timeline.CorrectOpExample(opSpec))
		for run := 1; run <= runs; run++ {
			metric.Total++
			call, callErr := liveGenerateToolCall(
				t.Context(), boundAtomic,
				fmt.Sprintf("只调用 %s，并严格按以下单个 Catalog op 构造 %s：%s", toolName, opSpec.Kind, example),
				liveSnapshotForSchemaCase("atomic_update"), true, toolName,
			)
			decoded := map[string]any{}
			if callErr == nil {
				callErr = json.Unmarshal([]byte(call.Function.Arguments), &decoded)
			}
			if callErr == nil {
				callErr = validateLiveToolArguments(
					specs[toolName], call.Function.Arguments,
				)
			}
			if callErr == nil && decoded["kind"] == opSpec.Kind {
				metric.Succeeded++
				continue
			}
			actualKind := fmt.Sprint(decoded["kind"])
			report.Failures = append(report.Failures, liveToolEvalFailure{
				Suite: "timeline_op_per_kind", Case: opSpec.Kind, Run: run,
				Expected: opSpec.Kind, Actual: actualKind, Error: errorText(callErr),
			})
		}
		metric.Rate = ratio(metric.Succeeded, metric.Total)
		report.PerKind[opSpec.Kind] = metric
	}

	for run := 1; run <= runs; run++ {
		report.Budget.Total++
		if budgetErr := liveGenerateNoToolCall(t.Context(), boundAll); budgetErr == nil {
			report.Budget.Succeeded++
		} else {
			report.Failures = append(report.Failures, liveToolEvalFailure{
				Suite: "budget", Case: "remaining_zero", Run: run,
				Expected: "no_tool_calls", Error: budgetErr.Error(),
			})
		}
	}
	report.Budget.Rate = ratio(report.Budget.Succeeded, report.Budget.Total)
	report.Schema.Rate = ratio(report.Schema.Succeeded, report.Schema.Total)
	report.Routing.Rate = ratio(report.Routing.Succeeded, report.Routing.Total)
	runLiveWorkflowEvaluation(t, &report, service, chat)
	writeLiveToolEvalReport(t, report)
	t.Logf(
		"TOOL_STABILITY_RESULT model=%s schema=%d/%d(%.2f%%) routing=%d/%d(%.2f%%) failures=%d",
		report.Model, report.Schema.Succeeded, report.Schema.Total, report.Schema.Rate*100,
		report.Routing.Succeeded, report.Routing.Total, report.Routing.Rate*100, len(report.Failures),
	)
	failedMetrics := failingLiveEvalMetrics(report)
	failedWorkflowSuites := logLiveWorkflowMetrics(t, report)
	if len(failedMetrics) > 0 ||
		len(failedWorkflowSuites) > 0 {
		encoded, _ := json.Marshal(report.Failures)
		t.Fatalf(
			"真实工具调用稳定性低于 %.0f%%（metrics=%s workflow=%s）: %s",
			liveToolStabilityTarget*100, strings.Join(failedMetrics, ","),
			strings.Join(failedWorkflowSuites, ","), encoded,
		)
	}
}

func TestLiveWorkflowToolCallingStability(t *testing.T) {
	service, chat, modelName := newLiveToolEvalHarness(t)
	report := liveToolEvalReport{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Model:       modelName,
	}
	runLiveWorkflowEvaluation(t, &report, service, chat)
	writeLiveToolEvalReport(t, report)
	t.Logf("TOOL_WORKFLOW_STABILITY_RESULT model=%s failures=%d", report.Model, len(report.Failures))
	if failed := logLiveWorkflowMetrics(t, report); len(failed) > 0 {
		encoded, _ := json.Marshal(report.Failures)
		t.Fatalf(
			"真实连续工作流稳定性低于 %.0f%%（workflow=%s）: %s",
			liveToolStabilityTarget*100, strings.Join(failed, ","), encoded,
		)
	}
}

func newLiveToolEvalHarness(
	t *testing.T,
) (*Service, model.ToolCallingChatModel, string) {
	t.Helper()
	if os.Getenv("RUSHES_LIVE_TOOL_EVAL") != "1" {
		t.Skip("设置 RUSHES_LIVE_TOOL_EVAL=1 才运行真实模型工具稳定性评测")
	}
	key := strings.TrimSpace(os.Getenv("RUSHES_DASHSCOPE_API_KEY"))
	if key == "" {
		t.Fatal("真实工具评测缺少 RUSHES_DASHSCOPE_API_KEY")
	}
	modelName := strings.TrimSpace(os.Getenv("RUSHES_QWEN_CHAT_MODEL"))
	if modelName == "" {
		modelName = providers.DefaultChatModel
	}
	tiers, err := providers.NewQwenTiers(t.Context(), providers.QwenTierConfig{
		APIKey: key, BaseURL: os.Getenv("RUSHES_DASHSCOPE_BASE_URL"), ChatModel: modelName,
	})
	if err != nil {
		t.Fatal(err)
	}
	database := agenttest.AgentTestDatabase(t)
	service, err := NewServiceWithModels(t.Context(), database, tiers.Chat, tiers.Vision)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	return service, tiers.Chat, modelName
}

func runLiveWorkflowEvaluation(
	t *testing.T,
	report *liveToolEvalReport,
	service *Service,
	chat model.ToolCallingChatModel,
) {
	t.Helper()
	report.WorkflowRuns = runLiveWorkflowSuites(t, service, chat)
	report.Workflows = liveWorkflowMetrics(report.WorkflowRuns)
	for _, run := range report.WorkflowRuns {
		if run.Succeeded {
			continue
		}
		failure := liveToolEvalFailure{
			Suite: "workflow:" + run.Suite,
			Case:  "setup",
			Run:   run.Run,
			Error: run.Error,
		}
		for _, step := range run.Steps {
			if step.Error == "" {
				continue
			}
			failure.Case = step.Step
			failure.Actual = step.Actual
			failure.Error = step.Error
			break
		}
		if failure.Case == "setup" && len(run.Steps) > 0 && !run.FinalStateValid {
			failure.Case = "final_state"
		}
		report.Failures = append(report.Failures, failure)
	}
}

func logLiveWorkflowMetrics(t *testing.T, report liveToolEvalReport) []string {
	t.Helper()
	failedWorkflowSuites := []string{}
	for _, suite := range liveWorkflowSuites() {
		metric := report.Workflows[suite.Name]
		t.Logf(
			"TOOL_WORKFLOW_RESULT model=%s suite=%s succeeded=%d/%d rate=%.2f%%",
			report.Model, suite.Name, metric.Succeeded, metric.Total, metric.Rate*100,
		)
		if metric.Rate < liveToolStabilityTarget {
			failedWorkflowSuites = append(failedWorkflowSuites, suite.Name)
		}
	}
	return failedWorkflowSuites
}

func TestLiveWorkflowDefinitionsAndScoring(t *testing.T) {
	suites := liveWorkflowSuites()
	wantMaxSteps := map[string]int{
		"initial_composition": 8,
		"beat_mix":            10,
		"talking_head":        20,
	}
	wantEvidence := map[string][]string{
		"initial_composition": {"shot.search"},
		"beat_mix":            {"shot.search"},
		"talking_head":        {"speech.search", "shot.search"},
	}
	if len(suites) != len(wantMaxSteps) {
		t.Fatalf("workflow suites=%d want=%d", len(suites), len(wantMaxSteps))
	}
	for _, suite := range suites {
		want, ok := wantMaxSteps[suite.Name]
		if !ok {
			t.Fatalf("unexpected workflow suite %q", suite.Name)
		}
		if strings.TrimSpace(suite.Goal) == "" || suite.MaxSteps != want ||
			len(suite.AllowedTools) == 0 ||
			!reflect.DeepEqual(suite.RequiredEvidence, wantEvidence[suite.Name]) {
			t.Fatalf(
				"workflow %s goal=%q max_steps=%d allowed=%v required_evidence=%v",
				suite.Name, suite.Goal, suite.MaxSteps,
				suite.AllowedTools, suite.RequiredEvidence,
			)
		}
		for _, toolName := range suite.RequiredEvidence {
			if !containsToolName(suite.AllowedTools, toolName) {
				t.Fatalf(
					"workflow %s 的必需证据工具未进入允许面: %s",
					suite.Name, toolName,
				)
			}
		}
		for _, toolName := range suite.AllowedTools {
			if containsToolName(retiredLLMToolNames, toolName) {
				t.Fatalf("workflow %s 仍允许旧复合工具 %s", suite.Name, toolName)
			}
		}
		for _, toolName := range append(
			append([]string{}, suite.AllowedTools...),
			retiredLLMToolNames...,
		) {
			if strings.Contains(strings.ToLower(suite.Goal), strings.ToLower(toolName)) {
				t.Fatalf("workflow %s 目标直接泄露工具名 %s", suite.Name, toolName)
			}
		}
		for _, scriptedLiteral := range []string{
			"240 到 600", "0 到 90", "270 到 330", "第 810 帧",
			"beat_frames=", "asset_live_", "clip_v",
		} {
			if strings.Contains(suite.Goal, scriptedLiteral) {
				t.Fatalf("workflow %s 目标泄露隐藏验收参数 %q", suite.Name, scriptedLiteral)
			}
		}
	}

	runs := []liveWorkflowRunReport{
		{Suite: "initial_composition", Run: 1, Succeeded: true},
		{Suite: "initial_composition", Run: 2, Succeeded: true},
		{Suite: "beat_mix", Run: 1, Succeeded: true},
		{Suite: "beat_mix", Run: 2, Succeeded: false},
		{Suite: "talking_head", Run: 1, Succeeded: true},
	}
	metrics := liveWorkflowMetrics(runs)
	if metric := metrics["initial_composition"]; metric.Succeeded != 2 || metric.Total != 2 || metric.Rate != 1 {
		t.Fatalf("initial metric=%#v", metric)
	}
	if metric := metrics["beat_mix"]; metric.Succeeded != 1 || metric.Total != 2 || metric.Rate != 0.5 {
		t.Fatalf("beat metric=%#v", metric)
	}
	if metric := metrics["talking_head"]; metric.Succeeded != 1 || metric.Total != 1 || metric.Rate != 1 {
		t.Fatalf("talking-head metric=%#v", metric)
	}
	if max(liveWorkflowMinimumRuns, liveEvalRuns()) < 5 {
		t.Fatal("live workflow suite 必须至少运行 5 次")
	}
}

func TestLiveWorkflowRunReportSerializesGateEvidenceExplicitly(t *testing.T) {
	report := liveWorkflowRunReport{
		Suite: "fixture", Run: 1,
		EvidenceChainValid: true,
		RequiredEvidence:   []string{"speech.search", "shot.search"},
		ObservedEvidence:   []string{"speech.search", "shot.search"},
		MissingEvidence:    []string{},
		LegacyBoundTools:   []string{},
		LegacyToolCalls:    []string{},
		Steps:              []liveWorkflowStepReport{},
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`"evidence_chain_valid":true`,
		`"required_evidence":["speech.search","shot.search"]`,
		`"observed_evidence":["speech.search","shot.search"]`,
		`"missing_evidence":[]`,
		`"legacy_bound_tools":[]`,
		`"legacy_tool_calls":[]`,
	} {
		if !strings.Contains(string(encoded), required) {
			t.Fatalf("workflow report omitted gate evidence %s: %s", required, encoded)
		}
	}
}

func TestTalkingHeadEvidenceChainRequiresReturnedAnchorCoordinate(t *testing.T) {
	t.Parallel()
	fixture := liveWorkflowFixture{
		PrimaryID: "asset_primary", SecondaryID: "asset_broll",
	}
	chain := newLiveTalkingHeadEvidenceChain(fixture)
	observe := func(name string, arguments string, output string) {
		t.Helper()
		if err := chain.observe(schema.ToolCall{
			Function: schema.FunctionCall{Name: name, Arguments: arguments},
		}, output); err != nil {
			t.Fatal(err)
		}
	}
	observe(
		"speech.search",
		`{"asset_id":"asset_primary","include_words":true}`,
		`{"status":"succeeded","asset_id":"asset_primary","utterances":[{`+
			`"utterance_id":"utt_initial","source_start_frame":0,"source_end_frame":90}],`+
			`"pauses":[{"pause_id":"pause_initial"}],`+
			`"similar_pairs":[{"earlier_utterance_id":"utt_a","later_utterance_id":"utt_b"}]}`,
	)
	for _, sourceRange := range [][2]int{{0, 90}, {240, 600}, {720, 780}} {
		observe(
			"timeline.delete",
			fmt.Sprintf(
				`{"kind":"delete_source_range","asset_id":"asset_primary",`+
					`"source_start_frame":%d,"source_end_frame":%d}`,
				sourceRange[0], sourceRange[1],
			),
			`{"status":"succeeded"}`,
		)
	}
	observe(
		"timeline.inspect",
		`{}`,
		`{"status":"succeeded","data":{"tracks":[{"track_id":"visual_base","clips":[`+
			`{"timeline_clip_id":"clip_current","asset_id":"asset_primary","role":"a_roll",`+
			`"source_start_frame":1260,"source_end_frame":1440}`+
			`] }]}}`,
	)
	observe(
		"speech.search",
		`{"timeline_clip_id":"clip_current","query":"指纹解锁"}`,
		`{"status":"succeeded","timeline_clip_id":"clip_current","utterances":[{`+
			`"utterance_id":"utt_fingerprint","source_start_frame":1320,`+
			`"source_end_frame":1440,"timeline_start_frame":810,"timeline_end_frame":930,`+
			`"text":"指纹解锁按键仍然位于键盘右上角。"}]}`,
	)
	observe(
		"shot.search",
		`{"query":"键盘 指纹"}`,
		`{"shots":[{"shot_id":"shot_fingerprint","asset_id":"asset_broll",`+
			`"source_start_frame":0,"source_end_frame":90,"duration_frames":90,`+
			`"semantic_role":"b_roll"}]}`,
	)
	observe(
		"timeline.insert",
		`{"kind":"insert_clip","track_id":"visual_overlay","asset_id":"asset_broll",`+
			`"role":"b_roll","source_start_frame":0,"source_end_frame":90,`+
			`"timeline_start_frame":811}`,
		`{"status":"succeeded"}`,
	)
	if err := chain.validate(); err == nil ||
		!strings.Contains(err.Error(), "直接采用 speech.search 坐标") {
		t.Fatalf("wrong coordinate validation=%v", err)
	}
	observe(
		"timeline.insert",
		`{"kind":"insert_clip","track_id":"visual_overlay","asset_id":"asset_broll",`+
			`"role":"b_roll","source_start_frame":0,"source_end_frame":90,`+
			`"timeline_start_frame":810}`,
		`{"status":"succeeded"}`,
	)
	if err := chain.validate(); err != nil {
		t.Fatalf("valid chain=%v", err)
	}
}

type singleAttemptWorkflowModel struct {
	calls int
}

func (stub *singleAttemptWorkflowModel) WithTools(
	[]*schema.ToolInfo,
) (model.ToolCallingChatModel, error) {
	return stub, nil
}

func (stub *singleAttemptWorkflowModel) Generate(
	context.Context,
	[]*schema.Message,
	...model.Option,
) (*schema.Message, error) {
	stub.calls++
	return schema.AssistantMessage("没有工具调用", nil), nil
}

func (stub *singleAttemptWorkflowModel) Stream(
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

func TestLiveWorkflowGateAcceptsFinalReplyAndRequiresSucceededToolResult(t *testing.T) {
	t.Parallel()
	stub := &singleAttemptWorkflowModel{}
	if calls, err := liveGenerateWorkflowToolCalls(
		t.Context(),
		stub,
		NewWorldStateSnapshot(map[string]any{}),
		[]*schema.Message{schema.UserMessage("完成一条原子工作流")},
	); err != nil || len(calls) != 0 {
		t.Fatalf("合法最终回复 calls=%v err=%v", calls, err)
	}
	if stub.calls != 1 {
		t.Fatalf("单个 workflow step 调用了模型 %d 次，不能隐藏重试", stub.calls)
	}

	if err := validateLiveWorkflowToolOutput(
		"timeline.check",
		`{"status":"succeeded","observation":"valid"}`,
	); err != nil {
		t.Fatalf("succeeded ToolResult=%v", err)
	}
	for name, fixture := range map[string]struct {
		tool   string
		output string
	}{
		"shots": {
			tool: "shot.search",
			output: `{"shots":[{"shot_id":"shot_1","asset_id":"asset_1",` +
				`"duration_frames":90,"semantic_role":"b_roll"}],"total_matches":1}`,
		},
		"shots_empty": {
			tool:   "shot.search",
			output: `{"shots":[],"total_matches":0}`,
		},
		"beats": {
			tool: "audio.analyze_beats",
			output: `{"asset_id":"asset_bgm","timeline_fps":30,` +
				`"beat_frames":[0,15],"waveform":{"sample_interval_frames":1}}`,
		},
		"speech": {
			tool: "speech.search",
			output: `{"status":"succeeded","utterances":[` +
				`{"utterance_id":"utt_1","source_start_frame":0,"source_end_frame":30}]}`,
		},
		"speech_empty": {
			tool:   "speech.search",
			output: `{"status":"succeeded","utterances":[],"usage_note":"no matches"}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := validateLiveWorkflowToolOutput(fixture.tool, fixture.output); err != nil {
				t.Fatalf("%s typed result=%v", fixture.tool, err)
			}
		})
	}
	for name, output := range map[string]string{
		"queued":         `{"status":"queued","observation":"not terminal"}`,
		"missing_status": `{"observation":"missing status"}`,
		"invalid_json":   `{`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := validateLiveWorkflowToolOutput("timeline.check", output); err == nil {
				t.Fatalf("非终态或非法 ToolResult 通过: %s", output)
			}
		})
	}
	for name, output := range map[string]string{
		"failed":            `{"status":"failed","observation":"failed"}`,
		"validation_failed": `{"status":"validation_failed","observation":"invalid timeline"}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := validateLiveWorkflowToolOutput("timeline.insert", output); err != nil {
				t.Fatalf("可恢复失败没有作为有效 ToolResult 回灌: %v", err)
			}
			if liveWorkflowToolOutputSucceeded("timeline.insert", output) {
				t.Fatalf("可恢复失败被误计为成功: %s", output)
			}
		})
	}
}

type scriptedWorkflowModel struct {
	calls          []schema.ToolCall
	next           int
	boundTools     []string
	beforeGenerate func([]string) error
}

func (stub *scriptedWorkflowModel) WithTools(
	infos []*schema.ToolInfo,
) (model.ToolCallingChatModel, error) {
	stub.boundTools = make([]string, 0, len(infos))
	for _, info := range infos {
		stub.boundTools = append(stub.boundTools, info.Name)
	}
	return stub, nil
}

func (stub *scriptedWorkflowModel) Generate(
	context.Context,
	[]*schema.Message,
	...model.Option,
) (*schema.Message, error) {
	if stub.beforeGenerate != nil {
		if err := stub.beforeGenerate(append([]string(nil), stub.boundTools...)); err != nil {
			return nil, err
		}
	}
	if stub.next >= len(stub.calls) {
		return schema.AssistantMessage("Harness 已完成自动检查，工作流完成。", nil), nil
	}
	call := stub.calls[stub.next]
	stub.next++
	return schema.AssistantMessage("", []schema.ToolCall{call}), nil
}

func (stub *scriptedWorkflowModel) Stream(
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

type scriptedWorkflowTurnModel struct {
	turns [][]schema.ToolCall
	next  int
}

func (stub *scriptedWorkflowTurnModel) WithTools(
	[]*schema.ToolInfo,
) (model.ToolCallingChatModel, error) {
	return stub, nil
}

func (stub *scriptedWorkflowTurnModel) Generate(
	context.Context,
	[]*schema.Message,
	...model.Option,
) (*schema.Message, error) {
	if stub.next >= len(stub.turns) {
		return schema.AssistantMessage("Harness 已完成自动检查，工作流完成。", nil), nil
	}
	calls := stub.turns[stub.next]
	stub.next++
	return schema.AssistantMessage("", calls), nil
}

func (stub *scriptedWorkflowTurnModel) Stream(
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

func TestLiveWorkflowFixturesExecuteThroughRegistryAndMeetFinalPostconditions(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	prepareLiveWorkflowBeatEnvironment(t)

	for index, name := range []string{"initial_composition", "beat_mix", "talking_head"} {
		fixture, fixtureErr := prepareLiveWorkflowFixture(t, service, name, index+101)
		if fixtureErr != nil {
			t.Fatalf("%s fixture: %v", name, fixtureErr)
		}
		suite, ok := liveWorkflowSuiteByName(liveWorkflowSuites(), name)
		if !ok {
			t.Fatalf("missing suite %s", name)
		}
		scriptedCalls := scriptedLiveWorkflowCalls(t, fixture, name)
		stub := &scriptedWorkflowModel{calls: scriptedCalls}
		runContext := withTestTurnLeaseSession(t, service, t.Context(), fixture.DraftID)
		report := runLiveWorkflowSuite(
			runContext,
			service,
			stub,
			fixture,
			suite,
			index+101,
		)
		if !report.Succeeded || !report.FinalStateValid || stub.next != len(scriptedCalls) {
			t.Fatalf("%s report=%#v calls=%d/%d", name, report, stub.next, len(scriptedCalls))
		}
	}
}

func TestLiveWorkflowRunnerUsesProductionLeaseAndReceiptBoundary(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	prepareLiveWorkflowBeatEnvironment(t)
	fixture, err := prepareLiveWorkflowFixture(t, service, "initial_composition", 206)
	if err != nil {
		t.Fatal(err)
	}
	suite, ok := liveWorkflowSuiteByName(liveWorkflowSuites(), "initial_composition")
	if !ok {
		t.Fatal("missing initial workflow")
	}
	calls := scriptedLiveWorkflowCalls(t, fixture, suite.Name)
	expectedReceipts := map[string]string{}
	for _, call := range calls {
		if isTerminalTimelineMutation(call.Function.Name) {
			expectedReceipts[call.ID] = call.Function.Name
		}
	}
	if len(expectedReceipts) == 0 {
		t.Fatal("scripted workflow 缺少 mutation receipt 目标")
	}

	var turnID, sourceMessageID string
	sawInitialMutationSchema := false
	if passed := t.Run("attempt", func(t *testing.T) {
		runContext := withTestTurnLeaseSession(t, service, t.Context(), fixture.DraftID)
		leaseSession := timelineEditLeaseSessionFromContext(runContext)
		turnID, sourceMessageID = rushestools.TurnIdentity(runContext)
		stub := &scriptedWorkflowModel{calls: calls}
		generation := 0
		stub.beforeGenerate = func(boundTools []string) error {
			generation++
			requiresLease := false
			for _, name := range boundTools {
				if toolRequiresTimelineEditLease(name) {
					requiresLease = true
					break
				}
			}
			if !requiresLease {
				return nil
			}
			if generation == 1 {
				sawInitialMutationSchema = true
				var leases int
				if err := database.Read().QueryRowContext(t.Context(), `
					SELECT COUNT(*) FROM agent_edit_leases WHERE draft_id=?`, fixture.DraftID,
				).Scan(&leases); err != nil {
					return err
				}
				if leases != 0 {
					return fmt.Errorf("provider 首次看见已加载 mutation schema 时提前持有 %d 个 lease", leases)
				}
			}
			return nil
		}
		report := runLiveWorkflowSuite(runContext, service, stub, fixture, suite, 206)
		if !report.Succeeded || !report.FinalStateValid || stub.next != len(calls) {
			t.Fatalf("report=%#v calls=%d/%d", report, stub.next, len(calls))
		}
		// 与 production runTurn defer 顺序一致，在 turn context 取消前同步释放。
		leaseSession.close()
	}); !passed {
		return
	}
	if !sawInitialMutationSchema {
		t.Fatal("provider 未观察到预加载的 mutation schema")
	}

	var liveLeases int
	if err := database.Read().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM agent_edit_leases WHERE draft_id=?", fixture.DraftID,
	).Scan(&liveLeases); err != nil || liveLeases != 0 {
		t.Fatalf("cleanup leases=%d err=%v", liveLeases, err)
	}
	rows, err := database.Read().QueryContext(t.Context(), `
		SELECT turn_id,source_message_id,tool_call_id,tool_name,argument_fingerprint
		FROM agent_tool_receipts WHERE draft_id=? ORDER BY tool_call_id`, fixture.DraftID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	seen := map[string]bool{}
	for rows.Next() {
		var receiptTurnID, receiptSourceID, callID, toolName, fingerprint string
		if err := rows.Scan(
			&receiptTurnID, &receiptSourceID, &callID, &toolName, &fingerprint,
		); err != nil {
			t.Fatal(err)
		}
		if receiptTurnID != turnID || receiptSourceID != sourceMessageID ||
			expectedReceipts[callID] != toolName ||
			!strings.HasPrefix(fingerprint, "sha256:") {
			t.Fatalf(
				"receipt turn=%q source=%q call=%q tool=%q fingerprint=%q",
				receiptTurnID, receiptSourceID, callID, toolName, fingerprint,
			)
		}
		seen[callID] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(seen) != len(expectedReceipts) {
		t.Fatalf("receipts=%v want=%v", seen, expectedReceipts)
	}
}

func TestLiveWorkflowRunnerRetriesOnlyFailedPrimitive(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	fixture, err := prepareLiveWorkflowFixture(t, service, "initial_composition", 203)
	if err != nil {
		t.Fatal(err)
	}
	suite, ok := liveWorkflowSuiteByName(liveWorkflowSuites(), "initial_composition")
	if !ok {
		t.Fatal("missing initial workflow")
	}
	scripted := scriptedLiveWorkflowCalls(t, fixture, suite.Name)
	failed := schema.ToolCall{
		ID: "failed_primitive",
		Function: schema.FunctionCall{
			Name: "timeline.insert",
			Arguments: `{"kind":"insert_clip","asset_id":"missing_asset",` +
				`"source_start_frame":0,"source_end_frame":60}`,
		},
	}
	calls := append([]schema.ToolCall{}, scripted[:2]...)
	calls = append(calls, failed)
	calls = append(calls, scripted[2:]...)
	stub := &scriptedWorkflowModel{calls: calls}
	runContext := withTestTurnLeaseSession(t, service, t.Context(), fixture.DraftID)
	report := runLiveWorkflowSuite(runContext, service, stub, fixture, suite, 203)
	if !report.Succeeded || !report.FinalStateValid || stub.next != len(calls) {
		t.Fatalf("retry report=%#v calls=%d/%d", report, stub.next, len(calls))
	}
	if len(report.Steps) != len(calls)+1 || report.Steps[2].Succeeded ||
		report.Steps[2].Error != "" {
		t.Fatalf("failed primitive trace=%#v", report.Steps)
	}
}

func TestLiveWorkflowRunnerAcceptsParallelReadTurn(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	fixture, err := prepareLiveWorkflowFixture(t, service, "talking_head", 204)
	if err != nil {
		t.Fatal(err)
	}
	suite, ok := liveWorkflowSuiteByName(liveWorkflowSuites(), "talking_head")
	if !ok {
		t.Fatal("missing talking-head workflow")
	}
	calls := scriptedLiveWorkflowCalls(t, fixture, suite.Name)
	turns := [][]schema.ToolCall{{calls[0], calls[1]}}
	for _, call := range calls[2:] {
		turns = append(turns, []schema.ToolCall{call})
	}
	stub := &scriptedWorkflowTurnModel{turns: turns}
	runContext := withTestTurnLeaseSession(t, service, t.Context(), fixture.DraftID)
	report := runLiveWorkflowSuite(runContext, service, stub, fixture, suite, 204)
	if !report.Succeeded || !report.FinalStateValid || stub.next != len(turns) {
		t.Fatalf("parallel read report=%#v turns=%d/%d", report, stub.next, len(turns))
	}
	if len(report.Steps) != len(turns)+1 || len(report.Steps[0].Calls) != 2 ||
		report.Steps[0].Actual != "speech.search,shot.search" {
		t.Fatalf("parallel read trace=%#v", report.Steps)
	}
}

func TestLiveWorkflowRunnerDoesNotGateMutationOnWorkflowEvidence(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	fixture, err := prepareLiveWorkflowFixture(t, service, "initial_composition", 205)
	if err != nil {
		t.Fatal(err)
	}
	suite, ok := liveWorkflowSuiteByName(liveWorkflowSuites(), "initial_composition")
	if !ok {
		t.Fatal("missing initial workflow")
	}
	scripted := scriptedLiveWorkflowCalls(t, fixture, suite.Name)
	calls := append([]schema.ToolCall{}, scripted[1:]...)
	stub := &scriptedWorkflowModel{calls: calls}
	runContext := withTestTurnLeaseSession(t, service, t.Context(), fixture.DraftID)
	report := runLiveWorkflowSuite(runContext, service, stub, fixture, suite, 205)
	if report.Succeeded || !report.FinalStateValid ||
		!reflect.DeepEqual(
			report.MissingEvidence,
			[]string{"shot.search"},
		) {
		t.Fatalf("missing evidence gate report=%#v", report)
	}
	if !strings.Contains(report.Error, "missing=[shot.search]") {
		t.Fatalf("missing-evidence terminal error=%q", report.Error)
	}
}

func scriptedLiveWorkflowCalls(
	t *testing.T,
	fixture liveWorkflowFixture,
	suite string,
) []schema.ToolCall {
	t.Helper()
	steps := scriptedLiveWorkflowSteps(fixture, suite)
	calls := make([]schema.ToolCall, 0, len(steps))
	for stepIndex, step := range steps {
		rawArguments, err := json.Marshal(step.ExpectedArguments)
		if err != nil {
			t.Fatal(err)
		}
		arguments := map[string]any{}
		if err := json.Unmarshal(rawArguments, &arguments); err != nil {
			t.Fatal(err)
		}
		if step.Name == "fade_broll" {
			arguments["timeline_clip_id"] = "clip_v5_001"
		}
		encoded, err := json.Marshal(arguments)
		if err != nil {
			t.Fatal(err)
		}
		calls = append(calls, schema.ToolCall{
			ID: fmt.Sprintf("script_%s_%d", suite, stepIndex+1),
			Function: schema.FunctionCall{
				Name: step.ExpectedTool, Arguments: string(encoded),
			},
		})
	}
	return calls
}

func TestLiveWorkflowFixtureSetupBuildsExecutableState(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	prepareLiveWorkflowBeatEnvironment(t)

	for index, suite := range []string{"initial_composition", "beat_mix", "talking_head"} {
		fixture, fixtureErr := prepareLiveWorkflowFixture(t, service, suite, index+1)
		if fixtureErr != nil {
			t.Fatalf("%s fixture: %v", suite, fixtureErr)
		}
		snapshot, snapshotErr := NewContextBuilder(database).Snapshot(
			rushestools.WithDraftID(t.Context(), fixture.DraftID),
			fixture.DraftID,
		)
		if snapshotErr != nil {
			t.Fatalf("%s snapshot: %v", suite, snapshotErr)
		}
		if snapshot.Sections["assets"] == nil {
			t.Fatalf("%s snapshot missing assets section", suite)
		}
		var linkedAssets int
		if queryErr := database.Read().QueryRowContext(
			t.Context(),
			"SELECT COUNT(*) FROM draft_asset_links WHERE draft_id=?",
			fixture.DraftID,
		).Scan(&linkedAssets); queryErr != nil {
			t.Fatalf("%s linked assets: %v", suite, queryErr)
		}
		if linkedAssets < 2 {
			t.Fatalf("%s linked assets=%d want>=2", suite, linkedAssets)
		}
		if suite == "talking_head" {
			var currentVersion int
			if queryErr := database.Read().QueryRowContext(
				t.Context(),
				"SELECT COALESCE(timeline_current_version,0) FROM drafts WHERE draft_id=?",
				fixture.DraftID,
			).Scan(&currentVersion); queryErr != nil {
				t.Fatalf("%s current timeline: %v", suite, queryErr)
			}
			if currentVersion == 0 {
				t.Fatalf("%s fixture must persist an initial timeline", suite)
			}
			output, executeErr := service.ExecuteTool(
				rushestools.WithDraftID(t.Context(), fixture.DraftID),
				"audio.analyze_speech_pauses",
				rushestools.SpeechPauseAnalysisInput{
					AssetID:        fixture.PrimaryID,
					ThresholdDB:    -35,
					MinPauseFrames: 15,
					MaxPauses:      100,
				},
			)
			if executeErr != nil {
				t.Fatalf("%s speech pause fixture: %v", suite, executeErr)
			}
			analysis := output.(rushestools.SpeechPauseAnalysisResult)
			foundFixturePause := false
			for _, pause := range analysis.Pauses {
				if pause.SourceStartFrame >= 718 && pause.SourceStartFrame <= 722 &&
					pause.SourceEndFrame >= 778 && pause.SourceEndFrame <= 782 {
					foundFixturePause = true
					break
				}
			}
			if !foundFixturePause {
				t.Fatalf("%s speech pause fixture result=%#v", suite, analysis)
			}
		}
	}
}

func runLiveWorkflowSuites(
	t *testing.T,
	service *Service,
	chat model.ToolCallingChatModel,
) []liveWorkflowRunReport {
	t.Helper()
	prepareLiveWorkflowBeatEnvironment(t)
	runs := max(liveWorkflowMinimumRuns, liveEvalRuns())
	reports := make([]liveWorkflowRunReport, 0, len(liveWorkflowSuites())*runs)
	for _, template := range liveWorkflowSuites() {
		for run := 1; run <= runs; run++ {
			fixture, err := prepareLiveWorkflowFixture(t, service, template.Name, run)
			if err != nil {
				reports = append(reports, liveWorkflowRunReport{
					Suite: template.Name, Run: run, Error: err.Error(),
					RequiredEvidence: append([]string{}, template.RequiredEvidence...),
					ObservedEvidence: []string{},
					MissingEvidence:  append([]string{}, template.RequiredEvidence...),
					LegacyBoundTools: []string{},
					LegacyToolCalls:  []string{},
				})
				continue
			}
			suite, ok := liveWorkflowSuiteByName(liveWorkflowSuites(), template.Name)
			if !ok {
				reports = append(reports, liveWorkflowRunReport{
					Suite: template.Name, Run: run, DraftID: fixture.DraftID,
					Error:            "workflow 定义不存在",
					RequiredEvidence: append([]string{}, template.RequiredEvidence...),
					ObservedEvidence: []string{},
					MissingEvidence:  append([]string{}, template.RequiredEvidence...),
					LegacyBoundTools: []string{},
					LegacyToolCalls:  []string{},
				})
				continue
			}
			runContext := withTestTurnLeaseSession(t, service, t.Context(), fixture.DraftID)
			reports = append(reports, runLiveWorkflowSuite(runContext, service, chat, fixture, suite, run))
		}
	}
	return reports
}

func runLiveWorkflowSuite(
	parent context.Context,
	service *Service,
	chat model.ToolCallingChatModel,
	fixture liveWorkflowFixture,
	suite liveWorkflowSuite,
	run int,
) liveWorkflowRunReport {
	report := liveWorkflowRunReport{
		Suite: suite.Name, Run: run, DraftID: fixture.DraftID,
		EvidenceChainValid: suite.Name != "talking_head",
		RequiredEvidence:   append([]string{}, suite.RequiredEvidence...),
		ObservedEvidence:   []string{},
		MissingEvidence:    append([]string{}, suite.RequiredEvidence...),
		LegacyBoundTools:   []string{},
		LegacyToolCalls:    []string{},
		Steps:              make([]liveWorkflowStepReport, 0, suite.MaxSteps),
	}
	ctx := rushestools.WithDraftID(parent, fixture.DraftID)
	ctx = withToolDisclosureSession(ctx)
	ctx = withToolRecoveryState(ctx, newToolRecoveryState())
	ctx = withTerminalTimelineTruthState(ctx, newTerminalTimelineTruthState())
	ctx = agentexec.WithTurnInteractionState(
		ctx,
		agentexec.NewTurnInteractionState(service.indexedResources),
	)
	history := []*schema.Message{schema.UserMessage(suite.Goal)}
	for start := 0; start < len(suite.AllowedTools); start += 5 {
		end := min(start+5, len(suite.AllowedTools))
		encoded, _ := json.Marshal(rushestools.ToolLoadResult{
			Status: string(rushestools.StatusSucceeded), LoadedNames: suite.AllowedTools[start:end],
			AlreadyLoaded: []string{}, NotLoadable: []string{},
		})
		history = append(history, schema.ToolMessage(
			string(encoded), fmt.Sprintf("fixture-load-%d", start/5), schema.WithToolName("tool.load"),
		))
	}
	observedEvidence := map[string]bool{}
	talkingHeadEvidence := newLiveTalkingHeadEvidenceChain(fixture)
	receiptMiddleware := newToolRecoveryMiddleware(
		func(string) bool { return false }, service.tools.ModelReceiptPolicy,
	)
	for stepIndex := 0; stepIndex < suite.MaxSteps; stepIndex++ {
		stepReport := liveWorkflowStepReport{
			Step: fmt.Sprintf("step_%02d", stepIndex+1),
		}
		selectionMessages := append([]*schema.Message{}, history...)
		selected, err := loadedModelActionSpecs(ctx, service.tools, selectionMessages)
		if err != nil {
			stepReport.Error = "已加载 action schema 解析失败: " + err.Error()
			report.Steps = append(report.Steps, stepReport)
			report.Error = stepReport.Error
			return report
		}
		stepReport.BoundTools = liveWorkflowSpecNames(selected)
		if legacyBound := retiredToolsIn(stepReport.BoundTools); len(legacyBound) > 0 {
			report.LegacyBoundTools = append(report.LegacyBoundTools, legacyBound...)
			stepReport.Error = "已加载 action schema 仍包含旧复合工具: " + strings.Join(legacyBound, ",")
			report.Steps = append(report.Steps, stepReport)
			report.Error = stepReport.Error
			return report
		}
		bound, err := bindLiveWorkflowTools(ctx, chat, selected)
		if err != nil {
			stepReport.Error = "绑定已加载 action schema 失败: " + err.Error()
			report.Steps = append(report.Steps, stepReport)
			report.Error = stepReport.Error
			return report
		}
		snapshot, err := NewContextBuilder(service.database).Snapshot(ctx, fixture.DraftID)
		if err != nil {
			stepReport.Error = "重建 WorldState 失败: " + err.Error()
			report.Steps = append(report.Steps, stepReport)
			report.Error = stepReport.Error
			return report
		}
		calls, err := liveGenerateWorkflowToolCalls(ctx, bound, snapshot, selectionMessages)
		if err == nil && len(calls) == 0 {
			stepReport.Succeeded = true
			report.Steps = append(report.Steps, stepReport)
			finalErr := validateLiveWorkflowFinalState(ctx, service, fixture, suite.Name)
			report.FinalStateValid = finalErr == nil
			if suite.Name == "talking_head" {
				if evidenceErr := talkingHeadEvidence.validate(); evidenceErr != nil {
					report.EvidenceChainValid = false
					report.EvidenceChainError = evidenceErr.Error()
				} else {
					report.EvidenceChainValid = true
				}
			}
			report.ObservedEvidence, report.MissingEvidence =
				liveWorkflowEvidenceStatus(suite.RequiredEvidence, observedEvidence)
			if finalErr == nil && len(report.MissingEvidence) == 0 && report.EvidenceChainValid {
				report.Succeeded = len(report.LegacyBoundTools) == 0 && len(report.LegacyToolCalls) == 0
				return report
			}
			report.Error = fmt.Sprintf(
				"模型提前收尾: final=%v missing=%v evidence=%s",
				finalErr, report.MissingEvidence, report.EvidenceChainError,
			)
			return report
		}
		names := make([]string, 0, len(calls))
		stepReport.Calls = make([]liveWorkflowCallReport, 0, len(calls))
		for callIndex := range calls {
			call := &calls[callIndex]
			names = append(names, call.Function.Name)
			stepReport.Calls = append(stepReport.Calls, liveWorkflowCallReport{
				Actual: call.Function.Name, Arguments: call.Function.Arguments,
			})
			if err == nil && strings.TrimSpace(call.ID) == "" {
				err = fmt.Errorf(
					"模型工具调用[%d] %s 缺少 provider tool_call_id",
					callIndex, call.Function.Name,
				)
			}
			if containsToolName(retiredLLMToolNames, call.Function.Name) {
				report.LegacyToolCalls = append(report.LegacyToolCalls, call.Function.Name)
			}
			if err == nil && !containsToolName(suite.AllowedTools, call.Function.Name) {
				err = fmt.Errorf(
					"workflow %s 选择了越界工具 %s，允许=%s",
					suite.Name, call.Function.Name, strings.Join(suite.AllowedTools, ","),
				)
			}
			if err == nil {
				spec, ok := liveWorkflowSpecByName(selected, call.Function.Name)
				if !ok {
					err = fmt.Errorf("已绑定工具面缺少模型选择的 spec: %s", call.Function.Name)
				} else {
					err = validateLiveToolArguments(spec, call.Function.Arguments)
				}
			}
		}
		stepReport.Actual = strings.Join(names, ",")
		if len(calls) == 1 {
			stepReport.Arguments = calls[0].Function.Arguments
		}
		var toolMessages []*schema.Message
		if err == nil {
			toolMessages, err = invokeLiveWorkflowTools(
				ctx, service, selected, calls, receiptMiddleware,
			)
		}
		if err != nil {
			stepReport.Error = err.Error()
			report.Steps = append(report.Steps, stepReport)
			report.Error = stepReport.Error
			return report
		}
		stepReport.Succeeded = true
		for callIndex, call := range calls {
			output := toolMessages[callIndex].Content
			if validationErr := validateLiveWorkflowToolOutput(
				call.Function.Name, output,
			); validationErr != nil {
				stepReport.Error = validationErr.Error()
				report.Steps = append(report.Steps, stepReport)
				report.Error = stepReport.Error
				return report
			}
			succeeded := liveWorkflowToolOutputSucceeded(call.Function.Name, output)
			stepReport.Calls[callIndex].Succeeded = succeeded
			stepReport.Succeeded = stepReport.Succeeded && succeeded
			if succeeded && liveWorkflowToolOutputProvidesEvidence(
				call.Function.Name, output,
			) {
				observedEvidence[call.Function.Name] = true
			}
			if succeeded && suite.Name == "talking_head" {
				if evidenceErr := talkingHeadEvidence.observe(call, output); evidenceErr != nil {
					stepReport.Error = "口播证据链解析失败: " + evidenceErr.Error()
					report.Steps = append(report.Steps, stepReport)
					report.Error = stepReport.Error
					return report
				}
			}
		}
		report.ObservedEvidence, report.MissingEvidence =
			liveWorkflowEvidenceStatus(suite.RequiredEvidence, observedEvidence)
		report.Steps = append(report.Steps, stepReport)
		history = append(history, schema.AssistantMessage("", calls))
		history = append(history, toolMessages...)
	}
	finalErr := validateLiveWorkflowFinalState(ctx, service, fixture, suite.Name)
	if finalErr == nil {
		report.FinalStateValid = true
	}
	if suite.Name == "talking_head" {
		if evidenceErr := talkingHeadEvidence.validate(); evidenceErr != nil {
			report.EvidenceChainValid = false
			report.EvidenceChainError = evidenceErr.Error()
		} else {
			report.EvidenceChainValid = true
			report.EvidenceChainError = ""
		}
	}
	report.ObservedEvidence, report.MissingEvidence =
		liveWorkflowEvidenceStatus(suite.RequiredEvidence, observedEvidence)
	switch {
	case finalErr != nil:
		report.Error = fmt.Sprintf(
			"达到最多 %d 步且最终状态未收敛: %v",
			suite.MaxSteps, finalErr,
		)
	case len(report.MissingEvidence) > 0:
		report.Error = fmt.Sprintf(
			"达到最多 %d 步且最终状态有效，但缺少必需检测/检索证据: %s",
			suite.MaxSteps, strings.Join(report.MissingEvidence, ","),
		)
	case !report.EvidenceChainValid:
		report.Error = fmt.Sprintf(
			"达到最多 %d 步且最终状态有效，但口播编辑证据链无效: %s",
			suite.MaxSteps, report.EvidenceChainError,
		)
	default:
		report.Error = fmt.Sprintf("达到最多 %d 步但 workflow 未完成", suite.MaxSteps)
	}
	return report
}

func liveGenerateWorkflowToolCalls(
	parent context.Context,
	chat model.ToolCallingChatModel,
	snapshot WorldStateSnapshot,
	history []*schema.Message,
) ([]schema.ToolCall, error) {
	rawSnapshot, err := snapshot.Marshal()
	if err != nil {
		return nil, err
	}
	worldState := schema.SystemMessage("【WorldState 参考快照】\n" + string(rawSnapshot))
	worldState.Extra = map[string]any{"context_phase": "world_state_reference"}
	messages := []*schema.Message{schema.SystemMessage(coreSystemPrompt), worldState}
	if playbook := taskPlaybookMessage(snapshot); playbook != nil {
		messages = append(messages, playbook)
	}
	messages = append(messages, history...)

	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	response, generateErr := chat.Generate(ctx, messages)
	cancel()
	switch {
	case generateErr != nil:
		return nil, generateErr
	case response == nil:
		return nil, errors.New("模型返回 nil")
	case len(response.ToolCalls) == 0:
		return nil, nil
	default:
		return response.ToolCalls, nil
	}
}

func bindLiveWorkflowTools(
	ctx context.Context,
	chat model.ToolCallingChatModel,
	specs []rushestools.Spec,
) (model.ToolCallingChatModel, error) {
	infos := make([]*schema.ToolInfo, 0, len(specs))
	for _, spec := range specs {
		info, err := spec.Implementation.Info(ctx)
		if err != nil {
			return nil, fmt.Errorf("%s schema: %w", spec.Name, err)
		}
		infos = append(infos, info)
	}
	return chat.WithTools(infos)
}

func invokeLiveWorkflowTools(
	ctx context.Context,
	service *Service,
	specs []rushestools.Spec,
	calls []schema.ToolCall,
	middlewares ...compose.ToolMiddleware,
) ([]*schema.Message, error) {
	router, err := newToolRouter(
		ctx,
		compose.ToolsNodeConfig{
			Tools:               implementationsForSpecs(specs),
			ToolCallMiddlewares: middlewares,
		},
		service.tools.Spec,
	)
	if err != nil {
		return nil, err
	}
	messages, err := router.Invoke(ctx, schema.AssistantMessage("", calls))
	if err != nil {
		return nil, err
	}
	if len(messages) != len(calls) {
		return nil, fmt.Errorf(
			"工具结果数=%d，期望与调用数=%d 一致", len(messages), len(calls),
		)
	}
	for index, message := range messages {
		if message == nil || message.Role != schema.Tool ||
			message.ToolCallID != calls[index].ID ||
			message.ToolName != calls[index].Function.Name {
			return nil, fmt.Errorf(
				"工具结果[%d] 未按调用顺序返回: message=%#v call=%#v",
				index, message, calls[index],
			)
		}
	}
	return messages, nil
}

func validateLiveWorkflowToolOutput(toolName, output string) error {
	switch toolName {
	case "shot.search":
		var result rushestools.ShotSearchResult
		if err := json.Unmarshal([]byte(output), &result); err != nil {
			return fmt.Errorf("执行 %s 返回非法 ShotSearchResult: %w", toolName, err)
		}
		for _, shot := range result.Shots {
			if shot.ShotID == "" || shot.AssetID == "" || shot.DurationFrames <= 0 ||
				(shot.SemanticRole != "" && shot.SemanticRole != "b_roll") {
				return fmt.Errorf("执行 %s 返回非 B-roll 或不完整镜头: %#v", toolName, shot)
			}
		}
		return nil
	case "audio.analyze_beats":
		var result rushestools.AudioBeatAnalysisResult
		if err := json.Unmarshal([]byte(output), &result); err != nil {
			return fmt.Errorf("执行 %s 返回非法 AudioBeatAnalysisResult: %w", toolName, err)
		}
		if result.AssetID == "" || result.TimelineFPS <= 0 || len(result.BeatFrames) == 0 {
			return fmt.Errorf(
				"执行 %s 未返回可核验节拍: asset_id=%q timeline_fps=%d beats=%d",
				toolName, result.AssetID, result.TimelineFPS, len(result.BeatFrames),
			)
		}
		return nil
	case "speech.search":
		var result rushestools.SpeechSearchResult
		if err := json.Unmarshal([]byte(output), &result); err != nil {
			return fmt.Errorf("执行 %s 返回非法 SpeechSearchResult: %w", toolName, err)
		}
		if result.Status != string(rushestools.StatusSucceeded) &&
			result.Status != string(rushestools.StatusFailed) {
			return fmt.Errorf(
				"执行 %s 返回未知口播状态: status=%s utterances=%d recovery=%s",
				toolName, result.Status, len(result.Utterances), result.Recovery,
			)
		}
		return nil
	default:
		var result rushestools.ToolResult
		if err := json.Unmarshal([]byte(output), &result); err != nil {
			return fmt.Errorf("执行 %s 返回非法 ToolResult: %w", toolName, err)
		}
		switch result.Status {
		case string(rushestools.StatusSucceeded),
			string(rushestools.StatusRejected),
			string(rushestools.StatusFailed),
			string(rushestools.StatusValidationFailed):
			return nil
		default:
			return fmt.Errorf(
				"执行 %s 返回非终态或未知状态 %s: %s",
				toolName, result.Status, result.Observation,
			)
		}
	}
}

func liveWorkflowToolOutputSucceeded(toolName, output string) bool {
	switch toolName {
	case "shot.search", "audio.analyze_beats":
		return true
	case "speech.search":
		var result rushestools.SpeechSearchResult
		return json.Unmarshal([]byte(output), &result) == nil &&
			result.Status == string(rushestools.StatusSucceeded)
	default:
		var result rushestools.ToolResult
		return json.Unmarshal([]byte(output), &result) == nil &&
			result.Status == string(rushestools.StatusSucceeded)
	}
}

func liveWorkflowToolOutputProvidesEvidence(toolName, output string) bool {
	switch toolName {
	case "shot.search":
		var result rushestools.ShotSearchResult
		return json.Unmarshal([]byte(output), &result) == nil && len(result.Shots) > 0
	case "audio.analyze_beats":
		var result rushestools.AudioBeatAnalysisResult
		return json.Unmarshal([]byte(output), &result) == nil && len(result.BeatFrames) > 0
	case "speech.search":
		var result rushestools.SpeechSearchResult
		return json.Unmarshal([]byte(output), &result) == nil &&
			result.Status == string(rushestools.StatusSucceeded) &&
			len(result.Utterances) > 0
	default:
		return false
	}
}

func liveWorkflowEvidenceStatus(
	required []string,
	observed map[string]bool,
) ([]string, []string) {
	succeeded := make([]string, 0, len(required))
	missing := make([]string, 0, len(required))
	for _, toolName := range required {
		if observed[toolName] {
			succeeded = append(succeeded, toolName)
		} else {
			missing = append(missing, toolName)
		}
	}
	return succeeded, missing
}

type liveTalkingHeadEvidenceChain struct {
	fixture                  liveWorkflowFixture
	sourceDeletes            map[string]bool
	searchedShotRanges       map[string]bool
	postDeleteARollClipIDs   map[string]bool
	deleteStarted            bool
	initialSpeechObserved    bool
	anchorTimelineStartFrame *int
	anchorSearchObserved     bool
	anchoredBrollInserted    bool
}

func newLiveTalkingHeadEvidenceChain(
	fixture liveWorkflowFixture,
) *liveTalkingHeadEvidenceChain {
	return &liveTalkingHeadEvidenceChain{
		fixture:                fixture,
		sourceDeletes:          map[string]bool{},
		searchedShotRanges:     map[string]bool{},
		postDeleteARollClipIDs: map[string]bool{},
	}
}

func (chain *liveTalkingHeadEvidenceChain) observe(
	call schema.ToolCall,
	output string,
) error {
	switch call.Function.Name {
	case "shot.search":
		var result rushestools.ShotSearchResult
		if err := json.Unmarshal([]byte(output), &result); err != nil {
			return err
		}
		for _, shot := range result.Shots {
			chain.searchedShotRanges[fmt.Sprintf(
				"%s:%d-%d",
				shot.AssetID, shot.SourceStartFrame, shot.SourceEndFrame,
			)] = true
		}
	case "timeline.delete":
		var input struct {
			Kind             string `json:"kind"`
			AssetID          string `json:"asset_id"`
			SourceStartFrame int    `json:"source_start_frame"`
			SourceEndFrame   int    `json:"source_end_frame"`
		}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
			return err
		}
		if input.Kind == "delete_source_range" &&
			input.AssetID == chain.fixture.PrimaryID {
			chain.deleteStarted = true
			key := fmt.Sprintf("%d-%d", input.SourceStartFrame, input.SourceEndFrame)
			switch key {
			case "0-90", "240-600", "720-780":
				chain.sourceDeletes[key] = true
			}
		}
	case "timeline.inspect":
		if !chain.allSourceDeletesObserved() {
			return nil
		}
		var result struct {
			Data struct {
				Tracks []struct {
					TrackID string `json:"track_id"`
					Clips   []struct {
						TimelineClipID   string `json:"timeline_clip_id"`
						AssetID          string `json:"asset_id"`
						Role             string `json:"role"`
						SourceStartFrame int    `json:"source_start_frame"`
						SourceEndFrame   int    `json:"source_end_frame"`
					} `json:"clips"`
				} `json:"tracks"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(output), &result); err != nil {
			return err
		}
		for _, track := range result.Data.Tracks {
			if track.TrackID != "visual_base" {
				continue
			}
			for _, clip := range track.Clips {
				if clip.AssetID == chain.fixture.PrimaryID &&
					clip.Role == "a_roll" &&
					clip.SourceStartFrame <= 1320 &&
					clip.SourceEndFrame >= 1440 &&
					clip.TimelineClipID != "" {
					chain.postDeleteARollClipIDs[clip.TimelineClipID] = true
				}
			}
		}
	case "speech.search":
		var input rushestools.SpeechSearchInput
		if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
			return err
		}
		var result rushestools.SpeechSearchResult
		if err := json.Unmarshal([]byte(output), &result); err != nil {
			return err
		}
		if !chain.deleteStarted &&
			result.AssetID == chain.fixture.PrimaryID &&
			input.IncludeWords &&
			len(result.Utterances) > 0 &&
			len(result.Pauses) > 0 &&
			len(result.SimilarPairs) > 0 {
			chain.initialSpeechObserved = true
		}
		hasTargetQueryOrWindow := strings.TrimSpace(input.Query) != "" ||
			(input.SourceStartFrame != nil &&
				input.SourceEndFrame != nil &&
				*input.SourceStartFrame <= 1320 &&
				*input.SourceEndFrame >= 1440)
		if !chain.allSourceDeletesObserved() ||
			strings.TrimSpace(input.TimelineClipID) == "" ||
			!hasTargetQueryOrWindow {
			return nil
		}
		if result.TimelineClipID != input.TimelineClipID {
			return nil
		}
		for _, utterance := range result.Utterances {
			if utterance.SourceStartFrame != 1320 ||
				utterance.SourceEndFrame != 1440 ||
				!strings.Contains(
					agentexec.NormalizeSpeechText(utterance.Text),
					agentexec.NormalizeSpeechText("指纹解锁按键仍然位于键盘右上角"),
				) ||
				utterance.TimelineStartFrame == nil ||
				utterance.TimelineEndFrame == nil ||
				*utterance.TimelineEndFrame <= *utterance.TimelineStartFrame {
				continue
			}
			start := *utterance.TimelineStartFrame
			chain.anchorTimelineStartFrame = &start
			chain.anchorSearchObserved = true
			break
		}
	case "timeline.insert":
		if !chain.anchorSearchObserved ||
			chain.anchorTimelineStartFrame == nil {
			return nil
		}
		var input struct {
			Kind               string `json:"kind"`
			TrackID            string `json:"track_id"`
			AssetID            string `json:"asset_id"`
			Role               string `json:"role"`
			SourceStartFrame   int    `json:"source_start_frame"`
			SourceEndFrame     int    `json:"source_end_frame"`
			TimelineStartFrame int    `json:"timeline_start_frame"`
		}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
			return err
		}
		if input.Kind == "insert_clip" &&
			input.TrackID == "visual_overlay" &&
			(input.Role == "" || input.Role == "b_roll") &&
			chain.searchedShotRanges[fmt.Sprintf(
				"%s:%d-%d",
				input.AssetID, input.SourceStartFrame, input.SourceEndFrame,
			)] &&
			input.TimelineStartFrame == *chain.anchorTimelineStartFrame {
			chain.anchoredBrollInserted = true
		}
	}
	return nil
}

func (chain *liveTalkingHeadEvidenceChain) allSourceDeletesObserved() bool {
	return chain.sourceDeletes["0-90"] &&
		chain.sourceDeletes["240-600"] &&
		chain.sourceDeletes["720-780"]
}

func (chain *liveTalkingHeadEvidenceChain) validate() error {
	missing := make([]string, 0, 4)
	if !chain.initialSpeechObserved {
		missing = append(missing, "删除前含逐词、气口和相似重讲的 speech.search 证据")
	}
	if !chain.allSourceDeletesObserved() {
		missing = append(missing, "三次 delete_source_range 成功调用")
	}
	if !chain.anchorSearchObserved {
		missing = append(missing, "携当前 clip ID 的 speech.search 时间线锚点")
	}
	if len(chain.searchedShotRanges) == 0 {
		missing = append(missing, "shot.search 返回的可用镜头范围")
	}
	if !chain.anchoredBrollInserted {
		missing = append(missing, "直接采用 speech.search 坐标的 B-roll 插入")
	}
	if len(missing) > 0 {
		return errors.New(strings.Join(missing, "；"))
	}
	return nil
}

func validateLiveWorkflowFinalState(
	ctx context.Context,
	service *Service,
	fixture liveWorkflowFixture,
	suite string,
) error {
	document, err := timeline.Latest(ctx, service.database, fixture.DraftID)
	if err != nil {
		return fmt.Errorf("读取最终时间线: %w", err)
	}
	if report := timeline.Validate(document); !report.Valid {
		return fmt.Errorf("最终时间线结构无效: %v", report.Issues)
	}
	rawCheck, err := service.executor.ExecuteTool(
		ctx,
		"timeline.check",
		rushestools.TimelineCheckInput{},
	)
	if err != nil {
		return fmt.Errorf("独立执行最终检查: %w", err)
	}
	check, ok := rawCheck.(rushestools.ToolResult)
	if !ok {
		return fmt.Errorf("最终检查结果类型=%T", rawCheck)
	}
	if check.Status != string(rushestools.StatusSucceeded) {
		return fmt.Errorf("最终检查状态=%s observation=%s", check.Status, check.Observation)
	}

	switch suite {
	case "initial_composition":
		primary := timelineTrackClips(document, "visual_base")
		if document.Version != 2 || document.DurationFrames != 120 || len(primary) != 2 ||
			primary[0].AssetID != fixture.PrimaryID ||
			primary[0].SourceStartFrame != 0 || primary[0].SourceEndFrame != 60 ||
			primary[0].TimelineStartFrame != 0 || primary[0].TimelineEndFrame != 60 ||
			primary[1].AssetID != fixture.SecondaryID ||
			primary[1].SourceStartFrame != 0 || primary[1].SourceEndFrame != 60 ||
			primary[1].TimelineStartFrame != 60 || primary[1].TimelineEndFrame != 120 {
			return fmt.Errorf(
				"初版最终状态不符: version=%d duration=%d primary=%#v",
				document.Version, document.DurationFrames, primary,
			)
		}
	case "beat_mix":
		primary := timelineTrackClips(document, "visual_base")
		bgm := timelineTrackClips(document, "bgm")
		if document.Version != 3 || document.DurationFrames != 120 ||
			len(primary) != 2 || len(bgm) != 1 ||
			primary[0].AssetID != fixture.PrimaryID ||
			primary[0].SourceStartFrame != 0 || primary[0].SourceEndFrame != 60 ||
			primary[0].TimelineStartFrame != 0 || primary[0].TimelineEndFrame != 60 ||
			primary[1].AssetID != fixture.SecondaryID ||
			primary[1].SourceStartFrame != 0 || primary[1].SourceEndFrame != 60 ||
			primary[1].TimelineStartFrame != 60 || primary[1].TimelineEndFrame != 120 ||
			bgm[0].AssetID != fixture.AudioID || bgm[0].Role != "bgm" ||
			bgm[0].SourceStartFrame != 0 || bgm[0].SourceEndFrame != 120 ||
			bgm[0].TimelineStartFrame != 0 || bgm[0].TimelineEndFrame != 120 {
			return fmt.Errorf(
				"卡点最终状态不符: version=%d duration=%d primary=%#v bgm=%#v",
				document.Version, document.DurationFrames, primary, bgm,
			)
		}
		beatGrid, _ := bgm[0].Metadata["beat_grid"].(map[string]any)
		if err := liveWorkflowMapContains(beatGrid, map[string]any{
			"bpm":                120,
			"beat_frames":        []any{0, 15, 30, 45, 60, 75, 90, 105},
			"strong_beat_frames": []any{0, 60},
			"downbeat_frames":    []any{0, 60},
			"bar_phase":          0,
			"analysis_method":    "aubio-tempo+specflux-onset",
		}, "beat_grid"); err != nil {
			return fmt.Errorf("卡点最终 BGM 缺少完整 beat_grid: %v metadata=%#v", err, bgm[0].Metadata)
		}
	case "talking_head":
		if document.Version != 6 ||
			sourceCoverage(document, fixture.PrimaryID, 240, 600) != 0 ||
			sourceCoverage(document, fixture.PrimaryID, 900, 1260) != 360 ||
			sourceCoverage(document, fixture.PrimaryID, 0, 90) != 0 ||
			sourceCoverage(document, fixture.PrimaryID, 90, 180) != 90 ||
			sourceCoverage(document, fixture.PrimaryID, 720, 780) != 0 {
			return fmt.Errorf(
				"口播删除结果不符: version=%d primary=%#v",
				document.Version, timelineTrackClips(document, "visual_base"),
			)
		}
		fingerprintStart, _, fingerprintMapped := sourceRangeOnCurrentTimeline(
			document, fixture.PrimaryID, 1320, 1440,
		)
		if !fingerprintMapped {
			return errors.New("保留的指纹解锁句缺少当前时间线映射")
		}
		overlays := timelineTrackClips(document, "visual_overlay")
		if len(overlays) != 1 ||
			overlays[0].AssetID != fixture.SecondaryID ||
			overlays[0].TimelineStartFrame != fingerprintStart ||
			overlays[0].TimelineEndFrame-overlays[0].TimelineStartFrame < 45 ||
			overlays[0].SourceStartFrame != 0 ||
			overlays[0].SourceEndFrame != 90 ||
			overlays[0].Role != "b_roll" ||
			overlays[0].FadeInFrames != 7 ||
			overlays[0].FadeOutFrames != 7 {
			return fmt.Errorf("口播 B-roll 最终状态不符: %#v", overlays)
		}
		if !reflect.DeepEqual(
			clipSourceRanges(timelineTrackClips(document, "visual_base")),
			clipSourceRanges(timelineTrackClips(document, "original_audio")),
		) {
			return errors.New("口播原声音画 source coverage 漂移")
		}
		quality, _ := check.Data["speech_quality"].(map[string]any)
		if quality["residual_breath_count"] != 0 ||
			quality["short_retained_island_count"] != 0 ||
			quality["short_b_roll_clip_count"] != 0 {
			return fmt.Errorf("口播质量未收敛: %#v", quality)
		}
	default:
		return fmt.Errorf("未知 live workflow suite %q", suite)
	}
	return nil
}

func liveWorkflowSuites() []liveWorkflowSuite {
	return []liveWorkflowSuite{
		{
			Name: "initial_composition",
			Goal: "请把当前草稿中唯一的城市夜景完整镜头与唯一的山峰云海完整镜头，按城市在前、山峰在后，组成一条总长 120 帧的初版时间线。" +
				"素材清单已由 Harness 注入 material_catalog；编辑前必须实际检索可用镜头证据，不裁短镜头，完成后直接声明交付边界。" +
				"请自行读取所需事实并通过可组合原语连续推进；独立只读可以在同一消息并行，每个写调用只做一个可观察动作。",
			MaxSteps: 8,
			AllowedTools: []string{
				"plan.update",
				"shot.search", "timeline.insert",
			},
			RequiredEvidence: []string{"shot.search"},
		},
		{
			Name: "beat_mix",
			Goal: "请完成一条 4 秒、120 帧的卡点混剪：唯一的城市推进完整镜头在前，唯一的山峰揭示完整镜头在后，两个镜头等长且不裁短；" +
				"唯一的 BGM 覆盖全程；Harness 必须按需分析并自动投影完整节拍证据，模型不得搬运 beat grid。完成后主动检查结构和拍点对齐。" +
				"编辑前必须实际检索可用镜头证据，并使用 WorldState 中 Harness 注入的完整 beat_analysis。" +
				"请自行读取所需事实、选择调用顺序并连续推进；独立只读可以在同一消息并行，每个写调用只做一个可观察动作。",
			MaxSteps: 10,
			AllowedTools: []string{
				"plan.update",
				"shot.search",
				"timeline.insert",
			},
			RequiredEvidence: []string{"shot.search"},
		},
		{
			Name: "talking_head",
			Goal: "请完成这条 #140 形态口播的可回滚初剪。请依据持久化逐句、逐词、相似重讲、气口和镜头证据自主定位并落实这些创作决定：" +
				"开头完全相同的负面评价只保留较后一遍；前后两组各四句的键盘同义重讲只保留较晚且完整的一组；删除中间唯一的显著气口；" +
				"除这三处明确的删除决定外，其余口播都必须保留，包括两组重讲之间的过渡句和较晚一组之前的重讲引导；" +
				"保留“指纹解锁按键仍然位于键盘右上角”这句，并把唯一的键盘、同色键帽与指纹按键完整特写覆盖到该句当前时间线起点，长度使用该镜头完整 90 帧，淡入淡出各 7 帧。" +
				"编辑前必须实际检索逐词口播证据和可用镜头证据，不得只凭 WorldState 中的摘要直接写入；" +
				"放置特写时必须用检索到的该句 source range；删除后从 Harness 刷新的 CurrentTimelineView 找到覆盖它的当前 A-roll clip ID，再以该 clip ID 重查原句并直接使用返回的当前时间线起点，不得自行估算或拿较晚键盘重讲的起点代替。" +
				"三处删除决定必须分别有成功的时间线写入结果；完成前逐项对照原始目标与真实 ToolResult，任何一处尚未成功就继续执行，不能用计划更新或最终文字代替。" +
				"中间编辑只依据真实回执和刷新后的 CurrentTimelineView 推进；完成前让 Stop Gate 对最新版本统一检查结构、内容、口播质量和 B-roll 最短时长。" +
				"请从初始目标、WorldState 与每一步真实 observation 自主选择下一原语；独立只读可以在同一消息并行，每个写调用只做一个可观察动作。",
			MaxSteps: 20,
			AllowedTools: []string{
				"plan.update",
				"speech.search",
				"shot.search", "timeline.delete", "timeline.insert", "timeline.update",
			},
			RequiredEvidence: []string{"speech.search", "shot.search"},
		},
	}
}

// scriptedLiveWorkflowSteps 只为无付费的 Registry/fixture 执行回归提供确定输入。
// 真实模型门禁不读取此表，也不按预设工具序列或参数评分。
func scriptedLiveWorkflowSteps(
	fixture liveWorkflowFixture,
	suite string,
) []scriptedWorkflowStep {
	switch suite {
	case "initial_composition":
		return []scriptedWorkflowStep{
			{Name: "search_shots", ExpectedTool: "shot.search", ExpectedArguments: map[string]any{
				"query": "城市 山峰", "semantic_roles": []any{"b_roll"},
				"min_duration_frames": 45, "top_k": 8,
			}},
			{Name: "insert_city", ExpectedTool: "timeline.insert", ExpectedArguments: map[string]any{
				"kind": "insert_clip", "asset_id": fixture.PrimaryID,
				"source_start_frame": 0, "source_end_frame": 60,
			}},
			{Name: "insert_mountain", ExpectedTool: "timeline.insert", ExpectedArguments: map[string]any{
				"kind": "insert_clip", "asset_id": fixture.SecondaryID,
				"source_start_frame": 0, "source_end_frame": 60,
			}},
		}
	case "beat_mix":
		return []scriptedWorkflowStep{
			{Name: "search_shots", ExpectedTool: "shot.search", ExpectedArguments: map[string]any{
				"query": "城市 山峰", "semantic_roles": []any{"b_roll"},
				"min_duration_frames": 60, "top_k": 8,
			}},
			{Name: "insert_first_visual", ExpectedTool: "timeline.insert", ExpectedArguments: map[string]any{
				"kind": "insert_clip", "asset_id": fixture.PrimaryID,
				"source_start_frame": 0, "source_end_frame": 60,
			}},
			{Name: "insert_second_visual", ExpectedTool: "timeline.insert", ExpectedArguments: map[string]any{
				"kind": "insert_clip", "asset_id": fixture.SecondaryID,
				"source_start_frame": 0, "source_end_frame": 60,
			}},
			{Name: "insert_bgm", ExpectedTool: "timeline.insert", ExpectedArguments: map[string]any{
				"kind": "insert_clip", "track_id": "bgm", "asset_id": fixture.AudioID,
				"source_start_frame": 0, "source_end_frame": 120,
			}},
		}
	case "talking_head":
		return []scriptedWorkflowStep{
			{Name: "search_speech", ExpectedTool: "speech.search", ExpectedArguments: map[string]any{
				"asset_id": fixture.PrimaryID, "include_words": true,
			}},
			{Name: "search_broll", ExpectedTool: "shot.search", ExpectedArguments: map[string]any{
				"query": "键盘 同色键帽 指纹按键", "min_duration_frames": 45, "top_k": 5,
			}},
			{Name: "delete_earlier_similarity", ExpectedTool: "timeline.delete", ExpectedArguments: map[string]any{
				"kind": "delete_source_range", "asset_id": fixture.PrimaryID,
				"source_start_frame": 240, "source_end_frame": 600,
			}},
			{Name: "delete_opening_repetition", ExpectedTool: "timeline.delete", ExpectedArguments: map[string]any{
				"kind": "delete_source_range", "asset_id": fixture.PrimaryID,
				"source_start_frame": 0, "source_end_frame": 90,
			}},
			{Name: "delete_breath_pause", ExpectedTool: "timeline.delete", ExpectedArguments: map[string]any{
				"kind": "delete_source_range", "asset_id": fixture.PrimaryID,
				"source_start_frame": 720, "source_end_frame": 780,
			}},
			{Name: "search_fingerprint_after_deletes", ExpectedTool: "speech.search", ExpectedArguments: map[string]any{
				"timeline_clip_id": "clip_v1_008",
				"query":            "指纹解锁按键仍然位于键盘右上角",
				"max_utterances":   5,
				"include_pauses":   false,
				"include_similar":  false,
			}},
			{Name: "insert_broll", ExpectedTool: "timeline.insert", ExpectedArguments: map[string]any{
				"kind": "insert_clip", "track_id": "visual_overlay",
				"asset_id": fixture.SecondaryID, "timeline_start_frame": 810,
				"source_start_frame": 0, "source_end_frame": 90, "role": "b_roll",
			}},
			{Name: "fade_broll", ExpectedTool: "timeline.update", ExpectedArguments: map[string]any{
				"kind": "set_clip_fades", "fade_in_frames": 7, "fade_out_frames": 7,
			}},
		}
	default:
		return nil
	}
}

func prepareLiveWorkflowBeatEnvironment(t *testing.T) {
	t.Helper()
	fakeBin := t.TempDir()
	scripts := map[string]string{
		"aubiotrack": "#!/bin/sh\nprintf '0.000000\\n0.500000\\n1.000000\\n1.500000\\n2.000000\\n2.500000\\n3.000000\\n3.500000\\n'\n",
		"aubioonset": "#!/bin/sh\nprintf '0.000000\\n2.000000\\n'\n",
	}
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(fakeBin, name), []byte(body), 0o755); err != nil {
			t.Fatalf("创建 live workflow %s fixture 失败: %v", name, err)
		}
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func prepareLiveWorkflowFixture(
	t *testing.T,
	service *Service,
	suite string,
	run int,
) (liveWorkflowFixture, error) {
	t.Helper()
	fixture := liveWorkflowFixture{
		DraftID:     fmt.Sprintf("draft_live_%s_%d", suite, run),
		PrimaryID:   fmt.Sprintf("asset_live_%s_%d_primary", suite, run),
		SecondaryID: fmt.Sprintf("asset_live_%s_%d_secondary", suite, run),
		AudioID:     fmt.Sprintf("asset_live_%s_%d_audio", suite, run),
	}
	agenttest.CreateAgentDraft(t, service.database, fixture.DraftID)
	switch suite {
	case "initial_composition":
		if err := seedLiveWorkflowVideo(
			t.Context(), service, fixture.DraftID, fixture.PrimaryID,
			"城市夜景推进镜头", "城市 夜景", 60, false,
		); err != nil {
			return fixture, err
		}
		if err := seedLiveWorkflowVideo(
			t.Context(), service, fixture.DraftID, fixture.SecondaryID,
			"山峰云海揭示镜头", "山峰 云海", 60, false,
		); err != nil {
			return fixture, err
		}
	case "beat_mix":
		if err := seedLiveWorkflowVideo(
			t.Context(), service, fixture.DraftID, fixture.PrimaryID,
			"城市推进镜头", "城市 推进", 60, false,
		); err != nil {
			return fixture, err
		}
		if err := seedLiveWorkflowVideo(
			t.Context(), service, fixture.DraftID, fixture.SecondaryID,
			"山峰揭示镜头", "山峰 揭示", 60, false,
		); err != nil {
			return fixture, err
		}
		audioPath := filepath.Join(
			service.database.Paths.Temporary,
			fmt.Sprintf("live-beat-%d.wav", run),
		)
		if _, err := media.RunCommand(
			t.Context(), "ffmpeg", "-y", "-f", "lavfi", "-i",
			"sine=frequency=440:sample_rate=44100:duration=4",
			"-c:a", "pcm_s16le", audioPath,
		); err != nil {
			return fixture, fmt.Errorf("创建 beat workflow 音频失败: %w", err)
		}
		if err := seedLiveWorkflowAudio(
			t.Context(), service, fixture.DraftID, fixture.AudioID, audioPath, 4,
		); err != nil {
			return fixture, err
		}
	case "talking_head":
		if err := seedLiveWorkflowVideo(
			t.Context(), service, fixture.DraftID, fixture.PrimaryID,
			"", "", 1560, true,
		); err != nil {
			return fixture, err
		}
		if err := seedLiveWorkflowVideo(
			t.Context(), service, fixture.DraftID, fixture.SecondaryID,
			"手指按压键盘右上角指纹键并展示同色键帽",
			"键盘 键帽 指纹 产品特写", 90, false,
		); err != nil {
			return fixture, err
		}
		utterances, err := json.Marshal(issue140AtomicUtterances())
		if err != nil {
			return fixture, err
		}
		pauses, err := json.Marshal([]map[string]any{{
			"pause_id":           "pause_issue_140_breath",
			"source_start_frame": 720, "source_end_frame": 780,
			"delete_start_frame": 720, "delete_end_frame": 780,
			"detection_method": "fixture_vad",
		}})
		if err != nil {
			return fixture, err
		}
		if _, err := service.database.Write().ExecContext(t.Context(), `
			INSERT INTO transcripts(
				transcript_id,asset_id,provider_id,raw_preserved,utterances_json,vad_segments_json
			) VALUES(?,?, 'live-workflow-fixture',0,?,?)`,
			"transcript_"+fixture.PrimaryID, fixture.PrimaryID, string(utterances), string(pauses),
		); err != nil {
			return fixture, fmt.Errorf("写入口播 transcript fixture: %w", err)
		}
		selections := make([]agenttest.TimelineSelection, 0, 8)
		for _, sourceRange := range [][2]int{
			{0, 90},
			{90, 240},
			{240, 420},
			{420, 600},
			{600, 780},
			{780, 1080},
			{1080, 1320},
			{1320, 1560},
		} {
			selections = append(selections, agenttest.TimelineSelection{
				AssetID: fixture.PrimaryID, AssetKind: "video", HasAudio: true,
				SourceStartFrame: sourceRange[0], SourceEndFrame: sourceRange[1], Role: "a_roll",
			})
		}
		document, err := agenttest.ComposeTimeline(fixture.DraftID, 1, selections)
		if err != nil {
			return fixture, err
		}
		persisted, err := seedTimelineVersion(service,
			t.Context(), fixture.DraftID, document, "live_workflow_fixture", nil)

		if err != nil {
			return fixture, fmt.Errorf("持久化口播 workflow 时间线: %w", err)
		}
		if persisted.Status != string(rushestools.StatusSucceeded) {
			return fixture, fmt.Errorf("持久化口播 workflow 时间线状态=%s", persisted.Status)
		}
	default:
		return fixture, fmt.Errorf("未知 live workflow suite %q", suite)
	}
	return fixture, nil
}

func seedLiveWorkflowVideo(
	ctx context.Context,
	service *Service,
	draftID string,
	assetID string,
	description string,
	tags string,
	durationFrames int,
	hasAudio bool,
) error {
	probe, err := json.Marshal(map[string]any{
		"duration_sec": float64(durationFrames) / liveWorkflowTimelineFPS,
		"has_audio":    hasAudio,
	})
	if err != nil {
		return err
	}
	referencePath := "/tmp/" + assetID + ".mp4"
	var size int64 = 1
	if hasAudio {
		referencePath, size, err = ensureLiveWorkflowVideoSource(
			ctx, service.database.Paths.Temporary, durationFrames,
		)
		if err != nil {
			return err
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	transaction, err := service.database.Write().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开始 video fixture %s 事务: %w", assetID, err)
	}
	defer func() { _ = transaction.Rollback() }()
	if _, err := transaction.ExecContext(ctx, `
		INSERT INTO assets(
			asset_id,storage_mode,reference_path,kind,source,filename,hash,size,
			probe_json,ingest_status,understanding_status,usable
		) VALUES(?, 'reference', ?, 'video', 'local_path', ?, ?, ?, ?, 'ready', 'ready', 1)`,
		assetID, referencePath, assetID+".mp4", assetID, size, string(probe),
	); err != nil {
		return fmt.Errorf("写入 video fixture asset %s: %w", assetID, err)
	}
	if _, err := transaction.ExecContext(ctx, `
		INSERT INTO draft_asset_links(draft_id,asset_id,rel_dir,linked_at)
		VALUES(?, ?, 'Broll', ?)`,
		draftID, assetID, now,
	); err != nil {
		return fmt.Errorf("关联 video fixture %s 到 draft %s: %w", assetID, draftID, err)
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("提交 video fixture %s: %w", assetID, err)
	}
	semanticRole := "b_roll"
	if description == "" {
		description = "人物正面口播镜头"
		tags = "人物 口播"
		semanticRole = "a_roll"
	}
	summary, err := json.Marshal(map[string]any{
		"asset_id": assetID, "semantic_role": semanticRole, "overall": description,
		"segments": []map[string]any{{
			"source_start_frame": 0, "source_end_frame": durationFrames,
			"description": description, "tags": strings.Fields(tags), "quality": "usable",
		}},
	})
	if err != nil {
		return err
	}
	if _, err := service.database.Write().ExecContext(ctx, `
		INSERT INTO material_summaries(
			summary_id,asset_id,version,status,summary_json,fingerprint,prompt_version,created_at
		) VALUES(?, ?, 1, 'ready', ?, ?, 'live-workflow-v1', ?)`,
		"summary_"+assetID, assetID, string(summary), "fingerprint_"+assetID, now,
	); err != nil {
		return fmt.Errorf("写入 material summary fixture %s: %w", assetID, err)
	}
	snapshotID := "shot_index_live_" + assetID
	snapshotSummary, _ := json.Marshal(map[string]any{"semantic_role": semanticRole})
	if _, err := service.database.Write().ExecContext(ctx, `
		INSERT INTO shot_index_snapshots(
			index_snapshot_id,asset_content_hash,generation,analyzer_version,
			output_schema_version,source_asset_id,status,summary_json,created_at,published_at
		) VALUES(?,?,1,'live-workflow-v1',1,?,'ready',?,?,?)`,
		snapshotID, assetID, assetID, string(snapshotSummary), now, now,
	); err != nil {
		return fmt.Errorf("写入 shot snapshot fixture %s: %w", assetID, err)
	}
	encodedTags, _ := json.Marshal(strings.Fields(tags))
	if _, err := service.database.Write().ExecContext(ctx, `
		INSERT INTO shots(
			index_snapshot_id,shot_id,asset_content_hash,source_start_frame,source_end_frame,
			boundary_version,boundary_kind,boundary_confidence,lineage_parent_shot_id,
			representative_frames_json,description,tags_json,subjects_json,actions_json,
			setting_json,shot_scale,composition,lighting_json,mood_json,edit_hints_json,
			quality_json,search_text,search_tokens_json,deep_coverage_json,created_at
		) VALUES(?,?,?,0,?,1,'fixture',1,NULL,'[]',?,?,'["画面主体"]','["展示"]',
			?,'中景','居中构图','[]','[]','[]','{"label":"usable"}',?,?,'[]',?)`,
		snapshotID, "shot_live_"+assetID, assetID, durationFrames,
		description, string(encodedTags), string(encodedTags),
		description+" "+tags, string(encodedTags), now,
	); err != nil {
		return fmt.Errorf("写入 shot row fixture %s: %w", assetID, err)
	}
	return nil
}

func ensureLiveWorkflowVideoSource(
	ctx context.Context,
	temporaryDirectory string,
	durationFrames int,
) (string, int64, error) {
	path := filepath.Join(
		temporaryDirectory,
		fmt.Sprintf("live-workflow-video-%d-audio.mp4", durationFrames),
	)
	if info, err := os.Stat(path); err == nil {
		return path, info.Size(), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", 0, fmt.Errorf("检查 live workflow 视频 fixture: %w", err)
	}
	durationSeconds := float64(durationFrames) / liveWorkflowTimelineFPS
	pauseStartSeconds := min(24.0, max(0.0, durationSeconds/2-1))
	pauseEndSeconds := min(durationSeconds, pauseStartSeconds+2)
	if _, err := media.RunCommand(
		ctx, "ffmpeg", "-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", fmt.Sprintf(
			"color=c=black:s=160x90:r=%d:d=%.6f",
			liveWorkflowTimelineFPS, durationSeconds,
		),
		"-f", "lavfi", "-i", fmt.Sprintf(
			"sine=frequency=440:sample_rate=16000:duration=%.6f",
			durationSeconds,
		),
		"-af", fmt.Sprintf(
			"volume=0:enable='between(t,%.6f,%.6f)'",
			pauseStartSeconds, pauseEndSeconds,
		),
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "35",
		"-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", path,
	); err != nil {
		return "", 0, fmt.Errorf("创建 live workflow 视频 fixture: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", 0, fmt.Errorf("读取 live workflow 视频 fixture: %w", err)
	}
	return path, info.Size(), nil
}

func seedLiveWorkflowAudio(
	ctx context.Context,
	service *Service,
	draftID string,
	assetID string,
	path string,
	durationSeconds int,
) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	probe, err := json.Marshal(map[string]any{
		"duration_sec": durationSeconds, "has_audio": true,
	})
	if err != nil {
		return err
	}
	transaction, err := service.database.Write().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开始 audio fixture %s 事务: %w", assetID, err)
	}
	defer func() { _ = transaction.Rollback() }()
	if _, err := transaction.ExecContext(ctx, `
		INSERT INTO assets(
			asset_id,storage_mode,reference_path,kind,source,filename,hash,size,
			probe_json,ingest_status,understanding_status,usable
		) VALUES(?, 'reference', ?, 'audio', 'local_path', ?, ?, ?, ?, 'ready', 'none', 1)`,
		assetID, path, filepath.Base(path), assetID, info.Size(), string(probe),
	); err != nil {
		return fmt.Errorf("写入 audio fixture asset %s: %w", assetID, err)
	}
	if _, err := transaction.ExecContext(ctx, `
		INSERT INTO draft_asset_links(draft_id,asset_id,rel_dir,linked_at)
		VALUES(?, ?, 'BGM', ?)`,
		draftID, assetID, time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("关联 audio fixture %s 到 draft %s: %w", assetID, draftID, err)
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("提交 audio fixture %s: %w", assetID, err)
	}
	return nil
}

func liveWorkflowMapContains(actual, expected map[string]any, path string) error {
	for key, want := range expected {
		got, ok := actual[key]
		if !ok {
			return fmt.Errorf("%s.%s 缺失", path, key)
		}
		switch typedWant := want.(type) {
		case map[string]any:
			typedGot, ok := got.(map[string]any)
			if !ok {
				return fmt.Errorf("%s.%s 类型=%T want=object", path, key, got)
			}
			if err := liveWorkflowMapContains(typedGot, typedWant, path+"."+key); err != nil {
				return err
			}
		case []any:
			typedGot, ok := got.([]any)
			if !ok || len(typedGot) != len(typedWant) {
				return fmt.Errorf("%s.%s=%v want=%v", path, key, got, want)
			}
			for index := range typedWant {
				if !liveWorkflowScalarEqual(typedGot[index], typedWant[index]) {
					return fmt.Errorf(
						"%s.%s[%d]=%v want=%v", path, key, index, typedGot[index], typedWant[index],
					)
				}
			}
		default:
			if !liveWorkflowScalarEqual(got, want) {
				return fmt.Errorf("%s.%s=%v want=%v", path, key, got, want)
			}
		}
	}
	return nil
}

func liveWorkflowScalarEqual(actual, expected any) bool {
	actualNumber, actualNumeric := agentexec.NumericValue(actual)
	expectedNumber, expectedNumeric := agentexec.NumericValue(expected)
	if actualNumeric || expectedNumeric {
		return actualNumeric && expectedNumeric && actualNumber == expectedNumber
	}
	return reflect.DeepEqual(actual, expected)
}

func liveWorkflowMetrics(runs []liveWorkflowRunReport) map[string]liveToolEvalMetric {
	metrics := map[string]liveToolEvalMetric{}
	for _, run := range runs {
		metric := metrics[run.Suite]
		metric.Total++
		if run.Succeeded {
			metric.Succeeded++
		}
		metrics[run.Suite] = metric
	}
	for name, metric := range metrics {
		metric.Rate = ratio(metric.Succeeded, metric.Total)
		metrics[name] = metric
	}
	return metrics
}

func failingLiveEvalMetrics(report liveToolEvalReport) []string {
	failed := []string{}
	appendFailures := func(prefix string, metrics map[string]liveToolEvalMetric) {
		names := make([]string, 0, len(metrics))
		for name := range metrics {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			metric := metrics[name]
			if metric.Total == 0 || metric.Rate < liveToolStabilityTarget {
				failed = append(failed, prefix+":"+name)
			}
		}
	}
	appendFailures("schema", report.SchemaCases)
	appendFailures("routing", report.RoutingCases)
	appendFailures("routing_variant", report.RoutingVariants)
	appendFailures("timeline_op", report.PerKind)
	if report.Budget.Total == 0 || report.Budget.Rate < liveToolStabilityTarget {
		failed = append(failed, "budget:remaining_zero")
	}
	return failed
}

func TestLiveMetricGateRejectsAnyFailedCase(t *testing.T) {
	t.Parallel()
	passing := liveToolEvalMetric{Succeeded: 5, Total: 5, Rate: 1}
	report := liveToolEvalReport{
		SchemaCases: map[string]liveToolEvalMetric{
			"healthy": passing,
			"broken":  {Succeeded: 0, Total: 5, Rate: 0},
		},
		RoutingCases: map[string]liveToolEvalMetric{"route": passing},
		RoutingVariants: map[string]liveToolEvalMetric{
			"variant": passing,
		},
		PerKind: map[string]liveToolEvalMetric{"insert_clip": passing},
		Budget:  passing,
	}
	if got := failingLiveEvalMetrics(report); !reflect.DeepEqual(got, []string{"schema:broken"}) {
		t.Fatalf("failed metrics=%v", got)
	}
	report.SchemaCases["broken"] = passing
	report.PerKind["delete_range"] = liveToolEvalMetric{Succeeded: 4, Total: 5, Rate: 0.8}
	report.Budget = liveToolEvalMetric{}
	if got := failingLiveEvalMetrics(report); !reflect.DeepEqual(
		got,
		[]string{"timeline_op:delete_range", "budget:remaining_zero"},
	) {
		t.Fatalf("failed metrics=%v", got)
	}
}

func liveWorkflowSuiteByName(suites []liveWorkflowSuite, name string) (liveWorkflowSuite, bool) {
	for _, suite := range suites {
		if suite.Name == name {
			return suite, true
		}
	}
	return liveWorkflowSuite{}, false
}

func liveWorkflowSpecByName(specs []rushestools.Spec, name string) (rushestools.Spec, bool) {
	for _, spec := range specs {
		if spec.Name == name {
			return spec, true
		}
	}
	return rushestools.Spec{}, false
}

func liveWorkflowSpecNames(specs []rushestools.Spec) []string {
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, spec.Name)
	}
	return names
}

func retiredToolsIn(names []string) []string {
	legacy := []string{}
	for _, name := range names {
		if containsToolName(retiredLLMToolNames, name) {
			legacy = append(legacy, name)
		}
	}
	return legacy
}

func TestRetiredLLMToolSetCoversIssue141Surface(t *testing.T) {
	t.Parallel()
	expected := []string{
		"media.search_shots",
		"memory.update",
		"render.final_mp4",
		"render.inspect_preview",
		"render.preview",
		"render.status",
		"speech.inspect",
		"timeline.apply_patch",
		"timeline.apply_patches",
		"timeline.compose_initial",
		"timeline.edit_talking_head",
		"timeline.recut_to_beats",
		"timeline.validate",
		"understand.materials",
	}
	actual := append([]string(nil), retiredLLMToolNames...)
	sort.Strings(actual)
	sort.Strings(expected)
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("retired LLM tools=%v want=%v", actual, expected)
	}
}

func liveSchemaCases() []liveToolEvalCase {
	cases := []liveToolEvalCase{
		{Name: "shot_search", Prompt: "请只检索适合覆盖‘指纹解锁位于键盘右上角’这句口播的 B-roll 镜头，最多返回 8 个。", Expected: []string{"shot.search"}},
		{Name: "speech_search", Prompt: "请读取 clip_v1_001 的持久化逐句口播索引，检索‘指纹解锁’，同时返回气口和相似台词证据。", Expected: []string{"speech.search"}},
		{Name: "ask_user", Prompt: "用户要求的核心叙事目标存在两种实质冲突，素材和上下文都无法推断，且没有安全默认值。请用 decision_type=critical 发出一张允许自由输入的阻塞性二选一决策卡，只问这个核心分歧。", Expected: []string{"interaction.ask_user"}},
		{Name: "decision_answer", Prompt: "请提交决策 decision_style_1 的答案 option_id=fast，补充说明为强节奏。", Expected: []string{"decision.answer"}},
		{Name: "plan_update", Prompt: "请把已确定的创作计划持久记录下来：风格是克制电影感，节奏决定为前缓后快；使用增量合并，不要整体重置。", Expected: []string{"plan.update"}},
		{Name: "initial_first_insert", Prompt: "当前没有时间线。请开始初版组装，只先把 asset_video_1 的源 0 到 90 帧作为 b_roll 插入 visual_base；后续片段等拿到新版本后再插。", Expected: []string{"timeline.insert"}},
		{Name: "timeline_insert", Prompt: "请只插入一个字幕：0 到 90 帧，文本为‘示例字幕’，样式 default。", Expected: []string{"timeline.insert"}},
		{Name: "timeline_delete", Prompt: "请只删除时间线片段 clip_v1_002。", Expected: []string{"timeline.delete"}},
		{Name: "timeline_update", Prompt: "请将时间线片段 clip_v1_001 的结尾裁到第 75 帧，只更新这一个目标。", Expected: []string{"timeline.update"}},
		{Name: "timeline_split", Prompt: "请在第 45 帧切分时间线片段 clip_v1_001。", Expected: []string{"timeline.split"}},
		{Name: "beat_bgm_insert", Prompt: "卡点主视频已按真实拍点逐段插入，总长 1440 帧。请只把 asset_bgm_1 的源 0 到 1440 帧插入 bgm 轨，起点 0；metadata.beat_grid 使用已检测的 bpm=120、beat_frames=[0,30,60,90]、strong_beat_frames=[0,60]、downbeat_frames=[0]、bar_phase=0、analysis_method=aubio。", Expected: []string{"timeline.insert"}},
		{Name: "talking_head_delete", Prompt: "口播证据和当前时间线已经读取；我明确选择删除较早一遍重说，它当前映射为时间线 360 到 480 帧。请只删除这个连续范围。", Expected: []string{"timeline.delete"}},
		{Name: "talking_head_broll_insert", Prompt: "口播已清理并重新读取；请把 B-roll 素材 asset_video_1 的源 30 到 120 帧作为 visual_overlay 插入当前时间线第 600 帧，只做这一次插入。", Expected: []string{"timeline.insert"}},
		{Name: "talking_head_broll_fade", Prompt: "请只给刚插入的 B-roll 片段 clip_v4_001 设置 7 帧淡入和 7 帧淡出。", Expected: []string{"timeline.update"}},
		{Name: "confirm", Prompt: "请为危险的时间线清空操作创建确认：工具 timeline.delete，参数 kind=remove_track_clips、track_id=sfx。", Expected: []string{"interaction.confirm_action"}},
	}
	for index := range cases {
		cases[index].Snapshot = liveSnapshotForSchemaCase(cases[index].Name)
	}
	return cases
}

func liveCatalogToolLoadCases() []liveToolEvalCase {
	// Keep a fixed, auditable ten-prompt sample spanning evidence, interaction,
	// planning, memory and every atomic timeline action family.
	byName := map[string]liveToolEvalCase{}
	for _, evalCase := range liveSchemaCases() {
		byName[evalCase.Name] = evalCase
	}
	result := make([]liveToolEvalCase, 0, 10)
	for _, name := range []string{
		"shot_search", "speech_search", "ask_user", "plan_update",
		"initial_first_insert", "timeline_delete", "timeline_update", "timeline_split",
	} {
		result = append(result, byName[name])
	}
	result = append(result,
		liveToolEvalCase{
			Name: "memory_set", Expected: []string{"memory.set"},
			Prompt:   "用户明确说：‘我以后所有项目都偏好克制的电影感节奏。’请把这条跨项目稳定偏好写入长期记忆。",
			Snapshot: liveSnapshotForSchemaCase("memory_set"),
		},
		liveToolEvalCase{
			Name: "memory_remove", Expected: []string{"memory.remove"},
			Prompt:   "用户明确要求忘记已经保存的 pacing 长期偏好；请只提交删除该记忆键的 action，确认由 PolicyGate 处理。",
			Snapshot: liveSnapshotForSchemaCase("memory_remove"),
		},
	)
	return result
}

func TestLiveCatalogToolLoadCasesAreAuditable(t *testing.T) {
	cases := liveCatalogToolLoadCases()
	if len(cases) != 10 {
		t.Fatalf("Catalog/tool.load live cases=%d want=10", len(cases))
	}
	wantActions := map[string]bool{
		"shot.search": false, "speech.search": false,
		"interaction.ask_user": false, "plan.update": false,
		"memory.set": false, "memory.remove": false,
		"timeline.insert": false, "timeline.delete": false,
		"timeline.update": false, "timeline.split": false,
	}
	for _, evalCase := range cases {
		if strings.TrimSpace(evalCase.Name) == "" || strings.TrimSpace(evalCase.Prompt) == "" ||
			len(evalCase.Expected) != 1 {
			t.Fatalf("不可审计的 Catalog/tool.load case: %#v", evalCase)
		}
		if _, expected := wantActions[evalCase.Expected[0]]; !expected {
			t.Fatalf("意外 action: %s", evalCase.Expected[0])
		}
		wantActions[evalCase.Expected[0]] = true
	}
	for name, covered := range wantActions {
		if !covered {
			t.Errorf("Catalog/tool.load live eval 未覆盖 %s", name)
		}
	}
}

func liveRoutingCases() []liveToolEvalCase {
	const contextPrefix = `已读取当前客观状态：timeline_fps=30；当前 timeline_id=draft_eval:v7；A-roll asset_aroll_1 已有持久化逐句索引，主视频 clip 为 clip_v1_001；B-roll asset_video_1、asset_video_2 已完成逐镜头理解；BGM asset_bgm_1；SFX asset_sfx_1；当前时间线存在且已验证，预览为 preview_123，渲染任务为 job_render_123。`
	cases := []liveToolEvalCase{
		{Name: "route_shot_search", Prompt: contextPrefix + "\nspeech.search 已返回 utt_fingerprint_1，文本是‘指纹解锁位于键盘右上角’。用户：不用再读取台词，只调用镜头检索找合适的 B-roll，暂时不剪。", Expected: []string{"shot.search"}},
		{Name: "route_speech_search", Prompt: contextPrefix + "\n用户：读取 clip_v1_001 的逐句 ASR，检索重复说到‘指纹解锁’的地方并给出客观相似证据，暂时不删。", Expected: []string{"speech.search"}},
		{Name: "route_plan_update", Prompt: contextPrefix + "\n用户：先不要继续剪，把已确定的创作方向记到持久计划本里：整体克制、高潮段加快节奏，供下回合继续。", Expected: []string{"plan.update"}},
		{Name: "route_atomic_gain_update", Prompt: contextPrefix + "\n用户：已取得真实 ID，只把 clip_v1_001 音量调到 -6dB。", Expected: []string{"timeline.update"}},
		{Name: "route_atomic_fade_update", Prompt: contextPrefix + "\n用户：已取得真实 ID；clip_v1_001 当前淡入、淡出均为 0 帧。保持淡入为 0，现在只把淡出设为 8 帧。", Expected: []string{"timeline.update"}},
		{Name: "route_beat_insert", Prompt: contextPrefix + "\n节拍和镜头选择已完成；下一段明确使用 asset_video_2 的源 180 到 300 帧，作为 b_roll 追加到 visual_base，片尾正好落在选定拍点。现在只提交这一段插入。", Expected: []string{"timeline.insert"}},
		{Name: "route_beat_insert_after_recoverable_failure", Prompt: contextPrefix + "\n上一原子插入因所选 shot 只有 80 帧而失败，当前时间线未变化。shot.search 已返回替代镜头 shot_video_2b：asset_video_2 源 180 到 300 帧，正好 120 帧；创作选择不变。现在只重试这一个失败的插入，不重做其他已成功片段。", Expected: []string{"timeline.insert"}},
		{Name: "route_atomic_update_after_field_failure", Prompt: contextPrefix + "\n上一工具结果：{\"status\":\"failed\",\"observation\":\"时间线补丁字段预校验失败：时间线补丁 trim_clip_edge 的字段 timeline_frame 缺少必填字段\",\"data\":{\"op_kind\":\"trim_clip_edge\",\"invalid_field\":\"timeline_frame\",\"expected_schema\":{\"required\":[\"kind\",\"timeline_clip_id\",\"timeline_frame\",\"edge\"]},\"correct_example\":{\"kind\":\"trim_clip_edge\",\"timeline_clip_id\":\"clip_v1_001\",\"timeline_frame\":75,\"edge\":\"end\"},\"recovery\":\"只修正当前 op 的字段名与类型后重新调用；不要原样重发失败参数。\"}}。字段错误已明确；按原子顺序先把 clip_v1_001 的结尾裁到 75 帧。请选择下一步工具。", Expected: []string{"timeline.update"}},
		{Name: "route_talking_head_delete", Prompt: contextPrefix + "\n逐句证据与最新时间线已读取；较早一遍重说当前映射为时间线 360 到 480 帧，我明确选择删除它。现在只提交这个连续范围。", Expected: []string{"timeline.delete"}},
	}
	snapshot := liveFullTaskSnapshot()
	for index := range cases {
		cases[index].Snapshot = snapshot
	}
	return cases
}

func liveGenerateToolCall(
	parent context.Context,
	chat model.ToolCallingChatModel,
	prompt string,
	snapshot WorldStateSnapshot,
	forced bool,
	allowedName string,
) (schema.ToolCall, error) {
	return liveGenerateToolCallWithPlaybook(parent, chat, prompt, snapshot, forced, allowedName, true)
}

func liveGenerateUserMemoryResponse(
	parent context.Context,
	chat model.ToolCallingChatModel,
	evalCase userMemoryModelEvalCase,
) (*schema.Message, error) {
	messages, err := userMemoryEvalMessages(evalCase)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		ctx, cancel := context.WithTimeout(parent, 90*time.Second)
		response, generateErr := chat.Generate(ctx, messages)
		cancel()
		if generateErr == nil && response != nil {
			return response, nil
		}
		if generateErr != nil {
			lastErr = generateErr
		} else {
			lastErr = errors.New("模型返回 nil")
		}
		if attempt < 3 {
			time.Sleep(time.Duration(attempt) * 200 * time.Millisecond)
		}
	}
	return nil, lastErr
}

func userMemoryExpectedBehavior(evalCase userMemoryModelEvalCase) string {
	parts := []string{}
	if evalCase.RequiredTool != "" {
		parts = append(parts, "required="+evalCase.RequiredTool)
	}
	if len(evalCase.ForbiddenTools) > 0 {
		parts = append(parts, "forbidden="+strings.Join(evalCase.ForbiddenTools, "|"))
	}
	return strings.Join(parts, ",")
}

func liveGenerateToolCallWithPlaybook(
	parent context.Context,
	chat model.ToolCallingChatModel,
	prompt string,
	snapshot WorldStateSnapshot,
	forced bool,
	allowedName string,
	includePlaybook bool,
) (schema.ToolCall, error) {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		ctx, cancel := context.WithTimeout(parent, 90*time.Second)
		options := []model.Option{}
		if forced {
			options = append(options, model.WithToolChoice(schema.ToolChoiceForced, allowedName))
		}
		messages := []*schema.Message{schema.SystemMessage(coreSystemPrompt)}
		if playbook := taskPlaybookMessage(snapshot); includePlaybook && playbook != nil {
			messages = append(messages, playbook)
		}
		messages = append(messages, schema.UserMessage(prompt))
		response, err := chat.Generate(ctx, messages, options...)
		cancel()
		if err == nil && response != nil && len(response.ToolCalls) > 0 {
			return response.ToolCalls[0], nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("模型未调用工具，文本回复=%q", agentexec.TruncateText(responseContent(response), 240))
		}
		if attempt < 3 {
			time.Sleep(time.Duration(attempt) * 200 * time.Millisecond)
		}
	}
	return schema.ToolCall{}, lastErr
}

func liveGenerateNoToolCall(parent context.Context, chat model.ToolCallingChatModel) error {
	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	defer cancel()
	response, err := chat.Generate(ctx, []*schema.Message{
		schema.SystemMessage(coreSystemPrompt + "\n\n" + turnBudgetInstruction(0)),
		schema.UserMessage("时间预算已经耗尽。请总结当前状态，不要继续执行任何工具。"),
	})
	if err != nil {
		return err
	}
	if response != nil && len(response.ToolCalls) > 0 {
		return fmt.Errorf("remaining=0 时仍调用工具 %s", response.ToolCalls[0].Function.Name)
	}
	return nil
}

func liveRoutingAblationCases() []liveRoutingVariant {
	fullByName := map[string]liveToolEvalCase{}
	for _, evalCase := range liveRoutingCases() {
		fullByName[evalCase.Name] = evalCase
	}
	minimal := map[string]WorldStateSnapshot{
		"route_beats":               liveSnapshotForSchemaCase("beats"),
		"route_talking_head_delete": liveSnapshotForSchemaCase("talking_head_delete"),
		"route_atomic_gain_update":  liveSnapshotForSchemaCase("atomic_update"),
		"route_plan_update": NewWorldStateSnapshot(map[string]any{
			"assets":   map[string]any{"audio_roles": []any{}, "material_catalog": []any{}},
			"timeline": nil,
		}),
	}
	variants := make([]liveRoutingVariant, 0, len(minimal)*3+2)
	for name, snapshot := range minimal {
		evalCase := fullByName[name]
		variants = append(variants,
			liveRoutingVariant{Name: "full", Case: evalCase, IncludePlaybook: true},
			liveRoutingVariant{Name: "minimal", Case: liveToolEvalCase{
				Name: evalCase.Name, Prompt: evalCase.Prompt, Expected: evalCase.Expected, Snapshot: snapshot,
			}, IncludePlaybook: true},
			liveRoutingVariant{Name: "no_playbook", Case: evalCase, IncludePlaybook: false},
		)
	}
	variants = append(variants,
		liveRoutingVariant{Name: "plan_activation_exploratory", IncludePlaybook: true, Case: liveToolEvalCase{
			Name: "interrupted_multi_topic", Expected: []string{"plan.update"},
			Prompt: "多主题剪辑做到一半时用户要求先暂停。已确定但未执行：第二主题保留访谈开头，第三主题改成快节奏。" +
				"请先用两个平铺键 second_topic、third_topic 固化这些决定供下回合继续，不要嵌套 topics 对象。",
			Snapshot: liveFullTaskSnapshot(),
		}},
		liveRoutingVariant{Name: "first_cut_autonomous", IncludePlaybook: true, Case: liveToolEvalCase{
			Name: "first_cut_executes_without_approval", Expected: []string{"timeline.insert"},
			Prompt: "WorldState 已确认尚无时间线；逐句证据也已读取：utt_1=开场介绍（保留），" +
				"utt_2=重复口误（删除），utt_3=核心结论（保留）。素材和目标均已明确，" +
				"请直接开始第一次口播首剪：先把 asset_aroll_1 的源 0 到 900 帧作为 a_roll 插入 visual_base，" +
				"后续删除和 B-roll 等拿到首个版本后再做；不要询问可逆细节。",
			Snapshot: NewWorldStateSnapshot(map[string]any{
				"assets": map[string]any{
					"audio_roles": []any{},
					"material_catalog": []any{map[string]any{
						"asset_id": "asset_aroll_1", "transcript_provider": "qwen_asr",
					}},
				},
				"timeline": nil,
			}),
		}},
		liveRoutingVariant{Name: "incremental_edit", IncludePlaybook: true, Case: liveToolEvalCase{
			Name: "incremental_edit_skips_edl", Expected: []string{"timeline.update"},
			Prompt:   "首剪已经确认并完成。只把真实片段 clip_v1_001 的音量调整到 -6dB，不要再次询问。",
			Snapshot: liveSnapshotForSchemaCase("atomic_update"),
		}},
	)
	return variants
}

func liveSnapshotForSchemaCase(name string) WorldStateSnapshot {
	assets := map[string]any{
		"audio_roles":      []any{},
		"material_catalog": []any{},
	}
	sections := map[string]any{"assets": assets, "timeline": nil}
	switch name {
	case "beats", "beat_bgm_insert":
		assets["audio_roles"] = []any{map[string]any{
			"asset_id": "asset_bgm_1", "suggested_role": "bgm",
		}}
		assets["material_catalog"] = []any{map[string]any{
			"asset_id": "asset_bgm_1", "suggested_role": "bgm",
		}}
	case "speech_search", "talking_head_delete", "talking_head_broll_insert", "talking_head_broll_fade":
		assets["material_catalog"] = []any{map[string]any{
			"asset_id": "asset_aroll_1", "transcript_provider": "qwen_asr",
		}}
		sections["timeline"] = map[string]any{"track_count": 1}
	case "atomic_update",
		"timeline_insert", "timeline_delete", "timeline_update", "timeline_split",
		"timeline_check", "inspect", "preview", "final",
		"status", "preview_check", "confirm":
		sections["timeline"] = map[string]any{"track_count": 1}
	}
	return NewWorldStateSnapshot(sections)
}

func liveFullTaskSnapshot() WorldStateSnapshot {
	return NewWorldStateSnapshot(map[string]any{
		"assets": map[string]any{
			"audio_roles": []any{
				map[string]any{"asset_id": "asset_bgm_1", "suggested_role": "bgm"},
				map[string]any{"asset_id": "asset_sfx_1", "suggested_role": "sfx"},
			},
			"material_catalog": []any{
				map[string]any{"asset_id": "asset_bgm_1", "suggested_role": "bgm"},
				map[string]any{
					"asset_id": "asset_aroll_1", "transcript_provider": "qwen_asr",
				},
			},
		},
		"timeline": map[string]any{"track_count": 3},
	})
}

func validateLiveToolArguments(spec rushestools.Spec, raw string) error {
	if spec.InputType == nil {
		return errors.New("工具没有输入类型")
	}
	target := reflect.New(spec.InputType)
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target.Interface()); err != nil {
		return fmt.Errorf("参数不符合 Go schema: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("包含多个 JSON 值")
		}
		return fmt.Errorf("参数不符合 Go schema: %w", err)
	}
	if toolName, input, atomic := liveAtomicTimelineInput(target.Elem().Interface()); atomic {
		if spec.Name != "" && spec.Name != toolName {
			return fmt.Errorf("原子时间线输入类型属于 %s，不属于 %s", toolName, spec.Name)
		}
		if _, err := rushestools.TimelineAtomicOperation(toolName, input); err != nil {
			return fmt.Errorf("参数不符合原子 Catalog schema: %w", err)
		}
		return nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &object); err != nil {
		return err
	}
	for index := 0; index < spec.InputType.NumField(); index++ {
		field := spec.InputType.Field(index)
		if !strings.Contains(field.Tag.Get("jsonschema"), "required") {
			continue
		}
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		value, exists := object[name]
		if !exists || string(value) == "null" || string(value) == `""` || string(value) == "[]" || string(value) == "{}" {
			return fmt.Errorf("缺少必填字段 %s", name)
		}
	}
	return nil
}

func liveAtomicTimelineInput(value any) (string, any, bool) {
	switch typed := value.(type) {
	case rushestools.TimelineInsertInput:
		return "timeline.insert", typed, true
	case rushestools.TimelineDeleteInput:
		return "timeline.delete", typed, true
	case rushestools.TimelineUpdateInput:
		return "timeline.update", typed, true
	case rushestools.TimelineSplitInput:
		return "timeline.split", typed, true
	default:
		return "", nil, false
	}
}

func TestValidateLiveToolArgumentsChecksAtomicTimelineContract(t *testing.T) {
	t.Parallel()
	spec := rushestools.Spec{
		Name: "timeline.update", InputType: reflect.TypeFor[rushestools.TimelineUpdateInput](),
	}
	if err := validateLiveToolArguments(
		spec,
		`{"kind":"trim_clip_edge","timeline_clip_id":"clip_1","timeline_frame":75,"edge":"end"}`,
	); err != nil {
		t.Fatalf("合法 update 被拒绝: %v", err)
	}
	for _, raw := range []string{
		`{"kind":"trim_clip_edge","timeline_clip_id":"clip_1","target_frame":75,"edge":"end"}`,
		`{"kind":"delete_clip","timeline_clip_id":"clip_1"}`,
		`{"kind":"replace_clip","timeline_clip_id":"clip_1","asset_id":"asset_2","asset_kind":"video"}`,
		`{"ops":[{"kind":"adjust_gain","timeline_clip_id":"clip_1","gain_db":-3}]}`,
		`{"kind":"trim_clip_edge","timeline_clip_id":"clip_1","timeline_frame":75,"edge":"end"}{"kind":"trim_clip_edge","timeline_clip_id":"clip_1","timeline_frame":60,"edge":"end"}`,
	} {
		if err := validateLiveToolArguments(spec, raw); err == nil {
			t.Errorf("非法原子 update 错误通过 raw=%s", raw)
		}
	}
}

// liveEvalRuns 是每个 live 评测 case 的重复次数。随机模型下 n=1 是 0/100% 粗信号，
// M5 把默认与下限都提到 5，保证每个 case 的 rate 至少建立在 n≥5 上；RUSHES_TOOL_EVAL_RUNS
// 可显式调高（上限 50，防成本失控），但不得低于 5。
func liveEvalRuns() int {
	const floor = 5
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv("RUSHES_TOOL_EVAL_RUNS")))
	if err != nil || value < floor {
		return floor
	}
	return min(value, 50)
}

func writeLiveToolEvalReport(t *testing.T, report liveToolEvalReport) {
	t.Helper()
	path := strings.TrimSpace(os.Getenv("RUSHES_TOOL_EVAL_REPORT"))
	if path == "" {
		return
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func responseContent(response *schema.Message) string {
	if response == nil {
		return ""
	}
	return response.Content
}

func containsToolName(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func ratio(success, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(success) / float64(total)
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
