package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/compose"
	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

const (
	// 首次执行之外最多自动重试 5 次；只用于只读工具的瞬时故障。
	maxToolExecutionRetries = 5
	// 工具把结构化失败回灌模型后，最多允许 5 次“修改方案再试”。
	maxModelRepairAttempts = 5
	// 交替 fail→success 会不断清空连击预算（recordSuccess），单靠 maxModelRepairAttempts
	// 无法收敛（#95 H4）。turn 级累计计数不因成功重置，累计到此阈值同样触发穷尽。
	maxCumulativeRepairAttempts = 10
	// 64 KiB 仅是早期观测阈值，不改变结果语义；speech.search 等合法大结果仍完整回灌。
	toolResultSoftLimitBytes = 64 << 10
)

type toolRecoveryContextKey struct{}

type toolFailureSnapshot struct {
	Tool              string
	Arguments         string
	Observation       string
	ExecutionAttempts int
	TargetKey         string
	// CommittedTimelineID is set only when a timeline mutation durably wrote a
	// version but returned validation_failed. A later timeline.check can prove
	// that exact or a newer version valid after the independent contract changes.
	CommittedTimelineID string
}

type toolRecoveryState struct {
	mu                       sync.Mutex
	failedCalls              map[string]toolFailureSnapshot
	pendingFailures          map[string]toolFailureSnapshot
	pendingOrder             []string
	pendingRejections        map[string]toolFailureSnapshot
	rejectionOrder           []string
	hadFailure               bool
	rootTool                 string
	repairFailures           int
	cumulativeRepairFailures int
	exhausted                bool
	latest                   toolFailureSnapshot
}

type recoveryDecision struct {
	blocked       bool
	duplicate     bool
	exhausted     bool
	repairAttempt int
	latest        toolFailureSnapshot
}

func newToolRecoveryState() *toolRecoveryState {
	return &toolRecoveryState{
		failedCalls:       map[string]toolFailureSnapshot{},
		pendingFailures:   map[string]toolFailureSnapshot{},
		pendingRejections: map[string]toolFailureSnapshot{},
	}
}

func withToolRecoveryState(ctx context.Context, state *toolRecoveryState) context.Context {
	return context.WithValue(ctx, toolRecoveryContextKey{}, state)
}

func toolRecoveryFromContext(ctx context.Context) *toolRecoveryState {
	state, _ := ctx.Value(toolRecoveryContextKey{}).(*toolRecoveryState)
	return state
}

func (state *toolRecoveryState) beforeCall(name, arguments string) recoveryDecision {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.exhausted {
		return recoveryDecision{blocked: true, exhausted: true, repairAttempt: state.repairFailures, latest: state.latest}
	}
	fingerprint := toolCallFingerprint(name, arguments)
	if previous, exists := state.failedCalls[fingerprint]; exists {
		// timeline.check 同时依赖 timeline 与独立更新的 content contract。
		// 即便显式 timeline_id 不变，plan.update 后同一调用也可能恢复；允许重检，
		// 实际失败仍由 recordFailure 累计并受 turn 级预算约束。
		if name == "timeline.check" {
			return recoveryDecision{}
		}
		state.repairFailures++
		state.cumulativeRepairFailures++
		state.evaluateExhaustion()
		state.latest = previous
		return recoveryDecision{
			blocked: true, duplicate: true, exhausted: state.exhausted,
			repairAttempt: state.repairFailures, latest: previous,
		}
	}
	return recoveryDecision{}
}

func (state *toolRecoveryState) recordFailure(snapshot toolFailureSnapshot) recoveryDecision {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.cumulativeRepairFailures++
	if !state.hadFailure {
		state.hadFailure = true
		state.rootTool = snapshot.Tool
	} else {
		state.repairFailures++
	}
	state.evaluateExhaustion()
	state.failedCalls[toolCallFingerprint(snapshot.Tool, snapshot.Arguments)] = snapshot
	target := snapshot.TargetKey
	if target == "" {
		target = toolRecoveryTargetKey(snapshot.Tool, snapshot.Arguments, "")
	}
	if _, exists := state.pendingFailures[target]; !exists {
		state.pendingOrder = append(state.pendingOrder, target)
	}
	state.pendingFailures[target] = snapshot
	state.latest = snapshot
	return recoveryDecision{
		exhausted: state.exhausted, repairAttempt: state.repairFailures, latest: snapshot,
	}
}

func (state *toolRecoveryState) recordRejection(snapshot toolFailureSnapshot) {
	state.mu.Lock()
	defer state.mu.Unlock()
	fingerprint := toolCallFingerprint(snapshot.Tool, snapshot.Arguments)
	if _, exists := state.pendingRejections[fingerprint]; !exists {
		state.rejectionOrder = append(state.rejectionOrder, fingerprint)
	}
	state.pendingRejections[fingerprint] = snapshot
	state.latest = snapshot
}

func (state *toolRecoveryState) recordSuccess(tool, arguments, rawResult string) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.resolveRejectionLocked(toolCallFingerprint(tool, arguments))
	target := toolRecoveryTargetKey(tool, arguments, rawResult)
	resolvedTargets := map[string]struct{}{}
	if _, resolvesFailure := state.pendingFailures[target]; resolvesFailure {
		resolvedTargets[target] = struct{}{}
	}
	if tool == "timeline.check" {
		if successTimelineID := timelineIDFromRecoveryTargetKey(target); successTimelineID != "" {
			for pendingTarget, pending := range state.pendingFailures {
				failedTimelineID := timelineIDFromRecoveryTargetKey(pendingTarget)
				if timelineIDAtLeast(successTimelineID, failedTimelineID) ||
					timelineIDAtLeast(successTimelineID, pending.CommittedTimelineID) {
					resolvedTargets[pendingTarget] = struct{}{}
				}
			}
		}
	}
	if len(resolvedTargets) == 0 {
		// 参数在 schema 解码前失败时可能没有任何可识别目标。允许同一工具后续一次
		// 有效成功核销这个「未知目标」失败，但绝不让另一个已识别目标的成功越权核销。
		if _, resolvesUnknownTarget := state.pendingFailures[tool]; !resolvesUnknownTarget {
			return
		}
		resolvedTargets[tool] = struct{}{}
	}
	for resolvedTarget := range resolvedTargets {
		pending := state.pendingFailures[resolvedTarget]
		if pending.Tool != tool && (tool != "timeline.check" || pending.CommittedTimelineID == "") {
			delete(resolvedTargets, resolvedTarget)
		}
	}
	if len(resolvedTargets) == 0 {
		return
	}
	for resolvedTarget := range resolvedTargets {
		delete(state.pendingFailures, resolvedTarget)
	}
	for fingerprint, snapshot := range state.failedCalls {
		targetKey := snapshot.TargetKey
		if targetKey == "" {
			targetKey = toolRecoveryTargetKey(snapshot.Tool, snapshot.Arguments, "")
		}
		if _, resolved := resolvedTargets[targetKey]; resolved {
			delete(state.failedCalls, fingerprint)
		}
	}
	keptOrder := state.pendingOrder[:0]
	for _, pendingTarget := range state.pendingOrder {
		if _, resolved := resolvedTargets[pendingTarget]; !resolved {
			keptOrder = append(keptOrder, pendingTarget)
		}
	}
	state.pendingOrder = keptOrder
	state.hadFailure = len(state.pendingFailures) != 0
	if state.hadFailure {
		state.rootTool = state.pendingOrder[0]
		state.latest = state.pendingFailures[state.pendingOrder[len(state.pendingOrder)-1]]
		state.evaluateExhaustion()
		return
	}
	state.rootTool = ""
	state.repairFailures = 0
	// 累计修复计数是 turn 级、不因单次成功重置（#95 H4）：交替 fail→success 不能无限
	// 刷新预算。连击照常清零，但累计到阈值仍维持穷尽（evaluateExhaustion 会把这类
	// 「连击已清零、累计仍超」的穷尽记成 cumulative 分因，即 H-B P2「预算重叠」信号）。
	state.evaluateExhaustion()
	state.latest = toolFailureSnapshot{}
}

