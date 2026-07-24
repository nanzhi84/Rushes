package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

type userMemoryModelContractGolden struct {
	Cases []userMemoryModelEvalCase `json:"cases"`
}

type userMemoryModelEvalCase struct {
	Name                 string                   `json:"name"`
	Prompt               string                   `json:"prompt"`
	SnapshotSections     map[string]any           `json:"snapshot_sections"`
	AvailableTools       []string                 `json:"available_tools"`
	RequiredTool         string                   `json:"required_tool"`
	ForbiddenTools       []string                 `json:"forbidden_tools"`
	ExpectedMemory       *userMemoryExpectedEntry `json:"expected_memory,omitempty"`
	RequiredToolSemantic string                   `json:"required_tool_semantic,omitempty"`
	MockResponse         userMemoryMockResponse   `json:"mock_response"`
}

type userMemoryExpectedEntry struct {
	Key      string `json:"key"`
	Kind     string `json:"kind"`
	Semantic string `json:"semantic"`
}

type userMemoryMockResponse struct {
	Content   string                   `json:"content,omitempty"`
	ToolCalls []userMemoryMockToolCall `json:"tool_calls"`
}

type userMemoryMockToolCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type goldenUserMemoryModel struct {
	responses  map[string]*schema.Message
	boundTools []string
	messages   []*schema.Message
}

func (modelValue *goldenUserMemoryModel) WithTools(infos []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	bound := &goldenUserMemoryModel{responses: modelValue.responses}
	for _, info := range infos {
		if info == nil || strings.TrimSpace(info.Name) == "" {
			return nil, fmt.Errorf("golden mock 收到空工具合同")
		}
		bound.boundTools = append(bound.boundTools, info.Name)
	}
	return bound, nil
}

func (modelValue *goldenUserMemoryModel) Generate(
	_ context.Context,
	messages []*schema.Message,
	_ ...model.Option,
) (*schema.Message, error) {
	modelValue.messages = append([]*schema.Message(nil), messages...)
	prompt := ""
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index] != nil && messages[index].Role == schema.User {
			prompt = messages[index].Content
			break
		}
	}
	response, ok := modelValue.responses[prompt]
	if !ok {
		return nil, fmt.Errorf("golden mock 没有 prompt=%q 的响应", prompt)
	}
	return response, nil
}

func (modelValue *goldenUserMemoryModel) Stream(
	ctx context.Context,
	messages []*schema.Message,
	options ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	response, err := modelValue.Generate(ctx, messages, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{response}), nil
}

func TestUserMemoryModelContractGolden(t *testing.T) {
	evalCases := loadUserMemoryModelEvalCases(t)
	database := agenttest.AgentTestDatabase(t)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	toolInfos, specs, err := userMemoryEvalToolContracts(t.Context(), service)
	if err != nil {
		t.Fatal(err)
	}

	responses := make(map[string]*schema.Message, len(evalCases))
	for _, evalCase := range evalCases {
		calls := make([]schema.ToolCall, 0, len(evalCase.MockResponse.ToolCalls))
		for index, call := range evalCase.MockResponse.ToolCalls {
			calls = append(calls, schema.ToolCall{
				ID:       fmt.Sprintf("golden_%s_%d", evalCase.Name, index+1),
				Function: schema.FunctionCall{Name: call.Name, Arguments: call.Arguments},
			})
		}
		responses[evalCase.Prompt] = schema.AssistantMessage(evalCase.MockResponse.Content, calls)
	}
	baseModel := &goldenUserMemoryModel{responses: responses}

	for _, evalCase := range evalCases {
		t.Run(evalCase.Name, func(t *testing.T) {
			infos := make([]*schema.ToolInfo, 0, len(evalCase.AvailableTools))
			for _, name := range evalCase.AvailableTools {
				info := toolInfos[name]
				if info == nil {
					t.Fatalf("golden 引用了未注册工具 %s", name)
				}
				infos = append(infos, info)
			}
			boundModel, err := baseModel.WithTools(infos)
			if err != nil {
				t.Fatal(err)
			}
			bound := boundModel.(*goldenUserMemoryModel)
			if strings.Join(bound.boundTools, "\x00") != strings.Join(evalCase.AvailableTools, "\x00") {
				t.Fatalf("绑定工具漂移: got=%v want=%v", bound.boundTools, evalCase.AvailableTools)
			}
			messages, err := userMemoryEvalMessages(evalCase)
			if err != nil {
				t.Fatal(err)
			}
			response, err := bound.Generate(t.Context(), messages)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateUserMemoryModelResponse(evalCase, response, specs); err != nil {
				t.Fatal(err)
			}
			captured := messageContents(bound.messages)
			if !strings.Contains(captured, coreSystemPrompt) ||
				!strings.Contains(captured, "【WorldState 参考快照") ||
				!strings.Contains(captured, `"user_memory"`) ||
				!strings.Contains(captured, evalCase.Prompt) {
				t.Fatalf("评测没有走生产上下文管线: %s", captured)
			}
			if evalCase.Name == "uses_injected_preference_without_asking" &&
				!strings.Contains(captured, "成片节奏偏快，切点密度可高于默认") {
				t.Fatal("注入的长期偏好没有进入模型消息")
			}
		})
	}
}

