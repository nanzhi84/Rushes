package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/agenttest"
)

type timeoutRecordingModel struct {
	mu            sync.Mutex
	generateCalls int
	streamCalls   int
	toolCount     int
	inputs        [][]*schema.Message
	streamFirst   bool
	withToolsErr  error
}

func (stub *timeoutRecordingModel) WithTools(
	tools []*schema.ToolInfo,
) (model.ToolCallingChatModel, error) {
	stub.mu.Lock()
	stub.toolCount = len(tools)
	err := stub.withToolsErr
	stub.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return stub, nil
}

func (stub *timeoutRecordingModel) Generate(
	_ context.Context,
	input []*schema.Message,
	_ ...model.Option,
) (*schema.Message, error) {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	stub.generateCalls++
	stub.inputs = append(stub.inputs, cloneRetryTestMessages(input))
	return nil, context.DeadlineExceeded
}

func (stub *timeoutRecordingModel) Stream(
	_ context.Context,
	input []*schema.Message,
	_ ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	stub.streamCalls++
	stub.inputs = append(stub.inputs, cloneRetryTestMessages(input))
	if stub.streamFirst && stub.streamCalls > 1 {
		return schema.StreamReaderFromArray([]*schema.Message{
			schema.AssistantMessage("恢复成功", nil),
		}), nil
	}
	return nil, context.DeadlineExceeded
}

func cloneRetryTestMessages(input []*schema.Message) []*schema.Message {
	result := make([]*schema.Message, len(input))
	for index, message := range input {
		if message == nil {
			continue
		}
		cloned := *message
		result[index] = &cloned
	}
	return result
}

