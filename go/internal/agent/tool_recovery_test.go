package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/cloudwego/eino/compose"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func TestToolRecoveryRetriesSafeErrorsAndReturnsThemToModel(t *testing.T) {
	state := newToolRecoveryState()
	ctx := withToolRecoveryState(t.Context(), state)
	calls := 0
	middleware := newToolRecoveryMiddleware()
	endpoint := middleware.Invokable(func(context.Context, *compose.ToolInput) (*compose.ToolOutput, error) {
		calls++
		return nil, errors.New("temporary read failure")
	})
	output, err := endpoint(ctx, &compose.ToolInput{Name: "asset.list_assets", Arguments: `{}`})
	if err != nil || calls != maxToolExecutionRetries+1 {
		t.Fatalf("calls=%d output=%#v err=%v", calls, output, err)
	}
	payload := decodeRecoveryPayload(t, output.Result)
	data := payload["data"].(map[string]any)
	if data["error_code"] != "tool_execution_error" || data["execution_attempts"] != float64(6) {
		t.Fatalf("payload=%#v", payload)
	}
	harness := data["harness_recovery"].(map[string]any)
	if harness["automatic_retries"] != float64(5) || !state.unresolved() {
		t.Fatalf("harness=%#v state=%#v", harness, state)
	}
}

func TestToolRecoveryFormattingHelpersCoverMalformedValues(t *testing.T) {
	t.Parallel()
	if isStructuredToolFailure("not-json") || isStructuredToolFailure(`{"status":"succeeded"}`) ||
		!isStructuredToolFailure(`{"status":"failed"}`) ||
		!isStructuredToolFailure(`{"status":"validation_failed"}`) {
		t.Fatal("structured failure detection mismatch")
	}
	if value := toolArgumentsForReport(`{"x":1}`); value.(map[string]any)["x"] != float64(1) {
		t.Fatalf("decoded arguments=%#v", value)
	}
	invalid := toolArgumentsForReport("not-json").(map[string]any)
	if invalid["raw_arguments"] != "not-json" {
		t.Fatalf("invalid arguments=%#v", invalid)
	}
	if truncateText(" abc ", 0) != "abc" || truncateText("abcdef", 3) != "abc…" {
		t.Fatal("truncateText mismatch")
	}
	reportSyntheticToolFailure(context.Background(), "missing-reporter", `{}`, "not-json")
	var phases []string
	var finalErr error
	reporterCtx := rushestools.WithReporter(t.Context(), func(
		_ context.Context, _ string, phase string, _, _ any, err error,
	) {
		phases = append(phases, phase)
		finalErr = err
	})
	reportSyntheticToolFailure(reporterCtx, "synthetic", "not-json", "not-json")
	if len(phases) != 2 || phases[0] != "started" || phases[1] != "finished" || finalErr == nil {
		t.Fatalf("phases=%v finalErr=%v", phases, finalErr)
	}
	reportSyntheticToolFailure(reporterCtx, "synthetic", `{}`, `{"status":"failed"}`)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if !errors.Is(waitForToolRetry(cancelled, 1), context.Canceled) {
		t.Fatal("cancelled retry should return context.Canceled")
	}
}

func TestToolRecoveryDoesNotBlindlyReplayMutations(t *testing.T) {
	calls := 0
	middleware := newToolRecoveryMiddleware()
	endpoint := middleware.Invokable(func(context.Context, *compose.ToolInput) (*compose.ToolOutput, error) {
		calls++
		return nil, errors.New("commit result unknown")
	})
	output, err := endpoint(
		withToolRecoveryState(t.Context(), newToolRecoveryState()),
		&compose.ToolInput{Name: "timeline.apply_patches", Arguments: `{"ops":[{"kind":"split_clip"}]}`},
	)
	if err != nil || calls != 1 {
		t.Fatalf("calls=%d output=%#v err=%v", calls, output, err)
	}
	payload := decodeRecoveryPayload(t, output.Result)
	data := payload["data"].(map[string]any)
	if data["retryable"] != false || data["execution_attempts"] != float64(1) {
		t.Fatalf("payload=%#v", payload)
	}
}