func (state *toolRecoveryState) resolveConfirmation(arguments, rawResult string) {
	var confirmation rushestools.ConfirmActionInput
	var result rushestools.ToolResult
	if json.Unmarshal([]byte(arguments), &confirmation) != nil ||
		json.Unmarshal([]byte(rawResult), &result) != nil ||
		(result.Status != string(rushestools.StatusWaiting) &&
			result.Status != string(rushestools.StatusSucceeded)) ||
		agentexec.InterfaceString(result.Data["decision_id"]) == "" {
		return
	}
	encodedArguments, err := json.Marshal(confirmation.Arguments)
	if err != nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	state.resolveRejectionLocked(toolCallFingerprint(confirmation.ToolName, string(encodedArguments)))
}

func (state *toolRecoveryState) resolveRejectionLocked(fingerprint string) {
	if _, exists := state.pendingRejections[fingerprint]; !exists {
		return
	}
	delete(state.pendingRejections, fingerprint)
	for index, pendingFingerprint := range state.rejectionOrder {
		if pendingFingerprint == fingerprint {
			state.rejectionOrder = append(state.rejectionOrder[:index], state.rejectionOrder[index+1:]...)
			break
		}
	}
	switch {
	case len(state.pendingFailures) != 0:
		state.latest = state.pendingFailures[state.pendingOrder[len(state.pendingOrder)-1]]
	case len(state.pendingRejections) != 0:
		state.latest = state.pendingRejections[state.rejectionOrder[len(state.rejectionOrder)-1]]
	default:
		state.latest = toolFailureSnapshot{}
	}
}

// evaluateExhaustion 在持锁下把穷尽从 false 翻成 true（累计计数只增不减，不会反向），并按
// 分因记度量一次：streak = 连击超限；cumulative = 连击未超但 turn 级累计超限（交替
// fail→success 被累计计数挡住，H4 / H-B P2「预算重叠」）。
func (state *toolRecoveryState) evaluateExhaustion() {
	if state.exhausted {
		return
	}
	streak := state.repairFailures >= maxModelRepairAttempts
	cumulative := state.cumulativeRepairFailures >= maxCumulativeRepairAttempts
	if !streak && !cumulative {
		return
	}
	state.exhausted = true
	if cumulative && !streak {
		metricRecoveryCumulativeExhausted.Inc()
	} else {
		metricRecoveryStreakExhausted.Inc()
	}
}

func (state *toolRecoveryState) unresolved() bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.hadFailure || len(state.pendingRejections) != 0
}

func (state *toolRecoveryState) recoveryExhausted() bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.exhausted
}

func (state *toolRecoveryState) summary() string {
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.hadFailure && len(state.pendingRejections) == 0 {
		return ""
	}
	if !state.hadFailure {
		latest := state.pendingRejections[state.rejectionOrder[len(state.rejectionOrder)-1]]
		return fmt.Sprintf(
			"工具：%s；参数：%s；策略拒绝：%s",
			latest.Tool,
			agentexec.TruncateText(canonicalToolArguments(latest.Arguments), 320),
			agentexec.TruncateText(latest.Observation, 600),
		)
	}
	return fmt.Sprintf(
		"工具：%s；参数：%s；最后错误：%s；模型修复失败次数：%d/%d（本回合累计 %d/%d）",
		state.latest.Tool,
		agentexec.TruncateText(canonicalToolArguments(state.latest.Arguments), 320),
		agentexec.TruncateText(state.latest.Observation, 600),
		state.repairFailures,
		maxModelRepairAttempts,
		state.cumulativeRepairFailures,
		maxCumulativeRepairAttempts,
	)
}

func toolCallFingerprint(name, arguments string) string {
	return name + "\x00" + canonicalToolArguments(arguments)
}

func timelineIDFromRecoveryTargetKey(target string) string {
	const prefix = "timeline.check\x00"
	if !strings.HasPrefix(target, prefix) {
		return ""
	}
	var identity map[string]any
	if json.Unmarshal([]byte(strings.TrimPrefix(target, prefix)), &identity) != nil {
		return ""
	}
	return agentexec.InterfaceString(identity["timeline_id"])
}

func timelineIDAtLeast(successID, failedID string) bool {
	successDraft, successVersion, successOK := splitTimelineID(successID)
	failedDraft, failedVersion, failedOK := splitTimelineID(failedID)
	return successOK && failedOK && successDraft == failedDraft && successVersion >= failedVersion
}

func splitTimelineID(timelineID string) (string, int, bool) {
	separator := strings.LastIndex(timelineID, ":v")
	if separator <= 0 {
		return "", 0, false
	}
	version, err := strconv.Atoi(timelineID[separator+2:])
	if err != nil || version < 0 {
		return "", 0, false
	}
	return timelineID[:separator], version, true
}

func committedMutationValidationTimelineID(name, raw string) string {
	if !isTerminalTimelineMutation(name) {
		return ""
	}
	var result rushestools.ToolResult
	if json.Unmarshal([]byte(raw), &result) != nil ||
		result.Status != string(rushestools.StatusValidationFailed) {
		return ""
	}
	timelineID := agentexec.InterfaceString(result.Data["timeline_id"])
	if !isValidTimelineVersionID(timelineID) {
		return ""
	}
	return timelineID
}

// toolRecoveryTargetKey 从参数中只提取稳定目标，让同一目标的修正参数成功能核销失败，
// 但同名工具操作另一个素材、clip 或 timeline 不能掩盖原失败。完全无法解码或没有
// 稳定目标的调用退回工具名，供 schema 参数修正与无参工具恢复。
func toolRecoveryTargetKey(name, arguments, rawResult string) string {
	// timeline.check 的空参数表示「当前版本」，真实 timeline_id 只在结果里。
	// 优先用结果 ID 归一化，使同版本的显式/省略参数能相互恢复，旧版本成功则不能核销新版本失败。
	if name == "timeline.check" && rawResult != "" {
		var result rushestools.ToolResult
		if json.Unmarshal([]byte(rawResult), &result) == nil {
			if timelineID := agentexec.InterfaceString(result.Data["timeline_id"]); timelineID != "" {
				encoded, _ := json.Marshal(map[string]any{"timeline_id": timelineID})
				return name + "\x00" + string(encoded)
			}
		}
	}
	var values map[string]any
	if json.Unmarshal([]byte(arguments), &values) != nil {
		return name
	}
	identity := map[string]any{}
	switch name {
	case "asset.import_local_file":
		if path, _ := values["path"].(string); path != "" {
			identity["path"] = filepath.Clean(path)
		}
	case "render.start":
		for _, key := range []string{"timeline_id", "kind"} {
			if value, exists := values[key]; exists {
				identity[key] = value
			}
		}
		orientation := strings.ToLower(strings.TrimSpace(agentexec.InterfaceString(values["orientation"])))
		if orientation == "" {
			orientation = "auto"
		}
		identity["orientation"] = orientation
	case "shot.search":
		if query, _ := values["query"].(string); strings.TrimSpace(query) != "" {
			identity["query"] = strings.ToLower(strings.Join(strings.Fields(query), " "))
		}
		for _, key := range []string{
			"asset_ids", "semantic_roles", "tags", "min_duration_frames", "max_duration_frames",
			"exclude_used", "after_shot_id",
		} {
			if value, exists := values[key]; exists {
				identity[key] = canonicalRecoveryIdentityValue(value)
			}
		}
	case "memory.set":
		keys := map[string]struct{}{}
		if entries, ok := values["entries"].([]any); ok {
			for _, rawEntry := range entries {
				entry, _ := rawEntry.(map[string]any)
				if key, _ := entry["key"].(string); key != "" {
					keys[key] = struct{}{}
				}
			}
		}
		if len(keys) != 0 {
			sortedKeys := make([]string, 0, len(keys))
			for key := range keys {
				sortedKeys = append(sortedKeys, key)
			}
			sort.Strings(sortedKeys)
			identity["keys"] = sortedKeys
		}
	}
	if len(identity) != 0 {
		encoded, err := json.Marshal(identity)
		if err != nil {
			return name
		}
		return name + "\x00" + string(encoded)
	}
	if name == "timeline.update" || name == "timeline.delete" || name == "timeline.split" {
		for _, key := range []string{"timeline_clip_id", "track_id", "clip_id"} {
			if value, exists := values[key]; exists {
				encoded, err := json.Marshal(map[string]any{key: value})
				if err == nil {
					return name + "\x00" + string(encoded)
				}
			}
		}
	}
	for _, key := range []string{
		"check", "timeline_id", "timeline_clip_id", "track_id", "clip_id",
		"asset_id", "asset_ids", "job_id", "preview_id", "decision_id", "keys",
	} {
		if value, exists := values[key]; exists {
			identity[key] = value
		}
	}
	// 已有时间线对象 ID 时，kind 和帧坐标都是可纠正的操作参数，不属于目标身份；
	// 否则操作种类及完整范围共同定义目标，避免另一个范围的成功掩盖当前失败。
	_, hasTimelineClipID := values["timeline_clip_id"]
	_, hasTrackID := values["track_id"]
	_, hasClipID := values["clip_id"]
	if !hasTimelineClipID && !hasTrackID && !hasClipID {
		for _, key := range []string{
			"kind", "start_frame", "end_frame", "source_start_frame", "source_end_frame",
			"timeline_start_frame", "timeline_end_frame",
		} {
			if value, exists := values[key]; exists {
				identity[key] = value
			}
		}
	}
	if len(identity) == 0 {
		return name
	}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return name
	}
	return name + "\x00" + string(encoded)
}