func TestTimeoutRetryChatModelRetriesFiveTimesAndCompactsToolResults(t *testing.T) {
	t.Parallel()
	stub := &timeoutRecordingModel{}
	retry := &timeoutRetryChatModel{
		inner: stub, maxRetries: maxModelTimeoutRetries,
		delay: func(attempt int) time.Duration { return time.Duration(attempt) * time.Millisecond },
		wait:  func(context.Context, time.Duration) error { return nil },
	}
	cutFrames := make([]int, 0, 5000)
	for frame := 1; frame <= 5000; frame++ {
		cutFrames = append(cutFrames, frame*15)
	}
	toolPayload, err := json.Marshal(map[string]any{
		"status": "succeeded",
		"data": map[string]any{
			"cut_frames": cutFrames,
			"analysis":   strings.Repeat("完整工具观察", 4000),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	toolMessage := &schema.Message{
		Role: schema.Tool, Content: string(toolPayload), ToolCallID: "call_recut", ToolName: "timeline.recut_to_beats",
	}
	input := []*schema.Message{schema.UserMessage("继续剪辑"), toolMessage}
	originalContent := toolMessage.Content
	notices := make([]modelRetryNotice, 0, maxModelTimeoutRetries)
	ctx := withModelRetryReporter(t.Context(), func(notice modelRetryNotice) {
		notices = append(notices, notice)
	})

	_, generateErr := retry.Generate(ctx, input)
	var timeoutErr *modelResponseTimeoutError
	if !errors.As(generateErr, &timeoutErr) || timeoutErr.Retries != maxModelTimeoutRetries {
		t.Fatalf("generate error=%#v", generateErr)
	}
	if stub.generateCalls != maxModelTimeoutRetries+1 || len(notices) != maxModelTimeoutRetries {
		t.Fatalf("calls=%d notices=%#v", stub.generateCalls, notices)
	}
	for index, notice := range notices {
		if notice.Attempt != index+1 || notice.MaxRetries != maxModelTimeoutRetries ||
			notice.Delay != time.Duration(index+1)*time.Millisecond {
			t.Fatalf("notice[%d]=%#v", index, notice)
		}
	}
	if toolMessage.Content != originalContent {
		t.Fatal("重试压缩修改了原始消息")
	}
	for callIndex, messages := range stub.inputs[1:] {
		compacted := messages[1]
		if compacted.ToolCallID != "call_recut" || compacted.ToolName != "timeline.recut_to_beats" {
			t.Fatalf("工具关联字段丢失: %#v", compacted)
		}
		if utf8.RuneCountInString(compacted.Content) > modelRetryPerToolBudgets[callIndex] {
			t.Fatalf("retry %d 超出单条预算: %d", callIndex+1, utf8.RuneCountInString(compacted.Content))
		}
		var decoded any
		if err := json.Unmarshal([]byte(compacted.Content), &decoded); err != nil {
			t.Fatalf("retry %d 工具摘要不是合法 JSON: %v\n%s", callIndex+1, err, compacted.Content)
		}
	}

	bound, err := retry.WithTools([]*schema.ToolInfo{{Name: "asset.list_assets"}})
	if err != nil || bound == nil || stub.toolCount != 1 {
		t.Fatalf("WithTools 未透传: bound=%T tools=%d err=%v", bound, stub.toolCount, err)
	}
}

func TestModelRetryReporterPublishesTurnStreamState(t *testing.T) {
	t.Parallel()
	stub := &timeoutRecordingModel{}
	retry := &timeoutRetryChatModel{
		inner: stub, maxRetries: 1,
		delay: func(int) time.Duration { return 375 * time.Millisecond },
		wait:  func(context.Context, time.Duration) error { return nil },
	}
	service := &Service{hub: NewTurnStreamHub(0)}
	_, err := retry.Generate(
		service.withModelRetryReporting(t.Context(), "draft_retry_event"),
		[]*schema.Message{schema.UserMessage("继续")},
	)
	var timeoutErr *modelResponseTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("err=%v", err)
	}
	events := service.hub.Snapshot("draft_retry_event")
	if len(events) != 1 || events[0]["type"] != "model_retry" ||
		events[0]["attempt"] != 1 || events[0]["max_retries"] != 1 ||
		events[0]["reason"] != "模型响应超时" || events[0]["next_delay_ms"] != int64(375) {
		t.Fatalf("events=%#v", events)
	}
}

func TestTimeoutRetryChatModelStreamRetriesOnlyBeforeFirstChunk(t *testing.T) {
	t.Parallel()
	stub := &timeoutRecordingModel{streamFirst: true}
	retry := &timeoutRetryChatModel{
		inner: stub, maxRetries: maxModelTimeoutRetries,
		delay: func(int) time.Duration { return 0 },
		wait:  func(context.Context, time.Duration) error { return nil },
	}
	notices := 0
	ctx := withModelRetryReporter(t.Context(), func(modelRetryNotice) { notices++ })
	stream, err := retry.Stream(ctx, []*schema.Message{schema.UserMessage("继续")})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	message, err := stream.Recv()
	if err != nil || message.Content != "恢复成功" || stub.streamCalls != 2 || notices != 1 {
		t.Fatalf("message=%#v err=%v calls=%d notices=%d", message, err, stub.streamCalls, notices)
	}
}

// scriptedStreamModel 按脚本逐次返回 Stream 结果，用于精确复现终态回复直通流式的重试边界
// （#95 H5）：工具调用轮与「前导阶段」失败仍缓冲后重试，一旦越过首个可见正文的直通承诺点就
// 不再重试。
type scriptedStreamModel struct {
	mu      sync.Mutex
	calls   int
	scripts []func() (*schema.StreamReader[*schema.Message], error)
}

func (stub *scriptedStreamModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return stub, nil
}

func (*scriptedStreamModel) Generate(
	context.Context, []*schema.Message, ...model.Option,
) (*schema.Message, error) {
	return nil, errors.New("unused")
}

func (stub *scriptedStreamModel) Stream(
	context.Context, []*schema.Message, ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	stub.mu.Lock()
	index := stub.calls
	stub.calls++
	stub.mu.Unlock()
	if index >= len(stub.scripts) {
		return nil, fmt.Errorf("脚本已用尽：第 %d 次调用", index+1)
	}
	return stub.scripts[index]()
}

// streamChunksThenError 构造一条先逐块推送、再（当 err 非 nil 时）以该错误终止的模型流副本。
func streamChunksThenError(chunks []*schema.Message, err error) func() (*schema.StreamReader[*schema.Message], error) {
	return func() (*schema.StreamReader[*schema.Message], error) {
		reader, writer := schema.Pipe[*schema.Message](len(chunks) + 1)
		for _, chunk := range chunks {
			writer.Send(chunk, nil)
		}
		if err != nil {
			writer.Send(nil, err)
		}
		writer.Close()
		return reader, nil
	}
}

func newTestStreamRetry(inner model.ToolCallingChatModel) *timeoutRetryChatModel {
	return &timeoutRetryChatModel{
		inner: inner, maxRetries: maxModelTimeoutRetries,
		delay: func(int) time.Duration { return 0 },
		wait:  func(context.Context, time.Duration) error { return nil },
	}
}

func TestStreamToolRoundBuffersAndRetriesPartialFailure(t *testing.T) {
	t.Parallel()
	toolChunk := func() *schema.Message {
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID: "call_edit", Function: schema.FunctionCall{Name: "timeline.edit_talking_head", Arguments: `{}`},
		}})
	}
	stub := &scriptedStreamModel{scripts: []func() (*schema.StreamReader[*schema.Message], error){
		// 工具调用轮中途断流：已判为工具轮，完整缓冲时撞到超时 → 丢弃后重试。
		streamChunksThenError([]*schema.Message{toolChunk()}, context.DeadlineExceeded),
		// 重试成功：完整的工具调用轮。
		streamChunksThenError([]*schema.Message{toolChunk()}, nil),
	}}
	retry := newTestStreamRetry(stub)
	notices := 0
	ctx := withModelRetryReporter(t.Context(), func(modelRetryNotice) { notices++ })
	stream, err := retry.Stream(ctx, []*schema.Message{schema.UserMessage("继续")})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	message, err := stream.Recv()
	if err != nil || len(message.ToolCalls) != 1 || message.ToolCalls[0].ID != "call_edit" {
		t.Fatalf("工具轮重试后应产出完整 tool_call：message=%#v err=%v", message, err)
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) || stub.calls != 2 || notices != 1 {
		t.Fatalf("工具轮 partial 失败应缓冲后重试一次：err=%v calls=%d notices=%d", err, stub.calls, notices)
	}
}