func TestToolRecoveryDoesNotRetryDeterministicSchemaErrors(t *testing.T) {
	type reportedEvent struct {
		phase string
		err   error
	}
	events := []reportedEvent{}
	ctx := rushestools.WithReporter(
		withToolRecoveryState(t.Context(), newToolRecoveryState()),
		func(_ context.Context, _ string, phase string, _, _ any, err error) {
			events = append(events, reportedEvent{phase: phase, err: err})
		},
	)
	calls := 0
	endpoint := newToolRecoveryMiddleware().Invokable(
		func(context.Context, *compose.ToolInput) (*compose.ToolOutput, error) {
			calls++
			return nil, errors.New("json: cannot unmarshal string into Go struct field only_usable of type bool")
		},
	)
	output, err := endpoint(ctx, &compose.ToolInput{
		Name: "asset.list_assets", Arguments: `{"only_usable":"yes"}`,
	})
	if err != nil || calls != 1 {
		t.Fatalf("deterministic schema error was retried: calls=%d output=%#v err=%v", calls, output, err)
	}
	payload := decodeRecoveryPayload(t, output.Result)
	data := payload["data"].(map[string]any)
	if data["retryable"] != false || data["execution_attempts"] != float64(1) ||
		data["harness_recovery"].(map[string]any)["automatic_retries"] != float64(0) {
		t.Fatalf("payload=%#v", payload)
	}
	if len(events) != 2 || events[0].phase != "started" || events[1].phase != "finished" ||
		events[1].err == nil {
		t.Fatalf("schema failure must have one visible terminal trace: events=%#v", events)
	}
}

func TestToolRecoveryPreservesStructuredBusinessFailureForModel(t *testing.T) {
	state := newToolRecoveryState()
	endpoint := newToolRecoveryMiddleware().Invokable(
		func(context.Context, *compose.ToolInput) (*compose.ToolOutput, error) {
			return &compose.ToolOutput{Result: `{
				"status":"validation_failed",
				"observation":"片段相互重叠",
				"data":{"error_code":"timeline_invalid","failed_op_index":3,"recovery":"修正第三个操作"}
			}`}, nil
		},
	)
	output, err := endpoint(
		withToolRecoveryState(t.Context(), state),
		&compose.ToolInput{Name: "timeline.apply_patches", Arguments: `{"ops":[]}`},
	)
	if err != nil {
		t.Fatal(err)
	}
	payload := decodeRecoveryPayload(t, output.Result)
	data := payload["data"].(map[string]any)
	if payload["status"] != "failed" || payload["observation"] != "片段相互重叠" ||
		data["error_code"] != "timeline_invalid" || data["failed_op_index"] != float64(3) ||
		data["recovery"] != "修正第三个操作" || data["harness_recovery"] == nil {
		t.Fatalf("structured business failure was not preserved: payload=%#v", payload)
	}
}

func TestToolRecoveryCollapsesInternalRetryReporterEvents(t *testing.T) {
	state := newToolRecoveryState()
	events := []string{}
	ctx := rushestools.WithReporter(
		withToolRecoveryState(t.Context(), state),
		func(_ context.Context, _ string, phase string, _, _ any, _ error) {
			events = append(events, phase)
		},
	)
	calls := 0
	middleware := newToolRecoveryMiddleware()
	endpoint := middleware.Invokable(func(ctx context.Context, _ *compose.ToolInput) (*compose.ToolOutput, error) {
		reporter, ok := rushestools.ReporterFromContext(ctx)
		if !ok {
			t.Fatal("missing reporter")
		}
		reporter(ctx, "asset.list_assets", "started", map[string]any{}, nil, nil)
		calls++
		err := errors.New("temporary read failure")
		reporter(ctx, "asset.list_assets", "finished", map[string]any{}, nil, err)
		return nil, err
	})
	output, err := endpoint(ctx, &compose.ToolInput{Name: "asset.list_assets", Arguments: `{}`})
	if err != nil || output == nil || calls != maxToolExecutionRetries+1 {
		t.Fatalf("calls=%d output=%#v err=%v", calls, output, err)
	}
	if len(events) != 2 || events[0] != "started" || events[1] != "finished" {
		t.Fatalf("内部重试不应展开成多条 UI 记录：events=%v", events)
	}
}