func canonicalRecoveryIdentityValue(value any) any {
	items, ok := value.([]any)
	if !ok {
		return value
	}
	stringsOnly := make([]string, 0, len(items))
	for _, item := range items {
		text, textOK := item.(string)
		if !textOK {
			return value
		}
		stringsOnly = append(stringsOnly, strings.TrimSpace(text))
	}
	sort.Strings(stringsOnly)
	return stringsOnly
}

func canonicalToolArguments(arguments string) string {
	var value any
	if json.Unmarshal([]byte(arguments), &value) == nil {
		if encoded, err := json.Marshal(value); err == nil {
			return string(encoded)
		}
	}
	return strings.TrimSpace(arguments)
}

func newToolRecoveryMiddleware(retrySafe func(string) bool) compose.ToolMiddleware {
	return compose.ToolMiddleware{
		Invokable: func(next compose.InvokableToolEndpoint) compose.InvokableToolEndpoint {
			return func(ctx context.Context, input *compose.ToolInput) (*compose.ToolOutput, error) {
				state := toolRecoveryFromContext(ctx)
				if state != nil {
					if decision := state.beforeCall(input.Name, input.Arguments); decision.blocked {
						return &compose.ToolOutput{Result: blockedToolCallOutput(input, decision)}, nil
					}
				}
				ctx = rushestools.WithToolCallID(ctx, input.CallID)

				// registry reporter 位于工具实现内部；若直接重放 next，每次内部重试都会
				// 在消息流和数据库里生成一条失败工具记录。保留第一次 started 以持续
				// 展示运行状态，只把最后一次 finished 交给原 reporter，从而让 1+5 次
				// 执行在用户界面仍表现为一次有明确终态的工具调用。
				originalReporter, hasReporter := rushestools.ReporterFromContext(ctx)
				var reportName string
				var reportContext context.Context
				var reportInput, reportOutput any
				var reportErr error
				reportStarted := false
				reportFinished := false
				if hasReporter {
					ctx = rushestools.WithReporter(ctx, func(
						reportCtx context.Context, name, phase string, reportedInput, output any, err error,
					) {
						reportContext = reportCtx
						switch phase {
						case "started":
							reportName, reportInput = name, reportedInput
							if reportStarted {
								return
							}
							reportStarted = true
							originalReporter(reportCtx, name, phase, reportedInput, nil, nil)
						case "finished":
							reportFinished = true
							reportName, reportInput = name, reportedInput
							reportOutput, reportErr = output, err
						}
					})
					defer func() {
						if !reportStarted {
							return
						}
						if !reportFinished && reportErr == nil {
							reportErr = errors.New("工具没有返回完成状态")
						}
						originalReporter(reportContext, reportName, "finished", reportInput, reportOutput, reportErr)
					}()
					// JSON/schema 解码和前置条件检查发生在注册工具的 reporter 之前。
					// 这里先发 started，实际工具若也上报 started 会被上面的包装器合并；
					// 因此任何失败路径都能在 UI 里形成唯一、完整的 started/finished。
					if reporter, ok := rushestools.ReporterFromContext(ctx); ok {
						reporter(ctx, input.Name, "started", toolArgumentsForReport(input.Arguments), nil, nil)
					}
				}

				attempts := 0
				var output *compose.ToolOutput
				var err error
				for {
					attempts++
					output, err = next(ctx, input)
					if err == nil || !toolErrorCanRetry(retrySafe, input.Name, err) || attempts > maxToolExecutionRetries {
						break
					}
					if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
						if !reportFinished {
							reportFinished, reportErr = true, err
						}
						return nil, err
					}
					if waitErr := waitForToolRetry(ctx, attempts); waitErr != nil {
						if !reportFinished {
							reportFinished, reportErr = true, waitErr
						}
						return nil, waitErr
					}
				}
				if err != nil {
					// 拦截器策略拒绝（如破坏性工具缺确认）：回灌模型结构化提示，但不算工具执行
					// 失败——不记恢复账、不触发重试、不消耗修复预算（#103 G2）。
					var rejection *rushestools.InterceptorRejection
					if errors.As(err, &rejection) {
						if state != nil && agentexec.InterfaceString(rejection.Data["error_code"]) ==
							string(rushestools.ErrCodeConfirmationRequired) {
							state.recordRejection(toolFailureSnapshot{
								Tool: input.Name, Arguments: input.Arguments,
								Observation: rejection.Observation,
							})
						}
						if !reportFinished {
							reportFinished = true
							reportOutput, reportErr = rejectionToolResult(rejection), nil
						}
						return &compose.ToolOutput{Result: marshalInterceptorRejection(rejection)}, nil
					}
					if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
						if !reportFinished {
							reportFinished, reportErr = true, err
						}
						return nil, err
					}
					if !reportFinished {
						reportFinished, reportErr = true, err
					}
					raw := executionErrorOutput(input.Name, err, attempts, toolErrorCanRetry(retrySafe, input.Name, err))
					return &compose.ToolOutput{Result: decorateToolFailure(ctx, input, raw, attempts)}, nil
				}
				if output == nil {
					missingResultErr := errors.New("工具没有返回结果")
					if !reportFinished {
						reportFinished, reportErr = true, missingResultErr
					}
					raw := executionErrorOutput(input.Name, missingResultErr, attempts, false)
					return &compose.ToolOutput{Result: decorateToolFailure(ctx, input, raw, attempts)}, nil
				}
				if !isStructuredToolFailure(output.Result) {
					output.Result = attachToolRequestFingerprint(input.Name, input.Arguments, output.Result)
				}
				draftID, _ := rushestools.DraftID(ctx)
				if isStructuredToolFailure(output.Result) {
					output.Result = decorateToolFailure(ctx, input, output.Result, attempts)
					reportFinished, reportOutput, reportErr = true, toolResultForReport(output.Result), nil
				} else if !isConfirmedToolRecoverySuccessWithExecutionProof(
					input.Name, input.Arguments, output.Result, draftID,
					validToolRequestFingerprint(input.Name, input.Arguments, output.Result),
				) {
					raw := marshalToolFailure(
						"工具返回了无法核验的结果状态，不能据此确认执行成功。",
						map[string]any{
							"error_code":       string(rushestools.ErrCodeToolExecutionError),
							"recovery":         "检查工具返回格式与状态；只有该工具约定的成功、排队或等待状态才能确认恢复。",
							"raw_result_bytes": len([]byte(output.Result)),
						},
					)
					output.Result = decorateToolFailure(ctx, input, raw, attempts)
					reportFinished, reportOutput, reportErr = true, toolResultForReport(output.Result), nil
				} else if state != nil {
					if input.Name == "interaction.confirm_action" {
						state.resolveConfirmation(input.Arguments, output.Result)
					}
					state.recordSuccess(input.Name, input.Arguments, output.Result)
				}
				observeToolResultSize(input.Name, output.Result)
				return output, nil
			}
		},
	}
}

