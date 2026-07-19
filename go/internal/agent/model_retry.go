package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

const maxModelTimeoutRetries = 5

var (
	modelRetryTotalRuneBudgets = [...]int{24000, 18000, 14000, 10000, 8000}
	modelRetryPerToolBudgets   = [...]int{8000, 6000, 5000, 4000, 3000}
)

type modelResponseTimeoutError struct {
	Retries int
	LastErr error
}

func (err *modelResponseTimeoutError) Error() string {
	return fmt.Sprintf("模型响应超时（已自动重试 %d 次）", err.Retries)
}

func (err *modelResponseTimeoutError) Unwrap() error { return err.LastErr }

// modelContextLengthError 表示压缩重试后模型输入仍超出上下文上限的终态。
type modelContextLengthError struct {
	Retries int
	LastErr error
}

func (err *modelContextLengthError) Error() string {
	return fmt.Sprintf("模型上下文超出上限（已自动压缩重试 %d 次）", err.Retries)
}

func (err *modelContextLengthError) Unwrap() error { return err.LastErr }

// modelRetryReason 是触发自动重试的错误类别内部标识；label 返回面向用户的简体中文原因短语。
type modelRetryReason int

const (
	modelRetryReasonNone modelRetryReason = iota
	modelRetryReasonTimeout
	modelRetryReasonContextLength
)

func (reason modelRetryReason) label() string {
	if reason == modelRetryReasonContextLength {
		return "上下文超出模型上限"
	}
	return "模型响应超时"
}

type modelRetryNotice struct {
	Attempt    int
	MaxRetries int
	Delay      time.Duration
	Reason     string
}

type modelRetryReporter func(modelRetryNotice)
type modelRetryContextKey struct{}

func withModelRetryReporter(ctx context.Context, reporter modelRetryReporter) context.Context {
	if reporter == nil {
		return ctx
	}
	return context.WithValue(ctx, modelRetryContextKey{}, reporter)
}

func reportModelRetry(ctx context.Context, notice modelRetryNotice) {
	reporter, _ := ctx.Value(modelRetryContextKey{}).(modelRetryReporter)
	if reporter != nil {
		reporter(notice)
	}
}

type timeoutRetryChatModel struct {
	inner      model.ToolCallingChatModel
	maxRetries int
	delay      func(int) time.Duration
	wait       func(context.Context, time.Duration) error
}

func newTimeoutRetryChatModel(inner model.ToolCallingChatModel) model.ToolCallingChatModel {
	if inner == nil {
		return nil
	}
	return &timeoutRetryChatModel{
		inner: inner, maxRetries: maxModelTimeoutRetries,
		delay: modelRetryDelay, wait: waitForModelRetry,
	}
}

func (retry *timeoutRetryChatModel) WithTools(
	tools []*schema.ToolInfo,
) (model.ToolCallingChatModel, error) {
	bound, err := retry.inner.WithTools(tools)
	if err != nil {
		return nil, err
	}
	return &timeoutRetryChatModel{
		inner: bound, maxRetries: retry.maxRetries, delay: retry.delay, wait: retry.wait,
	}, nil
}

func (retry *timeoutRetryChatModel) Generate(
	ctx context.Context,
	input []*schema.Message,
	options ...model.Option,
) (*schema.Message, error) {
	messages := input
	for completedRetries := 0; ; {
		response, err := retry.inner.Generate(ctx, messages, options...)
		if err == nil {
			recordModelResponseUsage(ctx, response)
			return response, nil
		}
		completedRetries, messages, err = retry.nextAttempt(ctx, input, completedRetries, err)
		if err != nil {
			return nil, err
		}
	}
}

