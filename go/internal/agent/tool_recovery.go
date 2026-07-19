package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/agentexec"
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
)

type toolRecoveryContextKey struct{}

type toolFailureSnapshot struct {
	Tool              string
	Arguments         string
	Observation       string
	ExecutionAttempts int
}

type toolRecoveryState struct {
	mu                       sync.Mutex
	failedCalls              map[string]toolFailureSnapshot
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
	return &toolRecoveryState{failedCalls: map[string]toolFailureSnapshot{}}
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
	state.latest = snapshot
	return recoveryDecision{
		exhausted: state.exhausted, repairAttempt: state.repairFailures, latest: snapshot,
	}
}

func (state *toolRecoveryState) recordSuccess(_ string) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.hadFailure {
		return
	}
	// 任一成功工具调用都表示模型已经读取了新状态、换了方案或完成了
	// 恢复步骤。修复预算按连续失败链计算，不能让回合早期一个已绕过的
	// 空状态错误（例如尚无时间线时 inspect）污染后续完全不同的编辑工具。
	state.failedCalls = map[string]toolFailureSnapshot{}
	state.hadFailure = false
	state.rootTool = ""
	state.repairFailures = 0
	// 累计修复计数是 turn 级、不因单次成功重置（#95 H4）：交替 fail→success 不能无限
	// 刷新预算。连击照常清零，但累计到阈值仍维持穷尽（evaluateExhaustion 会把这类
	// 「连击已清零、累计仍超」的穷尽记成 cumulative 分因，即 H-B P2「预算重叠」信号）。
	state.evaluateExhaustion()
	state.latest = toolFailureSnapshot{}
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
	return state.hadFailure
}

func (state *toolRecoveryState) recoveryExhausted() bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.exhausted
}

func (state *toolRecoveryState) summary() string {
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.hadFailure {
		return ""
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
				if isStructuredToolFailure(output.Result) {
					output.Result = decorateToolFailure(ctx, input, output.Result, attempts)
				} else if state != nil {
					state.recordSuccess(input.Name)
				}
				return output, nil
			}
		},
	}
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

// retrySafeFromEffect 从工具注册表的 Effect 分级派生「瞬时失败可重试」白名单（#103 G1），
// 取代此前硬编码的九工具白名单。只读工具一律重试安全；timeline.validate 与 speech.inspect
// 虽被归为 EffectReversible，却仍然重试安全——两者都经事务型 reducer 按稳定键落盘，顺序
// 重放幂等，重试瞬时失败不会重复提交已生效状态。该白名单刻意宽于 G3 的只读并发集合（后者
// 必须严格 EffectReadOnly）：重试是顺序执行，speech.inspect「并发首调重复建索引」这一
// 使它无法归 EffectReadOnly 的隐患在顺序重试下并不成立。
func retrySafeFromEffect(effectOf func(string) (rushestools.Effect, bool)) func(string) bool {
	return func(name string) bool {
		if effect, ok := effectOf(name); ok && effect == rushestools.EffectReadOnly {
			return true
		}
		switch name {
		case "timeline.validate", "speech.inspect":
			return true
		default:
			// 写时间线、创建决策、排队理解/渲染等调用不能在提交状态未知时盲目重放。
			return false
		}
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

func (service *Service) terminalFailureReply(
	ctx context.Context,
	draftID, messageID string,
	turnErr error,
) string {
	var timeoutErr *modelResponseTimeoutError
	if errors.As(turnErr, &timeoutErr) {
		return service.emitAssistantReply(draftID, messageID, fmt.Sprintf(
			"本轮没有完成：模型响应超时，已自动重试 %d 次仍未恢复。系统已停止重试。你可以继续给出下一步指令，我会从当前最新时间线接着执行。",
			timeoutErr.Retries,
		))
	}
	var contextLengthErr *modelContextLengthError
	if errors.As(turnErr, &contextLengthErr) {
		return service.emitAssistantReply(draftID, messageID, fmt.Sprintf(
			"本轮没有完成：对话上下文超出了模型长度上限，已自动压缩并重试 %d 次仍无法容纳。系统已停止重试。你可以精简指令或另开新话题后再试，我会从当前最新时间线接着执行。",
			contextLengthErr.Retries,
		))
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

	content := ""
	if service.chatModel != nil {
		messages, err := service.modelMessages(ctx, draftID)
		if err == nil {
			messages = append(messages,
				schema.SystemMessage("你正在为一次失败的剪辑执行做最终收尾。禁止调用任何工具。必须用简体中文明确告诉用户：本轮没有完成、最后失败在哪、系统已停止无意义重试、用户可以继续给出下一步指令。不要声称已经完成。"),
				schema.UserMessage("请根据以下真实终态生成简短回复：\n"+details),
			)
			response, generateErr := service.chatModel.Generate(
				ctx, messages, model.WithToolChoice(schema.ToolChoiceForbidden),
			)
			if generateErr == nil && response != nil && strings.TrimSpace(response.Content) != "" {
				content = strings.TrimSpace(response.Content)
			}
		}
	}
	if content == "" {
		content = "本轮没有完成，系统已经停止重复失败的工具调用。最后问题：" + details + "。你可以继续告诉我下一步怎么处理，我会从当前最新时间线接着执行。"
	}
	return service.emitAssistantReply(draftID, messageID, content)
}