func observeToolResultSize(name, result string) {
	bytes := len([]byte(result))
	metricToolResultBytes.Observe(int64(bytes))
	if bytes <= toolResultSoftLimitBytes {
		return
	}
	metricToolResultOversize.Inc()
	slog.Warn(
		"工具结果超过软上限，保持完整回灌并记录观测",
		"tool", name, "result_bytes", bytes, "soft_limit_bytes", toolResultSoftLimitBytes,
	)
}

func unknownToolRecoveryHandler(ctx context.Context, name, arguments string) (string, error) {
	input := &compose.ToolInput{Name: name, Arguments: arguments}
	if state := toolRecoveryFromContext(ctx); state != nil {
		if decision := state.beforeCall(name, arguments); decision.blocked {
			return blockedToolCallOutput(input, decision), nil
		}
	}
	raw := marshalToolFailure(
		"模型调用了不存在的工具："+name,
		map[string]any{
			"error_code": string(rushestools.ErrCodeUnknownTool),
			"recovery":   "只使用当前系统实际注册的工具名，并根据工具 schema 重新调用",
		},
	)
	output := decorateToolFailure(ctx, input, raw, 0)
	reportSyntheticToolFailure(ctx, name, arguments, output)
	return output, nil
}

func decorateToolFailure(
	ctx context.Context,
	input *compose.ToolInput,
	raw string,
	executionAttempts int,
) string {
	payload := map[string]any{}
	if json.Unmarshal([]byte(raw), &payload) != nil {
		payload = map[string]any{"status": string(rushestools.StatusFailed), "observation": agentexec.TruncateText(raw, 1000)}
	}
	payload["status"] = string(rushestools.StatusFailed)
	observation, _ := payload["observation"].(string)
	data, _ := payload["data"].(map[string]any)
	if data == nil {
		data = map[string]any{}
	}
	state := toolRecoveryFromContext(ctx)
	decision := recoveryDecision{}
	if state != nil {
		decision = state.recordFailure(toolFailureSnapshot{
			Tool: input.Name, Arguments: input.Arguments,
			Observation: observation, ExecutionAttempts: executionAttempts,
			TargetKey:           toolRecoveryTargetKey(input.Name, input.Arguments, raw),
			CommittedTimelineID: committedMutationValidationTimelineID(input.Name, raw),
		})
	}
	data["harness_recovery"] = recoveryMetadata(decision, executionAttempts)
	if budget := turnBudgetFromContext(ctx); budget != nil {
		data["remaining_tool_rounds"] = budget.remainingToolRounds()
	}
	payload["data"] = data
	encoded, err := json.Marshal(payload)
	if err != nil {
		return marshalToolFailure("工具失败，且失败详情无法序列化", map[string]any{
			"error_code": string(rushestools.ErrCodeFailureSerialization),
			"recovery":   "重试上一步工具调用；若仍失败，缩小参数范围或改用其他工具。",
		})
	}
	return string(encoded)
}

func blockedToolCallOutput(input *compose.ToolInput, decision recoveryDecision) string {
	observation := "检测到与之前完全相同的失败工具调用，已跳过重复执行。必须修改参数、先读取最新状态，或改用其他工具。"
	errorCode := rushestools.ErrCodeDuplicateFailedToolCall
	recovery := "必须修改参数、先读取最新状态，或改用其他工具后再重试，不要原样重复同一调用。"
	if decision.exhausted {
		observation = "工具自修复次数已经用尽。停止继续调用工具，立即向用户说明未完成的步骤、最后错误，并等待下一步指令。"
		errorCode = rushestools.ErrCodeToolRecoveryExhausted
		recovery = "停止继续调用工具，立即向用户说明未完成的步骤与最后错误，并等待下一步指令。"
	}
	return marshalToolFailure(observation, map[string]any{
		"error_code":       string(errorCode),
		"recovery":         recovery,
		"tool":             input.Name,
		"last_failure":     agentexec.TruncateText(decision.latest.Observation, 600),
		"harness_recovery": recoveryMetadata(decision, decision.latest.ExecutionAttempts),
	})
}

func recoveryMetadata(decision recoveryDecision, executionAttempts int) map[string]any {
	remaining := max(0, maxModelRepairAttempts-decision.repairAttempt)
	action := "读取 observation 和 data，修改参数后再调用；不得原样重复同一工具调用"
	if decision.exhausted {
		remaining = 0
		action = "停止工具调用，向用户明确说明失败原因并等待下一步指令"
	}
	return map[string]any{
		"execution_attempts":      executionAttempts,
		"automatic_retries":       max(0, executionAttempts-1),
		"model_repair_attempt":    decision.repairAttempt,
		"max_model_repairs":       maxModelRepairAttempts,
		"remaining_model_repairs": remaining,
		"duplicate_call_blocked":  decision.duplicate,
		"exhausted":               decision.exhausted,
		"next_action":             action,
	}
}

func executionErrorOutput(name string, err error, attempts int, retryable bool) string {
	recovery := "根据错误修改参数或先调用 inspect/list 工具读取最新状态"
	if retryable {
		recovery = "已完成有限次自动重试；请根据最终错误调整方案，不要原样重复"
	}
	return marshalToolFailure("工具 "+name+" 执行失败："+agentexec.TruncateText(err.Error(), 800), map[string]any{
		"error_code":         string(rushestools.ErrCodeToolExecutionError),
		"retryable":          retryable,
		"execution_attempts": attempts,
		"recovery":           recovery,
	})
}

func marshalToolFailure(observation string, data map[string]any) string {
	encoded, _ := json.Marshal(map[string]any{
		"status": string(rushestools.StatusFailed), "observation": observation, "data": data,
	})
	return string(encoded)
}

func isStructuredToolFailure(raw string) bool {
	var payload struct {
		Status string `json:"status"`
	}
	if json.Unmarshal([]byte(raw), &payload) != nil {
		return false
	}
	return payload.Status == string(rushestools.StatusFailed) || payload.Status == string(rushestools.StatusValidationFailed)
}

func toolResultForReport(raw string) any {
	var result rushestools.ToolResult
	if json.Unmarshal([]byte(raw), &result) == nil {
		return result
	}
	return raw
}

const toolRequestFingerprintField = "request_fingerprint"