func TestUserMemoryModelContractRejectsMemoryPollution(t *testing.T) {
	evalCase := loadUserMemoryModelEvalCases(t)[0]
	database := agenttest.AgentTestDatabase(t)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	_, specs, err := userMemoryEvalToolContracts(t.Context(), service)
	if err != nil {
		t.Fatal(err)
	}
	valid := `{"entries":[{"key":"pacing","kind":"preference","statement":"用户长期偏好视频节奏快一点","evidence_quote":"节奏都要快一点"}]}`
	tests := []struct {
		name  string
		calls []schema.ToolCall
	}{
		{
			name: "opposite memory",
			calls: []schema.ToolCall{memoryEvalToolCall(`{"entries":[` +
				`{"key":"pacing","kind":"preference","statement":"用户不喜欢快节奏，应该慢一点","evidence_quote":"节奏都要快一点"}]}`)},
		},
		{
			name: "rejected memory",
			calls: []schema.ToolCall{memoryEvalToolCall(`{"entries":[` +
				`{"key":"pacing","kind":"preference","statement":"用户拒绝快节奏，希望舒缓处理","evidence_quote":"节奏都要快一点"}]}`)},
		},
		{
			name: "extra entry",
			calls: []schema.ToolCall{memoryEvalToolCall(`{"entries":[` +
				`{"key":"pacing","kind":"preference","statement":"用户长期偏好视频节奏快一点","evidence_quote":"节奏都要快一点"},` +
				`{"key":"subtitle_style","kind":"preference","statement":"用户偏好花字","evidence_quote":"花字风格"}]}`)},
		},
		{
			name: "remove alongside write",
			calls: []schema.ToolCall{memoryEvalToolCall(`{"entries":[` +
				`{"key":"pacing","kind":"preference","statement":"用户长期偏好视频节奏快一点","evidence_quote":"节奏都要快一点"}],` +
				`"remove_keys":["subtitle_style"]}`)},
		},
		{
			name:  "second update",
			calls: []schema.ToolCall{memoryEvalToolCall(valid), memoryEvalToolCall(valid)},
		},
		{
			name: "unbound tool",
			calls: []schema.ToolCall{{
				ID: "unbound", Function: schema.FunctionCall{Name: "interaction.ask_user", Arguments: `{}`},
			}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := schema.AssistantMessage("", test.calls)
			if err := validateUserMemoryModelResponse(evalCase, response, specs); err == nil {
				t.Fatal("污染响应错误通过用户记忆模型合同")
			}
		})
	}
	t.Run("opposite plan", func(t *testing.T) {
		useCase := loadUserMemoryModelEvalCases(t)[2]
		response := schema.AssistantMessage("", []schema.ToolCall{{
			ID: "opposite_plan",
			Function: schema.FunctionCall{
				Name: "plan.update", Arguments: `{"plan":{"rhythm":"不要快节奏，改成慢剪"}}`,
			},
		}})
		if err := validateUserMemoryModelResponse(useCase, response, specs); err == nil {
			t.Fatal("反向节奏计划错误通过用户记忆模型合同")
		}
	})
	t.Run("cross field plan", func(t *testing.T) {
		useCase := loadUserMemoryModelEvalCases(t)[2]
		response := schema.AssistantMessage("", []schema.ToolCall{{
			ID: "cross_field_plan",
			Function: schema.FunctionCall{
				Name: "plan.update", Arguments: `{"plan":{"rhythm":"slow","note":"快只是反例"}}`,
			},
		}})
		if err := validateUserMemoryModelResponse(useCase, response, specs); err == nil {
			t.Fatal("跨字段拼接的伪快节奏计划错误通过用户记忆模型合同")
		}
	})
}

