package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/cloudwego/eino/compose"
	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

type effectProbeExecutor struct{}

const (
	independentFailureRetryCount = 5
	alternatingFailureProbeCount = 10
)

func (effectProbeExecutor) ExecuteTool(context.Context, string, any) (any, error) {
	return nil, nil
}

// testRetrySafe 用真实注册表的 Effect 分级构造重试白名单，供下面的恢复机制单测复用（避免重新
// 硬编码工具名）；DB 随测试生命周期用 t.TempDir()+Cleanup 回收。分类正确性另由 tools 包
// TestToolEffectClassificationTable 保证。
func testRetrySafe(t *testing.T) func(string) bool {
	t.Helper()
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	registry, err := rushestools.NewRegistry(database, effectProbeExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	return retrySafeFromEffect(registry.Effect)
}

func successfulTimelineCheckResult(timelineID, observation string) string {
	return `{"status":"succeeded","observation":"` + observation + `","data":{"timeline_id":"` + timelineID +
		`","validation_report":{"valid":true,"structural_valid":true,"content_contract_valid":true,"checks":["schema"],"issues":[]}}}`
}

func successfulTimelineCheckData(timelineID string) map[string]any {
	return map[string]any{
		"timeline_id": timelineID,
		"validation_report": map[string]any{
			"valid": true, "structural_valid": true, "content_contract_valid": true,
			"checks": []any{"schema"}, "issues": []any{},
		},
	}
}

func TestRetrySafeFromEffectAllowlist(t *testing.T) {
	t.Parallel()
	retrySafe := testRetrySafe(t)
	for _, name := range []string{
		"asset.list_assets", "shot.search", "speech.search", "timeline.inspect",
		"timeline.check", "preview.check",
	} {
		if !retrySafe(name) {
			t.Fatalf("%s 应为重试安全", name)
		}
	}
	for _, name := range []string{
		"media.detect_shots", "speech.transcribe", "audio.analyze_beats",
		"audio.analyze_speech_pauses", "plan.update", "memory.set", "memory.remove",
		"interaction.ask_user", "interaction.confirm_action", "decision.answer",
		"asset.import_local_file", "unknown.tool",
	} {
		if retrySafe(name) {
			t.Fatalf("%s 不应为重试安全", name)
		}
	}
}

func TestToolRecoveryDoesNotRetrySpeechTranscribe(t *testing.T) {
	for _, arguments := range []string{
		`{"asset_id":"asset_a"}`,
		`{"asset_id":"asset_a","force_refresh":true}`,
	} {
		t.Run(arguments, func(t *testing.T) {
			calls := 0
			middleware := newToolRecoveryMiddleware(testRetrySafe(t))
			endpoint := middleware.Invokable(func(context.Context, *compose.ToolInput) (*compose.ToolOutput, error) {
				calls++
				return nil, errors.New("temporary commit result unknown")
			})
			output, err := endpoint(t.Context(), &compose.ToolInput{
				Name: "speech.transcribe", Arguments: arguments,
			})
			if err != nil || calls != 1 {
				t.Fatalf("calls=%d output=%#v err=%v", calls, output, err)
			}
			payload := decodeRecoveryPayload(t, output.Result)
			data := payload["data"].(map[string]any)
			if data["retryable"] != false || data["execution_attempts"] != float64(1) {
				t.Fatalf("payload=%#v", payload)
			}
		})
	}
}

func TestToolRecoveryRetriesSafeErrorsAndReturnsThemToModel(t *testing.T) {
	state := newToolRecoveryState()
	ctx := withToolRecoveryState(t.Context(), state)
	calls := 0
	middleware := newToolRecoveryMiddleware(testRetrySafe(t))
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
	if data["automatic_retries"] != float64(5) || state.unresolved() ||
		payload["error_code"] != "tool_execution_error" || payload["message"] == "" ||
		payload["recovery"] == "" || payload["invalid_fields"] == nil || payload["current_state"] == nil {
		t.Fatalf("独立失败信封不完整: payload=%#v state=%#v", payload, state)
	}
}

func TestToolRecoveryFailsClosedOnNonTerminalModelToolResult(t *testing.T) {
	for _, test := range []struct {
		status       string
		metricChange int64
	}{
		{status: "queued", metricChange: 1},
		{status: "running", metricChange: 1},
		{status: "completed"},
		{status: "mystery"},
	} {
		t.Run(test.status, func(t *testing.T) {
			before := metricLLMNonTerminalToolResult.Value()
			endpoint := newToolRecoveryMiddleware(testRetrySafe(t)).Invokable(
				func(context.Context, *compose.ToolInput) (*compose.ToolOutput, error) {
					return &compose.ToolOutput{Result: `{"status":"` + test.status + `","data":{"job_id":"job_1"}}`}, nil
				},
			)
			output, err := endpoint(
				rushestools.WithDraftID(t.Context(), "draft"),
				&compose.ToolInput{Name: "preview.generate", Arguments: `{"timeline_id":"draft:v1"}`},
			)
			if err != nil {
				t.Fatal(err)
			}
			payload := decodeRecoveryPayload(t, output.Result)
			data := payload["data"].(map[string]any)
			if payload["status"] != string(rushestools.StatusFailed) ||
				data["error_code"] != string(rushestools.ErrCodeToolExecutionError) ||
				data["returned_status"] != test.status {
				t.Fatalf("payload=%#v", payload)
			}
			if metricLLMNonTerminalToolResult.Value() != before+test.metricChange {
				t.Fatalf("non-terminal metric=%d want=%d", metricLLMNonTerminalToolResult.Value(), before+test.metricChange)
			}
		})
	}
}

func TestToolRecoveryAddsSucceededReceiptToTypedReadResult(t *testing.T) {
	endpoint := newToolRecoveryMiddleware(testRetrySafe(t)).Invokable(
		func(context.Context, *compose.ToolInput) (*compose.ToolOutput, error) {
			return &compose.ToolOutput{Result: `{"draft_id":"draft","assets":[],"total":0}`}, nil
		},
	)
	output, err := endpoint(
		rushestools.WithDraftID(t.Context(), "draft"),
		&compose.ToolInput{Name: "asset.list_assets", Arguments: `{}`},
	)
	if err != nil {
		t.Fatal(err)
	}
	payload := decodeRecoveryPayload(t, output.Result)
	if payload["status"] != string(rushestools.StatusSucceeded) ||
		payload[toolRequestFingerprintField] == "" {
		t.Fatalf("typed model receipt=%#v", payload)
	}
}

func TestToolRecoveryShotSearchRoleProofMatchesExecutorSemantics(t *testing.T) {
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	registry, err := rushestools.NewRegistry(database, effectProbeExecutor{})
	if err != nil {
		t.Fatal(err)
	}

	const arguments = `{"semantic_roles":["b_roll"]}`
	for _, test := range []struct {
		name         string
		semanticRole string
		wantSuccess  bool
	}{
		{name: "empty role is unknown", semanticRole: "", wantSuccess: true},
		{name: "legacy visual role is unknown", semanticRole: "visual", wantSuccess: true},
		{name: "matching explicit role", semanticRole: "b_roll", wantSuccess: true},
		{name: "opposite explicit role", semanticRole: "a_roll", wantSuccess: false},
		{name: "opposite explicit alias", semanticRole: "a-roll", wantSuccess: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, err := json.Marshal(rushestools.ShotSearchResult{
				Status: string(rushestools.StatusSucceeded), IndexSnapshotID: "snapshot_role",
				SynonymVersion: "v1", FrozenAssetIDs: []string{"asset_A"}, SearchReady: true,
				Shots: []rushestools.ShotCandidate{{
					IndexSnapshotID: "snapshot_role", ShotID: "shot_1", AssetID: "asset_A",
					SourceStartFrame: 0, SourceEndFrame: 30, DurationFrames: 30,
					BoundaryVersion: 1, SemanticRole: test.semanticRole,
				}},
				TotalMatches: 1, ReturnedCandidates: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			endpoint := newToolRecoveryMiddleware(
				retrySafeFromEffect(registry.Effect), registry.ModelReceiptPolicy,
			).Invokable(func(context.Context, *compose.ToolInput) (*compose.ToolOutput, error) {
				calls++
				return &compose.ToolOutput{Result: string(raw)}, nil
			})
			output, err := endpoint(
				rushestools.WithDraftID(t.Context(), "draft"),
				&compose.ToolInput{Name: "shot.search", Arguments: arguments},
			)
			if err != nil || output == nil || calls != 1 {
				t.Fatalf("calls=%d output=%#v err=%v", calls, output, err)
			}
			payload := decodeRecoveryPayload(t, output.Result)
			gotSuccess := payload["status"] == string(rushestools.StatusSucceeded)
			if gotSuccess != test.wantSuccess {
				t.Fatalf("role=%q payload=%#v want_success=%v", test.semanticRole, payload, test.wantSuccess)
			}
			if test.wantSuccess && !validToolRequestFingerprint("shot.search", arguments, output.Result) {
				t.Fatalf("role=%q succeeded receipt 缺少完整请求 proof: %s", test.semanticRole, output.Result)
			}
			if !test.wantSuccess && !isStructuredToolFailure(output.Result) {
				t.Fatalf("role=%q 相反角色未 fail-closed: %s", test.semanticRole, output.Result)
			}
		})
	}
}

func TestToolRecoveryMissingStatusFailsClosedForExplicitReceipts(t *testing.T) {
	for _, name := range []string{"plan.update", "decision.answer"} {
		t.Run(name, func(t *testing.T) {
			endpoint := newToolRecoveryMiddleware(testRetrySafe(t)).Invokable(
				func(context.Context, *compose.ToolInput) (*compose.ToolOutput, error) {
					return &compose.ToolOutput{Result: `{"error":"adapter boom"}`}, nil
				},
			)
			output, err := endpoint(
				rushestools.WithDraftID(t.Context(), "draft"),
				&compose.ToolInput{Name: name, Arguments: `{}`},
			)
			if err != nil {
				t.Fatal(err)
			}
			payload := decodeRecoveryPayload(t, output.Result)
			data := payload["data"].(map[string]any)
			if payload["status"] != string(rushestools.StatusFailed) ||
				data["error_code"] != string(rushestools.ErrCodeToolExecutionError) ||
				data["returned_status"] != "missing" {
				t.Fatalf("missing-status adapter failure was accepted: %#v", payload)
			}
		})
	}
}

func TestToolRecoverySuppliesStableCodeForEveryFailureStatus(t *testing.T) {
	tests := []struct {
		status string
		code   rushestools.ToolErrorCode
	}{
		{status: "failed", code: rushestools.ErrCodeToolExecutionError},
		{status: "validation_failed", code: rushestools.ErrCodeToolValidationFailed},
		{status: "cancelled", code: rushestools.ErrCodeToolCancelled},
		{status: "timeout", code: rushestools.ErrCodeToolTimeout},
	}
	for _, test := range tests {
		t.Run(test.status, func(t *testing.T) {
			endpoint := newToolRecoveryMiddleware(testRetrySafe(t)).Invokable(
				func(context.Context, *compose.ToolInput) (*compose.ToolOutput, error) {
					return &compose.ToolOutput{Result: `{"status":"` + test.status + `"}`}, nil
				},
			)
			output, err := endpoint(t.Context(), &compose.ToolInput{
				Name: "timeline.update", Arguments: `{"kind":"adjust_gain"}`,
			})
			if err != nil {
				t.Fatal(err)
			}
			payload := decodeRecoveryPayload(t, output.Result)
			data := payload["data"].(map[string]any)
			if payload["status"] != test.status || data["error_code"] != string(test.code) {
				t.Fatalf("payload=%#v", payload)
			}
		})
	}
}

func TestToolRecoveryRejectsModelMutationWithoutDurableReceiptIdentity(t *testing.T) {
	tests := []struct {
		name, turnID, sourceMessageID, callID string
	}{
		{name: "missing call id", turnID: "turn-receipt", sourceMessageID: "message-receipt"},
		{name: "missing source message", turnID: "turn-receipt", callID: "call-receipt"},
		{name: "missing turn", sourceMessageID: "message-receipt", callID: "call-receipt"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			endpoint := newToolRecoveryMiddleware(testRetrySafe(t)).Invokable(
				func(context.Context, *compose.ToolInput) (*compose.ToolOutput, error) {
					calls++
					return &compose.ToolOutput{Result: `{"status":"succeeded","data":{"timeline_id":"draft:v2"}}`}, nil
				},
			)
			ctx := rushestools.WithDraftID(t.Context(), "draft")
			ctx = rushestools.WithTurnIdentity(ctx, test.turnID, test.sourceMessageID)
			output, err := endpoint(ctx, &compose.ToolInput{
				Name: "timeline.update", CallID: test.callID,
				Arguments: `{"kind":"adjust_gain","timeline_clip_id":"clip-1","gain_db":-6}`,
			})
			if err != nil || output == nil || calls != 0 {
				t.Fatalf("calls=%d output=%#v err=%v", calls, output, err)
			}
			payload := decodeRecoveryPayload(t, output.Result)
			data := payload["data"].(map[string]any)
			if payload["status"] != string(rushestools.StatusFailed) ||
				data["error_code"] != string(rushestools.ErrCodeToolExecutionError) ||
				!strings.Contains(payload["observation"].(string), "调用身份") {
				t.Fatalf("payload=%#v", payload)
			}
		})
	}

	// Isolated harness calls have no model turn identity and remain governed by
	// the direct executor/admission tests instead of this model-only fence.
	if modelMutationReceiptIdentityMissing(t.Context(), &compose.ToolInput{
		Name: "timeline.update",
	}) {
		t.Fatal("non-model harness call was classified as a model receipt violation")
	}
	if modelMutationReceiptIdentityMissing(
		rushestools.WithTurnIdentity(t.Context(), "turn", "message"),
		&compose.ToolInput{Name: "timeline.inspect"},
	) {
		t.Fatal("read-only tool was classified as a mutation receipt violation")
	}
	if modelMutationReceiptIdentityMissing(t.Context(), nil) {
		t.Fatal("nil input was classified as a mutation receipt violation")
	}
}

func TestToolRecoveryObservesOversizeResultWithoutTruncating(t *testing.T) {
	largeResult := `{"status":"succeeded","data":{"timeline_id":"draft:v2","validation_report":{"valid":true,"structural_valid":true,"content_contract_valid":true,"checks":["schema"],"issues":[]}},"observation":"` +
		strings.Repeat("中", toolResultSoftLimitBytes) + `"}`
	oversizeBefore := metricToolResultOversize.Value()
	countBefore, _, _, _ := metricToolResultBytes.Snapshot()
	middleware := newToolRecoveryMiddleware(testRetrySafe(t))
	endpoint := middleware.Invokable(func(context.Context, *compose.ToolInput) (*compose.ToolOutput, error) {
		return &compose.ToolOutput{Result: largeResult}, nil
	})
	output, err := endpoint(
		rushestools.WithDraftID(t.Context(), "draft"),
		&compose.ToolInput{Name: "timeline.check", Arguments: `{}`},
	)
	if err != nil {
		t.Fatal(err)
	}
	if output == nil || !strings.Contains(output.Result, strings.Repeat("中", toolResultSoftLimitBytes)) ||
		!validToolRequestFingerprint("timeline.check", `{}`, output.Result) {
		t.Fatalf("超限结果正文丢失或缺少请求 proof: got_bytes=%d want_at_least=%d", len(output.Result), len(largeResult))
	}
	if metricToolResultOversize.Value() != oversizeBefore+1 {
		t.Fatalf("oversize=%d want=%d", metricToolResultOversize.Value(), oversizeBefore+1)
	}
	countAfter, _, _, _ := metricToolResultBytes.Snapshot()
	if countAfter != countBefore+1 {
		t.Fatalf("result histogram count=%d want=%d", countAfter, countBefore+1)
	}
}

func TestTruncateTextUsesRuneBoundaries(t *testing.T) {
	got := agentexec.TruncateText(" 你好世界 ", 3)
	if got != "你好世…" || !utf8.ValidString(got) || strings.ContainsRune(got, utf8.RuneError) {
		t.Fatalf("rune-safe truncate=%q valid=%v", got, utf8.ValidString(got))
	}
}

func TestToolRecoveryFormattingHelpersCoverMalformedValues(t *testing.T) {
	t.Parallel()
	direct := map[string]any{"x": 1}
	if got := toolArgumentsObject(direct); got["x"] != 1 {
		t.Fatalf("direct arguments=%#v", got)
	}
	if toolArgumentsObject("not-json") != nil || toolArgumentsObject(make(chan int)) != nil ||
		toolArgumentsObject([]int{1}) != nil {
		t.Fatal("malformed argument objects should be rejected")
	}
	type argumentsFixture struct {
		Value int `json:"value"`
	}
	if got := toolArgumentsObject(argumentsFixture{Value: 7}); got["value"] != float64(7) {
		t.Fatalf("encoded arguments=%#v", got)
	}
	for value, want := range map[any]int64{
		int(1): 1, int32(2): 2, int64(3): 3, float64(4): 4, float64(4.5): 0, "5": 0,
	} {
		if got := positiveInteger(value); got != want {
			t.Fatalf("positiveInteger(%#v)=%d, want %d", value, got, want)
		}
	}
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
	if agentexec.TruncateText(" abc ", 0) != "abc" || agentexec.TruncateText("abcdef", 3) != "abc…" {
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

func TestToolRecoveryUnknownOrMalformedStatusDoesNotResolveFailure(t *testing.T) {
	for name, result := range map[string]string{
		"unknown_status": `{"status":"mystery","observation":"not verified"}`,
		"malformed":      `not-json`,
	} {
		t.Run(name, func(t *testing.T) {
			state := newToolRecoveryState()
			ctx := withToolRecoveryState(t.Context(), state)
			endpoint := newToolRecoveryMiddleware(testRetrySafe(t)).Invokable(
				func(_ context.Context, input *compose.ToolInput) (*compose.ToolOutput, error) {
					if strings.Contains(input.Arguments, `"kind":"trim_clip"`) {
						return &compose.ToolOutput{Result: marshalToolFailure("kind invalid", nil)}, nil
					}
					return &compose.ToolOutput{Result: result}, nil
				},
			)
			if _, err := endpoint(ctx, &compose.ToolInput{
				Name: "timeline.update", Arguments: `{"kind":"trim_clip","timeline_clip_id":"clip_1"}`,
			}); err != nil || state.unresolved() {
				t.Fatalf("initial failure err=%v unresolved=%v", err, state.unresolved())
			}
			output, err := endpoint(ctx, &compose.ToolInput{
				Name: "timeline.update", Arguments: `{"kind":"trim_clip_edge","timeline_clip_id":"clip_1"}`,
			})
			if err != nil || state.unresolved() || !isStructuredToolFailure(output.Result) {
				t.Fatalf("output=%#v err=%v unresolved=%v", output, err, state.unresolved())
			}
		})
	}
}

func TestConfirmedToolRecoverySuccessRequiresToolSpecificStatus(t *testing.T) {
	tests := []struct {
		name, tool, arguments, result string
		want                          bool
	}{
		{"check success with version proof", "timeline.check", `{}`, successfulTimelineCheckResult("draft:v2", "valid"), true},
		{"check success without contract accepts empty failures", "timeline.check", `{}`, `{"status":"succeeded","data":{"timeline_id":"draft:v2","validation_report":{"valid":true,"structural_valid":true,"content_contract_valid":true,"checks":["schema"],"issues":[]},"contract_failures":[]}}`, true},
		{"check success missing version proof", "timeline.check", `{}`, `{"status":"succeeded"}`, false},
		{"check success malformed version proof", "timeline.check", `{}`, `{"status":"succeeded","data":{"timeline_id":"draft"}}`, false},
		{"check success contradictory report", "timeline.check", `{}`, `{"status":"succeeded","data":{"timeline_id":"draft:v2","validation_report":{"valid":false,"structural_valid":true,"content_contract_valid":true,"checks":[],"issues":[]}}}`, false},
		{"check success rejects null checks", "timeline.check", `{}`, `{"status":"succeeded","data":{"timeline_id":"draft:v2","validation_report":{"valid":true,"structural_valid":true,"content_contract_valid":true,"checks":null,"issues":[]}}}`, false},
		{"check success rejects structural issues", "timeline.check", `{}`, `{"status":"succeeded","data":{"timeline_id":"draft:v2","validation_report":{"valid":true,"structural_valid":true,"content_contract_valid":true,"checks":["schema"],"issues":[{"code":"invalid_document","message":"invalid"}]}}}`, false},
		{"check success rejects blank check", "timeline.check", `{}`, `{"status":"succeeded","data":{"timeline_id":"draft:v2","validation_report":{"valid":true,"structural_valid":true,"content_contract_valid":true,"checks":[" "],"issues":[]}}}`, false},
		{"check success rejects unexpected top-level contract", "timeline.check", `{}`, `{"status":"succeeded","data":{"timeline_id":"draft:v2","validation_report":{"valid":true,"structural_valid":true,"content_contract_valid":true,"checks":[],"issues":[]},"content_contract":{"pass":false,"items":[{"pass":false}]},"contract_failures":[{"pass":false}]}}`, false},
		{"check success accepts consistent empty contract", "timeline.check", `{}`, `{"status":"succeeded","data":{"timeline_id":"draft:v2","validation_report":{"valid":true,"structural_valid":true,"content_contract_valid":true,"checks":["schema"],"issues":[],"content_contract":{"pass":true,"items":[]}},"content_contract":{"pass":true,"items":[]},"contract_failures":[]}}`, true},
		{"check success accepts consistent passing contract", "timeline.check", `{}`, `{"status":"succeeded","data":{"timeline_id":"draft:v2","validation_report":{"valid":true,"structural_valid":true,"content_contract_valid":true,"checks":["schema"],"issues":[],"content_contract":{"pass":true,"items":[{"check":"target_duration","pass":true,"message":"duration matches"}]}},"content_contract":{"pass":true,"items":[{"check":"target_duration","pass":true,"message":"duration matches"}]},"contract_failures":[]}}`, true},
		{"check success rejects passing contract item with error code", "timeline.check", `{}`, `{"status":"succeeded","data":{"timeline_id":"draft:v2","validation_report":{"valid":true,"structural_valid":true,"content_contract_valid":true,"checks":["schema"],"issues":[],"content_contract":{"pass":true,"items":[{"check":"on_beat_ratio","pass":true,"error_code":"missing_beat_grid","message":"ok"}]}},"content_contract":{"pass":true,"items":[{"check":"on_beat_ratio","pass":true,"error_code":"missing_beat_grid","message":"ok"}]},"contract_failures":[]}}`, false},
		{"check success rejects nil contract items", "timeline.check", `{}`, `{"status":"succeeded","data":{"timeline_id":"draft:v2","validation_report":{"valid":true,"structural_valid":true,"content_contract_valid":true,"checks":["schema"],"issues":[],"content_contract":{"pass":true}},"content_contract":{"pass":true},"contract_failures":[]}}`, false},
		{"check success rejects incomplete contract item", "timeline.check", `{}`, `{"status":"succeeded","data":{"timeline_id":"draft:v2","validation_report":{"valid":true,"structural_valid":true,"content_contract_valid":true,"checks":["schema"],"issues":[],"content_contract":{"pass":true,"items":[{"pass":true}]}},"content_contract":{"pass":true,"items":[{"pass":true}]},"contract_failures":[]}}`, false},
		{"check success rejects duplicate contract checks", "timeline.check", `{}`, `{"status":"succeeded","data":{"timeline_id":"draft:v2","validation_report":{"valid":true,"structural_valid":true,"content_contract_valid":true,"checks":["schema"],"issues":[],"content_contract":{"pass":true,"items":[{"check":"duration","pass":true,"message":"one"},{"check":"duration","pass":true,"message":"two"}]}},"content_contract":{"pass":true,"items":[{"check":"duration","pass":true,"message":"one"},{"check":"duration","pass":true,"message":"two"}]},"contract_failures":[]}}`, false},
		{"check success rejects null-normalized contract field", "timeline.check", `{}`, `{"status":"succeeded","data":{"timeline_id":"draft:v2","validation_report":{"valid":true,"structural_valid":true,"content_contract_valid":true,"checks":["schema"],"issues":[],"content_contract":{"pass":true,"items":[{"check":"duration","pass":true,"error_code":null,"message":"ok"}]}},"content_contract":{"pass":true,"items":[{"check":"duration","pass":true,"message":"ok"}]},"contract_failures":[]}}`, false},
		{"check success contract requires failure list", "timeline.check", `{}`, `{"status":"succeeded","data":{"timeline_id":"draft:v2","validation_report":{"valid":true,"structural_valid":true,"content_contract_valid":true,"checks":[],"issues":[],"content_contract":{"pass":true,"items":[{"pass":true}]}},"content_contract":{"pass":true,"items":[{"pass":true}]}}}`, false},
		{"check success rejects inconsistent contract copies", "timeline.check", `{}`, `{"status":"succeeded","data":{"timeline_id":"draft:v2","validation_report":{"valid":true,"structural_valid":true,"content_contract_valid":true,"checks":[],"issues":[],"content_contract":{"pass":true,"items":[{"pass":true}]}},"content_contract":{"pass":false,"items":[{"pass":false}]},"contract_failures":[]}}`, false},
		{"check success missing report", "timeline.check", `{}`, `{"status":"succeeded","data":{"timeline_id":"draft:v2"}}`, false},
		{"timeline mutation with version proof", "timeline.update", `{}`, `{"status":"succeeded","data":{"timeline_id":"draft:v2"}}`, true},
		{"timeline mutation missing version proof", "timeline.update", `{}`, `{"status":"succeeded"}`, false},
		{"timeline mutation malformed version proof", "timeline.update", `{}`, `{"status":"succeeded","data":{"timeline_id":"draft"}}`, false},
		{"memory set exact keys", "memory.set", `{"entries":[{"key":"kept"}]}`, `{"status":"succeeded","data":{"written_keys":["kept"]}}`, true},
		{"memory remove idempotent subset", "memory.remove", `{"keys":["existing","missing"]}`, `{"status":"succeeded","data":{"removed_keys":["existing"]}}`, true},
		{"memory remove empty idempotent subset", "memory.remove", `{"keys":["missing"]}`, `{"status":"succeeded","data":{"removed_keys":[]}}`, true},
		{"memory remove real output shape", "memory.remove", `{"keys":["existing","missing"]}`, `{"status":"succeeded","data":{"removed_keys":["existing"],"written_keys":[],"evicted_keys":[],"total":0}}`, true},
		{"memory remove rejects writes", "memory.remove", `{"keys":["missing"]}`, `{"status":"succeeded","data":{"removed_keys":[],"written_keys":["other"],"evicted_keys":[]}}`, false},
		{"memory remove rejects evictions", "memory.remove", `{"keys":["missing"]}`, `{"status":"succeeded","data":{"removed_keys":[],"written_keys":[],"evicted_keys":["important"]}}`, false},
		{"memory remove rejects null side-effect proof", "memory.remove", `{"keys":["missing"]}`, `{"status":"succeeded","data":{"removed_keys":[],"written_keys":null,"evicted_keys":[]}}`, false},
		{"memory remove outside requested keys", "memory.remove", `{"keys":["existing"]}`, `{"status":"succeeded","data":{"removed_keys":["other"]}}`, false},
		{"memory remove duplicate proof keys", "memory.remove", `{"keys":["existing"]}`, `{"status":"succeeded","data":{"removed_keys":["existing","existing"]}}`, false},
		{"typed result cannot use generic success", "shot.search", `{}`, `{"status":"succeeded"}`, false},
		{"preview succeeded", "preview.generate", `{"timeline_id":"draft:v2","orientation":"portrait"}`, `{"status":"succeeded","data":{"preview_id":"preview_1","job_id":"job_1","job_status":"succeeded","timeline_id":"draft:v2","timeline_version":2,"orientation":"portrait"}}`, true},
		{"preview wrong request draft", "preview.generate", `{"timeline_id":"draft_A:v2"}`, `{"status":"succeeded","data":{"preview_id":"preview_1","job_id":"job_1","job_status":"succeeded","timeline_id":"draft_B:v2","timeline_version":2,"orientation":"auto"}}`, false},
		{"preview missing preview id", "preview.generate", `{"timeline_id":"draft:v2"}`, `{"status":"succeeded","data":{"job_id":"job_1","job_status":"succeeded","timeline_id":"draft:v2","timeline_version":2,"orientation":"auto"}}`, false},
		{"preview inconsistent job status", "preview.generate", `{"timeline_id":"draft:v2"}`, `{"status":"succeeded","data":{"preview_id":"preview_1","job_id":"job_1","job_status":"running","timeline_id":"draft:v2","timeline_version":2,"orientation":"auto"}}`, false},
		{"preview wrong orientation", "preview.generate", `{"timeline_id":"draft:v2","orientation":"portrait"}`, `{"status":"succeeded","data":{"preview_id":"preview_1","job_id":"job_1","job_status":"succeeded","timeline_id":"draft:v2","timeline_version":2,"orientation":"landscape"}}`, false},
		{"other queued", "timeline.check", `{}`, `{"status":"queued"}`, false},
		{"interaction waiting", "interaction.ask_user", `{}`, `{"status":"waiting_user","data":{"decision_id":"decision_1","turn_should_end":true}}`, true},
		{"interaction waiting missing decision", "interaction.ask_user", `{}`, `{"status":"waiting_user","data":{"turn_should_end":true}}`, false},
		{"other waiting", "timeline.check", `{}`, `{"status":"waiting_user"}`, false},
		{"understanding queued", "media.detect_shots", `{"asset_id":"asset"}`, `{"draft_id":"draft","job_id":"job_1","asset_id":"asset","status":"queued"}`, false},
		{"understanding queued missing job", "media.detect_shots", `{"asset_id":"asset"}`, `{"draft_id":"draft","asset_id":"asset","status":"queued"}`, false},
		{"understanding queued wrong asset", "media.detect_shots", `{"asset_id":"asset_A"}`, `{"draft_id":"draft","job_id":"job_1","asset_id":"asset_B","status":"queued"}`, false},
		{"understanding succeeded", "media.detect_shots", `{"asset_id":"asset"}`, `{"draft_id":"draft","asset_id":"asset","status":"succeeded","summary":{"asset_id":"asset","timeline_fps":30}}`, true},
		{"understanding succeeded wrong summary", "media.detect_shots", `{"asset_id":"asset"}`, `{"draft_id":"draft","asset_id":"asset","status":"succeeded","summary":{"asset_id":"other","timeline_fps":30}}`, false},
		{"understanding generic success", "media.detect_shots", `{}`, `{"status":"succeeded"}`, false},
		{"typed read result missing snapshot proof", "shot.search", `{}`, `{"status":"succeeded","shots":[],"total_matches":0}`, false},
		{"shot search rejects missing shots", "shot.search", `{}`, `{"status":"succeeded","index_snapshot_id":"snap","synonym_version":"v1","frozen_asset_ids":["asset_A"],"search_ready":true,"total_matches":0,"returned_candidates":0,"truncated":false}`, false},
		{"real empty frozen snapshot result", "shot.search", `{}`, `{"status":"succeeded","index_snapshot_id":"snap","synonym_version":"v1","frozen_asset_ids":["asset_A"],"search_ready":true,"shots":[],"total_matches":0,"returned_candidates":0,"truncated":false}`, true},
		{"shot search rejects null shots", "shot.search", `{}`, `{"status":"succeeded","index_snapshot_id":"snap","synonym_version":"v1","frozen_asset_ids":["asset_A"],"search_ready":true,"shots":null,"total_matches":0,"returned_candidates":0,"truncated":false}`, false},
		{"shot search rejects non-array shots", "shot.search", `{}`, `{"status":"succeeded","index_snapshot_id":"snap","synonym_version":"v1","frozen_asset_ids":["asset_A"],"search_ready":true,"shots":{},"total_matches":0,"returned_candidates":0,"truncated":false}`, false},
		{"typed read count exceeds total", "shot.search", `{}`, `{"status":"succeeded","index_snapshot_id":"snap","synonym_version":"v1","frozen_asset_ids":["asset_A"],"search_ready":true,"shots":[{}],"total_matches":0,"returned_candidates":1,"truncated":false}`, false},
		{"shot search query match", "shot.search", `{"query":"cat","asset_ids":["asset_A"]}`, `{"status":"succeeded","query":"cat","index_snapshot_id":"snap","synonym_version":"v1","frozen_asset_ids":["asset_A"],"search_ready":true,"shots":[{"index_snapshot_id":"snap","shot_id":"shot_1","asset_id":"asset_A","source_start_frame":0,"source_end_frame":30,"duration_frames":30,"boundary_version":1,"score":0.8}],"total_matches":1,"returned_candidates":1,"truncated":false}`, true},
		{"shot search wrong query", "shot.search", `{"query":"cat"}`, `{"status":"succeeded","query":"dog","index_snapshot_id":"snap","synonym_version":"v1","frozen_asset_ids":["asset_A"],"search_ready":true,"shots":[],"total_matches":0,"returned_candidates":0,"truncated":false}`, false},
		{"shot search invalid entity", "shot.search", `{"query":"cat"}`, `{"status":"succeeded","query":"cat","index_snapshot_id":"snap","synonym_version":"v1","frozen_asset_ids":["asset_A"],"search_ready":true,"shots":[{}],"total_matches":1,"returned_candidates":1,"truncated":false}`, false},
		{"shot search outside asset filter", "shot.search", `{"asset_ids":["asset_A"]}`, `{"status":"succeeded","index_snapshot_id":"snap","synonym_version":"v1","frozen_asset_ids":["asset_B"],"search_ready":true,"shots":[],"total_matches":0,"returned_candidates":0,"truncated":false}`, false},
		{"shot search top k truncation", "shot.search", `{"top_k":1}`, `{"status":"succeeded","index_snapshot_id":"snap","synonym_version":"v1","frozen_asset_ids":["asset_A"],"search_ready":true,"shots":[{"index_snapshot_id":"snap","shot_id":"shot_1","asset_id":"asset_A","source_start_frame":0,"source_end_frame":30,"duration_frames":30,"boundary_version":1,"score":0.8}],"total_matches":2,"returned_candidates":1,"truncated":true}`, true},
		{"shot search wrong candidate snapshot", "shot.search", `{}`, `{"status":"succeeded","index_snapshot_id":"snap","synonym_version":"v1","frozen_asset_ids":["asset_A"],"search_ready":true,"shots":[{"index_snapshot_id":"other","shot_id":"shot_1","asset_id":"asset_A","source_start_frame":0,"source_end_frame":30,"duration_frames":30,"boundary_version":1,"score":0.8}],"total_matches":1,"returned_candidates":1,"truncated":false}`, false},
		{"shot search duplicate shot refs", "shot.search", `{"top_k":2}`, `{"status":"succeeded","index_snapshot_id":"snap","synonym_version":"v1","frozen_asset_ids":["asset_A"],"search_ready":true,"shots":[{"index_snapshot_id":"snap","shot_id":"shot_1","asset_id":"asset_A","source_start_frame":0,"source_end_frame":30,"duration_frames":30,"boundary_version":1,"score":0.8},{"index_snapshot_id":"snap","shot_id":"shot_1","asset_id":"asset_A","source_start_frame":0,"source_end_frame":30,"duration_frames":30,"boundary_version":1,"score":0.8}],"total_matches":2,"returned_candidates":2,"truncated":false}`, false},
		{"shot search returned count mismatch", "shot.search", `{}`, `{"status":"succeeded","index_snapshot_id":"snap","synonym_version":"v1","frozen_asset_ids":["asset_A"],"search_ready":true,"shots":[],"total_matches":1,"returned_candidates":1,"truncated":true}`, false},
		{"shot search rejects fractional count", "shot.search", `{}`, `{"status":"succeeded","index_snapshot_id":"snap","synonym_version":"v1","frozen_asset_ids":["asset_A"],"search_ready":true,"shots":[],"total_matches":0.5,"returned_candidates":0,"truncated":false}`, false},
		{"shot search rejects non-boolean readiness", "shot.search", `{}`, `{"status":"succeeded","index_snapshot_id":"snap","synonym_version":"v1","frozen_asset_ids":["asset_A"],"search_ready":"yes","shots":[],"total_matches":0,"returned_candidates":0,"truncated":false}`, false},
		{"shot search rejects oversized top k", "shot.search", `{"top_k":31}`, `{"status":"succeeded","index_snapshot_id":"snap","synonym_version":"v1","frozen_asset_ids":["asset_A"],"search_ready":true,"shots":[],"total_matches":0,"returned_candidates":0,"truncated":false}`, false},
		{"shot search rejects inconsistent truncation", "shot.search", `{}`, `{"status":"succeeded","index_snapshot_id":"snap","synonym_version":"v1","frozen_asset_ids":["asset_A"],"search_ready":true,"shots":[],"total_matches":1,"returned_candidates":0,"truncated":false}`, false},
		{"shot search rejects blank frozen asset", "shot.search", `{}`, `{"status":"succeeded","index_snapshot_id":"snap","synonym_version":"v1","frozen_asset_ids":[" "],"search_ready":true,"shots":[],"total_matches":0,"returned_candidates":0,"truncated":false}`, false},
		{"shot search rejects duplicate frozen assets", "shot.search", `{}`, `{"status":"succeeded","index_snapshot_id":"snap","synonym_version":"v1","frozen_asset_ids":["asset_A","asset_A"],"search_ready":true,"shots":[],"total_matches":0,"returned_candidates":0,"truncated":false}`, false},
		{"shot search rejects frozen asset count drift", "shot.search", `{"asset_ids":["asset_A"]}`, `{"status":"succeeded","index_snapshot_id":"snap","synonym_version":"v1","frozen_asset_ids":["asset_A","asset_B"],"search_ready":true,"shots":[],"total_matches":0,"returned_candidates":0,"truncated":false}`, false},
		{"shot search rejects frozen asset identity drift", "shot.search", `{"asset_ids":["asset_A","asset_B"]}`, `{"status":"succeeded","index_snapshot_id":"snap","synonym_version":"v1","frozen_asset_ids":["asset_A","asset_C"],"search_ready":true,"shots":[],"total_matches":0,"returned_candidates":0,"truncated":false}`, false},
		{"shot search rejects candidate outside frozen set", "shot.search", `{}`, `{"status":"succeeded","index_snapshot_id":"snap","synonym_version":"v1","frozen_asset_ids":["asset_A"],"search_ready":true,"shots":[{"index_snapshot_id":"snap","shot_id":"shot_1","asset_id":"asset_B","source_start_frame":0,"source_end_frame":30,"duration_frames":30,"boundary_version":1,"score":0.8}],"total_matches":1,"returned_candidates":1,"truncated":false}`, false},
		{"shot search rejects duration outside request", "shot.search", `{"max_duration_frames":10}`, `{"status":"succeeded","index_snapshot_id":"snap","synonym_version":"v1","frozen_asset_ids":["asset_A"],"search_ready":true,"shots":[{"index_snapshot_id":"snap","shot_id":"shot_1","asset_id":"asset_A","source_start_frame":0,"source_end_frame":30,"duration_frames":30,"boundary_version":1,"score":0.8}],"total_matches":1,"returned_candidates":1,"truncated":false}`, false},
		{"deep search exact proof", "shot.deep_search", `{"query":"spin","index_snapshot_id":"snap","candidate_shots":[{"asset_id":"asset_A","shot_id":"shot_1"}],"requirements":["person spins"]}`, `{"status":"succeeded","query":"spin","index_snapshot_id":"snap","analyzer_version":"deep-v1","candidates":[{"index_snapshot_id":"snap","asset_id":"asset_A","shot_id":"shot_1","source_start_frame":0,"source_end_frame":30,"boundary_version":1,"verification":"match","score":0.9,"requirements":[{"criterion":"person spins","status":"observed","observation":"visible spin","frame_ids":["f1"]}],"exclusions":[],"preferences":[],"observations":["person rotates"],"frame_evidence":[{"frame_id":"f1","source_frame":10,"timestamp_ms":333,"position":"ordered_1_of_3","object_hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","object_size":10,"newly_added":true}],"deep_coverage":["temporal_action"]}],"total_candidates":1,"returned_candidates":1,"new_frame_count":1,"reused_frame_count":0,"cache_hit":false}`, true},
		{"deep search rejects wrong snapshot", "shot.deep_search", `{"query":"spin","index_snapshot_id":"snap","candidate_shots":[{"asset_id":"asset_A","shot_id":"shot_1"}],"requirements":["person spins"]}`, `{"status":"succeeded","query":"spin","index_snapshot_id":"snap","analyzer_version":"deep-v1","candidates":[{"index_snapshot_id":"other","asset_id":"asset_A","shot_id":"shot_1","source_start_frame":0,"source_end_frame":30,"boundary_version":1,"verification":"match","score":0.9,"requirements":[{"criterion":"person spins","status":"observed","observation":"visible spin","frame_ids":["f1"]}],"exclusions":[],"preferences":[],"observations":["person rotates"],"frame_evidence":[{"frame_id":"f1","source_frame":10,"timestamp_ms":333,"position":"ordered_1_of_3","object_hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","object_size":10,"newly_added":true}],"deep_coverage":["temporal_action"]}],"total_candidates":1,"returned_candidates":1,"new_frame_count":1,"reused_frame_count":0,"cache_hit":false}`, false},
		{"deep search rejects criterion drift", "shot.deep_search", `{"query":"spin","index_snapshot_id":"snap","candidate_shots":[{"asset_id":"asset_A","shot_id":"shot_1"}],"requirements":["person spins"]}`, `{"status":"succeeded","query":"spin","index_snapshot_id":"snap","analyzer_version":"deep-v1","candidates":[{"index_snapshot_id":"snap","asset_id":"asset_A","shot_id":"shot_1","source_start_frame":0,"source_end_frame":30,"boundary_version":1,"verification":"match","score":0.9,"requirements":[{"criterion":"different","status":"observed","observation":"visible spin","frame_ids":["f1"]}],"exclusions":[],"preferences":[],"observations":["person rotates"],"frame_evidence":[{"frame_id":"f1","source_frame":10,"timestamp_ms":333,"position":"ordered_1_of_3","object_hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","object_size":10,"newly_added":true}],"deep_coverage":["temporal_action"]}],"total_candidates":1,"returned_candidates":1,"new_frame_count":1,"reused_frame_count":0,"cache_hit":false}`, false},
		{"typed read wrong field type", "shot.search", `{}`, `{"shots":"已完成","total_matches":"全部"}`, false},
		{"incomplete typed result", "shot.search", `{}`, `{"status":"succeeded","shots":[],"total_matches":0,"returned_candidates":0,"truncated":false}`, false},
		{"asset list paginated", "asset.list_assets", `{"limit":1}`, `{"draft_id":"draft","assets":[{"asset_id":"asset_1"}],"total":2,"next_after":"asset_1"}`, true},
		{"asset list count exceeds total", "asset.list_assets", `{}`, `{"draft_id":"draft","assets":[{"asset_id":"asset_1"}],"total":0}`, false},
		{"asset list missing asset identity", "asset.list_assets", `{}`, `{"draft_id":"draft","assets":[{}],"total":1}`, false},
		{"asset list wrong kind", "asset.list_assets", `{"kind":"video"}`, `{"draft_id":"draft","assets":[{"asset_id":"asset_1","kind":"audio"}],"total":1}`, false},
		{"asset list wrong usable state", "asset.list_assets", `{"only_usable":true}`, `{"draft_id":"draft","assets":[{"asset_id":"asset_1","usable":false}],"total":1}`, false},
		{"asset list before cursor", "asset.list_assets", `{"after":"asset_10"}`, `{"draft_id":"draft","assets":[{"asset_id":"asset_09"}],"total":1}`, false},
		{"asset list exceeds limit", "asset.list_assets", `{"limit":1}`, `{"draft_id":"draft","assets":[{"asset_id":"asset_1"},{"asset_id":"asset_2"}],"total":2}`, false},
		{"asset list missing pagination cursor", "asset.list_assets", `{"limit":1}`, `{"draft_id":"draft","assets":[{"asset_id":"asset_1"}],"total":2}`, false},
		{"asset list wrong pagination cursor", "asset.list_assets", `{"limit":1}`, `{"draft_id":"draft","assets":[{"asset_id":"asset_1"}],"total":2,"next_after":"asset_2"}`, false},
		{"asset list unexpected terminal cursor", "asset.list_assets", `{"limit":1}`, `{"draft_id":"draft","assets":[{"asset_id":"asset_1"}],"total":1,"next_after":"asset_1"}`, false},
		{"beats match asset", "audio.analyze_beats", `{"asset_id":"asset_A"}`, `{"asset_id":"asset_A","timeline_fps":30,"beat_frames":[]}`, true},
		{"beats wrong asset", "audio.analyze_beats", `{"asset_id":"asset_A"}`, `{"asset_id":"asset_B","timeline_fps":30,"beat_frames":[]}`, false},
		{"pauses match clip", "audio.analyze_speech_pauses", `{"timeline_clip_id":"clip_A"}`, `{"asset_id":"asset_A","timeline_clip_id":"clip_A","timeline_fps":30,"pauses":[]}`, true},
		{"pauses wrong clip", "audio.analyze_speech_pauses", `{"timeline_clip_id":"clip_A"}`, `{"asset_id":"asset_A","timeline_clip_id":"clip_B","timeline_fps":30,"pauses":[]}`, false},
		{"transcribe match asset", "speech.transcribe", `{"asset_id":"asset_A"}`, `{"transcript_id":"transcript_A","asset_id":"asset_A","timeline_fps":30}`, true},
		{"transcribe wrong asset", "speech.transcribe", `{"asset_id":"asset_A"}`, `{"transcript_id":"transcript_B","asset_id":"asset_B","timeline_fps":30}`, false},
		{"preview check matches request", "preview.check", `{"preview_id":"preview_A","check":"decode"}`, `{"preview_id":"preview_A","check":"decode","issues":[]}`, true},
		{"preview check wrong target", "preview.check", `{"preview_id":"preview_A","check":"decode"}`, `{"preview_id":"preview_B","check":"decode","issues":[]}`, false},
		{"preview check wrong check", "preview.check", `{"preview_id":"preview_A","check":"decode"}`, `{"preview_id":"preview_A","check":"visual","issues":[]}`, false},
		{"speech search success", "speech.search", `{"asset_id":"asset"}`, `{"status":"succeeded","transcript_id":"transcript","asset_id":"asset","timeline_fps":30,"provider_id":"test","utterances":[],"utterance_total":0,"truncated":false,"usage_note":"ok"}`, true},
		{"speech search wrong asset", "speech.search", `{"asset_id":"asset_A"}`, `{"status":"succeeded","transcript_id":"transcript","asset_id":"asset_B","timeline_fps":30,"provider_id":"test","utterances":[],"utterance_total":0,"truncated":false,"usage_note":"ok"}`, false},
		{"speech search clip match", "speech.search", `{"timeline_clip_id":"clip_A"}`, `{"status":"succeeded","transcript_id":"transcript","asset_id":"asset","timeline_clip_id":"clip_A","timeline_fps":30,"provider_id":"test","utterances":[],"utterance_total":0,"truncated":false,"usage_note":"ok"}`, true},
		{"speech search missing core field", "speech.search", `{"asset_id":"asset"}`, `{"status":"succeeded","transcript_id":"transcript","asset_id":"asset","timeline_fps":30}`, false},
		{"unknown", "timeline.check", `{}`, `{"status":"mystery"}`, false},
		{"malformed", "timeline.check", `{}`, `not-json`, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.result = attachToolRequestFingerprint(test.tool, test.arguments, test.result)
			draftID := ""
			if test.tool == "media.detect_shots" || test.tool == "timeline.check" ||
				isTerminalTimelineMutation(test.tool) {
				draftID = "draft"
			}
			if got := isConfirmedToolRecoverySuccess(test.tool, test.arguments, test.result, draftID); got != test.want {
				t.Fatalf("got=%v want=%v", got, test.want)
			}
		})
	}
	shots := make([]rushestools.ShotCandidate, 30)
	for index := range shots {
		shots[index] = rushestools.ShotCandidate{
			IndexSnapshotID: "snapshot_top_30", ShotID: "shot_" + strconv.Itoa(index+1),
			AssetID: "asset_A", SourceStartFrame: index * 30,
			SourceEndFrame: index*30 + 30, DurationFrames: 30, BoundaryVersion: 1,
		}
	}
	largeTopK, err := json.Marshal(rushestools.ShotSearchResult{
		Status: string(rushestools.StatusSucceeded), IndexSnapshotID: "snapshot_top_30",
		SynonymVersion: "v1", FrozenAssetIDs: []string{"asset_A"}, SearchReady: true,
		Shots: shots, TotalMatches: 31, ReturnedCandidates: 30, Truncated: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	largeTopKProof := attachToolRequestFingerprint("shot.search", `{"top_k":30}`, string(largeTopK))
	if !isConfirmedToolRecoverySuccess("shot.search", `{"top_k":30}`, largeTopKProof, "") {
		t.Fatal("top_k=30 的冻结快照结果应通过回执校验")
	}
	if isConfirmedToolRecoverySuccess(
		"asset.list_assets", `{}`, `{"draft_id":"draft_B","assets":[],"total":0}`, "draft_A",
	) {
		t.Fatal("asset.list_assets result from another active draft was accepted")
	}
	if isConfirmedToolRecoverySuccess(
		"media.detect_shots", `{"asset_id":"asset"}`,
		`{"draft_id":"draft_B","job_id":"job_1","asset_id":"asset","status":"queued"}`, "draft_A",
	) {
		t.Fatal("media.detect_shots result from another active draft was accepted")
	}
	if !isConfirmedToolRecoverySuccess(
		"timeline.inspect", `{}`,
		attachToolRequestFingerprint("timeline.inspect", `{}`, `{"status":"succeeded","data":{"timeline_id":"draft_A:v1"}}`),
		"draft_A",
	) {
		t.Fatal("timeline.inspect result for the active draft was rejected")
	}
	if !isConfirmedToolRecoverySuccess(
		"timeline.inspect", `{}`,
		attachToolRequestFingerprint("timeline.inspect", `{}`, `{"status":"succeeded","data":{"timeline_exists":false}}`),
		"draft_A",
	) {
		t.Fatal("timeline.inspect empty result for the active draft was rejected")
	}
	if isConfirmedToolRecoverySuccess(
		"timeline.inspect", `{}`,
		attachToolRequestFingerprint("timeline.inspect", `{}`, `{"status":"succeeded","observation":"别的草稿已读取","data":{"timeline_id":"draft_B:v1"}}`),
		"draft_A",
	) {
		t.Fatal("timeline.inspect result from another active draft was accepted")
	}
	if isConfirmedToolRecoverySuccess(
		"timeline.check", `{}`,
		attachToolRequestFingerprint("timeline.check", `{}`, successfulTimelineCheckResult("draft_B:v1", "别的草稿已通过")),
		"draft_A",
	) {
		t.Fatal("timeline.check result from another active draft was accepted")
	}
}

func TestShotDeepSearchRecoveryProofHandlesTruncationAndPersistedFrameCache(t *testing.T) {
	frame := rushestools.ShotDeepFrameEvidence{
		FrameID: "f1", SourceFrame: 10, TimestampMS: 333, Position: "ordered_1_of_3",
		ObjectHash: strings.Repeat("a", 64), ObjectSize: 10, NewlyAdded: true,
	}
	candidate := rushestools.ShotDeepCandidate{
		IndexSnapshotID: "snapshot", AssetID: "asset_a", ShotID: "shot_a",
		SourceStartFrame: 0, SourceEndFrame: 30, BoundaryVersion: 1,
		Verification: "match", Score: 0.9,
		Requirements: []rushestools.ShotDeepCriterionEvidence{},
		Exclusions:   []rushestools.ShotDeepCriterionEvidence{},
		Preferences:  []rushestools.ShotDeepCriterionEvidence{},
		Observations: []string{"人物可见"}, FrameEvidence: []rushestools.ShotDeepFrameEvidence{frame},
		DeepCoverage: []string{"appearance"},
	}
	confirm := func(input rushestools.ShotDeepSearchInput, result rushestools.ShotDeepSearchResult) bool {
		t.Helper()
		arguments, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		output, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		proof := attachToolRequestFingerprint("shot.deep_search", string(arguments), string(output))
		return isConfirmedToolRecoverySuccess("shot.deep_search", string(arguments), proof, "")
	}
	truncatedInput := rushestools.ShotDeepSearchInput{
		Query: "人物", IndexSnapshotID: "snapshot", ReturnTopK: 1,
		CandidateShots: []rushestools.ShotRefInput{
			{AssetID: "asset_a", ShotID: "shot_a"}, {AssetID: "asset_b", ShotID: "shot_b"},
		},
	}
	truncated := rushestools.ShotDeepSearchResult{
		Status: "succeeded", Query: "人物", IndexSnapshotID: "snapshot", AnalyzerVersion: "deep-v1",
		Candidates: []rushestools.ShotDeepCandidate{candidate}, TotalCandidates: 2, ReturnedCandidates: 1,
		NewFrameCount: 2, ReusedFrameCount: 0,
	}
	if !confirm(truncatedInput, truncated) {
		t.Fatal("top-k 截断后的全工作量计数应通过 proof")
	}
	cachedInput := truncatedInput
	cachedInput.CandidateShots = cachedInput.CandidateShots[:1]
	cachedInput.ReturnTopK = 0
	cached := truncated
	cached.TotalCandidates = 1
	cached.NewFrameCount = 0
	cached.ReusedFrameCount = 1
	cached.CacheHit = true
	cached.Candidates[0].FrameEvidence[0].NewlyAdded = false
	if !confirm(cachedInput, cached) {
		t.Fatal("持久化帧 cache hit proof 被拒绝")
	}
	wrongVerification := cached
	wrongVerification.Candidates = append([]rushestools.ShotDeepCandidate(nil), cached.Candidates...)
	wrongVerification.Candidates[0].Verification = "partial"
	if confirm(cachedInput, wrongVerification) {
		t.Fatal("与逐项证据矛盾的 verification 不应通过 proof")
	}
	duplicateInput := cachedInput
	duplicateInput.CandidateShots = append(duplicateInput.CandidateShots, duplicateInput.CandidateShots[0])
	if confirm(duplicateInput, cached) {
		t.Fatal("重复请求 ShotRef 不应形成有效 proof")
	}
}

func TestToolRecoveryRejectsWrongDraftInspectWithoutLeakingSuccessObservation(t *testing.T) {
	type reportedEvent struct {
		phase  string
		output any
	}
	events := []reportedEvent{}
	state := newToolRecoveryState()
	ctx := rushestools.WithReporter(
		rushestools.WithDraftID(withToolRecoveryState(t.Context(), state), "draft_A"),
		func(_ context.Context, _ string, phase string, _, output any, _ error) {
			events = append(events, reportedEvent{phase: phase, output: output})
		},
	)
	endpoint := newToolRecoveryMiddleware(testRetrySafe(t)).Invokable(
		func(ctx context.Context, _ *compose.ToolInput) (*compose.ToolOutput, error) {
			if reporter, ok := rushestools.ReporterFromContext(ctx); ok {
				reporter(ctx, "timeline.inspect", "finished", map[string]any{}, rushestools.ToolResult{
					Status: "succeeded", Observation: "别的草稿已读取",
					Data: map[string]any{"timeline_id": "draft_B:v1"},
				}, nil)
			}
			return &compose.ToolOutput{Result: `{"status":"succeeded","observation":"别的草稿已读取","data":{"timeline_id":"draft_B:v1"}}`}, nil
		},
	)
	output, err := endpoint(ctx, &compose.ToolInput{Name: "timeline.inspect", Arguments: `{}`})
	if err != nil || output == nil || !isStructuredToolFailure(output.Result) ||
		strings.Contains(output.Result, "别的草稿已读取") || state.unresolved() {
		t.Fatalf("wrong-draft output leaked success: output=%#v err=%v", output, err)
	}
	if len(events) != 2 || events[1].phase != "finished" {
		t.Fatalf("events=%#v", events)
	}
	reported, ok := events[1].output.(rushestools.ToolResult)
	if !ok || reported.Status != string(rushestools.StatusFailed) ||
		strings.Contains(reported.Observation, "别的草稿已读取") {
		t.Fatalf("reporter leaked success: %#v", events[1].output)
	}
}

func TestToolRecoveryRejectsWrongDraftCheckWithoutLeakingSuccessObservation(t *testing.T) {
	type reportedEvent struct {
		phase  string
		output any
	}
	events := []reportedEvent{}
	state := newToolRecoveryState()
	ctx := rushestools.WithReporter(
		rushestools.WithDraftID(withToolRecoveryState(t.Context(), state), "draft_A"),
		func(_ context.Context, _ string, phase string, _, output any, _ error) {
			events = append(events, reportedEvent{phase: phase, output: output})
		},
	)
	endpoint := newToolRecoveryMiddleware(testRetrySafe(t)).Invokable(
		func(ctx context.Context, _ *compose.ToolInput) (*compose.ToolOutput, error) {
			if reporter, ok := rushestools.ReporterFromContext(ctx); ok {
				reporter(ctx, "timeline.check", "finished", map[string]any{}, rushestools.ToolResult{
					Status: "succeeded", Observation: "别的草稿已通过",
					Data: successfulTimelineCheckData("draft_B:v1"),
				}, nil)
			}
			return &compose.ToolOutput{Result: successfulTimelineCheckResult("draft_B:v1", "别的草稿已通过")}, nil
		},
	)
	output, err := endpoint(ctx, &compose.ToolInput{Name: "timeline.check", Arguments: `{}`})
	if err != nil || output == nil || !isStructuredToolFailure(output.Result) ||
		strings.Contains(output.Result, "别的草稿已通过") || state.unresolved() {
		t.Fatalf("wrong-draft output leaked success: output=%#v err=%v", output, err)
	}
	if len(events) != 2 || events[1].phase != "finished" {
		t.Fatalf("events=%#v", events)
	}
	reported, ok := events[1].output.(rushestools.ToolResult)
	if !ok || reported.Status != string(rushestools.StatusFailed) ||
		strings.Contains(reported.Observation, "别的草稿已通过") {
		t.Fatalf("reporter leaked success: %#v", events[1].output)
	}
}

func TestToolRecoveryRejectsWrongDraftMutationWithoutLeakingSuccessObservation(t *testing.T) {
	type reportedEvent struct {
		phase  string
		output any
	}
	events := []reportedEvent{}
	state := newToolRecoveryState()
	ctx := rushestools.WithReporter(
		rushestools.WithDraftID(withToolRecoveryState(t.Context(), state), "draft_A"),
		func(_ context.Context, _ string, phase string, _, output any, _ error) {
			events = append(events, reportedEvent{phase: phase, output: output})
		},
	)
	endpoint := newToolRecoveryMiddleware(testRetrySafe(t)).Invokable(
		func(ctx context.Context, _ *compose.ToolInput) (*compose.ToolOutput, error) {
			if reporter, ok := rushestools.ReporterFromContext(ctx); ok {
				reporter(ctx, "timeline.update", "finished", map[string]any{}, rushestools.ToolResult{
					Status: "succeeded", Observation: "别的草稿已编辑",
					Data: map[string]any{"timeline_id": "draft_B:v2"},
				}, nil)
			}
			return &compose.ToolOutput{Result: `{"status":"succeeded","observation":"别的草稿已编辑","data":{"timeline_id":"draft_B:v2"}}`}, nil
		},
	)
	output, err := endpoint(ctx, &compose.ToolInput{
		Name: "timeline.update", Arguments: `{"kind":"trim_clip_edge","timeline_clip_id":"clip_1"}`,
	})
	if err != nil || output == nil || !isStructuredToolFailure(output.Result) ||
		strings.Contains(output.Result, "别的草稿已编辑") || state.unresolved() {
		t.Fatalf("wrong-draft output leaked success: output=%#v err=%v", output, err)
	}
	if len(events) != 2 || events[1].phase != "finished" {
		t.Fatalf("events=%#v", events)
	}
	reported, ok := events[1].output.(rushestools.ToolResult)
	if !ok || reported.Status != string(rushestools.StatusFailed) ||
		strings.Contains(reported.Observation, "别的草稿已编辑") {
		t.Fatalf("reporter leaked success: %#v", events[1].output)
	}
}

func TestToolRecoveryProofBindsFullSemanticRequestAndTarget(t *testing.T) {
	tests := []struct {
		name, tool, request, proofRequest, result string
	}{
		{
			"detect shots analysis configuration", "media.detect_shots",
			`{"asset_id":"asset_A","depth":"deep","force_refresh":true}`,
			`{"asset_id":"asset_A","depth":"scan","force_refresh":false}`,
			`{"draft_id":"draft","job_id":"job_1","asset_id":"asset_A","status":"queued"}`,
		},
		{
			"speech semantic query", "speech.search",
			`{"asset_id":"asset_A","query":"保留这句","include_words":true}`,
			`{"asset_id":"asset_A","query":"删除那句","include_words":false}`,
			`{"status":"succeeded","transcript_id":"transcript","asset_id":"asset_A","timeline_fps":30,"provider_id":"test","utterances":[],"utterance_total":0,"truncated":false,"usage_note":"ok"}`,
		},
		{
			"decision card content", "interaction.ask_user",
			`{"question":"当前问题","decision_type":"critical"}`,
			`{"question":"另一个问题","decision_type":"critical"}`,
			`{"status":"waiting_user","data":{"decision_id":"decision_other","turn_should_end":true}}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw := attachToolRequestFingerprint(test.tool, test.proofRequest, test.result)
			if isConfirmedToolRecoverySuccess(test.tool, test.request, raw, "draft") {
				t.Fatal("另一请求的 proof 不得确认当前请求")
			}
		})
	}

	wrongRole := attachToolRequestFingerprint(
		"shot.search", `{"semantic_roles":["a_roll"],"min_duration_frames":60}`,
		`{"status":"succeeded","index_snapshot_id":"snap","synonym_version":"v1","frozen_asset_ids":["asset_A"],"search_ready":true,"shots":[{"index_snapshot_id":"snap","shot_id":"shot_1","asset_id":"asset_A","source_start_frame":0,"source_end_frame":30,"duration_frames":30,"boundary_version":1,"semantic_role":"b_roll","score":0.8}],"total_matches":1,"returned_candidates":1,"truncated":false}`,
	)
	if isConfirmedToolRecoverySuccess(
		"shot.search", `{"semantic_roles":["a_roll"],"min_duration_frames":60}`, wrongRole, "",
	) {
		t.Fatal("违反角色或时长筛选的 shot 不得通过")
	}
	normalizedRole := attachToolRequestFingerprint(
		"shot.search", `{"semantic_roles":[" B_ROLL "]}`,
		`{"status":"succeeded","index_snapshot_id":"snap","synonym_version":"v1","frozen_asset_ids":["asset_A"],"search_ready":true,"shots":[{"index_snapshot_id":"snap","shot_id":"shot_1","asset_id":"asset_A","source_start_frame":0,"source_end_frame":30,"duration_frames":30,"boundary_version":1,"semantic_role":"b_roll","score":0.8}],"total_matches":1,"returned_candidates":1,"truncated":false}`,
	)
	if !isConfirmedToolRecoverySuccess(
		"shot.search", `{"semantic_roles":[" B_ROLL "]}`, normalizedRole, "",
	) {
		t.Fatal("executor 规范化后合法的 semantic_role 不应被 proof 拒绝")
	}

	semanticTagMatch := attachToolRequestFingerprint(
		"shot.search", `{"tags":["city skyline"]}`,
		`{"status":"succeeded","index_snapshot_id":"snap","synonym_version":"v1","frozen_asset_ids":["asset_A"],"search_ready":true,"shots":[{"index_snapshot_id":"snap","shot_id":"shot_1","asset_id":"asset_A","source_start_frame":0,"source_end_frame":30,"duration_frames":30,"boundary_version":1,"description":"city skyline at night","score":0.8}],"total_matches":1,"returned_candidates":1,"truncated":false}`,
	)
	if !isConfirmedToolRecoverySuccess(
		"shot.search", `{"tags":["city skyline"]}`, semanticTagMatch, "",
	) {
		t.Fatal("executor 可接受的 description 语义标签命中不应被 proof 拒绝")
	}
	wrongSemanticTag := attachToolRequestFingerprint(
		"shot.search", `{"tags":["city skyline"]}`,
		`{"status":"succeeded","index_snapshot_id":"snap","synonym_version":"v1","frozen_asset_ids":["asset_A"],"search_ready":true,"shots":[{"index_snapshot_id":"snap","shot_id":"shot_1","asset_id":"asset_A","source_start_frame":0,"source_end_frame":30,"duration_frames":30,"boundary_version":1,"description":"forest trail","score":0.8}],"total_matches":1,"returned_candidates":1,"truncated":false}`,
	)
	if isConfirmedToolRecoverySuccess(
		"shot.search", `{"tags":["city skyline"]}`, wrongSemanticTag, "",
	) {
		t.Fatal("executor 会过滤的错误语义标签结果不得通过 proof")
	}

	if isConfirmedToolRecoverySuccess(
		"timeline.check", `{"timeline_id":"draft:v1"}`,
		attachToolRequestFingerprint(
			"timeline.check", `{"timeline_id":"draft:v1"}`,
			successfulTimelineCheckResult("draft:v2", "wrong version"),
		), "draft",
	) {
		t.Fatal("timeline.check 不得接受错误请求版本")
	}
	if isConfirmedToolRecoverySuccess(
		"preview.generate", `{"timeline_id":"draft:v2"}`,
		attachToolRequestFingerprint(
			"preview.generate", `{"timeline_id":"draft:v2"}`,
			`{"status":"succeeded","data":{"preview_id":"preview_1","job_id":"job_1","job_status":"succeeded","timeline_id":"other:v2","timeline_version":2,"orientation":"auto"}}`,
		), "draft",
	) {
		t.Fatal("preview.generate 不得接受错误 timeline")
	}
}

func TestToolRecoveryReporterUsesNormalizedFailureResult(t *testing.T) {
	type reportedEvent struct {
		phase  string
		output any
		err    error
	}
	events := []reportedEvent{}
	ctx := rushestools.WithReporter(t.Context(), func(
		_ context.Context, _ string, phase string, _, output any, err error,
	) {
		events = append(events, reportedEvent{phase: phase, output: output, err: err})
	})
	endpoint := newToolRecoveryMiddleware(testRetrySafe(t)).Invokable(
		func(ctx context.Context, input *compose.ToolInput) (*compose.ToolOutput, error) {
			reporter, ok := rushestools.ReporterFromContext(ctx)
			if !ok {
				t.Fatal("missing reporter")
			}
			original := rushestools.ToolResult{Status: "mystery", Observation: "已完成"}
			reporter(ctx, input.Name, "finished", map[string]any{}, original, nil)
			return &compose.ToolOutput{Result: `{"status":"mystery","observation":"已完成"}`}, nil
		},
	)
	output, err := endpoint(ctx, &compose.ToolInput{Name: "timeline.check", Arguments: `{}`})
	if err != nil || output == nil || !isStructuredToolFailure(output.Result) {
		t.Fatalf("output=%#v err=%v", output, err)
	}
	if len(events) != 2 || events[0].phase != "started" || events[1].phase != "finished" ||
		events[1].err != nil || !structuredToolOutputFailed(events[1].output) {
		t.Fatalf("events=%#v", events)
	}
	encoded, marshalErr := json.Marshal(events[1].output)
	var reported rushestools.ToolResult
	decodeErr := json.Unmarshal(encoded, &reported)
	if marshalErr != nil || decodeErr != nil || reported.Status != "failed" ||
		reported.Data["returned_status"] != "mystery" || strings.Contains(string(encoded), "已完成") {
		t.Fatalf("unverified result leaked to reporter: %s err=%v", encoded, marshalErr)
	}
}

func TestToolRecoveryDoesNotBlindlyReplayMutations(t *testing.T) {
	calls := 0
	middleware := newToolRecoveryMiddleware(testRetrySafe(t))
	endpoint := middleware.Invokable(func(context.Context, *compose.ToolInput) (*compose.ToolOutput, error) {
		calls++
		return nil, errors.New("commit result unknown")
	})
	output, err := endpoint(
		withToolRecoveryState(t.Context(), newToolRecoveryState()),
		&compose.ToolInput{Name: "timeline.update", Arguments: `{"ops":[{"kind":"split_clip"}]}`},
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
	endpoint := newToolRecoveryMiddleware(testRetrySafe(t)).Invokable(
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
		data["automatic_retries"] != float64(0) {
		t.Fatalf("payload=%#v", payload)
	}
	if len(events) != 2 || events[0].phase != "started" || events[1].phase != "finished" ||
		events[1].err == nil {
		t.Fatalf("schema failure must have one visible terminal trace: events=%#v", events)
	}
}

func TestToolRecoveryPreservesStructuredBusinessFailureForModel(t *testing.T) {
	state := newToolRecoveryState()
	endpoint := newToolRecoveryMiddleware(testRetrySafe(t)).Invokable(
		func(context.Context, *compose.ToolInput) (*compose.ToolOutput, error) {
			return &compose.ToolOutput{Result: `{
				"status":"validation_failed",
				"observation":"片段相互重叠",
				"data":{"error_code":"timeline_invalid","failed_op":{"kind":"trim_clip","timeline_clip_id":"clip_1"},"recovery":"修正当前操作"}
			}`}, nil
		},
	)
	output, err := endpoint(
		withToolRecoveryState(t.Context(), state),
		&compose.ToolInput{
			Name: "timeline.update",
			Arguments: `{"kind":"trim_clip","timeline_clip_id":"clip_1",` +
				`"source_start_frame":0,"source_end_frame":30}`,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	payload := decodeRecoveryPayload(t, output.Result)
	data := payload["data"].(map[string]any)
	if payload["status"] != "validation_failed" || payload["observation"] != "片段相互重叠" ||
		data["error_code"] != "timeline_invalid" || data["failed_op"] == nil ||
		data["recovery"] != "修正当前操作" || payload["error_code"] != "timeline_invalid" ||
		payload["message"] != "片段相互重叠" || payload["current_state"] == nil ||
		payload["invalid_fields"] == nil {
		t.Fatalf("structured business failure was not preserved: payload=%#v", payload)
	}
}

func TestToolRecoveryPreservesTopLevelTypedFailureForModel(t *testing.T) {
	endpoint := newToolRecoveryMiddleware(testRetrySafe(t)).Invokable(
		func(context.Context, *compose.ToolInput) (*compose.ToolOutput, error) {
			return &compose.ToolOutput{Result: `{
				"status":"failed",
				"error_code":"shot_deep_analysis_failed",
				"message":"新增帧视觉复核失败。",
				"recovery":"使用相同快照和 ShotRef 直接重试。",
				"invalid_candidate_shots":[{"asset_id":"asset_a","shot_id":"shot_a"}],
				"candidates":[],"total_candidates":0,"returned_candidates":0,
				"new_frame_count":0,"reused_frame_count":0,"cache_hit":false
			}`}, nil
		},
	)
	output, err := endpoint(t.Context(), &compose.ToolInput{
		Name: "shot.deep_search", Arguments: `{"query":"动作","index_snapshot_id":"snapshot","candidate_shots":[{"asset_id":"asset_a","shot_id":"shot_a"}]}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := decodeRecoveryPayload(t, output.Result)
	data := payload["data"].(map[string]any)
	if payload["error_code"] != "shot_deep_analysis_failed" ||
		payload["message"] != "新增帧视觉复核失败。" ||
		payload["recovery"] != "使用相同快照和 ShotRef 直接重试。" ||
		data["error_code"] != payload["error_code"] || data["message"] != payload["message"] {
		t.Fatalf("typed failure was not preserved: payload=%#v", payload)
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
	middleware := newToolRecoveryMiddleware(testRetrySafe(t))
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

func TestToolFailuresAreIndependentAndNeverExhaustPerCallAdmission(t *testing.T) {
	state := newToolRecoveryState()
	ctx := withToolRecoveryState(t.Context(), state)
	calls := 0
	middleware := newToolRecoveryMiddleware(testRetrySafe(t))
	endpoint := middleware.Invokable(func(context.Context, *compose.ToolInput) (*compose.ToolOutput, error) {
		calls++
		return &compose.ToolOutput{Result: marshalToolFailure("bad clip id", map[string]any{
			"error_code": "invalid_clip",
		})}, nil
	})
	input := &compose.ToolInput{Name: "timeline.update", Arguments: `{"bgm_asset_id":"bad"}`}
	first, err := endpoint(ctx, input)
	if err != nil || calls != 1 || state.unresolved() {
		t.Fatalf("first=%#v calls=%d err=%v", first, calls, err)
	}

	for range independentFailureRetryCount {
		if _, err = endpoint(ctx, input); err != nil {
			t.Fatal(err)
		}
	}
	if calls != independentFailureRetryCount+1 {
		t.Fatalf("每次失败都应进入 executor: calls=%d", calls)
	}
	other, err := endpoint(ctx, &compose.ToolInput{Name: "timeline.inspect", Arguments: `{}`})
	if err != nil || calls != independentFailureRetryCount+2 {
		t.Fatalf("无关工具应继续执行: output=%#v calls=%d err=%v", other, calls, err)
	}
}

func TestToolRecoveryCanonicalizesDuplicateJSONArguments(t *testing.T) {
	state := newToolRecoveryState()
	ctx := withToolRecoveryState(t.Context(), state)
	calls := 0
	endpoint := newToolRecoveryMiddleware(testRetrySafe(t)).Invokable(
		func(context.Context, *compose.ToolInput) (*compose.ToolOutput, error) {
			calls++
			return &compose.ToolOutput{Result: marshalToolFailure("invalid range", nil)}, nil
		},
	)
	for _, arguments := range []string{`{"b":2,"a":1}`, `{ "a": 1, "b": 2 }`} {
		output, callErr := endpoint(ctx, &compose.ToolInput{Name: "timeline.update", Arguments: arguments})
		if callErr != nil || decodeRecoveryPayload(t, output.Result)["status"] != "failed" {
			t.Fatalf("output=%#v err=%v", output, callErr)
		}
	}
	if calls != 2 || state.unresolved() {
		t.Fatalf("规范化后相同参数仍必须独立执行: calls=%d state=%#v", calls, state)
	}
}

func TestDistinctFailuresRemainBoundedOnlyByTurnBudget(t *testing.T) {
	state := newToolRecoveryState()
	ctx := withToolRecoveryState(t.Context(), state)
	calls := 0
	endpoint := newToolRecoveryMiddleware(testRetrySafe(t)).Invokable(
		func(context.Context, *compose.ToolInput) (*compose.ToolOutput, error) {
			calls++
			return &compose.ToolOutput{Result: marshalToolFailure("still invalid", nil)}, nil
		},
	)
	for attempt := 0; attempt <= independentFailureRetryCount; attempt++ {
		_, err := endpoint(ctx, &compose.ToolInput{
			Name: "timeline.update", Arguments: `{"attempt":` + string(rune('0'+attempt)) + `}`,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls != independentFailureRetryCount+1 || state.unresolved() {
		t.Fatalf("calls=%d unresolved=%v", calls, state.unresolved())
	}
}

func TestToolRecoveryLetsModelCorrectArguments(t *testing.T) {
	state := newToolRecoveryState()
	ctx := rushestools.WithDraftID(withToolRecoveryState(t.Context(), state), "draft")
	calls := 0
	middleware := newToolRecoveryMiddleware(testRetrySafe(t))
	endpoint := middleware.Invokable(func(_ context.Context, input *compose.ToolInput) (*compose.ToolOutput, error) {
		calls++
		if input.Arguments == `{"value":"good"}` {
			return &compose.ToolOutput{Result: `{"status":"succeeded","observation":"fixed","data":{"timeline_id":"draft:v2"}}`}, nil
		}
		return &compose.ToolOutput{Result: marshalToolFailure("change value", nil)}, nil
	})
	if _, err := endpoint(ctx, &compose.ToolInput{Name: "timeline.update", Arguments: `{"value":"bad"}`}); err != nil {
		t.Fatal(err)
	}
	corrected, err := endpoint(ctx, &compose.ToolInput{Name: "timeline.update", Arguments: `{"value":"good"}`})
	if err != nil || calls != 2 || state.unresolved() || !json.Valid([]byte(corrected.Result)) {
		t.Fatalf("corrected=%#v calls=%d unresolved=%v err=%v", corrected, calls, state.unresolved(), err)
	}
}

func TestToolRecoverySuccessOnUnrelatedToolKeepsFailureUnresolved(t *testing.T) {
	state := newToolRecoveryState()
	ctx := withToolRecoveryState(t.Context(), state)
	calls := 0
	endpoint := newToolRecoveryMiddleware(testRetrySafe(t)).Invokable(
		func(_ context.Context, input *compose.ToolInput) (*compose.ToolOutput, error) {
			calls++
			if input.Name == "asset.list_assets" {
				return &compose.ToolOutput{Result: `{"draft_id":"draft","assets":[],"total":0,"usage_note":"fresh state"}`}, nil
			}
			return &compose.ToolOutput{Result: marshalToolFailure("not ready", nil)}, nil
		},
	)
	failedInput := &compose.ToolInput{Name: "timeline.update", Arguments: `{"timeline_clip_id":"stale"}`}
	if _, err := endpoint(ctx, failedInput); err != nil || state.unresolved() {
		t.Fatalf("initial failure err=%v unresolved=%v", err, state.unresolved())
	}
	if _, err := endpoint(ctx, &compose.ToolInput{Name: "asset.list_assets", Arguments: `{}`}); err != nil || state.unresolved() {
		t.Fatalf("unrelated success err=%v unresolved=%v", err, state.unresolved())
	}
	if _, err := endpoint(ctx, failedInput); err != nil || calls != 3 || state.unresolved() {
		t.Fatalf("repeated failure should execute independently err=%v calls=%d unresolved=%v", err, calls, state.unresolved())
	}
}

func TestToolRecoverySuccessOnDifferentTargetOfSameToolKeepsFailureUnresolved(t *testing.T) {
	state := newToolRecoveryState()
	ctx := withToolRecoveryState(t.Context(), state)
	endpoint := newToolRecoveryMiddleware(testRetrySafe(t)).Invokable(
		func(_ context.Context, input *compose.ToolInput) (*compose.ToolOutput, error) {
			if strings.Contains(input.Arguments, `"timeline_id":"draft:v2"`) {
				return &compose.ToolOutput{Result: marshalToolFailure("v2 contract failed", nil)}, nil
			}
			return &compose.ToolOutput{Result: successfulTimelineCheckResult("draft:v1", "old version valid")}, nil
		},
	)
	if _, err := endpoint(ctx, &compose.ToolInput{
		Name: "timeline.check", Arguments: `{"timeline_id":"draft:v2"}`,
	}); err != nil || state.unresolved() {
		t.Fatalf("current check failure err=%v unresolved=%v", err, state.unresolved())
	}
	if _, err := endpoint(ctx, &compose.ToolInput{
		Name: "timeline.check", Arguments: `{"timeline_id":"draft:v1"}`,
	}); err != nil || state.unresolved() {
		t.Fatalf("old-version success must not resolve current failure err=%v unresolved=%v", err, state.unresolved())
	}
}

func TestToolRecoveryLatestCheckCanRetrySameEmptyArgumentsAndResolveOlderVersion(t *testing.T) {
	state := newToolRecoveryState()
	ctx := rushestools.WithDraftID(withToolRecoveryState(t.Context(), state), "draft")
	calls := 0
	endpoint := newToolRecoveryMiddleware(testRetrySafe(t)).Invokable(
		func(context.Context, *compose.ToolInput) (*compose.ToolOutput, error) {
			calls++
			if calls == 1 {
				return &compose.ToolOutput{Result: `{
					"status":"validation_failed","observation":"v2 contract failed",
					"data":{"timeline_id":"draft:v2"}
				}`}, nil
			}
			return &compose.ToolOutput{Result: successfulTimelineCheckResult("draft:v3", "v3 valid")}, nil
		},
	)
	input := &compose.ToolInput{Name: "timeline.check", Arguments: `{}`}
	if _, err := endpoint(ctx, input); err != nil || state.unresolved() {
		t.Fatalf("first empty check err=%v unresolved=%v", err, state.unresolved())
	}
	if _, err := endpoint(ctx, input); err != nil || calls != 2 || state.unresolved() {
		t.Fatalf(
			"same dynamic arguments must check and resolve newer version: calls=%d err=%v unresolved=%v",
			calls, err, state.unresolved(),
		)
	}
}

func TestToolRecoveryExplicitCheckCanRetryAfterContractRepairOnSameVersion(t *testing.T) {
	state := newToolRecoveryState()
	ctx := withToolRecoveryState(t.Context(), state)
	checkCalls := 0
	endpoint := newToolRecoveryMiddleware(testRetrySafe(t)).Invokable(
		func(_ context.Context, input *compose.ToolInput) (*compose.ToolOutput, error) {
			if input.Name == "plan.update" {
				return &compose.ToolOutput{Result: `{"status":"succeeded","observation":"contract fixed"}`}, nil
			}
			checkCalls++
			if checkCalls == 1 {
				return &compose.ToolOutput{Result: `{
					"status":"validation_failed","observation":"contract failed",
					"data":{"timeline_id":"draft:v2"}
				}`}, nil
			}
			return &compose.ToolOutput{Result: successfulTimelineCheckResult("draft:v2", "contract valid")}, nil
		},
	)
	check := &compose.ToolInput{
		Name: "timeline.check", Arguments: `{"timeline_id":"draft:v2"}`,
	}
	if _, err := endpoint(ctx, check); err != nil || state.unresolved() {
		t.Fatalf("initial explicit check err=%v unresolved=%v", err, state.unresolved())
	}
	if _, err := endpoint(ctx, &compose.ToolInput{
		Name: "plan.update", Arguments: `{"plan":{"content_contract":{}}}`,
	}); err != nil || state.unresolved() {
		t.Fatalf("contract repair must not itself erase check failure: err=%v unresolved=%v", err, state.unresolved())
	}
	if _, err := endpoint(ctx, check); err != nil || checkCalls != 2 || state.unresolved() {
		t.Fatalf(
			"explicit same-version check must rerun after contract repair: calls=%d err=%v unresolved=%v",
			checkCalls, err, state.unresolved(),
		)
	}
}

func TestToolRecoveryCheckResolvesCommittedMutationContractFailure(t *testing.T) {
	state := newToolRecoveryState()
	ctx := withToolRecoveryState(t.Context(), state)
	endpoint := newToolRecoveryMiddleware(testRetrySafe(t)).Invokable(
		func(_ context.Context, input *compose.ToolInput) (*compose.ToolOutput, error) {
			switch input.Name {
			case "timeline.update":
				return &compose.ToolOutput{Result: `{
					"status":"validation_failed","observation":"v2 contract failed",
					"data":{"timeline_id":"draft:v2","contract_failures":[{"check":"target_duration"}]}
				}`}, nil
			case "plan.update":
				return &compose.ToolOutput{Result: `{"status":"succeeded","observation":"contract fixed"}`}, nil
			case "timeline.check":
				return &compose.ToolOutput{Result: successfulTimelineCheckResult("draft:v2", "v2 contract valid")}, nil
			default:
				t.Fatalf("unexpected tool %q", input.Name)
				return nil, nil
			}
		},
	)
	if _, err := endpoint(ctx, &compose.ToolInput{
		Name: "timeline.update", Arguments: `{"kind":"trim_clip_edge","timeline_clip_id":"clip_1"}`,
	}); err != nil || state.unresolved() {
		t.Fatalf("committed mutation failure err=%v unresolved=%v", err, state.unresolved())
	}
	if _, err := endpoint(ctx, &compose.ToolInput{
		Name: "plan.update", Arguments: `{"plan":{"content_contract":{}}}`,
	}); err != nil || state.unresolved() {
		t.Fatalf("contract repair must retain mutation failure err=%v unresolved=%v", err, state.unresolved())
	}
	if _, err := endpoint(ctx, &compose.ToolInput{
		Name: "timeline.check", Arguments: `{"timeline_id":"draft:v2"}`,
	}); err != nil || state.unresolved() {
		t.Fatalf("same-version check must resolve committed mutation failure err=%v unresolved=%v summary=%q",
			err, state.unresolved(), state.summary())
	}
}

func TestToolRecoveryCheckDoesNotResolveUncommittedMutationFailure(t *testing.T) {
	state := newToolRecoveryState()
	state.recordSuccess(
		"timeline.check", `{"timeline_id":"draft:v2"}`,
		`{"status":"succeeded","data":{"timeline_id":"draft:v2"}}`,
	)
	if state.unresolved() {
		t.Fatal("单次 mutation failure 不得形成跨调用未解决状态")
	}
}

func TestToolRecoveryCorrectedKindOnSameStableTargetResolvesFailure(t *testing.T) {
	state := newToolRecoveryState()
	ctx := rushestools.WithDraftID(withToolRecoveryState(t.Context(), state), "draft")
	endpoint := newToolRecoveryMiddleware(testRetrySafe(t)).Invokable(
		func(_ context.Context, input *compose.ToolInput) (*compose.ToolOutput, error) {
			if strings.Contains(input.Arguments, `"kind":"trim_clip"`) {
				return &compose.ToolOutput{Result: marshalToolFailure("kind 与字段不匹配", nil)}, nil
			}
			return &compose.ToolOutput{Result: `{"status":"succeeded","observation":"已修正","data":{"timeline_id":"draft:v2"}}`}, nil
		},
	)
	if _, err := endpoint(ctx, &compose.ToolInput{
		Name: "timeline.update", Arguments: `{"kind":"trim_clip","timeline_clip_id":"clip_1","edge":"end","timeline_frame":45}`,
	}); err != nil || state.unresolved() {
		t.Fatalf("initial failure err=%v unresolved=%v", err, state.unresolved())
	}
	if _, err := endpoint(ctx, &compose.ToolInput{
		Name: "timeline.update", Arguments: `{"kind":"trim_clip_edge","timeline_clip_id":"clip_1","edge":"end","timeline_frame":45}`,
	}); err != nil || state.unresolved() {
		t.Fatalf("corrected kind on same clip must resolve failure err=%v unresolved=%v", err, state.unresolved())
	}
}

func TestToolRecoverySuccessOnDifferentRangeKeepsFailureUnresolved(t *testing.T) {
	state := newToolRecoveryState()
	ctx := rushestools.WithDraftID(withToolRecoveryState(t.Context(), state), "draft")
	endpoint := newToolRecoveryMiddleware(testRetrySafe(t)).Invokable(
		func(_ context.Context, input *compose.ToolInput) (*compose.ToolOutput, error) {
			if strings.Contains(input.Arguments, `"start_frame":0`) {
				return &compose.ToolOutput{Result: marshalToolFailure("range invalid", nil)}, nil
			}
			return &compose.ToolOutput{Result: `{"status":"succeeded","observation":"deleted other range","data":{"timeline_id":"draft:v2"}}`}, nil
		},
	)
	if _, err := endpoint(ctx, &compose.ToolInput{
		Name: "timeline.delete", Arguments: `{"kind":"delete_range","start_frame":0,"end_frame":10}`,
	}); err != nil || state.unresolved() {
		t.Fatalf("initial range failure err=%v unresolved=%v", err, state.unresolved())
	}
	if _, err := endpoint(ctx, &compose.ToolInput{
		Name: "timeline.delete", Arguments: `{"kind":"delete_range","start_frame":20,"end_frame":30}`,
	}); err != nil || state.unresolved() {
		t.Fatalf("different range success must remain independent err=%v unresolved=%v", err, state.unresolved())
	}
}

func TestToolRecoveryImportSuccessOnlyResolvesSameNormalizedPath(t *testing.T) {
	state := newToolRecoveryState()
	state.recordSuccess(
		"asset.import_local_file", `{"path":"/media/other.mp4","storage_mode":"copy"}`,
		`{"status":"succeeded"}`,
	)
	if state.unresolved() {
		t.Fatal("单次导入失败不得形成跨调用未解决状态")
	}
	state.recordSuccess(
		"asset.import_local_file", `{"path":"/media/tmp/../failed.mp4","storage_mode":"copy","kind":"video"}`,
		`{"status":"succeeded"}`,
	)
	if state.unresolved() {
		t.Fatal("同一规范化路径修正其他参数后应核销原失败")
	}
}

func TestToolRecoveryMemorySetSuccessOnlyResolvesSameSortedKeys(t *testing.T) {
	state := newToolRecoveryState()
	state.recordSuccess(
		"memory.set", `{"entries":[{"key":"music_style","kind":"preference","statement":"轻快","evidence_quote":"音乐轻快"}]}`,
		`{"status":"succeeded"}`,
	)
	if state.unresolved() {
		t.Fatal("单次记忆写入失败不得形成跨调用未解决状态")
	}
	state.recordSuccess(
		"memory.set", `{"entries":[`+
			`{"key":"subtitle_style","kind":"preference","statement":"字幕简洁","evidence_quote":"字幕简洁"},`+
			`{"key":"pacing","kind":"preference","statement":"节奏偏快","evidence_quote":"偏快"}]}`,
		`{"status":"succeeded"}`,
	)
	if state.unresolved() {
		t.Fatal("同一组排序后记忆键修正内容参数后应核销原失败")
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
	if err != nil || state.unresolved() {
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
	repeated, err := unknownToolRecoveryHandler(ctx, "timeline.magic", `{}`)
	if err != nil || decodeRecoveryPayload(t, repeated)["data"].(map[string]any)["error_code"] != "unknown_tool" ||
		len(events) != 4 {
		t.Fatalf("repeated unknown tool should execute and trace independently: output=%s events=%v err=%v", repeated, events, err)
	}
}

func TestToolRecoveryPropagatesCancellation(t *testing.T) {
	middleware := newToolRecoveryMiddleware(testRetrySafe(t))
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

func TestAlternatingFailureAndSuccessNeverCreatesCrossCallAdmission(t *testing.T) {
	state := newToolRecoveryState()
	ctx := rushestools.WithDraftID(withToolRecoveryState(t.Context(), state), "draft")
	calls := 0
	endpoint := newToolRecoveryMiddleware(testRetrySafe(t)).Invokable(
		func(_ context.Context, input *compose.ToolInput) (*compose.ToolOutput, error) {
			calls++
			if input.Arguments == `{"state":"ready"}` {
				return &compose.ToolOutput{Result: `{"status":"succeeded","observation":"fresh state","data":{"timeline_exists":false}}`}, nil
			}
			return &compose.ToolOutput{Result: marshalToolFailure("still not ready", nil)}, nil
		},
	)
	failing := &compose.ToolInput{Name: "timeline.inspect", Arguments: `{}`}
	recovering := &compose.ToolInput{Name: "timeline.inspect", Arguments: `{"state":"ready"}`}

	for i := 0; i < alternatingFailureProbeCount; i++ {
		if _, err := endpoint(ctx, failing); err != nil {
			t.Fatal(err)
		}
		if _, err := endpoint(ctx, recovering); err != nil {
			t.Fatal(err)
		}
	}
	if calls != alternatingFailureProbeCount*2 || state.unresolved() {
		t.Fatalf("calls=%d unresolved=%v", calls, state.unresolved())
	}
}