func toolRequestFingerprint(name, arguments string) string {
	var value any
	if json.Unmarshal([]byte(arguments), &value) != nil {
		return ""
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(append(append([]byte(name), 0), canonical...))
	return fmt.Sprintf("sha256:%x", digest[:])
}

func attachToolRequestFingerprint(name, arguments, raw string) string {
	fingerprint := toolRequestFingerprint(name, arguments)
	if fingerprint == "" {
		return raw
	}
	var payload map[string]any
	if json.Unmarshal([]byte(raw), &payload) != nil || payload == nil {
		return raw
	}
	payload[toolRequestFingerprintField] = fingerprint
	encoded, err := json.Marshal(payload)
	if err != nil {
		return raw
	}
	return string(encoded)
}

func validToolRequestFingerprint(name, arguments, raw string) bool {
	expected := toolRequestFingerprint(name, arguments)
	if expected == "" {
		return false
	}
	var payload map[string]any
	return json.Unmarshal([]byte(raw), &payload) == nil &&
		agentexec.InterfaceString(payload[toolRequestFingerprintField]) == expected
}

func confirmedToolResultSuccessWithExecutionProof(
	name string,
	arguments any,
	result rushestools.ToolResult,
	draftID string,
	fullRequestBound bool,
) bool {
	switch result.Status {
	case string(rushestools.StatusSucceeded):
		switch {
		case isTerminalTimelineMutation(name):
			return fullRequestBound && timelineVersionBelongsToDraft(
				agentexec.InterfaceString(result.Data["timeline_id"]), draftID,
			)
		case name == "timeline.check":
			return validRequestedTimelineProof(arguments, result, true, draftID) &&
				validSuccessfulTimelineCheck(result)
		case name == "timeline.inspect":
			return validRequestedTimelineProof(arguments, result, false, draftID)
		case name == "render.start":
			return validRenderDispatchProof(arguments, result, "succeeded")
		case name == "job.read":
			return matchesRequestedString(arguments, "job_id", agentexec.InterfaceString(result.Data["job_id"]))
		case name == "memory.set":
			return matchesRequestedMemoryKeys(arguments, result, "entries", "written_keys")
		case name == "memory.remove":
			return matchesRemovedMemoryKeys(arguments, result)
		case name == "interaction.ask_user":
			return fullRequestBound && validDecisionProof(result, false)
		case name == "interaction.confirm_action":
			return false
		case name == "decision.answer", name == "plan.update", name == "asset.import_local_file":
			return fullRequestBound
		default:
			return false
		}
	case string(rushestools.StatusQueued):
		return name == "render.start" && validRenderDispatchProof(arguments, result, "queued")
	case string(rushestools.StatusWaiting):
		return (name == "interaction.ask_user" || name == "interaction.confirm_action") &&
			fullRequestBound && validDecisionProof(result, true)
	default:
		return false
	}
}

func timelineVersionBelongsToDraft(timelineID, draftID string) bool {
	timelineDraftID, _, valid := splitTimelineID(strings.TrimSpace(timelineID))
	return valid && strings.TrimSpace(draftID) != "" && timelineDraftID == strings.TrimSpace(draftID)
}

func validSuccessfulTimelineCheck(result rushestools.ToolResult) bool {
	encoded, err := json.Marshal(result.Data["validation_report"])
	if err != nil {
		return false
	}
	var rawReport map[string]json.RawMessage
	if json.Unmarshal(encoded, &rawReport) != nil {
		return false
	}
	var report struct {
		Valid                *bool                                 `json:"valid"`
		StructuralValid      *bool                                 `json:"structural_valid"`
		ContentContractValid *bool                                 `json:"content_contract_valid"`
		Checks               []string                              `json:"checks"`
		Issues               []timeline.ValidationIssue            `json:"issues"`
		ContentContract      *agentexec.ContractVerificationReport `json:"content_contract"`
	}
	if json.Unmarshal(encoded, &report) != nil ||
		report.Valid == nil || !*report.Valid ||
		report.StructuralValid == nil || !*report.StructuralValid ||
		report.ContentContractValid == nil || !*report.ContentContractValid ||
		report.Checks == nil || len(report.Checks) == 0 || report.Issues == nil ||
		len(report.Issues) != 0 {
		return false
	}
	seenChecks := make(map[string]struct{}, len(report.Checks))
	for _, check := range report.Checks {
		check = strings.TrimSpace(check)
		if check == "" {
			return false
		}
		if _, duplicate := seenChecks[check]; duplicate {
			return false
		}
		seenChecks[check] = struct{}{}
	}
	if report.ContentContract == nil {
		if _, hasNestedContract := rawReport["content_contract"]; hasNestedContract {
			return false
		}
		if _, hasTopLevel := result.Data["content_contract"]; hasTopLevel {
			return false
		}
		return validEmptyContractFailures(result.Data, false)
	}
	if !report.ContentContract.Pass {
		return false
	}
	if !validRawPassingContract(rawReport["content_contract"], result.Data["content_contract"]) {
		return false
	}
	if report.ContentContract.Items == nil {
		return false
	}
	seenContractChecks := make(map[string]struct{}, len(report.ContentContract.Items))
	for _, item := range report.ContentContract.Items {
		check := strings.TrimSpace(item.Check)
		if !item.Pass || check == "" || strings.TrimSpace(item.ErrorCode) != "" ||
			strings.TrimSpace(item.Message) == "" {
			return false
		}
		if _, duplicate := seenContractChecks[check]; duplicate {
			return false
		}
		seenContractChecks[check] = struct{}{}
	}
	return validEmptyContractFailures(result.Data, true)
}

func validRawPassingContract(nested json.RawMessage, topLevel any) bool {
	if len(nested) == 0 || strings.TrimSpace(string(nested)) == "null" {
		return false
	}
	var nestedValue any
	if json.Unmarshal(nested, &nestedValue) != nil {
		return false
	}
	encodedTopLevel, err := json.Marshal(topLevel)
	if err != nil {
		return false
	}
	var topLevelValue any
	if json.Unmarshal(encodedTopLevel, &topLevelValue) != nil || !reflect.DeepEqual(nestedValue, topLevelValue) {
		return false
	}
	var rawContract struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if json.Unmarshal(nested, &rawContract) != nil || rawContract.Items == nil {
		return false
	}
	for _, item := range rawContract.Items {
		if rawErrorCode, exists := item["error_code"]; exists && !validJSONString(rawErrorCode) {
			return false
		}
	}
	return true
}

func validJSONString(raw json.RawMessage) bool {
	if strings.TrimSpace(string(raw)) == "null" {
		return false
	}
	var value string
	return json.Unmarshal(raw, &value) == nil
}

func validEmptyContractFailures(data map[string]any, required bool) bool {
	rawFailures, exists := data["contract_failures"]
	if !exists {
		return !required
	}
	encodedFailures, err := json.Marshal(rawFailures)
	if err != nil {
		return false
	}
	var failures []agentexec.ContractVerificationItem
	if json.Unmarshal(encodedFailures, &failures) != nil || failures == nil {
		return false
	}
	return len(failures) == 0
}

func validRequestedTimelineProof(
	arguments any,
	result rushestools.ToolResult,
	requireTimeline bool,
	draftID string,
) bool {
	requested := strings.TrimSpace(agentexec.InterfaceString(toolArgumentsObject(arguments)["timeline_id"]))
	returned := strings.TrimSpace(agentexec.InterfaceString(result.Data["timeline_id"]))
	if requested != "" {
		return returned == requested && isValidTimelineVersionID(returned)
	}
	if returned != "" {
		returnedDraftID, _, valid := splitTimelineID(returned)
		return valid && draftID != "" && returnedDraftID == draftID
	}
	return !requireTimeline && draftID != "" && result.Data["timeline_exists"] == false
}

func matchesRequestedMemoryKeys(arguments any, result rushestools.ToolResult, requestField, resultField string) bool {
	requested := requestedMemoryKeys(arguments, requestField)
	returned, valid := recoveryStringSet(result.Data[resultField])
	return len(requested) > 0 && valid && reflect.DeepEqual(requested, returned)
}

func matchesRemovedMemoryKeys(arguments any, result rushestools.ToolResult) bool {
	requested := requestedMemoryKeys(arguments, "keys")
	removed, valid := recoveryStringSet(result.Data["removed_keys"])
	if len(requested) == 0 || !valid ||
		!optionalEmptyRecoveryStringSet(result.Data, "written_keys") ||
		!optionalEmptyRecoveryStringSet(result.Data, "evicted_keys") {
		return false
	}
	for key := range removed {
		if _, requestedKey := requested[key]; !requestedKey {
			return false
		}
	}
	return true
}

func optionalEmptyRecoveryStringSet(data map[string]any, field string) bool {
	value, exists := data[field]
	if !exists {
		return true
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return false
	}
	var values []string
	return json.Unmarshal(encoded, &values) == nil && values != nil && len(values) == 0
}

func requestedMemoryKeys(arguments any, field string) map[string]struct{} {
	raw := toolArgumentsObject(arguments)[field]
	values, ok := raw.([]any)
	if !ok {
		return nil
	}
	keys := make(map[string]struct{}, len(values))
	for _, value := range values {
		if object, objectOK := value.(map[string]any); objectOK {
			value = object["key"]
		}
		key := strings.TrimSpace(agentexec.InterfaceString(value))
		if key == "" {
			return nil
		}
		keys[key] = struct{}{}
	}
	return keys
}

func recoveryStringSet(value any) (map[string]struct{}, bool) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	var values []string
	if json.Unmarshal(encoded, &values) != nil {
		return nil, false
	}
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, false
		}
		if _, duplicate := result[value]; duplicate {
			return nil, false
		}
		result[value] = struct{}{}
	}
	return result, true
}