func memoryEvalToolCall(arguments string) schema.ToolCall {
	return schema.ToolCall{
		ID: "memory_eval", Function: schema.FunctionCall{Name: "memory.set", Arguments: arguments},
	}
}

func loadUserMemoryModelEvalCases(t *testing.T) []userMemoryModelEvalCase {
	t.Helper()
	path := filepath.Join("testdata", "user_memory_model_contract.golden.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s: %v", path, err)
	}
	var golden userMemoryModelContractGolden
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatalf("解析 %s: %v", path, err)
	}
	if len(golden.Cases) != 6 {
		t.Fatalf("用户记忆模型合同必须固定覆盖六个场景，实际 %d", len(golden.Cases))
	}
	seen := map[string]bool{}
	for _, evalCase := range golden.Cases {
		if strings.TrimSpace(evalCase.Name) == "" || seen[evalCase.Name] {
			t.Fatalf("场景名为空或重复: %q", evalCase.Name)
		}
		seen[evalCase.Name] = true
		available := stringSet(evalCase.AvailableTools)
		if evalCase.RequiredTool != "" && !available[evalCase.RequiredTool] {
			t.Fatalf("%s 的 required_tool 未出现在 available_tools", evalCase.Name)
		}
		for _, name := range evalCase.ForbiddenTools {
			if !available[name] {
				t.Fatalf("%s 的 forbidden_tool %s 未出现在 available_tools", evalCase.Name, name)
			}
		}
		for _, call := range evalCase.MockResponse.ToolCalls {
			if !available[call.Name] {
				t.Fatalf("%s 的 mock 调用了未绑定工具 %s", evalCase.Name, call.Name)
			}
		}
		if evalCase.ExpectedMemory != nil {
			if evalCase.RequiredTool != "memory.set" {
				t.Fatalf("%s 声明 expected_memory 但没有要求 memory.set", evalCase.Name)
			}
			if evalCase.ExpectedMemory.Semantic == "" {
				t.Fatalf("%s 的 expected_memory 缺少 semantic", evalCase.Name)
			}
		}
		if evalCase.RequiredToolSemantic != "" && evalCase.RequiredTool == "" {
			t.Fatalf("%s 声明 required_tool_semantic 但没有 required_tool", evalCase.Name)
		}
	}
	return golden.Cases
}

func userMemoryEvalToolContracts(
	ctx context.Context,
	service *Service,
) (map[string]*schema.ToolInfo, map[string]rushestools.Spec, error) {
	infos := map[string]*schema.ToolInfo{}
	specs := map[string]rushestools.Spec{}
	for _, spec := range service.tools.Specs(true) {
		if spec.Exposure != rushestools.ExposureLLM {
			continue
		}
		info, err := spec.Implementation.Info(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("读取 %s 合同: %w", spec.Name, err)
		}
		infos[spec.Name] = info
		specs[spec.Name] = spec
	}
	return infos, specs, nil
}

