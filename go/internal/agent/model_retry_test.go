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

type partialTimeoutModel struct {
	mu    sync.Mutex
	calls int
}

func (stub *partialTimeoutModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return stub, nil
}

func (*partialTimeoutModel) Generate(
	context.Context, []*schema.Message, ...model.Option,
) (*schema.Message, error) {
	return nil, errors.New("unused")
}

func (stub *partialTimeoutModel) Stream(
	context.Context, []*schema.Message, ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	stub.mu.Lock()
	stub.calls++
	call := stub.calls
	stub.mu.Unlock()
	if call > 1 {
		return schema.StreamReaderFromArray([]*schema.Message{
			schema.AssistantMessage("恢复成功", nil),
		}), nil
	}
	reader, writer := schema.Pipe[*schema.Message](2)
	writer.Send(schema.AssistantMessage("不完整输出", nil), nil)
	writer.Send(nil, context.DeadlineExceeded)
	writer.Close()
	return reader, nil
}

func TestTimeoutRetryChatModelDiscardsPartialStreamAndRetriesBeforeSideEffects(t *testing.T) {
	t.Parallel()
	stub := &partialTimeoutModel{}
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
	if err != nil || message.Content != "恢复成功" {
		t.Fatalf("first=%#v err=%v", message, err)
	}
	if _, err = stream.Recv(); !errors.Is(err, io.EOF) || stub.calls != 2 || notices != 1 {
		t.Fatalf("final err=%v calls=%d notices=%d", err, stub.calls, notices)
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

func TestForwardModelStreamPropagatesEOF(t *testing.T) {
	t.Parallel()
	source := schema.StreamReaderFromArray([]*schema.Message{schema.AssistantMessage("尾部", nil)})
	stream := forwardModelStream(schema.AssistantMessage("首部", nil), source)
	defer stream.Close()
	for _, expected := range []string{"首部", "尾部"} {
		message, err := stream.Recv()
		if err != nil || message.Content != expected {
			t.Fatalf("message=%#v err=%v", message, err)
		}
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("EOF=%v", err)
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
