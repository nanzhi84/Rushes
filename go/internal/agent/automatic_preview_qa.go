package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

const (
	automaticPreviewQAContextPhase     = "automatic_preview_qa"
	maxAutomaticPreviewQAPassesPerTurn = 3
)

var automaticPreviewCoreChecks = []string{
	"decode", "black", "freeze", "silence", "loudness",
}

type automaticPreviewQAContextKey struct{}

type automaticPreviewQAState struct {
	mu               sync.Mutex
	previewAttempted map[string]struct{}
	seenBlockers     map[string]struct{}
	continuations    int
	pending          stopGatePending
	previewBlocker   stopGatePending
	finalOverride    string
	validationGen    uint64
}

func newAutomaticPreviewQAState() *automaticPreviewQAState {
	return &automaticPreviewQAState{
		previewAttempted: map[string]struct{}{},
		seenBlockers:     map[string]struct{}{},
	}
}

func withAutomaticPreviewQAState(
	ctx context.Context,
	state *automaticPreviewQAState,
) context.Context {
	return context.WithValue(ctx, automaticPreviewQAContextKey{}, state)
}

func automaticPreviewQAStateFromContext(ctx context.Context) *automaticPreviewQAState {
	state, _ := ctx.Value(automaticPreviewQAContextKey{}).(*automaticPreviewQAState)
	return state
}

type previewQAClaim int

const (
	previewQAClaimed previewQAClaim = iota
	previewQAAlreadyClaimed
	previewQAExhausted
	previewQAInvalid
)