func userMemoryEvalMessages(evalCase userMemoryModelEvalCase) ([]*schema.Message, error) {
	snapshot := NewWorldStateSnapshot(evalCase.SnapshotSections)
	hash, err := snapshot.Hash()
	if err != nil {
		return nil, err
	}
	contextMessages, err := renderContextMessages(
		snapshot,
		snapshot,
		nil,
		storage.AgentContextCheckpoint{
			WindowID:         "user_memory_eval_" + evalCase.Name,
			WindowNumber:     1,
			BaseSnapshotHash: hash,
		},
		nil,
	)
	if err != nil {
		return nil, err
	}
	messages := []*schema.Message{schema.SystemMessage(coreSystemPrompt)}
	messages = append(messages, contextMessages...)
	messages = append(messages, schema.UserMessage(evalCase.Prompt))
	return messages, nil
}

func validateUserMemoryModelResponse(
	evalCase userMemoryModelEvalCase,
	response *schema.Message,
	specs map[string]rushestools.Spec,
) error {
	if response == nil {
		return fmt.Errorf("模型返回 nil")
	}
	available := stringSet(evalCase.AvailableTools)
	forbidden := stringSet(evalCase.ForbiddenTools)
	requiredCalls := []*schema.ToolCall{}
	for index := range response.ToolCalls {
		call := &response.ToolCalls[index]
		if !available[call.Function.Name] {
			return fmt.Errorf("调用了未绑定工具 %s", call.Function.Name)
		}
		if forbidden[call.Function.Name] {
			return fmt.Errorf("调用了禁止工具 %s", call.Function.Name)
		}
		spec, ok := specs[call.Function.Name]
		if !ok {
			return fmt.Errorf("调用了合同外工具 %s", call.Function.Name)
		}
		if err := validateLiveToolArguments(spec, call.Function.Arguments); err != nil {
			return fmt.Errorf("%s 参数无效: %w", call.Function.Name, err)
		}
		if call.Function.Name == evalCase.RequiredTool {
			requiredCalls = append(requiredCalls, call)
		}
	}
	if evalCase.RequiredTool != "" && len(requiredCalls) == 0 {
		return fmt.Errorf("没有调用必需工具 %s；%s", evalCase.RequiredTool, userMemoryResponseSummary(response))
	}
	if evalCase.ExpectedMemory != nil {
		if len(requiredCalls) != 1 {
			return fmt.Errorf("memory.set 必须恰好调用一次，实际 %d 次", len(requiredCalls))
		}
		var input rushestools.MemorySetInput
		if err := json.Unmarshal([]byte(requiredCalls[0].Function.Arguments), &input); err != nil {
			return fmt.Errorf("解析 memory.set: %w", err)
		}
		if len(input.Entries) != 1 {
			return fmt.Errorf("memory.set 必须只写入一条预期记忆，entries=%d", len(input.Entries))
		}
		entry := input.Entries[0]
		if entry.Key != evalCase.ExpectedMemory.Key || entry.Kind != evalCase.ExpectedMemory.Kind {
			return fmt.Errorf("memory.set 未精确写入预期长期记忆: %s", requiredCalls[0].Function.Arguments)
		}
		if err := validateUserMemorySemantic(evalCase.ExpectedMemory.Semantic, entry.Statement); err != nil {
			return fmt.Errorf("memory.set 记忆语义错误: %w", err)
		}
	}
	if evalCase.RequiredToolSemantic != "" {
		for _, call := range requiredCalls {
			if err := validateRequiredToolSemantic(
				evalCase.RequiredToolSemantic,
				call.Function.Name,
				call.Function.Arguments,
			); err != nil {
				return fmt.Errorf("%s 参数没有体现注入偏好: %w", evalCase.RequiredTool, err)
			}
		}
	}
	return nil
}

// memorySemanticSpec 是一个评测语义维度的确定性词袋：命中任一 negative 直接判负
// （反向限定优先），若配置了 requireTopic 则须先命中话题词圈定范围，再要求命中任一
// positive 正向表达。数据驱动的多维词袋取代原先只认 fast_pacing 的单维硬编码，
// 既能无 LLM-judge 地扩展新语义，又保持 CI 里的确定性。
type memorySemanticSpec struct {
	requireTopic []string
	positive     []string
	negative     []string
}