func TestToolRecoveryBlocksDuplicateFailuresAndExhaustsRepairBudget(t *testing.T) {
	state := newToolRecoveryState()
	ctx := withToolRecoveryState(t.Context(), state)
	calls := 0
	middleware := newToolRecoveryMiddleware()
	endpoint := middleware.Invokable(func(context.Context, *compose.ToolInput) (*compose.ToolOutput, error) {
		calls++
		return &compose.ToolOutput{Result: marshalToolFailure("bad clip id", map[string]any{
			"error_code": "invalid_clip",
		})}, nil
	})
	input := &compose.ToolInput{Name: "timeline.recut_to_beats", Arguments: `{"bgm_asset_id":"bad"}`}
	first, err := endpoint(ctx, input)
	if err != nil || calls != 1 || !state.unresolved() {
		t.Fatalf("first=%#v calls=%d err=%v", first, calls, err)
	}

	var last *compose.ToolOutput
	for range maxModelRepairAttempts {
		last, err = endpoint(ctx, input)
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("重复失败调用被实际执行：calls=%d", calls)
	}
	payload := decodeRecoveryPayload(t, last.Result)
	data := payload["data"].(map[string]any)
	if data["error_code"] != "tool_recovery_exhausted" {
		t.Fatalf("payload=%#v", payload)
	}
	blocked, blockErr := endpoint(ctx, &compose.ToolInput{
		Name: "timeline.inspect", Arguments: `{}`,
	})
	if blockErr != nil || calls != 1 ||
		decodeRecoveryPayload(t, blocked.Result)["observation"] == "" {
		t.Fatalf("blocked=%#v calls=%d err=%v", blocked, calls, blockErr)
	}
}

func TestToolRecoveryCanonicalizesDuplicateJSONArguments(t *testing.T) {
	state := newToolRecoveryState()
	ctx := withToolRecoveryState(t.Context(), state)
	calls := 0
	endpoint := newToolRecoveryMiddleware().Invokable(
		func(context.Context, *compose.ToolInput) (*compose.ToolOutput, error) {
			calls++
			return &compose.ToolOutput{Result: marshalToolFailure("invalid range", nil)}, nil
		},
	)
	if _, err := endpoint(ctx, &compose.ToolInput{
		Name: "timeline.apply_patches", Arguments: `{"b":2,"a":1}`,
	}); err != nil {
		t.Fatal(err)
	}
	blocked, err := endpoint(ctx, &compose.ToolInput{
		Name: "timeline.apply_patches", Arguments: `{ "a": 1, "b": 2 }`,
	})
	if err != nil || calls != 1 {
		t.Fatalf("canonical duplicate executed: calls=%d blocked=%#v err=%v", calls, blocked, err)
	}
	data := decodeRecoveryPayload(t, blocked.Result)["data"].(map[string]any)
	if data["error_code"] != "duplicate_failed_tool_call" {
		t.Fatalf("data=%#v", data)
	}
}