func (state *automaticPreviewQAState) claimResult(target string) previewQAClaim {
	if state == nil || strings.TrimSpace(target) == "" {
		return previewQAInvalid
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	claimKey := fmt.Sprintf("%s#%d", target, state.validationGen)
	if _, exists := state.previewAttempted[claimKey]; exists {
		return previewQAAlreadyClaimed
	}
	if len(state.previewAttempted) >= maxAutomaticPreviewQAPassesPerTurn {
		return previewQAExhausted
	}
	state.previewAttempted[claimKey] = struct{}{}
	return previewQAClaimed
}

func (state *automaticPreviewQAState) claim(target string) bool {
	return state.claimResult(target) == previewQAClaimed
}

func (state *automaticPreviewQAState) invalidateValidationProofs() {
	if state == nil {
		return
	}
	state.mu.Lock()
	state.validationGen++
	state.previewBlocker = stopGatePending{}
	state.mu.Unlock()
}

type stopGatePending struct {
	Decision        string
	TimelineID      string
	Trigger         string
	Check           rushestools.ToolResult
	Fingerprint     string
	Duplicate       bool
	Exhausted       bool
	PreviewRequired bool
	HookError       string
	Issues          []map[string]any
	RemainingIssues int
	ResultRef       string
	Run             stopGateRun
}

func (state *automaticPreviewQAState) setPending(pending stopGatePending) {
	if state == nil {
		return
	}
	state.mu.Lock()
	state.pending = pending
	state.mu.Unlock()
}

func (state *automaticPreviewQAState) takePending() stopGatePending {
	if state == nil {
		return stopGatePending{}
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	pending := state.pending
	state.pending = stopGatePending{}
	return pending
}

func (state *automaticPreviewQAState) setPreviewBlocker(pending stopGatePending) {
	if state == nil {
		return
	}
	state.mu.Lock()
	state.previewBlocker = pending
	state.mu.Unlock()
}

func (state *automaticPreviewQAState) previewBlockerFor(timelineID string) stopGatePending {
	if state == nil {
		return stopGatePending{}
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.previewBlocker.TimelineID != timelineID {
		return stopGatePending{}
	}
	return state.previewBlocker
}

func (state *automaticPreviewQAState) clearPreviewBlocker(timelineID string) {
	if state == nil {
		return
	}
	state.mu.Lock()
	if state.previewBlocker.TimelineID == timelineID {
		state.previewBlocker = stopGatePending{}
	}
	state.mu.Unlock()
}

func (state *automaticPreviewQAState) setFinalOverride(content string) {
	if state == nil || strings.TrimSpace(content) == "" {
		return
	}
	state.mu.Lock()
	state.finalOverride = content
	state.mu.Unlock()
}

func (state *automaticPreviewQAState) takeFinalOverride() string {
	if state == nil {
		return ""
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	content := state.finalOverride
	state.finalOverride = ""
	return content
}

func (state *automaticPreviewQAState) registerBlocker(fingerprint string) (duplicate, exhausted bool) {
	if state == nil {
		return false, false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	_, duplicate = state.seenBlockers[fingerprint]
	if duplicate {
		metricStopGateDeduplicated.Inc()
		return true, false
	}
	state.seenBlockers[fingerprint] = struct{}{}
	if state.continuations >= maxAutomaticPreviewQAPassesPerTurn {
		metricStopGateExhausted.Inc()
		return false, true
	}
	state.continuations++
	metricStopGateContinuation.Inc()
	return false, false
}

type automaticPreviewQAController struct {
	ShouldRun func(
		context.Context, []*schema.Message, *schema.Message,
	) (bool, error)
	Run func(
		context.Context, []*schema.Message, *schema.Message,
	) (*schema.Message, error)
}

type PreviewQAReport struct {
	Status         string                                `json:"status"`
	Trigger        string                                `json:"trigger"`
	TimelineID     string                                `json:"timeline_id,omitempty"`
	TimelineCheck  rushestools.ToolResult                `json:"timeline_check"`
	Orientation    string                                `json:"orientation"`
	PreviewID      string                                `json:"preview_id,omitempty"`
	JobID          string                                `json:"job_id,omitempty"`
	CoreChecks     []rushestools.PreviewInspectionResult `json:"core_checks"`
	VisualAdvisory *rushestools.PreviewInspectionResult  `json:"visual_advisory,omitempty"`
	Passed         bool                                  `json:"passed"`
	Degraded       bool                                  `json:"degraded"`
	Issues         []map[string]interface{}              `json:"issues"`
	Errors         []map[string]string                   `json:"errors"`
	Summary        string                                `json:"summary"`
}

func (service *Service) automaticPreviewQAController() *automaticPreviewQAController {
	return &automaticPreviewQAController{
		ShouldRun: service.shouldRunAutomaticPreviewQA,
		Run:       service.runAutomaticPreviewQA,
	}
}

func (service *Service) shouldRunAutomaticPreviewQA(
	ctx context.Context,
	messages []*schema.Message,
	candidate *schema.Message,
) (bool, error) {
	state := automaticPreviewQAStateFromContext(ctx)
	if state == nil {
		return false, nil
	}
	trigger := automaticPreviewQATrigger(ctx, messages, candidate)
	if trigger == "" {
		return false, nil
	}
	draftID, err := rushestools.DraftID(ctx)
	if err != nil {
		return false, err
	}
	document, err := timeline.Latest(ctx, service.database, draftID)
	if errors.Is(err, storage.ErrNotFound) {
		pending := stopGatePending{
			Decision: "block", Trigger: trigger,
			Issues: []map[string]any{{
				"code": "timeline_not_exists", "message": "当前草稿没有可终验的时间线",
				"recovery": "加载并使用 timeline.insert 插入首个 visual_base clip。",
			}},
		}
		pending.Fingerprint = stopGatePendingFingerprint(pending)
		if terminalCandidateAdmitsNotCompleted(candidate) {
			return false, nil
		}
		pending.Duplicate, pending.Exhausted = state.registerBlocker(pending.Fingerprint)
		if pending.Duplicate || pending.Exhausted {
			state.setFinalOverride(stopGateNotCompletedReply(pending))
			return false, nil
		}
		finalStatus, finishErr := service.finishStopGate(
			ctx, draftID, "", "blocked", "当前草稿没有可终验的时间线",
			"validation:timeline_missing", pending.Issues, stopGateRun{},
		)
		if finishErr != nil {
			pending.Decision, pending.HookError = finalStatus, finishErr.Error()
			pending.Issues = stopGateTracePersistenceIssues(finishErr)
		}
		state.setPending(pending)
		return true, nil
	}
	if err != nil {
		pending := stopGatePending{
			Decision: "hook_error", Trigger: trigger, HookError: err.Error(),
			Issues: []map[string]any{{
				"code": "stop_gate_hook_error", "message": "读取最新时间线失败: " + err.Error(),
				"recovery": "保留已完成事实并说明终验程序未完成；安全重试或等待存储恢复。",
			}},
			ResultRef: "validation:timeline_unavailable",
		}
		pending.Fingerprint = stopGatePendingFingerprint(pending)
		pending.Duplicate, pending.Exhausted = state.registerBlocker(pending.Fingerprint)
		if pending.Duplicate || pending.Exhausted {
			state.setFinalOverride(stopGateNotCompletedReply(pending))
			return false, nil
		}
		_, finishErr := service.finishStopGate(
			ctx, draftID, "", "hook_error", err.Error(), pending.ResultRef,
			pending.Issues, stopGateRun{},
		)
		if finishErr != nil {
			pending.HookError = errors.Join(err, finishErr).Error()
			pending.Issues = append(pending.Issues, stopGateTracePersistenceIssues(finishErr)...)
		}
		state.setPending(pending)
		return true, nil
	}
	truth := terminalTimelineTruthFromContext(ctx)
	snapshot := truth.snapshot()
	check := snapshot.checkResult
	previewRequired := automaticPreviewQARequired(ctx, messages, candidate)
	gateRun := stopGateRun{}
	if snapshot.checkTimelineID != document.TimelineID ||
		(check.Status != string(rushestools.StatusSucceeded) &&
			check.Status != string(rushestools.StatusValidationFailed)) {
		check, gateRun, err = service.executeAutomaticTimelineCheck(
			ctx, draftID, document.TimelineID, previewRequired,
		)
		if err == nil {
			truth.recordTimelineCheckResult(
				agentexec.InterfaceString(check.Data["timeline_id"]), check.Status, check,
			)
		}
	}
	if err != nil {
		pending := stopGatePending{
			Decision: "hook_error", TimelineID: document.TimelineID, Trigger: trigger,
			HookError: err.Error(),
			Issues: []map[string]any{{
				"code": "stop_gate_hook_error", "message": err.Error(),
				"recovery": "保留已完成事实并说明终验程序未完成；安全重试或等待 Harness 恢复。",
			}},
		}
		pending.Fingerprint = stopGatePendingFingerprint(pending)
		pending.Duplicate, pending.Exhausted = state.registerBlocker(pending.Fingerprint)
		if pending.Duplicate || pending.Exhausted {
			state.setFinalOverride(stopGateNotCompletedReply(pending))
			return false, nil
		}
		state.setPending(pending)
		return true, nil
	}
	if check.Status != string(rushestools.StatusSucceeded) {
		fingerprint := stopGateIssueFingerprint(document.TimelineID, check)
		pending := stopGatePending{
			Decision: "block", TimelineID: document.TimelineID, Trigger: trigger,
			Check: check, Fingerprint: fingerprint,
		}
		if terminalCandidateAdmitsNotCompleted(candidate) {
			return false, nil
		}
		pending.Duplicate, pending.Exhausted = state.registerBlocker(fingerprint)
		if pending.Duplicate || pending.Exhausted {
			state.setFinalOverride(stopGateNotCompletedReply(pending))
			return false, nil
		}
		state.setPending(pending)
		return true, nil
	}
	if blocker := state.previewBlockerFor(document.TimelineID); blocker.Decision != "" {
		if terminalCandidateAdmitsNotCompleted(candidate) {
			return false, nil
		}
		blocker.Duplicate, blocker.Exhausted = state.registerBlocker(blocker.Fingerprint)
		state.setFinalOverride(stopGateNotCompletedReply(blocker))
		return false, nil
	}
	if previewRequired {
		switch state.claimResult(document.TimelineID) {
		case previewQAClaimed:
			if gateRun.stepID == "" {
				gateRun = service.startStopGate(draftID, document.TimelineID)
			}
			state.setPending(stopGatePending{
				Decision: "preview", TimelineID: document.TimelineID, Trigger: trigger,
				Check: check, PreviewRequired: true, Run: gateRun,
			})
			return true, nil
		case previewQAExhausted:
			pending := stopGatePending{
				Decision: "block", TimelineID: document.TimelineID, Trigger: trigger,
				Exhausted: true, ResultRef: "validation:" + document.TimelineID,
				Issues: []map[string]any{{
					"code":     "preview_qa_budget_exhausted",
					"message":  "本回合已验收 3 个精确时间线版本，不能继续接受新的可交付声明",
					"recovery": "如实结束本回合并说明尚未完成；下一回合再对最新版本重新终验。",
				}},
			}
			finalStatus, finishErr := service.finishStopGate(
				ctx, draftID, document.TimelineID, "blocked", "Preview QA 版本预算已耗尽",
				pending.ResultRef, pending.Issues, gateRun,
			)
			if finishErr != nil {
				pending.Decision, pending.HookError = finalStatus, finishErr.Error()
				pending.Issues = stopGateTracePersistenceIssues(finishErr)
			}
			state.setFinalOverride(stopGateNotCompletedReply(pending))
			return false, nil
		case previewQAAlreadyClaimed, previewQAInvalid:
			return false, nil
		}
	}
	return false, nil
}

func automaticPreviewQATrigger(
	ctx context.Context,
	messages []*schema.Message,
	candidate *schema.Message,
) string {
	userText := withoutNegatedBoundaryActions(latestUserIntentText(messages))
	if hasExplicitPreviewIntent(userText) || requestsExplicitPreviewQA(userText) {
		return "explicit_preview_or_qa_request"
	}
	if terminalCandidateClaimsDeliverable(candidate) && hasMediaDeliveryBoundaryIntent(userText) {
		return "deliverable_declaration"
	}
	truth := terminalTimelineTruthFromContext(ctx).snapshot()
	if truth.mutationSequence > 0 {
		return "editing_or_delivery_turn"
	}
	return ""
}

func hasMediaDeliveryBoundaryIntent(text string) bool {
	return hasTimelineMutationIntent(text) || hasBeatEditIntent(text) ||
		hasExplicitPreviewIntent(text) || hasPreviewCheckIntent(text) ||
		hasUserFinalExportIntent(text) || containsBoundaryKeyword(
		text,
		"交付", "就绪", "完成成片", "成片完成", "视频完成", "剪完", "做完",
		"可以了吗", "可以了没", "好了没", "ready", "deliverable",
	)
}

func automaticPreviewQARequired(
	_ context.Context,
	messages []*schema.Message,
	candidate *schema.Message,
) bool {
	userText := withoutNegatedBoundaryActions(latestUserIntentText(messages))
	return hasExplicitPreviewIntent(userText) || requestsExplicitPreviewQA(userText) ||
		terminalCandidateClaimsDeliverable(candidate)
}

func requestsExplicitPreviewQA(text string) bool {
	if !hasPreviewCheckIntent(text) {
		return false
	}
	// “质检”本身也可能描述正在回复、代码检查或其他非媒体语境。只有用户明确
	// 指向预览/成片/视频，或点名一项可执行的信号检查时，才进入 Preview QA。
	return containsBoundaryKeyword(text,
		"预览", "成片", "视频", "preview_", "render_preview",
		"黑帧", "静帧", "静音", "响度", "解码",
	)
}

func terminalCandidateClaimsDeliverable(candidate *schema.Message) bool {
	if candidate == nil {
		return false
	}
	text := strings.ToLower(strings.TrimSpace(candidate.Content))
	if text == "" || containsBoundaryKeyword(text,
		"未完成", "尚未完成", "无法完成", "不能完成", "执行失败", "仍需处理",
		"还需要处理", "等待用户", "需要你提供", "not complete", "incomplete", "failed",
	) {
		return false
	}
	return containsBoundaryKeyword(text,
		"已完成", "完成了", "处理好了", "已处理", "已准备好", "可交付", "已经就绪",
		"done", "ready", "completed", "finished",
	)
}

func terminalCandidateAdmitsNotCompleted(candidate *schema.Message) bool {
	if candidate == nil {
		return false
	}
	return containsBoundaryKeyword(strings.ToLower(candidate.Content),
		"未完成", "尚未完成", "未达到可交付", "尚未达到可交付",
		"终验未通过", "stop gate 未通过", "not_completed", "not completed", "incomplete",
	)
}

func (service *Service) runAutomaticPreviewQA(
	ctx context.Context,
	messages []*schema.Message,
	candidate *schema.Message,
) (*schema.Message, error) {
	draftID, err := rushestools.DraftID(ctx)
	if err != nil {
		return nil, err
	}
	state := automaticPreviewQAStateFromContext(ctx)
	pending := state.takePending()
	if pending.Decision != "preview" {
		return stopGateFeedbackMessage(pending), nil
	}
	previewRequest := pending
	report := service.executeAutomaticPreviewQA(
		ctx, draftID, previewRequest.TimelineID, previewRequest.Trigger,
		automaticPreviewOrientation(messages),
		automaticPreviewNeedsVisual(messages),
	)
	gateStatus := "passed"
	resultRef := "preview:" + report.PreviewID
	if report.PreviewID == "" {
		resultRef = "preview:" + report.TimelineID
	}
	issues := previewReportActionableIssues(report)
	decision := ""
	switch {
	case report.Status == "timeline_changed", report.Status == "validation_failed":
		gateStatus, decision = "blocked", "block"
	case report.Status != "succeeded":
		gateStatus, decision = "hook_error", "hook_error"
	case !report.Passed:
		gateStatus, decision = "blocked", "block"
	}
	finalStatus, finishErr := service.recordStopGatePreviewOutcome(
		ctx, draftID, report, gateStatus, issues, resultRef, previewRequest.Run,
	)
	if finishErr != nil {
		decision = finalStatus
		issues = stopGateTracePersistenceIssues(finishErr)
		report.Summary = "Stop Gate trace 持久化失败: " + finishErr.Error()
	}
	if decision != "" {
		pending = stopGatePending{
			Decision: decision, TimelineID: report.TimelineID, Trigger: previewRequest.Trigger,
			Issues: issues, RemainingIssues: max(0, len(issues)-3), ResultRef: resultRef,
		}
		if decision == "hook_error" {
			pending.HookError = report.Summary
		}
		pending.Fingerprint = stopGatePreviewFingerprint(pending, report.Status)
		pending.Duplicate, pending.Exhausted = state.registerBlocker(pending.Fingerprint)
		state.setPreviewBlocker(pending)
		return stopGateFeedbackMessage(pending), nil
	}
	state.clearPreviewBlocker(report.TimelineID)
	encoded, err := json.Marshal(report)
	if err != nil {
		return nil, err
	}
	message := schema.SystemMessage(
		"【PreviewQAReport｜Harness 自动同回合证据】\n" + string(encoded) +
			"\n这是精确 timeline_id 的预览真值。不得调用 preview.generate 或 preview.check；" +
			"若报告暴露可由原子编辑修复的问题，继续编辑并等待 Harness 对新版本重新验收；" +
			"否则基于报告生成诚实终态回复。final export 仍只能由用户从 UI 触发。",
	)
	message.Extra = map[string]any{"context_phase": automaticPreviewQAContextPhase}
	return message, nil
}

func stopGateIssueFingerprint(timelineID string, check rushestools.ToolResult) string {
	encoded, _ := json.Marshal(map[string]any{
		"timeline_id": timelineID,
		"status":      check.Status,
		"issues":      stopGateActionableIssues(check),
	})
	return fmt.Sprintf("%x", sha256.Sum256(encoded))
}

func stopGateFeedbackMessage(pending stopGatePending) *schema.Message {
	payload := map[string]any{
		"gate": "stop", "decision": pending.Decision, "timeline_id": pending.TimelineID,
		"issues": []map[string]any{}, "remaining_issue_count": 0,
	}
	if pending.HookError != "" {
		payload["error_code"] = "stop_gate_hook_error"
		payload["message"] = agentexec.TruncateText(pending.HookError, 500)
		payload["recovery"] = "不要声明可交付；保留已完成事实并诚实说明终验程序未完成。"
	}
	if pending.Decision == "block" || pending.Decision == "hook_error" {
		issues := pending.Issues
		if len(issues) == 0 && pending.Decision == "block" {
			issues = stopGateActionableIssues(pending.Check)
		}
		payload["issues"] = issues[:min(3, len(issues))]
		remaining := max(0, len(issues)-3)
		if pending.RemainingIssues > remaining {
			remaining = pending.RemainingIssues
		}
		payload["remaining_issue_count"] = remaining
		resultRef := pending.ResultRef
		if resultRef == "" {
			resultRef = "validation:" + pending.TimelineID
		}
		payload["result_ref"] = resultRef
		payload["deduplicated"] = pending.Duplicate
		payload["exhausted"] = pending.Exhausted
		if pending.Exhausted {
			payload["recovery"] = "Stop continuation 已耗尽；下一次回复必须诚实使用 not_completed，列出未通过项和已完成事实。"
		} else if pending.Duplicate {
			payload["recovery"] = "时间线版本与阻塞原因未变化；必须调用 action 修改时间线，不能重复提交完成声明。"
		}
	}
	encoded, _ := json.Marshal(payload)
	message := schema.SystemMessage(
		"【StopGateFeedback｜Harness 终验反馈】\n" + string(encoded) +
			"\nblocked 不是执行失败。按 recovery 加载或调用原子 action 修复；只有 passed 后才可声明可交付。",
	)
	message.Extra = map[string]any{"context_phase": "stop_gate_feedback"}
	return message
}

func stopGateNotCompletedReply(pending stopGatePending) string {
	issues := pending.Issues
	if len(issues) == 0 {
		issues = stopGateActionableIssues(pending.Check)
	}
	parts := make([]string, 0, min(3, len(issues)))
	for _, issue := range issues[:min(3, len(issues))] {
		if message := strings.TrimSpace(agentexec.InterfaceString(issue["message"])); message != "" {
			parts = append(parts, message)
		}
	}
	detail := "Stop Gate 终验尚未通过"
	if pending.Decision == "hook_error" {
		detail = "Stop Gate 终验程序未能完成"
		if pending.HookError != "" {
			parts = append(parts, agentexec.TruncateText(pending.HookError, 240))
		}
	}
	if len(parts) > 0 {
		detail += "：" + strings.Join(parts, "；")
	}
	return "本回合未达到可交付状态。已成功提交的时间线修改均已保留；" + detail + "。"
}

func previewReportActionableIssues(report PreviewQAReport) []map[string]any {
	issues := make([]map[string]any, 0, len(report.Issues)+len(report.Errors))
	for _, issue := range report.Issues {
		copy := map[string]any{}
		for key, value := range issue {
			copy[key] = value
		}
		if strings.TrimSpace(agentexec.InterfaceString(copy["message"])) != "" {
			if strings.TrimSpace(agentexec.InterfaceString(copy["recovery"])) == "" {
				copy["recovery"] = "加载并使用合适的时间线原子 action 修复后重新终验。"
			}
			issues = append(issues, copy)
		}
	}
	for _, reportErr := range report.Errors {
		recovery := "保留已完成事实并说明终验程序未完成；安全重试或等待 Harness 恢复。"
		if reportErr["error_code"] == string(rushestools.ErrCodeStaleTarget) {
			recovery = "重新读取最新 timeline_id，并由 Stop Gate 对该精确版本重新终验。"
		}
		issues = append(issues, map[string]any{
			"code": reportErr["error_code"], "message": reportErr["message"],
			"recovery": recovery,
		})
	}
	if len(issues) == 0 && strings.TrimSpace(report.Summary) != "" {
		issues = append(issues, map[string]any{"code": "preview_qa_unmet", "message": report.Summary})
	}
	return issues
}

func stopGatePreviewFingerprint(pending stopGatePending, reportStatus string) string {
	encoded, _ := json.Marshal(map[string]any{
		"timeline_id": pending.TimelineID, "decision": pending.Decision,
		"status": reportStatus, "issues": pending.Issues,
	})
	return fmt.Sprintf("%x", sha256.Sum256(encoded))
}

func stopGatePendingFingerprint(pending stopGatePending) string {
	encoded, _ := json.Marshal(map[string]any{
		"timeline_id": pending.TimelineID, "decision": pending.Decision,
		"issues": pending.Issues, "hook_error": pending.HookError,
	})
	return fmt.Sprintf("%x", sha256.Sum256(encoded))
}

func (service *Service) recordStopGatePreviewOutcome(
	ctx context.Context,
	draftID string,
	report PreviewQAReport,
	status string,
	issues []map[string]any,
	resultRef string,
	run stopGateRun,
) (string, error) {
	return service.finishStopGate(
		ctx, draftID, report.TimelineID, status, previewQAEvidenceJSON(report), resultRef, issues, run,
	)
}

func stopGateTracePersistenceIssues(err error) []map[string]any {
	return []map[string]any{{
		"code":     "stop_gate_trace_persist_failed",
		"message":  "Stop Gate trace 持久化失败: " + err.Error(),
		"recovery": "不得声明可交付；等待存储恢复后对最新版本重新终验。",
	}}
}

func stopGateActionableIssues(check rushestools.ToolResult) []map[string]any {
	issues := make([]map[string]any, 0)
	appendIssue := func(value any) {
		encoded, err := json.Marshal(value)
		if err != nil {
			return
		}
		item := map[string]any{}
		if json.Unmarshal(encoded, &item) != nil {
			return
		}
		code := agentexec.InterfaceString(item["error_code"])
		if code == "" {
			code = agentexec.InterfaceString(item["code"])
		}
		if code == "" {
			code = agentexec.InterfaceString(item["check"])
		}
		message := agentexec.InterfaceString(item["message"])
		if message == "" {
			return
		}
		recovery := agentexec.InterfaceString(item["recovery"])
		if recovery == "" {
			recovery = "加载并使用合适的时间线原子 action 修复此项。"
		}
		issues = append(issues, map[string]any{"code": code, "message": message, "recovery": recovery})
	}
	// Structural invalidity makes later content-contract findings unreliable, so it
	// always occupies the highest-priority slots in the compact Stop Gate summary.
	if encoded, err := json.Marshal(check.Data["validation_report"]); err == nil {
		var report map[string]any
		if json.Unmarshal(encoded, &report) == nil {
			values, _ := report["issues"].([]any)
			for _, issue := range values {
				appendIssue(issue)
			}
		}
	}
	if encoded, err := json.Marshal(check.Data["contract_failures"]); err == nil {
		var failures []any
		if json.Unmarshal(encoded, &failures) == nil {
			for _, failure := range failures {
				appendIssue(failure)
			}
		}
	}
	if len(issues) == 0 && strings.TrimSpace(check.Observation) != "" {
		issues = append(issues, map[string]any{
			"code": "timeline_contract_unmet", "message": check.Observation,
			"recovery": "读取 CurrentTimelineView，并加载合适的时间线原子 action 修复。",
		})
	}
	return issues
}

func automaticPreviewNeedsVisual(messages []*schema.Message) bool {
	// visual 是高成本、按任务需要执行的 advisory。终态候选只是模型准备提交的
	// 文本，不能反向扩大用户任务范围；否则模型随口提到“画面”就会把明确的
	// 五项信号检查升级成视觉模型调用。
	text := latestUserIntentText(messages)
	return containsBoundaryKeyword(text,
		"视觉", "画面", "字幕", "文字", "水印", "裁切", "裁边", "构图", "调色",
		"颜色", "转场", "遮挡", "黑边", "主体", "b-roll", "broll", "visual",
	)
}

func automaticPreviewOrientation(messages []*schema.Message) string {
	text := latestUserIntentText(messages)
	// 先识别明确指向预览/成片的目标短语，避免“把横屏素材生成竖屏预览”
	// 同时出现两种方向时误把源素材方向当成输出方向。
	if containsBoundaryKeyword(text,
		"竖屏预览", "竖版预览", "纵向预览", "竖屏成片", "竖版成片",
		"portrait preview", "portrait video", "9:16 预览", "9:16预览",
	) {
		return "portrait"
	}
	if containsBoundaryKeyword(text,
		"横屏预览", "横版预览", "横向预览", "横屏成片", "横版成片",
		"landscape preview", "landscape video", "16:9 预览", "16:9预览",
	) {
		return "landscape"
	}
	portrait := containsBoundaryKeyword(text, "竖屏", "竖版", "纵向", "portrait", "9:16")
	landscape := containsBoundaryKeyword(text, "横屏", "横版", "横向", "landscape", "16:9")
	switch {
	case portrait && !landscape:
		return "portrait"
	case landscape && !portrait:
		return "landscape"
	default:
		return "auto"
	}
}

func (service *Service) executeAutomaticPreviewQA(
	ctx context.Context,
	draftID, expectedTimelineID, trigger, orientation string,
	includeVisual bool,
) PreviewQAReport {
	switch orientation {
	case "portrait", "landscape":
	default:
		orientation = "auto"
	}
	startedAt := time.Now()
	report := PreviewQAReport{
		Status: "failed", Trigger: trigger, TimelineID: expectedTimelineID, Orientation: orientation,
		CoreChecks: []rushestools.PreviewInspectionResult{},
		Issues:     []map[string]interface{}{},
		Errors:     []map[string]string{},
	}
	document, err := timeline.Latest(ctx, service.database, draftID)
	if err != nil {
		code := rushestools.ErrCodePreviewQATimelineMissing
		if !errors.Is(err, storage.ErrNotFound) {
			code = rushestools.ErrCodePreviewQATimelineRead
		}
		report.Status = "not_available"
		report.Errors = append(report.Errors, map[string]string{
			"error_code": string(code), "message": err.Error(),
		})
		report.Summary = "当前草稿没有可供 Preview QA 验收的时间线。"
		service.recordAutomaticPreviewQAReportStep(ctx, draftID, startedAt, report)
		return report
	}
	report.TimelineID = expectedTimelineID
	if report.TimelineID == "" {
		report.TimelineID = document.TimelineID
	}
	if document.TimelineID != report.TimelineID {
		report.Status = "timeline_changed"
		report.Errors = append(report.Errors, map[string]string{
			"error_code": string(rushestools.ErrCodeStaleTarget),
			"message": fmt.Sprintf(
				"Preview QA 目标版本已变化: expected=%s latest=%s",
				report.TimelineID, document.TimelineID,
			),
		})
		report.Summary = "终验期间时间线版本已变化；旧版本预览不得验收新版本。"
		service.recordAutomaticPreviewQAReportStep(ctx, draftID, startedAt, report)
		return report
	}

	truth := terminalTimelineTruthFromContext(ctx)
	snapshot := truth.snapshot()
	check := snapshot.checkResult
	if snapshot.checkTimelineID != report.TimelineID ||
		(check.Status != string(rushestools.StatusSucceeded) &&
			check.Status != string(rushestools.StatusValidationFailed)) {
		check, err = service.executeHarnessTimelineCheck(ctx, report.TimelineID)
		if err == nil {
			truth.recordTimelineCheckResult(
				agentexec.InterfaceString(check.Data["timeline_id"]), check.Status, check,
			)
		}
	}
	report.TimelineCheck = check
	if err != nil {
		report.Errors = append(report.Errors, map[string]string{
			"error_code": string(rushestools.ErrCodePreviewQATimelineCheck), "message": err.Error(),
		})
		report.Summary = "Harness 未能完成精确版本的 timeline.check，未生成预览。"
		service.recordAutomaticPreviewQAReportStep(ctx, draftID, startedAt, report)
		return report
	}
	if check.Status != string(rushestools.StatusSucceeded) {
		report.Status = "validation_failed"
		report.Summary = "精确版本 timeline.check 未通过，未生成预览；时间线保持不变。"
		service.recordAutomaticPreviewQAReportStep(ctx, draftID, startedAt, report)
		return report
	}

	generatedRaw, generateErr := service.executeHarnessOwnedPreviewStep(
		ctx, draftID, "preview.generate",
		rushestools.PreviewGenerateInput{
			TimelineID: document.TimelineID, Orientation: orientation,
		},
		"正在为精确时间线版本生成工作预览", "", "",
	)
	generated, generatedOK := terminalTruthToolResult(generatedRaw)
	if generateErr != nil || !generatedOK ||
		generated.Status != string(rushestools.StatusSucceeded) {
		message := "preview.generate 未返回成功终态"
		if generateErr != nil {
			message = generateErr.Error()
		} else if generatedOK && generated.Observation != "" {
			message = generated.Observation
		}
		report.Status = "render_failed"
		report.Errors = append(report.Errors, map[string]string{
			"error_code": string(rushestools.ErrCodePreviewQARender), "message": message,
		})
		report.Summary = "工作预览生成失败；失败未修改时间线。"
		service.recordAutomaticPreviewQAReportStep(ctx, draftID, startedAt, report)
		return report
	}
	report.PreviewID = agentexec.InterfaceString(generated.Data["preview_id"])
	report.JobID = agentexec.InterfaceString(generated.Data["job_id"])

	type checkOutcome struct {
		result rushestools.PreviewInspectionResult
		err    error
	}
	outcomes := make([]checkOutcome, len(automaticPreviewCoreChecks))
	var wait sync.WaitGroup
	for index, checkName := range automaticPreviewCoreChecks {
		index, checkName := index, checkName
		wait.Add(1)
		go func() {
			defer wait.Done()
			raw, checkErr := service.executeHarnessOwnedPreviewStep(
				ctx, draftID, "preview.check",
				rushestools.PreviewCheckInput{PreviewID: report.PreviewID, Check: checkName},
				"正在执行预览检查："+checkName, report.PreviewID, checkName,
			)
			if checkErr != nil {
				outcomes[index].err = checkErr
				return
			}
			result, ok := raw.(rushestools.PreviewInspectionResult)
			if !ok {
				outcomes[index].err = fmt.Errorf("preview.check %s 返回类型异常: %T", checkName, raw)
				return
			}
			outcomes[index].result = result
		}()
	}
	wait.Wait()
	coreErrors := 0
	for index, outcome := range outcomes {
		if outcome.err != nil {
			coreErrors++
			report.Errors = append(report.Errors, map[string]string{
				"error_code": string(rushestools.ErrCodePreviewQACheck),
				"check":      automaticPreviewCoreChecks[index],
				"message":    outcome.err.Error(),
			})
			continue
		}
		report.CoreChecks = append(report.CoreChecks, outcome.result)
		mergePreviewQAInspection(&report, outcome.result)
	}
	coreBlocking := previewQAHasErrorIssue(report.Issues)

	if includeVisual {
		raw, visualErr := service.executeHarnessOwnedPreviewStep(
			ctx, draftID, "preview.check",
			rushestools.PreviewCheckInput{PreviewID: report.PreviewID, Check: "visual"},
			"正在执行按需视觉建议检查", report.PreviewID, "visual",
		)
		if visualErr != nil {
			report.Degraded = true
			report.Issues = append(report.Issues, map[string]interface{}{
				"error_code": string(rushestools.ErrCodePreviewQAVisual),
				"check":      "visual", "severity": "warning", "message": visualErr.Error(),
			})
		} else if visual, ok := raw.(rushestools.PreviewInspectionResult); ok {
			report.VisualAdvisory = &visual
			mergePreviewQAInspection(&report, visual)
		} else {
			report.Degraded = true
			report.Issues = append(report.Issues, map[string]interface{}{
				"error_code": string(rushestools.ErrCodePreviewQAVisualInvalid),
				"check":      "visual", "severity": "warning",
				"message": fmt.Sprintf("preview.check visual 返回类型异常: %T", raw),
			})
		}
	}

	if coreErrors > 0 || len(report.CoreChecks) != len(automaticPreviewCoreChecks) {
		report.Status = "check_failed"
		report.Summary = "工作预览已生成，但至少一项 Harness 检查未能完成；时间线保持不变。"
	} else {
		report.Status = "succeeded"
		// visual 是建议性证据；只有五项核心信号检查中的 error 才阻断通过。
		report.Passed = !coreBlocking
		switch {
		case len(report.Issues) == 0:
			report.Summary = "工作预览通过五项并行信号检查；没有发现问题。"
		case report.Passed:
			report.Summary = fmt.Sprintf("工作预览检查完成：发现 %d 项非阻断提示。", len(report.Issues))
		default:
			report.Summary = fmt.Sprintf("工作预览检查完成：发现 %d 项提示，其中包含阻断错误。", len(report.Issues))
		}
	}
	service.recordAutomaticPreviewQAReportStep(ctx, draftID, startedAt, report)
	return report
}

func mergePreviewQAInspection(report *PreviewQAReport, result rushestools.PreviewInspectionResult) {
	report.Degraded = report.Degraded || result.Degraded
	report.Issues = append(report.Issues, result.Issues...)
}

func previewQAHasErrorIssue(issues []map[string]interface{}) bool {
	for _, issue := range issues {
		if strings.EqualFold(agentexec.InterfaceString(issue["severity"]), "error") {
			return true
		}
	}
	return false
}

func (service *Service) executeHarnessOwnedPreviewStep(
	ctx context.Context,
	draftID, name string,
	input any,
	progressNote, previewID, previewCheck string,
) (any, error) {
	stepID := agentexec.RandomID("step")
	startedAt := time.Now()
	argsSummary := compactJSON(input)
	service.hub.Record(draftID, StreamEvent{
		"type": TurnStreamToolStepStarted, "step_id": stepID, "tool": name,
		"args_summary": argsSummary, "harness_owned": true, "progress": 0,
	})
	service.hub.Record(draftID, StreamEvent{
		"type": TurnStreamToolStepProgress, "step_id": stepID, "tool": name,
		"harness_owned": true, "progress": 0.5, "note": progressNote,
	})
	executionCtx := rushestools.WithReporter(ctx, func(
		context.Context, string, string, any, any, error,
	) {
	})
	output, err := service.ExecuteTool(executionCtx, name, input)
	status := "succeeded"
	observation := previewQAEvidenceJSON(output)
	if err != nil {
		status, observation = "failed", err.Error()
	} else if structuredToolOutputFailed(output) {
		status = "failed"
	}
	durationMS := time.Since(startedAt).Milliseconds()
	service.hub.Record(draftID, StreamEvent{
		"type": TurnStreamToolStepFinished, "step_id": stepID, "tool": name,
		"status": status, "observation": observation, "harness_owned": true,
		"progress": 1, "duration_ms": durationMS,
	})
	_ = service.persistToolTrace(
		context.WithoutCancel(ctx), draftID, stepID, name, status,
		argsSummary, observation, previewID, previewCheck, map[string]any{
			"harness_owned": true, "progress": 1, "duration_ms": durationMS,
		},
	)
	return output, err
}

func (service *Service) recordAutomaticPreviewQAReportStep(
	ctx context.Context,
	draftID string,
	startedAt time.Time,
	report PreviewQAReport,
) {
	stepID := agentexec.RandomID("step")
	argsSummary := compactJSON(map[string]any{
		"timeline_id": report.TimelineID, "trigger": report.Trigger,
	})
	service.hub.Record(draftID, StreamEvent{
		"type": TurnStreamToolStepStarted, "step_id": stepID, "tool": "preview.qa_report",
		"args_summary": argsSummary, "harness_owned": true, "progress": 0,
	})
	service.hub.Record(draftID, StreamEvent{
		"type": TurnStreamToolStepProgress, "step_id": stepID, "tool": "preview.qa_report",
		"harness_owned": true, "progress": 0.5, "note": "正在汇总 PreviewQAReport",
	})
	status := "succeeded"
	if report.Status != "succeeded" {
		status = "failed"
	}
	observation := previewQAEvidenceJSON(report)
	durationMS := time.Since(startedAt).Milliseconds()
	service.hub.Record(draftID, StreamEvent{
		"type": TurnStreamToolStepFinished, "step_id": stepID, "tool": "preview.qa_report",
		"status": status, "observation": observation, "harness_owned": true,
		"progress": 1, "duration_ms": durationMS,
	})
	_ = service.persistToolTrace(
		context.WithoutCancel(ctx), draftID, stepID, "preview.qa_report", status,
		argsSummary, observation, report.PreviewID, "", map[string]any{
			"harness_owned": true, "progress": 1, "duration_ms": durationMS,
		},
	)
}

func previewQAEvidenceJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}