var memorySemanticSpecs = map[string]memorySemanticSpec{
	"fast_pacing": {
		negative: []string{"不", "拒绝", "禁止", "避免", "慢", "舒缓", "降低", "减少", "低密度", "太快"},
		positive: []string{
			"快节奏", "节奏偏快", "节奏加快", "加快节奏", "节奏更快", "节奏要快", "节奏都要快",
			"节奏快", "节奏偏好快", "紧凑节奏", "节奏紧凑", "高切点密度", "切点密度高",
			"提高切点密度", "切点密度偏高", "切点密度可高", "高密度切点",
		},
	},
	"prefers_more_broll": {
		requireTopic: []string{"b-roll", "broll", "空镜"},
		negative:     []string{"少", "减少", "太多", "不要", "别放", "降低", "别太"},
		positive:     []string{"多", "增", "提高", "更多", "铺满", "大量", "覆盖率高", "密集"},
	},
}

func hasMemorySemanticPhrase(semantic, value string) bool {
	spec, ok := memorySemanticSpecs[semantic]
	if !ok {
		return false
	}
	lower := strings.ToLower(value)
	for _, fragment := range spec.negative {
		if strings.Contains(lower, fragment) {
			return false
		}
	}
	if len(spec.requireTopic) > 0 && !containsAnySubstring(lower, spec.requireTopic) {
		return false
	}
	return containsAnySubstring(lower, spec.positive)
}

func containsAnySubstring(value string, fragments []string) bool {
	for _, fragment := range fragments {
		if strings.Contains(value, fragment) {
			return true
		}
	}
	return false
}

func validateUserMemorySemantic(semantic, value string) error {
	if _, ok := memorySemanticSpecs[semantic]; !ok {
		return fmt.Errorf("未知评测语义 %q", semantic)
	}
	if hasMemorySemanticPhrase(semantic, value) {
		return nil
	}
	return fmt.Errorf("记忆陈述未确定性命中语义 %q: %s", semantic, agentexec.TruncateText(value, 240))
}

func validateRequiredToolSemantic(semantic, toolName, arguments string) error {
	if _, ok := memorySemanticSpecs[semantic]; !ok {
		return fmt.Errorf("未知评测语义 %q", semantic)
	}
	if toolName != "plan.update" {
		return fmt.Errorf("工具 %s 不支持 %s 语义评测", toolName, semantic)
	}
	var input rushestools.PlanUpdateInput
	if err := json.Unmarshal([]byte(arguments), &input); err != nil {
		return fmt.Errorf("解析 plan.update: %w", err)
	}
	values := collectStringValues(input.Plan)
	if input.Contract != nil && strings.TrimSpace(input.Contract.Rhythm) != "" {
		values = append(values, input.Contract.Rhythm)
	}
	for _, value := range values {
		if hasMemorySemanticPhrase(semantic, value) {
			return nil
		}
	}
	return fmt.Errorf("plan 的单个语义值均未确定性命中语义 %q: %s", semantic, agentexec.TruncateText(arguments, 240))
}

func collectStringValues(value any) []string {
	switch typed := value.(type) {
	case string:
		return []string{typed}
	case map[string]any:
		values := []string{}
		for _, child := range typed {
			values = append(values, collectStringValues(child)...)
		}
		return values
	case []any:
		values := []string{}
		for _, child := range typed {
			values = append(values, collectStringValues(child)...)
		}
		return values
	default:
		return nil
	}
}

func userMemoryResponseSummary(response *schema.Message) string {
	if response == nil {
		return "response=nil"
	}
	calls := make([]string, 0, len(response.ToolCalls))
	for _, call := range response.ToolCalls {
		calls = append(calls, fmt.Sprintf(
			"%s(%s)", call.Function.Name, agentexec.TruncateText(call.Function.Arguments, 240),
		))
	}
	return fmt.Sprintf("tool_calls=%v content=%q", calls, agentexec.TruncateText(response.Content, 240))
}

func messageContents(messages []*schema.Message) string {
	contents := make([]string, 0, len(messages))
	for _, message := range messages {
		if message != nil {
			contents = append(contents, message.Content)
		}
	}
	return strings.Join(contents, "\n")
}

func stringSet(values []string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		result[value] = true
	}
	return result
}