func validRenderDispatchProof(arguments any, result rushestools.ToolResult, status string) bool {
	if strings.TrimSpace(agentexec.InterfaceString(result.Data["job_id"])) == "" ||
		positiveInteger(result.Data["timeline_version"]) <= 0 {
		return false
	}
	argumentMap := toolArgumentsObject(arguments)
	timelineID := strings.TrimSpace(agentexec.InterfaceString(argumentMap["timeline_id"]))
	_, requestedVersion, validTimelineID := splitTimelineID(timelineID)
	requestedKind := strings.ToLower(strings.TrimSpace(agentexec.InterfaceString(argumentMap["kind"])))
	requestedOrientation := strings.ToLower(strings.TrimSpace(agentexec.InterfaceString(argumentMap["orientation"])))
	if requestedOrientation == "" {
		requestedOrientation = "auto"
	}
	if !validTimelineID || int64(requestedVersion) != positiveInteger(result.Data["timeline_version"]) ||
		strings.TrimSpace(agentexec.InterfaceString(result.Data["timeline_id"])) != timelineID ||
		(requestedKind != "preview" && requestedKind != "final") ||
		strings.ToLower(strings.TrimSpace(agentexec.InterfaceString(result.Data["render_kind"]))) != requestedKind ||
		(requestedOrientation != "auto" && requestedOrientation != "portrait" && requestedOrientation != "landscape") ||
		strings.ToLower(strings.TrimSpace(agentexec.InterfaceString(result.Data["orientation"]))) != requestedOrientation {
		return false
	}
	jobStatus := agentexec.InterfaceString(result.Data["job_status"])
	if status == "queued" {
		return jobStatus == "pending" || jobStatus == "running"
	}
	return status == "succeeded" && jobStatus == "succeeded"
}

func validDecisionProof(result rushestools.ToolResult, shouldEnd bool) bool {
	if strings.TrimSpace(agentexec.InterfaceString(result.Data["decision_id"])) == "" {
		return false
	}
	turnShouldEnd, ok := result.Data["turn_should_end"].(bool)
	return ok && turnShouldEnd == shouldEnd
}

func toolArgumentsObject(arguments any) map[string]any {
	switch typed := arguments.(type) {
	case map[string]any:
		return typed
	case string:
		var values map[string]any
		if json.Unmarshal([]byte(typed), &values) == nil {
			return values
		}
		return nil
	default:
		encoded, err := json.Marshal(arguments)
		if err != nil {
			return nil
		}
		var values map[string]any
		if json.Unmarshal(encoded, &values) != nil {
			return nil
		}
		return values
	}
}

func positiveInteger(value any) int64 {
	switch typed := value.(type) {
	case int:
		return int64(typed)
	case int32:
		return int64(typed)
	case int64:
		return typed
	case float64:
		if typed > 0 && typed == float64(int64(typed)) {
			return int64(typed)
		}
	}
	return 0
}

func isConfirmedToolRecoverySuccess(name string, arguments any, raw, draftID string) bool {
	argumentJSON, _ := json.Marshal(arguments)
	if text, ok := arguments.(string); ok {
		argumentJSON = []byte(text)
	}
	return isConfirmedToolRecoverySuccessWithExecutionProof(
		name, arguments, raw, draftID,
		validToolRequestFingerprint(name, string(argumentJSON), raw),
	)
}

func isConfirmedToolRecoverySuccessWithExecutionProof(
	name string,
	arguments any,
	raw, draftID string,
	fullRequestBound bool,
) bool {
	var payload map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &payload) != nil || payload == nil {
		return false
	}
	if isTypedToolRecoveryResult(name) {
		if _, hasStatus := payload["status"]; hasStatus {
			return false
		}
		return validTypedToolRecoveryResult(name, arguments, raw, payload, draftID, fullRequestBound)
	}
	var status string
	if encodedStatus, exists := payload["status"]; exists {
		if json.Unmarshal(encodedStatus, &status) != nil {
			return false
		}
		if name == "media.detect_shots" {
			return fullRequestBound && validDetectShotsProof(arguments, raw, status, draftID)
		}
		if name == "speech.search" {
			return fullRequestBound && validSpeechSearchProof(arguments, raw, payload, status)
		}
		var result rushestools.ToolResult
		return json.Unmarshal([]byte(raw), &result) == nil &&
			confirmedToolResultSuccessWithExecutionProof(name, arguments, result, draftID, fullRequestBound)
	}
	return false
}

func isTypedToolRecoveryResult(name string) bool {
	switch name {
	case "asset.list_assets", "shot.search", "audio.analyze_beats",
		"audio.analyze_speech_pauses", "speech.transcribe", "preview.check":
		return true
	default:
		return false
	}
}

func validDetectShotsProof(arguments any, raw, status, draftID string) bool {
	var result rushestools.DetectShotsResult
	if json.Unmarshal([]byte(raw), &result) != nil || result.Status != status ||
		strings.TrimSpace(draftID) == "" || strings.TrimSpace(result.DraftID) != strings.TrimSpace(draftID) ||
		!matchesRequestedString(arguments, "asset_id", result.AssetID) {
		return false
	}
	switch status {
	case string(rushestools.StatusQueued):
		return strings.TrimSpace(result.JobID) != ""
	case "completed":
		return result.Summary != nil &&
			strings.TrimSpace(result.Summary.AssetID) == strings.TrimSpace(result.AssetID) &&
			result.Summary.TimelineFPS > 0
	default:
		return false
	}
}

func validSpeechSearchProof(
	arguments any,
	raw string,
	payload map[string]json.RawMessage,
	status string,
) bool {
	for _, field := range []string{
		"status", "transcript_id", "asset_id", "timeline_fps", "provider_id",
		"utterances", "utterance_total", "truncated", "usage_note",
	} {
		if _, exists := payload[field]; !exists {
			return false
		}
	}
	var result rushestools.SpeechSearchResult
	if status != string(rushestools.StatusSucceeded) ||
		json.Unmarshal([]byte(raw), &result) != nil ||
		result.Status != string(rushestools.StatusSucceeded) ||
		strings.TrimSpace(result.TranscriptID) == "" || result.TimelineFPS <= 0 ||
		result.UtteranceTotal < 0 || len(result.Utterances) > result.UtteranceTotal {
		return false
	}
	argumentMap := toolArgumentsObject(arguments)
	requestedAssetID := strings.TrimSpace(agentexec.InterfaceString(argumentMap["asset_id"]))
	requestedClipID := strings.TrimSpace(agentexec.InterfaceString(argumentMap["timeline_clip_id"]))
	return (requestedAssetID != "" || requestedClipID != "") &&
		(requestedAssetID == "" || strings.TrimSpace(result.AssetID) == requestedAssetID) &&
		(requestedClipID == "" || strings.TrimSpace(result.TimelineClipID) == requestedClipID)
}