func (retry *timeoutRetryChatModel) Stream(
	ctx context.Context,
	input []*schema.Message,
	options ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	messages := input
	for completedRetries := 0; ; {
		stream, err := retry.inner.Stream(ctx, messages, options...)
		if err != nil {
			completedRetries, messages, err = retry.nextAttempt(ctx, input, completedRetries, err)
			if err != nil {
				return nil, err
			}
			continue
		}

		// Eino 只有在这里返回 stream 后才会消费模型输出并执行 tool_call。
		// 因此必须先完整收齐一次模型响应：供应商经常先发送空增量、思考增量
		// 或未闭合的 tool_call，随后才因请求超时终止。把“收到首片段”当成
		// 已产生副作用会让这种超时绕过重试，正是长 ASR 结果后任务静止的原因。
		// 完整缓冲后，中途失败的响应尚未离开此边界，可以安全丢弃、压缩输入
		// 并重试；成功时仍按原始 chunk 顺序交给 ReAct，语义不会改变。
		buffered, receiveErr := bufferCompleteModelStream(stream)
		if receiveErr == nil {
			if response, concatErr := schema.ConcatMessages(buffered); concatErr == nil {
				recordModelResponseUsage(ctx, response)
			}
			return schema.StreamReaderFromArray(buffered), nil
		}
		completedRetries, messages, receiveErr = retry.nextAttempt(
			ctx, input, completedRetries, receiveErr,
		)
		if receiveErr != nil {
			return nil, receiveErr
		}
	}
}

func recordModelResponseUsage(ctx context.Context, response *schema.Message) {
	if response == nil || response.ResponseMeta == nil || response.ResponseMeta.Usage == nil {
		return
	}
	if state := turnBudgetFromContext(ctx); state != nil {
		state.recordUsage(response.ResponseMeta.Usage)
	}
}

func bufferCompleteModelStream(
	stream *schema.StreamReader[*schema.Message],
) ([]*schema.Message, error) {
	if stream == nil {
		return nil, errors.New("模型返回了空流")
	}
	defer stream.Close()
	buffered := make([]*schema.Message, 0, 8)
	for {
		message, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return buffered, nil
		}
		if err != nil {
			return nil, err
		}
		if message != nil {
			buffered = append(buffered, message)
		}
	}
}

func (retry *timeoutRetryChatModel) nextAttempt(
	ctx context.Context,
	input []*schema.Message,
	completedRetries int,
	requestErr error,
) (int, []*schema.Message, error) {
	reason := classifyRetryableModelError(ctx, requestErr)
	if reason == modelRetryReasonNone {
		return completedRetries, nil, requestErr
	}
	if completedRetries >= retry.maxRetries {
		if reason == modelRetryReasonContextLength {
			return completedRetries, nil, &modelContextLengthError{
				Retries: completedRetries, LastErr: requestErr,
			}
		}
		return completedRetries, nil, &modelResponseTimeoutError{
			Retries: completedRetries, LastErr: requestErr,
		}
	}

	retryAttempt := completedRetries + 1
	delay := retry.delay(retryAttempt)
	reportModelRetry(ctx, modelRetryNotice{
		Attempt: retryAttempt, MaxRetries: retry.maxRetries, Delay: delay, Reason: reason.label(),
	})
	if err := retry.wait(ctx, delay); err != nil {
		return completedRetries, nil, err
	}
	return retryAttempt, compactModelRetryInput(input, retryAttempt), nil
}

func modelRetryDelay(attempt int) time.Duration {
	if attempt <= 1 {
		return 250 * time.Millisecond
	}
	delay := 250 * time.Millisecond * time.Duration(1<<(attempt-1))
	if delay > 4*time.Second {
		return 4 * time.Second
	}
	return delay
}

func waitForModelRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func isRetryableModelTimeout(ctx context.Context, err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || ctx.Err() != nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return true
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{
		"context deadline exceeded", "client.timeout", "i/o timeout",
		"timeout awaiting response headers", "request timeout", "response timeout",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

// classifyRetryableModelError 判定一个模型调用错误是否值得压缩后重试，并区分原因。
// 超时优先判定以保持既有行为；context-length 类错误复用同一条压缩重试路径。
func classifyRetryableModelError(ctx context.Context, err error) modelRetryReason {
	if isRetryableModelTimeout(ctx, err) {
		return modelRetryReasonTimeout
	}
	if err != nil && ctx.Err() == nil && isContextLengthExceeded(err) {
		return modelRetryReasonContextLength
	}
	return modelRetryReasonNone
}

// isContextLengthExceeded 识别 dashscope/qwen 与 ark/doubao 返回的“上下文/输入超长”类
// 400 错误（大小写不敏感）。压缩工具结果后重试通常能让输入重新落入上限。典型样式：
//
//	dashscope/qwen: "Range of input length should be [1, 30720], but got 41234"
//	openai 兼容:     "This model's maximum context length is 32768 tokens. However, your messages resulted in ..."
//	ark/doubao:      "the request's input tokens exceed the model's context length limit, please reduce the length"
//
// 仅匹配与长度/上下文窗口直接相关的短语，避免把普通 400 参数错误误判成可重试。
func isContextLengthExceeded(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{
		"context length", "context_length_exceeded",
		"input length", "input tokens", "reduce the length",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

// compactToolResultMessages 把工具结果消息按「最新详细、更早最小」策略压进 totalBudget，
// 每条上限 perTool。失败重试压缩与回合内主动压缩（#95 H1b）共用此核心，返回是否真正改写。
func compactToolResultMessages(
	input []*schema.Message, totalBudget, perTool, retryAttempt int,
) ([]*schema.Message, bool) {
	result := append([]*schema.Message(nil), input...)
	toolIndexes := make([]int, 0)
	for index, message := range input {
		if message != nil && message.Role == schema.Tool {
			toolIndexes = append(toolIndexes, index)
		}
	}
	if len(toolIndexes) == 0 {
		return result, false
	}
	const minimumToolBudget = 160
	remaining := totalBudget
	changed := false
	// 最新工具结果优先获得细节，同时为每个更早的结果保留最小摘要预算。
	for position := len(toolIndexes) - 1; position >= 0; position-- {
		index := toolIndexes[position]
		reservedForOlder := position * minimumToolBudget
		budget := remaining - reservedForOlder
		if budget > perTool {
			budget = perTool
		}
		if budget < minimumToolBudget {
			budget = minimumToolBudget
		}
		cloned := *input[index]
		cloned.Content = compactToolResultForRetry(cloned.Content, budget, retryAttempt)
		if cloned.Content != input[index].Content {
			changed = true
		}
		result[index] = &cloned
		remaining -= utf8.RuneCountInString(cloned.Content)
		if remaining < 0 {
			remaining = 0
		}
	}
	return result, changed
}

func compactModelRetryInput(input []*schema.Message, retryAttempt int) []*schema.Message {
	policyIndex := retryAttempt - 1
	if policyIndex < 0 {
		policyIndex = 0
	}
	if policyIndex >= len(modelRetryTotalRuneBudgets) {
		policyIndex = len(modelRetryTotalRuneBudgets) - 1
	}
	result, _ := compactToolResultMessages(
		input, modelRetryTotalRuneBudgets[policyIndex], modelRetryPerToolBudgets[policyIndex], retryAttempt,
	)
	return result
}

const (
	// 工具结果累计 rune 超过软预算即在 react 循环内主动做有界摘要，避免 40 轮工具往返裸
	// 累积撞上下文上限（#95 H1b）。阈值取重试首次压缩的总预算，使主动压缩在「即将触发
	// context-length 重试」之前先行收敛。
	turnToolResultSoftBudgetRunes = 24000
	turnToolResultPerToolRunes    = 8000
)

// compactTurnToolResults 在工具结果累计 rune 超过软预算时，于 react 循环内主动压缩历史工具
// 消息。压缩仅作用于本次模型输入，不改动 eino 内部历史或落库记录；配合注入的收敛提示，
// 引导模型在细节被摘要前先用 plan.update 固化关键结论与 ID。返回是否发生压缩。
func compactTurnToolResults(messages []*schema.Message) ([]*schema.Message, bool) {
	cumulative := 0
	for _, message := range messages {
		if message != nil && message.Role == schema.Tool {
			cumulative += utf8.RuneCountInString(message.Content)
		}
	}
	if cumulative <= turnToolResultSoftBudgetRunes {
		return messages, false
	}
	return compactToolResultMessages(messages, turnToolResultSoftBudgetRunes, turnToolResultPerToolRunes, 1)
}

func compactToolResultForRetry(content string, budget, retryAttempt int) string {
	if budget <= 0 {
		return ""
	}
	originalRunes := utf8.RuneCountInString(content)
	if originalRunes <= budget {
		return content
	}

	var decoded any
	if json.Unmarshal([]byte(content), &decoded) == nil {
		compacted := compactRetryJSONValue(decoded, retryAttempt, 0)
		if object, ok := compacted.(map[string]any); ok {
			object["_retry_compacted"] = true
			object["_original_runes"] = originalRunes
		}
		if encoded, err := json.Marshal(compacted); err == nil && utf8.RuneCount(encoded) <= budget {
			return string(encoded)
		}
	}
	return boundedRetryPreview(content, originalRunes, budget)
}

func compactRetryJSONValue(value any, retryAttempt, depth int) any {
	if depth >= 6 {
		return map[string]any{"_compacted": true, "type": fmt.Sprintf("%T", value)}
	}
	policyIndex := retryAttempt - 1
	if policyIndex < 0 {
		policyIndex = 0
	}
	if policyIndex >= maxModelTimeoutRetries {
		policyIndex = maxModelTimeoutRetries - 1
	}
	arrayLimits := [...]int{16, 12, 10, 8, 6}
	stringLimits := [...]int{1000, 800, 600, 400, 300}

	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		result := make(map[string]any, len(typed))
		for _, key := range keys {
			result[key] = compactRetryJSONValue(typed[key], retryAttempt, depth+1)
		}
		return result
	case []any:
		limit := arrayLimits[policyIndex]
		if len(typed) <= limit {
			result := make([]any, 0, len(typed))
			for _, item := range typed {
				result = append(result, compactRetryJSONValue(item, retryAttempt, depth+1))
			}
			return result
		}
		sampled := make([]any, 0, limit)
		for _, index := range uniformSampleIndexes(len(typed), limit) {
			sampled = append(sampled, compactRetryJSONValue(typed[index], retryAttempt, depth+1))
		}
		return map[string]any{
			"_compacted_array": true, "total_items": len(typed), "sampled_items": sampled,
		}
	case string:
		return truncateRunesToLimit(typed, stringLimits[policyIndex])
	default:
		return typed
	}
}

func uniformSampleIndexes(total, limit int) []int {
	if total <= 0 || limit <= 0 {
		return nil
	}
	if total <= limit {
		indexes := make([]int, total)
		for index := range indexes {
			indexes[index] = index
		}
		return indexes
	}
	if limit == 1 {
		return []int{total - 1}
	}
	indexes := make([]int, 0, limit)
	for index := 0; index < limit; index++ {
		indexes = append(indexes, index*(total-1)/(limit-1))
	}
	return indexes
}

func boundedRetryPreview(content string, originalRunes, budget int) string {
	base := map[string]any{
		"_retry_compacted": true, "original_runes": originalRunes, "preview": "",
	}
	encoded, _ := json.Marshal(base)
	available := budget - utf8.RuneCount(encoded)
	if available < 0 {
		minimal := `{"_retry_compacted":true}`
		return truncateRunesToLimit(minimal, budget)
	}
	base["preview"] = truncateRunesToLimit(content, available)
	encoded, _ = json.Marshal(base)
	for utf8.RuneCount(encoded) > budget && available > 0 {
		available -= utf8.RuneCount(encoded) - budget
		if available < 0 {
			available = 0
		}
		base["preview"] = truncateRunesToLimit(content, available)
		encoded, _ = json.Marshal(base)
	}
	return string(encoded)
}

func truncateRunesToLimit(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	if limit == 1 {
		return "…"
	}
	return string(runes[:limit-1]) + "…"
}
