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

type modelRetryNotice struct {
	Attempt    int
	MaxRetries int
	Delay      time.Duration
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
	if !isRetryableModelTimeout(ctx, requestErr) {
		return completedRetries, nil, requestErr
	}
	if completedRetries >= retry.maxRetries {
		return completedRetries, nil, &modelResponseTimeoutError{
			Retries: completedRetries, LastErr: requestErr,
		}
	}

	retryAttempt := completedRetries + 1
	delay := retry.delay(retryAttempt)
	reportModelRetry(ctx, modelRetryNotice{
		Attempt: retryAttempt, MaxRetries: retry.maxRetries, Delay: delay,
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

func compactModelRetryInput(input []*schema.Message, retryAttempt int) []*schema.Message {
	result := append([]*schema.Message(nil), input...)
	toolIndexes := make([]int, 0)
	for index, message := range input {
		if message != nil && message.Role == schema.Tool {
			toolIndexes = append(toolIndexes, index)
		}
	}
	if len(toolIndexes) == 0 {
		return result
	}

	policyIndex := retryAttempt - 1
	if policyIndex < 0 {
		policyIndex = 0
	}
	if policyIndex >= len(modelRetryTotalRuneBudgets) {
		policyIndex = len(modelRetryTotalRuneBudgets) - 1
	}
	remaining := modelRetryTotalRuneBudgets[policyIndex]
	perTool := modelRetryPerToolBudgets[policyIndex]
	const minimumToolBudget = 160

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
		result[index] = &cloned
		remaining -= utf8.RuneCountInString(cloned.Content)
		if remaining < 0 {
			remaining = 0
		}
	}
	return result
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