func TestStreamRetriesOnPreambleFailureBeforeVisibleContent(t *testing.T) {
	t.Parallel()
	stub := &scriptedStreamModel{scripts: []func() (*schema.StreamReader[*schema.Message], error){
		// 前导（纯思考）分片后断流：尚未越过直通承诺点 → 安全重试。
		streamChunksThenError([]*schema.Message{{Role: schema.Assistant, ReasoningContent: "先想想"}}, context.DeadlineExceeded),
		// 重试成功：终态文本轮。
		streamChunksThenError([]*schema.Message{schema.AssistantMessage("恢复成功", nil)}, nil),
	}}
	retry := newTestStreamRetry(stub)
	notices := 0
	ctx := withModelRetryReporter(t.Context(), func(modelRetryNotice) { notices++ })
	stream, err := retry.Stream(ctx, []*schema.Message{schema.UserMessage("继续")})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	message, err := stream.Recv()
	if err != nil || message.Content != "恢复成功" {
		t.Fatalf("前导失败应重试到成功：message=%#v err=%v", message, err)
	}
	if stub.calls != 2 || notices != 1 {
		t.Fatalf("前导失败应重试一次：calls=%d notices=%d", stub.calls, notices)
	}
}

func TestStreamFinalRoundStreamsThroughWithoutRetryAfterContent(t *testing.T) {
	t.Parallel()
	stub := &scriptedStreamModel{scripts: []func() (*schema.StreamReader[*schema.Message], error){
		// 终态文本轮：首个可见正文后 provider 断流 → 已直通、不再重试，错误原样直达消费端。
		streamChunksThenError([]*schema.Message{schema.AssistantMessage("开始回答", nil)}, context.DeadlineExceeded),
		// 若错误地重试就会取到这条，断言不应到达。
		streamChunksThenError([]*schema.Message{schema.AssistantMessage("不应重试", nil)}, nil),
	}}
	retry := newTestStreamRetry(stub)
	notices := 0
	ctx := withModelRetryReporter(t.Context(), func(modelRetryNotice) { notices++ })
	stream, err := retry.Stream(ctx, []*schema.Message{schema.UserMessage("继续")})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	message, err := stream.Recv()
	if err != nil || message.Content != "开始回答" {
		t.Fatalf("终态轮首个正文应直通：message=%#v err=%v", message, err)
	}
	if _, err := stream.Recv(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("直通后 provider 断流应原样透出、不重试：err=%v", err)
	}
	if stub.calls != 1 || notices != 0 {
		t.Fatalf("直通轮不得重试：calls=%d notices=%d", stub.calls, notices)
	}
}