func validTypedToolRecoveryResult(
	name string,
	arguments any,
	raw string,
	payload map[string]json.RawMessage,
	draftID string,
	fullRequestBound bool,
) bool {
	requiredFields := map[string][]string{
		"asset.list_assets":           {"draft_id", "assets", "total"},
		"shot.search":                 {"shots", "total_matches", "page_start", "remaining_matches", "truncated"},
		"audio.analyze_beats":         {"asset_id", "timeline_fps", "beat_frames"},
		"audio.analyze_speech_pauses": {"timeline_fps", "pauses"},
		"speech.transcribe":           {"transcript_id", "asset_id", "timeline_fps"},
		"preview.check":               {"preview_id", "check", "issues"},
	}
	for _, field := range requiredFields[name] {
		if _, exists := payload[field]; !exists {
			return false
		}
	}
	switch name {
	case "asset.list_assets":
		var result rushestools.AssetListResult
		if json.Unmarshal([]byte(raw), &result) != nil || strings.TrimSpace(result.DraftID) == "" ||
			result.Assets == nil || result.Total < 0 || len(result.Assets) > result.Total {
			return false
		}
		if draftID != "" && strings.TrimSpace(result.DraftID) != draftID {
			return false
		}
		argumentMap := toolArgumentsObject(arguments)
		requestedKind := agentexec.InterfaceString(argumentMap["kind"])
		after := agentexec.InterfaceString(argumentMap["after"])
		requestedLimit := int(positiveInteger(argumentMap["limit"]))
		if requestedLimit == 0 || requestedLimit > 200 {
			requestedLimit = 200
		}
		onlyUsable, hasOnlyUsable := argumentMap["only_usable"]
		requestedUsable, usableIsBool := onlyUsable.(bool)
		if hasOnlyUsable && !usableIsBool {
			return false
		}
		for _, asset := range result.Assets {
			if strings.TrimSpace(asset.AssetID) == "" {
				return false
			}
			if requestedKind != "" && asset.Kind != requestedKind ||
				hasOnlyUsable && asset.Usable != requestedUsable ||
				after != "" && asset.AssetID <= after {
				return false
			}
		}
		if len(result.Assets) > requestedLimit {
			return false
		}
		if result.NextAfter != "" {
			return len(result.Assets) == requestedLimit && result.Total > len(result.Assets) &&
				result.NextAfter == result.Assets[len(result.Assets)-1].AssetID
		}
		return result.Total == len(result.Assets)
	case "shot.search":
		if !fullRequestBound {
			return false
		}
		rawShots, hasShots := payload["shots"]
		var encodedShots []json.RawMessage
		if !hasShots || json.Unmarshal(rawShots, &encodedShots) != nil {
			return false
		}
		for _, field := range []string{"total_matches", "page_start", "remaining_matches"} {
			if !validJSONInteger(payload[field]) {
				return false
			}
		}
		if !validJSONBoolean(payload["truncated"]) {
			return false
		}
		var result rushestools.ShotSearchResult
		if json.Unmarshal([]byte(raw), &result) != nil || result.TotalMatches < 0 ||
			len(result.Shots) > result.TotalMatches ||
			strings.TrimSpace(result.Query) != strings.TrimSpace(
				agentexec.InterfaceString(toolArgumentsObject(arguments)["query"]),
			) {
			return false
		}
		requestedAssets := requestedStringSet(arguments, "asset_ids")
		requestedRoles := requestedLowercaseStringSet(arguments, "semantic_roles")
		requestedTags := requestedStringSet(arguments, "tags")
		argumentMap := toolArgumentsObject(arguments)
		minDuration := int(positiveInteger(argumentMap["min_duration_frames"]))
		maxDuration := int(positiveInteger(argumentMap["max_duration_frames"]))
		limit := int(positiveInteger(argumentMap["limit"]))
		if limit == 0 {
			limit = 20
		}
		if limit > 100 {
			limit = 100
		}
		if len(result.Shots) > limit {
			return false
		}
		afterShotID := strings.TrimSpace(agentexec.InterfaceString(argumentMap["after_shot_id"]))
		if result.PageStart < 0 || result.RemainingMatches < 0 ||
			result.PageStart+len(result.Shots)+result.RemainingMatches != result.TotalMatches ||
			(afterShotID == "" && (result.PageStart != 0 || result.PageAfterShotID != "")) ||
			(afterShotID != "" && (result.PageStart <= 0 || result.PageAfterShotID != afterShotID)) ||
			result.Truncated != (result.RemainingMatches > 0) {
			return false
		}
		seenShotIDs := make(map[string]struct{}, len(result.Shots))
		for _, shot := range result.Shots {
			shotID := strings.TrimSpace(shot.ShotID)
			if shotID == "" || strings.TrimSpace(shot.AssetID) == "" || shotID == afterShotID ||
				shot.SourceEndFrame <= shot.SourceStartFrame ||
				shot.DurationFrames != shot.SourceEndFrame-shot.SourceStartFrame {
				return false
			}
			if _, duplicate := seenShotIDs[shotID]; duplicate {
				return false
			}
			seenShotIDs[shotID] = struct{}{}
			if len(requestedAssets) != 0 {
				if _, requested := requestedAssets[shot.AssetID]; !requested {
					return false
				}
			}
			if len(requestedRoles) != 0 {
				if _, requested := requestedRoles[strings.TrimSpace(shot.SemanticRole)]; !requested {
					return false
				}
			}
			if minDuration > 0 && shot.DurationFrames < minDuration ||
				maxDuration > 0 && shot.DurationFrames > maxDuration {
				return false
			}
			if len(requestedTags) != 0 && !shotMatchesAnyRequestedTag(shot, requestedTags) {
				return false
			}
		}
		if result.Truncated {
			return len(result.Shots) == limit && len(result.Shots) > 0 &&
				result.NextAfterShotID == result.Shots[len(result.Shots)-1].ShotID &&
				result.TotalMatches > len(result.Shots)
		}
		if result.NextAfterShotID != "" {
			return false
		}
		return result.PageStart+len(result.Shots) == result.TotalMatches
	case "audio.analyze_beats":
		if !fullRequestBound {
			return false
		}
		var result rushestools.AudioBeatAnalysisResult
		return json.Unmarshal([]byte(raw), &result) == nil &&
			matchesRequestedString(arguments, "asset_id", result.AssetID) &&
			result.TimelineFPS > 0 && result.BeatFrames != nil
	case "audio.analyze_speech_pauses":
		if !fullRequestBound {
			return false
		}
		var result rushestools.SpeechPauseAnalysisResult
		if json.Unmarshal([]byte(raw), &result) != nil || strings.TrimSpace(result.AssetID) == "" ||
			result.TimelineFPS <= 0 || result.Pauses == nil {
			return false
		}
		argumentMap := toolArgumentsObject(arguments)
		requestedAssetID := strings.TrimSpace(agentexec.InterfaceString(argumentMap["asset_id"]))
		requestedClipID := strings.TrimSpace(agentexec.InterfaceString(argumentMap["timeline_clip_id"]))
		return (requestedAssetID != "" || requestedClipID != "") &&
			(requestedAssetID == "" || strings.TrimSpace(result.AssetID) == requestedAssetID) &&
			(requestedClipID == "" || strings.TrimSpace(result.TimelineClipID) == requestedClipID)
	case "speech.transcribe":
		if !fullRequestBound {
			return false
		}
		var result rushestools.SpeechTranscribeResult
		return json.Unmarshal([]byte(raw), &result) == nil &&
			strings.TrimSpace(result.TranscriptID) != "" &&
			matchesRequestedString(arguments, "asset_id", result.AssetID) &&
			result.TimelineFPS > 0
	case "preview.check":
		var result rushestools.PreviewInspectionResult
		return json.Unmarshal([]byte(raw), &result) == nil &&
			matchesRequestedString(arguments, "preview_id", result.PreviewID) &&
			matchesRequestedPreviewCheck(arguments, result.Check) && result.Issues != nil
	default:
		return false
	}
}

func validJSONInteger(raw json.RawMessage) bool {
	if strings.TrimSpace(string(raw)) == "null" {
		return false
	}
	var value int
	return json.Unmarshal(raw, &value) == nil
}

func validJSONBoolean(raw json.RawMessage) bool {
	if strings.TrimSpace(string(raw)) == "null" {
		return false
	}
	var value bool
	return json.Unmarshal(raw, &value) == nil
}

func shotMatchesAnyRequestedTag(shot rushestools.ShotCandidate, requested map[string]struct{}) bool {
	requestedValues := make([]string, 0, len(requested))
	for value := range requested {
		requestedValues = append(requestedValues, value)
	}
	tokens := agentexec.SemanticTokens(strings.Join(requestedValues, " "))
	return len(tokens) != 0 &&
		agentexec.WeightedSemanticMatchScore(tokens, agentexec.ShotSemanticText(shot)) > 0
}

