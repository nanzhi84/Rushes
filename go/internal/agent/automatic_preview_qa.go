package agent

import (
	"context"
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
	maxAutomaticPreviewQAPassesPerTurn = 4
)

var automaticPreviewCoreChecks = []string{
	"decode", "black", "freeze", "silence", "loudness",
}

type automaticPreviewQAContextKey struct{}

type automaticPreviewQAState struct {
	mu        sync.Mutex
	attempted map[string]struct{}
}

func newAutomaticPreviewQAState() *automaticPreviewQAState {
	return &automaticPreviewQAState{attempted: map[string]struct{}{}}
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

func (state *automaticPreviewQAState) claim(target string) bool {
	if state == nil || strings.TrimSpace(target) == "" {
		return false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if _, exists := state.attempted[target]; exists ||
		len(state.attempted) >= maxAutomaticPreviewQAPassesPerTurn {
		return false
	}
	state.attempted[target] = struct{}{}
	return true
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
		return state.claim("missing_timeline"), nil
	}
	if err != nil {
		return false, err
	}
	return state.claim(document.TimelineID), nil
}

func automaticPreviewQATrigger(
	ctx context.Context,
	messages []*schema.Message,
	candidate *schema.Message,
) string {
	userText := withoutNegatedSurfaceActions(latestUserSurfaceText(messages))
	if requestsExplicitPreviewWorkflow(userText) || requestsExplicitPreviewQA(userText) {
		return "explicit_preview_or_qa_request"
	}
	truth := terminalTimelineTruthFromContext(ctx).snapshot()
	if truth.mutationSequence == 0 {
		return ""
	}
	if playbookRequiresPreviewQA(messages) && terminalCandidateClaimsDeliverable(candidate) {
		return "playbook_required"
	}
	if terminalCandidateClaimsDeliverable(candidate) {
		return "deliverable_declaration"
	}
	return ""
}

func requestsExplicitPreviewQA(text string) bool {
	if !requestsPreviewCheck(text) {
		return false
	}
	// “质检”本身也可能描述正在回复、代码检查或其他非媒体语境。只有用户明确
	// 指向预览/成片/视频，或点名一项可执行的信号检查时，才进入 Preview QA。
	return containsSurfaceKeyword(text,
		"预览", "成片", "视频", "preview_", "render_preview",
		"黑帧", "静帧", "静音", "响度", "解码",
	)
}

func playbookRequiresPreviewQA(messages []*schema.Message) bool {
	for _, message := range messages {
		if message == nil || message.Role != schema.System {
			continue
		}
		if required, _ := message.Extra["preview_qa_required"].(bool); required {
			return true
		}
	}
	return false
}

func terminalCandidateClaimsDeliverable(candidate *schema.Message) bool {
	if candidate == nil {
		return false
	}
	text := strings.ToLower(strings.TrimSpace(candidate.Content))
	if text == "" || containsSurfaceKeyword(text,
		"未完成", "尚未完成", "无法完成", "不能完成", "执行失败", "仍需处理",
		"还需要处理", "等待用户", "需要你提供", "not complete", "incomplete", "failed",
	) {
		return false
	}
	return containsSurfaceKeyword(text,
		"已完成", "完成了", "处理好了", "已处理", "已准备好", "可交付", "已经就绪",
		"done", "ready", "completed", "finished",
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
	report := service.executeAutomaticPreviewQA(
		ctx, draftID, automaticPreviewQATrigger(ctx, messages, candidate),
		automaticPreviewOrientation(messages),
		automaticPreviewNeedsVisual(messages),
	)
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

func automaticPreviewNeedsVisual(messages []*schema.Message) bool {
	// visual 是高成本、按任务需要执行的 advisory。终态候选只是模型准备提交的
	// 文本，不能反向扩大用户任务范围；否则模型随口提到“画面”就会把明确的
	// 五项信号检查升级成视觉模型调用。
	text := latestUserSurfaceText(messages)
	return containsSurfaceKeyword(text,
		"视觉", "画面", "字幕", "文字", "水印", "裁切", "裁边", "构图", "调色",
		"颜色", "转场", "遮挡", "黑边", "主体", "b-roll", "broll", "visual",
	)
}

func automaticPreviewOrientation(messages []*schema.Message) string {
	text := latestUserSurfaceText(messages)
	// 先识别明确指向预览/成片的目标短语，避免“把横屏素材生成竖屏预览”
	// 同时出现两种方向时误把源素材方向当成输出方向。
	if containsSurfaceKeyword(text,
		"竖屏预览", "竖版预览", "纵向预览", "竖屏成片", "竖版成片",
		"portrait preview", "portrait video", "9:16 预览", "9:16预览",
	) {
		return "portrait"
	}
	if containsSurfaceKeyword(text,
		"横屏预览", "横版预览", "横向预览", "横屏成片", "横版成片",
		"landscape preview", "landscape video", "16:9 预览", "16:9预览",
	) {
		return "landscape"
	}
	portrait := containsSurfaceKeyword(text, "竖屏", "竖版", "纵向", "portrait", "9:16")
	landscape := containsSurfaceKeyword(text, "横屏", "横版", "横向", "landscape", "16:9")
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
	draftID, trigger, orientation string,
	includeVisual bool,
) PreviewQAReport {
	switch orientation {
	case "portrait", "landscape":
	default:
		orientation = "auto"
	}
	startedAt := time.Now()
	report := PreviewQAReport{
		Status: "failed", Trigger: trigger, Orientation: orientation,
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
	report.TimelineID = document.TimelineID

	truth := terminalTimelineTruthFromContext(ctx)
	snapshot := truth.snapshot()
	check := snapshot.checkResult
	if snapshot.checkTimelineID != document.TimelineID ||
		(check.Status != string(rushestools.StatusSucceeded) &&
			check.Status != string(rushestools.StatusValidationFailed)) {
		check, err = service.executeAutomaticTimelineCheck(ctx, draftID, document.TimelineID)
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