func TestTimeoutRetryChatModelHonorsCallerCancellation(t *testing.T) {
	t.Parallel()
	stub := &timeoutRecordingModel{}
	retry := &timeoutRetryChatModel{
		inner: stub, maxRetries: maxModelTimeoutRetries,
		delay: modelRetryDelay, wait: waitForModelRetry,
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := retry.Generate(ctx, []*schema.Message{schema.UserMessage("停止")})
	if !errors.Is(err, context.DeadlineExceeded) || stub.generateCalls != 1 {
		t.Fatalf("err=%v calls=%d", err, stub.generateCalls)
	}
}

type countingFailureReplyModel struct{ calls int }

func (stub *countingFailureReplyModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return stub, nil
}

func (stub *countingFailureReplyModel) Generate(
	context.Context, []*schema.Message, ...model.Option,
) (*schema.Message, error) {
	stub.calls++
	return schema.AssistantMessage("模型猜测文案", nil), nil
}

func (stub *countingFailureReplyModel) Stream(
	context.Context, []*schema.Message, ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	return nil, errors.New("unused")
}

func TestTerminalFailureReplyClassifiesTimeoutWithoutAnotherModelCall(t *testing.T) {
	t.Parallel()
	stub := &countingFailureReplyModel{}
	service := &Service{hub: NewTurnStreamHub(0), chatModel: stub}
	content := service.terminalFailureReply(t.Context(), "draft_timeout", "msg_timeout", &modelResponseTimeoutError{
		Retries: maxModelTimeoutRetries, LastErr: context.DeadlineExceeded,
	})
	if stub.calls != 0 {
		t.Fatalf("超时失败文案不应再次调用模型: %d", stub.calls)
	}
	if !strings.Contains(content, "模型响应超时") || !strings.Contains(content, "自动重试 5 次") ||
		!strings.Contains(content, "当前最新时间线") {
		t.Fatalf("timeout content=%q", content)
	}
	for _, event := range service.hub.Snapshot("draft_timeout") {
		if event["type"] != "text_delta" {
			t.Fatalf("unexpected event=%#v", event)
		}
	}
}

func TestModelRetryHelpersCoverTimeoutKindsAndBackoff(t *testing.T) {
	t.Parallel()
	timeoutErr := &modelResponseTimeoutError{Retries: 5, LastErr: context.DeadlineExceeded}
	if timeoutErr.Error() != "模型响应超时（已自动重试 5 次）" ||
		!errors.Is(timeoutErr, context.DeadlineExceeded) {
		t.Fatalf("timeout error=%v", timeoutErr)
	}
	if withModelRetryReporter(t.Context(), nil) != t.Context() {
		t.Fatal("nil reporter 应原样返回 context")
	}
	reportModelRetry(t.Context(), modelRetryNotice{Attempt: 1})

	for attempt, expected := range map[int]time.Duration{
		0: 250 * time.Millisecond, 1: 250 * time.Millisecond,
		2: 500 * time.Millisecond, 5: 4 * time.Second, 9: 4 * time.Second,
	} {
		if actual := modelRetryDelay(attempt); actual != expected {
			t.Fatalf("delay(%d)=%s want=%s", attempt, actual, expected)
		}
	}
	if err := waitForModelRetry(t.Context(), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := waitForModelRetry(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if !errors.Is(waitForModelRetry(cancelled, time.Second), context.Canceled) {
		t.Fatal("等待退避必须响应取消")
	}

	if isRetryableModelTimeout(t.Context(), nil) ||
		isRetryableModelTimeout(t.Context(), context.Canceled) ||
		isRetryableModelTimeout(cancelled, context.DeadlineExceeded) ||
		isRetryableModelTimeout(t.Context(), errors.New("普通错误")) {
		t.Fatal("非超时错误被错误重试")
	}
	for _, err := range []error{
		context.DeadlineExceeded,
		testTimeoutNetworkError{},
		errors.New("Client.Timeout exceeded while awaiting headers"),
		errors.New("request timeout from provider"),
	} {
		if !isRetryableModelTimeout(t.Context(), err) {
			t.Fatalf("超时错误未识别: %v", err)
		}
	}

	failedBinding := &timeoutRecordingModel{withToolsErr: errors.New("bind failed")}
	retry := newTimeoutRetryChatModel(failedBinding).(*timeoutRetryChatModel)
	if _, err := retry.WithTools(nil); err == nil {
		t.Fatal("WithTools 错误未透传")
	}
}

type testTimeoutNetworkError struct{}

func (testTimeoutNetworkError) Error() string   { return "network timeout" }
func (testTimeoutNetworkError) Timeout() bool   { return true }
func (testTimeoutNetworkError) Temporary() bool { return true }

func TestModelRetryCompactionHelpersStayBoundedAndDeterministic(t *testing.T) {
	t.Parallel()
	plain := "短结果"
	if compactToolResultForRetry(plain, 100, 1) != plain ||
		compactToolResultForRetry(plain, 0, 1) != "" {
		t.Fatal("短结果或零预算处理错误")
	}
	preview := compactToolResultForRetry(strings.Repeat("非 JSON 工具输出", 100), 180, 1)
	var previewJSON map[string]any
	if utf8.RuneCountInString(preview) > 180 || json.Unmarshal([]byte(preview), &previewJSON) != nil ||
		previewJSON["_retry_compacted"] != true {
		t.Fatalf("preview=%q", preview)
	}
	if tiny := boundedRetryPreview("很长的输出", 100, 8); utf8.RuneCountInString(tiny) > 8 {
		t.Fatalf("tiny=%q", tiny)
	}

	largeArray := make([]any, 30)
	for index := range largeArray {
		largeArray[index] = map[string]any{
			"index": index, "description": strings.Repeat("镜头", 600),
		}
	}
	compacted := compactRetryJSONValue(map[string]any{
		"items": largeArray, "small": []any{1, 2}, "enabled": true,
	}, 0, 0).(map[string]any)
	items := compacted["items"].(map[string]any)
	if items["total_items"] != 30 || len(items["sampled_items"].([]any)) != 16 ||
		!reflect.DeepEqual(compacted["small"], []any{float64(1), float64(2)}) &&
			!reflect.DeepEqual(compacted["small"], []any{1, 2}) {
		t.Fatalf("compacted=%#v", compacted)
	}
	deep := compactRetryJSONValue(map[string]any{"hidden": true}, 99, 6).(map[string]any)
	if deep["_compacted"] != true {
		t.Fatalf("deep=%#v", deep)
	}
	if got := compactRetryJSONValue(strings.Repeat("字", 1000), 99, 0).(string); len([]rune(got)) > 300 {
		t.Fatalf("string len=%d", len([]rune(got)))
	}
	if compactRetryJSONValue(42, 1, 0) != 42 {
		t.Fatal("标量不应改变")
	}

	for _, test := range []struct {
		total, limit int
		want         []int
	}{
		{0, 3, nil}, {3, 0, nil}, {3, 5, []int{0, 1, 2}},
		{10, 1, []int{9}}, {10, 3, []int{0, 4, 9}},
	} {
		if got := uniformSampleIndexes(test.total, test.limit); !reflect.DeepEqual(got, test.want) {
			t.Fatalf("sample(%d,%d)=%v", test.total, test.limit, got)
		}
	}
	for _, test := range []struct {
		value string
		limit int
		want  string
	}{
		{"abc", 0, ""}, {"abc", 4, "abc"}, {"abc", 1, "…"}, {"abcdef", 4, "abc…"},
	} {
		if got := truncateRunesToLimit(test.value, test.limit); got != test.want {
			t.Fatalf("truncate=%q want=%q", got, test.want)
		}
	}

	input := []*schema.Message{nil, schema.UserMessage("无工具")}
	copyInput := compactModelRetryInput(input, -2)
	if len(copyInput) != len(input) || copyInput[1] != input[1] {
		t.Fatalf("no-tool compact=%#v", copyInput)
	}
	toolMessages := make([]*schema.Message, 0, 30)
	for index := 0; index < 30; index++ {
		toolMessages = append(toolMessages, &schema.Message{
			Role: schema.Tool, ToolCallID: fmt.Sprintf("call_%d", index),
			Content: strings.Repeat("大工具结果", 2000),
		})
	}
	bounded := compactModelRetryInput(toolMessages, 99)
	totalRunes := 0
	for _, message := range bounded {
		totalRunes += utf8.RuneCountInString(message.Content)
	}
	if totalRunes > modelRetryTotalRuneBudgets[len(modelRetryTotalRuneBudgets)-1] {
		t.Fatalf("tool total=%d", totalRunes)
	}
}

type firstReceiveScriptModel struct {
	calls      int
	emptyAfter bool
	nonTimeout bool
}

func (stub *firstReceiveScriptModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return stub, nil
}

func (*firstReceiveScriptModel) Generate(
	context.Context, []*schema.Message, ...model.Option,
) (*schema.Message, error) {
	return nil, errors.New("unused")
}

func (stub *firstReceiveScriptModel) Stream(
	context.Context, []*schema.Message, ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	stub.calls++
	if stub.nonTimeout {
		reader, writer := schema.Pipe[*schema.Message](1)
		writer.Send(nil, errors.New("decode failed"))
		writer.Close()
		return reader, nil
	}
	if stub.calls == 1 {
		reader, writer := schema.Pipe[*schema.Message](1)
		writer.Send(nil, context.DeadlineExceeded)
		writer.Close()
		return reader, nil
	}
	if stub.emptyAfter {
		return schema.StreamReaderFromArray([]*schema.Message{}), nil
	}
	return schema.StreamReaderFromArray([]*schema.Message{schema.AssistantMessage("ok", nil)}), nil
}

func TestTimeoutRetryStreamHandlesFirstReceiveErrorEmptyAndWaitFailure(t *testing.T) {
	t.Parallel()
	stub := &firstReceiveScriptModel{emptyAfter: true}
	retry := &timeoutRetryChatModel{
		inner: stub, maxRetries: 1,
		delay: func(int) time.Duration { return 0 },
		wait:  func(context.Context, time.Duration) error { return nil },
	}
	stream, err := retry.Stream(t.Context(), []*schema.Message{schema.UserMessage("x")})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) || stub.calls != 2 {
		t.Fatalf("empty err=%v calls=%d", err, stub.calls)
	}

	nonTimeout := &firstReceiveScriptModel{nonTimeout: true}
	retry.inner = nonTimeout
	if _, err := retry.Stream(t.Context(), nil); err == nil || err.Error() != "decode failed" || nonTimeout.calls != 1 {
		t.Fatalf("non-timeout err=%v calls=%d", err, nonTimeout.calls)
	}

	waitErr := errors.New("wait interrupted")
	retry.inner = &timeoutRecordingModel{}
	retry.wait = func(context.Context, time.Duration) error { return waitErr }
	if _, err := retry.Stream(t.Context(), nil); !errors.Is(err, waitErr) {
		t.Fatalf("wait err=%v", err)
	}
}

// contextLengthRecoveryModel 在前 failCalls 次调用返回 context-length 错误，
// 之后返回一条带 token 用量的成功回复，用于验证压缩重试链路与用量统计。
type contextLengthRecoveryModel struct {
	mu        sync.Mutex
	failCalls int
	failErr   error
	usage     *schema.TokenUsage
	calls     int
	inputs    [][]*schema.Message
}

func (stub *contextLengthRecoveryModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return stub, nil
}

func (stub *contextLengthRecoveryModel) Generate(
	_ context.Context, input []*schema.Message, _ ...model.Option,
) (*schema.Message, error) {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	stub.calls++
	stub.inputs = append(stub.inputs, cloneRetryTestMessages(input))
	if stub.calls <= stub.failCalls {
		return nil, stub.failErr
	}
	response := schema.AssistantMessage("恢复成功", nil)
	response.ResponseMeta = &schema.ResponseMeta{Usage: stub.usage}
	return response, nil
}

func (stub *contextLengthRecoveryModel) Stream(
	ctx context.Context, input []*schema.Message, options ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	response, err := stub.Generate(ctx, input, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{response}), nil
}

func TestContextLengthErrorTriggersCompactionRetryThenSucceeds(t *testing.T) {
	t.Parallel()
	stub := &contextLengthRecoveryModel{
		failCalls: 2,
		failErr:   errors.New("Range of input length should be [1, 30720], but got 41234"),
		usage:     &schema.TokenUsage{PromptTokens: 1200, CompletionTokens: 40, TotalTokens: 1240},
	}
	retry := &timeoutRetryChatModel{
		inner: stub, maxRetries: maxModelTimeoutRetries,
		delay: func(int) time.Duration { return 0 },
		wait:  func(context.Context, time.Duration) error { return nil },
	}
	toolPayload, err := json.Marshal(map[string]any{
		"status": "succeeded",
		"data":   map[string]any{"analysis": strings.Repeat("完整语音观察", 4000)},
	})
	if err != nil {
		t.Fatal(err)
	}
	toolMessage := &schema.Message{
		Role: schema.Tool, Content: string(toolPayload),
		ToolCallID: "call_inspect", ToolName: "speech.search",
	}
	originalContent := toolMessage.Content
	input := []*schema.Message{schema.UserMessage("继续剪辑"), toolMessage}
	notices := make([]modelRetryNotice, 0, 2)
	budget := newTurnBudgetState(8)
	ctx := withTurnBudgetState(
		withModelRetryReporter(t.Context(), func(notice modelRetryNotice) {
			notices = append(notices, notice)
		}),
		budget,
	)

	response, generateErr := retry.Generate(ctx, input)
	if generateErr != nil {
		t.Fatalf("context-length 压缩重试后应成功: %v", generateErr)
	}
	if response.Content != "恢复成功" || stub.calls != 3 {
		t.Fatalf("response=%#v calls=%d", response, stub.calls)
	}
	if len(notices) != 2 {
		t.Fatalf("notices=%#v", notices)
	}
	for index, notice := range notices {
		if notice.Attempt != index+1 || notice.Reason != "上下文超出模型上限" {
			t.Fatalf("notice[%d]=%#v", index, notice)
		}
	}
	if toolMessage.Content != originalContent {
		t.Fatal("重试压缩修改了原始消息")
	}
	compacted := stub.inputs[1][1]
	if compacted.ToolCallID != "call_inspect" || compacted.ToolName != "speech.search" {
		t.Fatalf("压缩丢失工具关联: %#v", compacted)
	}
	if utf8.RuneCountInString(compacted.Content) >= utf8.RuneCountInString(originalContent) {
		t.Fatalf("重试未压缩工具结果: %d", utf8.RuneCountInString(compacted.Content))
	}
	var decoded any
	if err := json.Unmarshal([]byte(compacted.Content), &decoded); err != nil {
		t.Fatalf("压缩后工具摘要不是合法 JSON: %v", err)
	}
	// usageSnapshot 正是 turn_ended 事件上报的 token_usage 数据源，
	// 断言它计入了重试成功后的那次模型响应。
	usage := budget.usageSnapshot()
	if usage == nil || usage["model_calls"] != 1 ||
		usage["prompt_tokens"] != 1200 || usage["total_tokens"] != 1240 {
		t.Fatalf("turn 统计未计入压缩重试后的用量: %#v", usage)
	}
}

func TestContextLengthErrorExhaustsAsContextLengthError(t *testing.T) {
	t.Parallel()
	providerErr := errors.New("the request's input tokens exceed the model's context length limit, please reduce the length")
	stub := &contextLengthRecoveryModel{failCalls: 1 << 30, failErr: providerErr}
	retry := &timeoutRetryChatModel{
		inner: stub, maxRetries: maxModelTimeoutRetries,
		delay: func(int) time.Duration { return 0 },
		wait:  func(context.Context, time.Duration) error { return nil },
	}
	_, err := retry.Generate(t.Context(), []*schema.Message{
		schema.UserMessage("继续"),
		{Role: schema.Tool, ToolCallID: "call_big", Content: strings.Repeat("大工具结果", 5000)},
	})
	var contextErr *modelContextLengthError
	if !errors.As(err, &contextErr) || contextErr.Retries != maxModelTimeoutRetries {
		t.Fatalf("耗尽重试后应返回 context-length 终态: %#v", err)
	}
	if contextErr.Error() != "模型上下文超出上限（已自动压缩重试 5 次）" || !errors.Is(contextErr, providerErr) {
		t.Fatalf("终态文案或 unwrap 错误: %v", contextErr)
	}
	if stub.calls != maxModelTimeoutRetries+1 {
		t.Fatalf("calls=%d", stub.calls)
	}
}

func TestIsContextLengthExceededDetectsProviderStyles(t *testing.T) {
	t.Parallel()
	retryable := []error{
		errors.New("Range of input length should be [1, 30720], but got 41234"),
		errors.New("This model's maximum context length is 32768 tokens. However, your messages resulted in 40000 tokens"),
		errors.New("the request's input tokens exceed the model's context length limit, please reduce the length"),
		errors.New(`{"error":{"code":"context_length_exceeded","message":"prompt too long"}}`),
	}
	for _, err := range retryable {
		if !isContextLengthExceeded(err) {
			t.Fatalf("context-length 文案未识别: %v", err)
		}
	}
	nonRetryable := []error{
		nil,
		errors.New("invalid value for parameter 'fps': must be positive"),
		errors.New("Client.Timeout exceeded while awaiting headers"),
		context.Canceled,
	}
	for _, err := range nonRetryable {
		if isContextLengthExceeded(err) {
			t.Fatalf("普通/超时错误被误判为 context-length: %v", err)
		}
	}
}

func TestClassifyRetryableModelErrorDistinguishesReasons(t *testing.T) {
	t.Parallel()
	contextErr := errors.New("Range of input length should be [1, 30720], but got 41234")
	if classifyRetryableModelError(t.Context(), context.DeadlineExceeded) != modelRetryReasonTimeout {
		t.Fatal("超时未归类为 timeout")
	}
	if classifyRetryableModelError(t.Context(), contextErr) != modelRetryReasonContextLength {
		t.Fatal("context-length 未归类为 contextLength")
	}
	if classifyRetryableModelError(t.Context(), errors.New("普通参数错误")) != modelRetryReasonNone ||
		classifyRetryableModelError(t.Context(), nil) != modelRetryReasonNone {
		t.Fatal("非可重试错误被错误归类")
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if classifyRetryableModelError(cancelled, contextErr) != modelRetryReasonNone {
		t.Fatal("已取消的 context 不应触发 context-length 重试")
	}
	if modelRetryReasonTimeout.label() != "模型响应超时" ||
		modelRetryReasonContextLength.label() != "上下文超出模型上限" {
		t.Fatal("重试原因中文文案错误")
	}
}

func TestModelRetryReporterPublishesContextLengthReason(t *testing.T) {
	t.Parallel()
	stub := &contextLengthRecoveryModel{
		failCalls: 1 << 30, failErr: errors.New("context_length_exceeded"),
	}
	retry := &timeoutRetryChatModel{
		inner: stub, maxRetries: 1,
		delay: func(int) time.Duration { return 200 * time.Millisecond },
		wait:  func(context.Context, time.Duration) error { return nil },
	}
	service := &Service{hub: NewTurnStreamHub(0)}
	_, err := retry.Generate(
		service.withModelRetryReporting(t.Context(), "draft_context_length"),
		[]*schema.Message{schema.UserMessage("继续")},
	)
	var contextErr *modelContextLengthError
	if !errors.As(err, &contextErr) {
		t.Fatalf("err=%v", err)
	}
	events := service.hub.Snapshot("draft_context_length")
	if len(events) != 1 || events[0]["type"] != "model_retry" ||
		events[0]["attempt"] != 1 || events[0]["max_retries"] != 1 ||
		events[0]["reason"] != "上下文超出模型上限" || events[0]["next_delay_ms"] != int64(200) {
		t.Fatalf("events=%#v", events)
	}
}

func TestTerminalFailureReplyClassifiesContextLengthWithoutAnotherModelCall(t *testing.T) {
	t.Parallel()
	stub := &countingFailureReplyModel{}
	service := &Service{hub: NewTurnStreamHub(0), chatModel: stub}
	content := service.terminalFailureReply(t.Context(), "draft_context", "msg_context", &modelContextLengthError{
		Retries: maxModelTimeoutRetries, LastErr: errors.New("context_length_exceeded"),
	})
	if stub.calls != 0 {
		t.Fatalf("上下文超限终态不应再次调用模型: %d", stub.calls)
	}
	if !strings.Contains(content, "上下文超出了模型长度上限") ||
		!strings.Contains(content, "压缩并重试 5 次") ||
		!strings.Contains(content, "当前最新时间线") {
		t.Fatalf("context content=%q", content)
	}
	for _, event := range service.hub.Snapshot("draft_context") {
		if event["type"] != "text_delta" {
			t.Fatalf("unexpected event=%#v", event)
		}
	}
}

func TestContextLengthRetryFinishesTurnAndReportsUsage(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_context_length_turn")
	stub := &contextLengthRecoveryModel{
		failCalls: 1,
		failErr:   errors.New("Range of input length should be [1, 30720], but got 41234"),
		usage:     &schema.TokenUsage{PromptTokens: 800, CompletionTokens: 20, TotalTokens: 820},
	}
	service, err := NewService(t.Context(), database, stub)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	_, stream, unsubscribe := service.Hub().Subscribe("draft_context_length_turn")
	defer unsubscribe()
	if !service.Queue().EnqueueUserMessage("draft_context_length_turn", "user_context_length", "继续推进") {
		t.Fatal("enqueue failed")
	}
	service.Queue().JoinDraft("draft_context_length_turn")

	var turnEnded StreamEvent
	for turnEnded == nil {
		select {
		case event := <-stream:
			if event["type"] == "turn_ended" {
				turnEnded = event
			}
		case <-time.After(5 * time.Second):
			t.Fatal("等待 turn_ended 超时")
		}
	}
	if turnEnded["outcome"] != "finished" {
		t.Fatalf("压缩重试后回合应成功: %#v", turnEnded)
	}
	usage, _ := turnEnded["token_usage"].(map[string]any)
	if usage["model_calls"] != 1 || usage["total_tokens"] != 820 {
		t.Fatalf("turn_ended 未计入重试后的用量: %#v", turnEnded)
	}
}

func TestCompactTurnToolResultsBoundsHistoryAndInjectsHint(t *testing.T) {
	t.Parallel()
	makeToolMsg := func(id string, repeat int) *schema.Message {
		payload := map[string]any{
			"asset_id": id,
			"segments": strings.Repeat("很长的逐镜头语义描述与标签，", repeat),
		}
		encoded, _ := json.Marshal(payload)
		return &schema.Message{Role: schema.Tool, Content: string(encoded)}
	}

	// 小历史：累计不超软预算，不压缩、不注入提示。
	small := []*schema.Message{schema.UserMessage("剪辑"), makeToolMsg("solo", 5)}
	if _, did := compactTurnToolResults(small); did {
		t.Fatal("小历史不应触发压缩")
	}

	// 大历史：12 条大工具结果，累计远超 turnToolResultSoftBudgetRunes。
	messages := []*schema.Message{schema.UserMessage("请理解全部素材并剪辑")}
	ids := make([]string, 0, 12)
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("asset_key_%02d", i)
		ids = append(ids, id)
		messages = append(messages, makeToolMsg(id, 500))
	}
	before := 0
	for _, m := range messages {
		if m.Role == schema.Tool {
			before += utf8.RuneCountInString(m.Content)
		}
	}
	if before <= turnToolResultSoftBudgetRunes {
		t.Fatalf("fixture 未超软预算：%d", before)
	}

	compacted, did := compactTurnToolResults(messages)
	if !did {
		t.Fatal("超软预算的工具历史应触发压缩")
	}
	after := 0
	for _, m := range compacted {
		if m.Role == schema.Tool {
			after += utf8.RuneCountInString(m.Content)
		}
	}
	if after >= before || after > turnToolResultSoftBudgetRunes+len(ids)*160 {
		t.Fatalf("压缩后未有界：before=%d after=%d", before, after)
	}
	// 最新工具结果优先保细节：关键 ID 必须保留（模型据此 plan.update 固化）。
	if newest := compacted[len(compacted)-1]; !strings.Contains(newest.Content, ids[len(ids)-1]) {
		t.Fatalf("最新工具结果关键 ID 丢失：%s not in %s", ids[len(ids)-1], newest.Content)
	}
	// compactTurnToolResults 不得改动传入切片：原始 messages 累计 rune 应不变。
	stillOriginal := 0
	for _, m := range messages {
		if m.Role == schema.Tool {
			stillOriginal += utf8.RuneCountInString(m.Content)
		}
	}
	if stillOriginal != before {
		t.Fatalf("原始 messages 被就地改写：before=%d now=%d", before, stillOriginal)
	}

	// 修饰器在压缩时追加收敛提示；未压缩时不追加。
	modified := turnBudgetMessageModifier(t.Context(), messages)
	if modified[0].Role != schema.System || !strings.Contains(modified[0].Content, "上下文压缩提醒") {
		t.Fatalf("压缩时应注入收敛提示：%s", modified[0].Content)
	}
	if smallModified := turnBudgetMessageModifier(t.Context(), small); strings.Contains(smallModified[0].Content, "上下文压缩提醒") {
		t.Fatal("未压缩时不应注入压缩提示")
	}
}