func TestToolRecoveryCapsDistinctModelRepairFailures(t *testing.T) {
	state := newToolRecoveryState()
	ctx := withToolRecoveryState(t.Context(), state)
	calls := 0
	endpoint := newToolRecoveryMiddleware().Invokable(
		func(context.Context, *compose.ToolInput) (*compose.ToolOutput, error) {
			calls++
			return &compose.ToolOutput{Result: marshalToolFailure("still invalid", nil)}, nil
		},
	)
	var last *compose.ToolOutput
	for attempt := 0; attempt <= maxModelRepairAttempts; attempt++ {
		var err error
		last, err = endpoint(ctx, &compose.ToolInput{
			Name: "timeline.apply_patches", Arguments: `{"attempt":` + string(rune('0'+attempt)) + `}`,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls != maxModelRepairAttempts+1 || !state.recoveryExhausted() {
		t.Fatalf("calls=%d exhausted=%v", calls, state.recoveryExhausted())
	}
	harness := decodeRecoveryPayload(t, last.Result)["data"].(map[string]any)["harness_recovery"].(map[string]any)
	if harness["exhausted"] != true || harness["remaining_model_repairs"] != float64(0) {
		t.Fatalf("harness=%#v", harness)
	}
	blocked, err := endpoint(ctx, &compose.ToolInput{
		Name: "timeline.inspect", Arguments: `{}`,
	})
	if err != nil || calls != maxModelRepairAttempts+1 ||
		decodeRecoveryPayload(t, blocked.Result)["data"].(map[string]any)["error_code"] != "tool_recovery_exhausted" {
		t.Fatalf("blocked=%#v calls=%d err=%v", blocked, calls, err)
	}
}

func TestToolRecoveryLetsModelCorrectArguments(t *testing.T) {
	state := newToolRecoveryState()
	ctx := withToolRecoveryState(t.Context(), state)
	calls := 0
	middleware := newToolRecoveryMiddleware()
	endpoint := middleware.Invokable(func(_ context.Context, input *compose.ToolInput) (*compose.ToolOutput, error) {
		calls++
		if input.Arguments == `{"value":"good"}` {
			return &compose.ToolOutput{Result: `{"status":"succeeded","observation":"fixed"}`}, nil
		}
		return &compose.ToolOutput{Result: marshalToolFailure("change value", nil)}, nil
	})
	if _, err := endpoint(ctx, &compose.ToolInput{Name: "timeline.recut_to_beats", Arguments: `{"value":"bad"}`}); err != nil {
		t.Fatal(err)
	}
	corrected, err := endpoint(ctx, &compose.ToolInput{Name: "timeline.recut_to_beats", Arguments: `{"value":"good"}`})
	if err != nil || calls != 2 || state.unresolved() || !json.Valid([]byte(corrected.Result)) {
		t.Fatalf("corrected=%#v calls=%d unresolved=%v err=%v", corrected, calls, state.unresolved(), err)
	}
}

func TestToolRecoverySuccessOnAnotherToolStartsFreshFailureChain(t *testing.T) {
	state := newToolRecoveryState()
	ctx := withToolRecoveryState(t.Context(), state)
	calls := 0
	endpoint := newToolRecoveryMiddleware().Invokable(
		func(_ context.Context, input *compose.ToolInput) (*compose.ToolOutput, error) {
			calls++
			if input.Name == "asset.list_assets" {
				return &compose.ToolOutput{Result: `{"status":"succeeded","observation":"fresh state"}`}, nil
			}
			return &compose.ToolOutput{Result: marshalToolFailure("not ready", nil)}, nil
		},
	)
	failedInput := &compose.ToolInput{Name: "timeline.inspect", Arguments: `{}`}
	if _, err := endpoint(ctx, failedInput); err != nil || !state.unresolved() {
		t.Fatalf("initial failure err=%v unresolved=%v", err, state.unresolved())
	}
	if _, err := endpoint(ctx, &compose.ToolInput{Name: "asset.list_assets", Arguments: `{}`}); err != nil || state.unresolved() {
		t.Fatalf("recovery step err=%v unresolved=%v", err, state.unresolved())
	}
	if _, err := endpoint(ctx, failedInput); err != nil || calls != 3 || !state.unresolved() {
		t.Fatalf("fresh failure err=%v calls=%d unresolved=%v", err, calls, state.unresolved())
	}
}

func TestUnknownToolBecomesRepairableToolResult(t *testing.T) {
	state := newToolRecoveryState()
	events := []string{}
	ctx := rushestools.WithReporter(
		withToolRecoveryState(t.Context(), state),
		func(_ context.Context, _ string, phase string, _, _ any, _ error) { events = append(events, phase) },
	)
	output, err := unknownToolRecoveryHandler(
		ctx, "timeline.magic", `{}`,
	)
	if err != nil || !state.unresolved() {
		t.Fatalf("output=%s err=%v", output, err)
	}
	payload := decodeRecoveryPayload(t, output)
	if payload["status"] != "failed" ||
		payload["data"].(map[string]any)["error_code"] != "unknown_tool" {
		t.Fatalf("payload=%#v", payload)
	}
	if len(events) != 2 || events[0] != "started" || events[1] != "finished" {
		t.Fatalf("unknown tool trace=%v", events)
	}
	blocked, err := unknownToolRecoveryHandler(ctx, "timeline.magic", `{}`)
	if err != nil || decodeRecoveryPayload(t, blocked)["data"].(map[string]any)["error_code"] != "duplicate_failed_tool_call" ||
		len(events) != 2 {
		t.Fatalf("duplicate unknown tool should be hidden from UI: output=%s events=%v err=%v", blocked, events, err)
	}
}

func TestToolRecoveryPropagatesCancellation(t *testing.T) {
	middleware := newToolRecoveryMiddleware()
	endpoint := middleware.Invokable(func(context.Context, *compose.ToolInput) (*compose.ToolOutput, error) {
		return nil, context.Canceled
	})
	if _, err := endpoint(t.Context(), &compose.ToolInput{Name: "asset.list_assets", Arguments: `{}`}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func decodeRecoveryPayload(t *testing.T, raw string) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("invalid payload=%q err=%v", raw, err)
	}
	return payload
}