func matchesRequestedString(arguments any, field, resultValue string) bool {
	requested := strings.TrimSpace(agentexec.InterfaceString(toolArgumentsObject(arguments)[field]))
	return requested != "" && strings.TrimSpace(resultValue) == requested
}

func requestedStringSet(arguments any, field string) map[string]struct{} {
	values, ok := toolArgumentsObject(arguments)[field].([]any)
	if !ok {
		return nil
	}
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		text, textOK := value.(string)
		if !textOK || strings.TrimSpace(text) == "" {
			return nil
		}
		result[strings.TrimSpace(text)] = struct{}{}
	}
	return result
}

func requestedLowercaseStringSet(arguments any, field string) map[string]struct{} {
	values := requestedStringSet(arguments, field)
	if values == nil {
		return nil
	}
	result := make(map[string]struct{}, len(values))
	for value := range values {
		result[strings.ToLower(value)] = struct{}{}
	}
	return result
}

func matchesRequestedPreviewCheck(arguments any, resultCheck string) bool {
	requested := strings.TrimSpace(agentexec.InterfaceString(
		toolArgumentsObject(arguments)["check"],
	))
	resultCheck = strings.TrimSpace(resultCheck)
	return validPreviewCheck(requested) && resultCheck == requested
}

func validPreviewCheck(check string) bool {
	switch strings.TrimSpace(check) {
	case "decode", "black", "freeze", "silence", "loudness", "visual":
		return true
	default:
		return false
	}
}

// retrySafeFromEffect 从工具注册表的 Effect 分级派生「瞬时失败可重试」白名单（#103 G1）。
// 只有纯 read/check 由 EffectReadOnly 自动放行；任何持久化调用在提交结果未知时都不能盲目重放。
func retrySafeFromEffect(effectOf func(string) (rushestools.Effect, bool)) func(string) bool {
	return func(name string) bool {
		effect, ok := effectOf(name)
		return ok && effect == rushestools.EffectReadOnly
	}
}

// toolErrorCanRetry intentionally requires both a retry-safe tool (derived from
// the registry Effect classification) and a recognisably transient failure.
// Retrying invalid JSON, schema violations, missing IDs or failed preconditions
// with identical arguments cannot heal and only hides useful feedback from the model.
func toolErrorCanRetry(retrySafe func(string) bool, name string, err error) bool {
	if err == nil || !retrySafe(name) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		isInterceptorRejection(err) {
		return false
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return true
	}
	message := strings.ToLower(strings.TrimSpace(err.Error()))
	for _, marker := range []string{
		"temporary", "temporarily", "timed out", "timeout", "i/o timeout",
		"connection reset", "connection refused", "connection aborted", "broken pipe",
		"service unavailable", "resource exhausted", "rate limit", "too many requests",
		"database is locked", "database is busy", "unexpected eof",
		"status 429", "status 502", "status 503", "status 504",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func toolArgumentsForReport(arguments string) any {
	var value any
	if json.Unmarshal([]byte(arguments), &value) == nil {
		return value
	}
	return map[string]any{"raw_arguments": agentexec.TruncateText(arguments, 1000)}
}

func reportSyntheticToolFailure(ctx context.Context, name, arguments, rawOutput string) {
	reporter, ok := rushestools.ReporterFromContext(ctx)
	if !ok {
		return
	}
	input := toolArgumentsForReport(arguments)
	reporter(ctx, name, "started", input, nil, nil)
	var result rushestools.ToolResult
	if json.Unmarshal([]byte(rawOutput), &result) == nil {
		reporter(ctx, name, "finished", input, result, nil)
		return
	}
	reporter(ctx, name, "finished", input, nil, errors.New(agentexec.TruncateText(rawOutput, 1000)))
}

func waitForToolRetry(ctx context.Context, attempt int) error {
	delay := time.Duration(1<<min(attempt-1, 4)) * 10 * time.Millisecond
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// emitAssistantReply 把一段最终回复按增量推送到 turn 流，并原样返回内容。
func (service *Service) emitAssistantReply(draftID, messageID, content string) string {
	for _, delta := range runeChunks(content, 12) {
		service.hub.Record(draftID, StreamEvent{
			"type": TurnStreamTextDelta, "message_id": messageID,
			"kind": "assistant", "delta": delta,
		})
	}
	return content
}

func terminalFailureReply(ctx context.Context, turnErr error) string {
	var guardErr *terminalReplyGuardError
	if errors.As(turnErr, &guardErr) {
		switch guardErr.kind {
		case "recovery_exhausted":
			return "本轮没有完成：工具自修复次数已经用尽，系统已停止继续调用。最后问题：" +
				agentexec.TruncateText(guardErr.details, 800) +
				"。当前时间线保留在最新已成功写入的版本；你可以继续让我从这里诊断或修复。"
		case "tool_failure_unresolved":
			return "本轮没有完成：工具调用仍处于失败状态，系统已拒绝未验收的成功声明。最后问题：" +
				agentexec.TruncateText(guardErr.details, 800) +
				"。当前时间线保留在最新已成功写入的版本；你可以继续让我从这里诊断或修复。"
		case "timeline_check_missing":
			return fmt.Sprintf(
				"本轮没有完成终态验收：编辑已写入 %s，但尚未对这个最新版本执行成功的 timeline.check，因此我不能声称剪辑已经完成。你可以继续让我验证并修复未通过项。",
				guardErr.mutationTimelineID,
			)
		case "timeline_mutation_unverified":
			return "本轮没有完成终态验收：时间线编辑返回了成功状态，但没有携带有效的 timeline_id，系统无法确认实际写入版本，因此已拒绝成功声明。你可以继续让我读取最新时间线并重新检查。"
		case "timeline_check_unverified":
			return "本轮没有完成终态验收：timeline.check 返回了成功状态，但没有携带有效的 timeline_id，系统无法确认实际检查版本，因此已拒绝成功声明。你可以继续让我读取最新时间线并重新检查。"
		case "timeline_check_stale":
			return fmt.Sprintf(
				"本轮没有完成终态验收：最新编辑是 %s，但最后成功检查的是 %s，检查结果已经过期，因此我不能声称剪辑已经完成。你可以继续让我检查最新版本。",
				guardErr.mutationTimelineID,
				guardErr.checkTimelineID,
			)
		case "timeline_latest_changed":
			return fmt.Sprintf(
				"本轮没有完成终态验收：检查后时间线又发生了变化，已编辑版本是 %s，当前最新版本是 %s，因此我不能声称剪辑已经完成。你可以继续让我读取并检查最新版本。",
				guardErr.mutationTimelineID,
				guardErr.latestTimelineID,
			)
		case "terminal_late_tool_call":
			return "本轮没有完成：模型在最终回复中又请求了工具调用，但该调用未被执行。系统已丢弃未验收的成功正文；你可以继续让我重新执行并检查最新时间线。"
		}
	}
	var timeoutErr *modelResponseTimeoutError
	if errors.As(turnErr, &timeoutErr) {
		return fmt.Sprintf(
			"本轮没有完成：模型响应超时，已自动重试 %d 次仍未恢复。系统已停止重试。你可以继续给出下一步指令，我会从当前最新时间线接着执行。",
			timeoutErr.Retries,
		)
	}
	var contextLengthErr *modelContextLengthError
	if errors.As(turnErr, &contextLengthErr) {
		return fmt.Sprintf(
			"本轮没有完成：对话上下文超出了模型长度上限，已自动压缩并重试 %d 次仍无法容纳。系统已停止重试。你可以精简指令或另开新话题后再试，我会从当前最新时间线接着执行。",
			contextLengthErr.Retries,
		)
	}

	state := toolRecoveryFromContext(ctx)
	details := ""
	if state != nil {
		details = state.summary()
	}
	if details == "" && turnErr != nil {
		details = agentexec.TruncateText(turnErr.Error(), 800)
	}
	if details == "" {
		details = "本轮执行没有生成可交付结果"
	}

	return "本轮没有完成，系统已经停止重复失败的工具调用。最后问题：" + details +
		"。你可以继续告诉我下一步怎么处理，我会从当前最新时间线接着执行。"
}
