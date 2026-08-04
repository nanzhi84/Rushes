package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

type stopGateRun struct {
	stepID    string
	startedAt time.Time
}

func (service *Service) startStopGate(draftID, timelineID string) stopGateRun {
	run := stopGateRun{stepID: agentexec.RandomID("step"), startedAt: time.Now()}
	service.hub.Record(draftID, StreamEvent{
		"type": TurnStreamStopGateStarted, "gate_id": "stop_gate", "trace_id": run.stepID,
		"timeline_id": timelineID,
		"status":      "checking", "harness_owned": true,
	})
	return run
}

func (service *Service) finishStopGate(
	ctx context.Context,
	draftID, timelineID, status, observation, resultRef string,
	issues []map[string]any,
	run stopGateRun,
) (string, error) {
	if run.stepID == "" {
		run = service.startStopGate(draftID, timelineID)
	}
	durationMS := time.Since(run.startedAt).Milliseconds()
	reportedIssues := issues[:min(3, len(issues))]
	persistErr := service.persistToolTrace(
		context.WithoutCancel(ctx), draftID, run.stepID, "stop.gate", status,
		compactJSON(map[string]any{"timeline_id": timelineID}), observation, "", "", map[string]any{
			"harness_owned": true, "duration_ms": durationMS, "timeline_id": timelineID,
			"gate_id": "stop_gate", "trace_id": run.stepID,
			"issues": reportedIssues, "remaining_issue_count": max(0, len(issues)-3),
			"result_ref": resultRef,
		},
	)
	if persistErr != nil {
		status = "hook_error"
		observation = "Stop Gate trace 持久化失败: " + persistErr.Error()
		issues = []map[string]any{{
			"code": "stop_gate_trace_persist_failed", "message": observation,
			"recovery": "不得声明可交付；等待存储恢复后对最新版本重新终验。",
		}}
		reportedIssues = issues
	}
	recordStopGateStatus(status)
	metricStopGateDurationMS.Observe(durationMS)
	service.hub.Record(draftID, StreamEvent{
		"type": TurnStreamStopGateFinished, "gate_id": "stop_gate", "trace_id": run.stepID,
		"timeline_id": timelineID,
		"status":      status, "issues": reportedIssues,
		"remaining_issue_count": max(0, len(issues)-3), "result_ref": resultRef,
		"observation": observation, "harness_owned": true, "duration_ms": durationMS,
	})
	return status, persistErr
}

func (service *Service) executeAutomaticTimelineCheck(
	ctx context.Context,
	draftID, timelineID string,
	deferPassed bool,
) (rushestools.ToolResult, stopGateRun, error) {
	normalization, err := service.executeHarnessTimelineNormalization(ctx, draftID, timelineID)
	checkedTimelineID := timelineID
	if err == nil && normalization.Status == string(rushestools.StatusSucceeded) {
		if normalizedID := agentexec.InterfaceString(normalization.Data["timeline_id"]); normalizedID != "" {
			checkedTimelineID = normalizedID
		}
		if changed, _ := normalization.Data["changed"].(bool); changed {
			terminalTimelineTruthFromContext(ctx).recordMutationTimelineID(checkedTimelineID)
		}
	}
	run := service.startStopGate(draftID, checkedTimelineID)
	result := rushestools.ToolResult{}
	if err == nil {
		result, err = service.executeHarnessTimelineCheck(ctx, checkedTimelineID)
		if changed, _ := normalization.Data["changed"].(bool); changed && result.Data != nil {
			result.Data["harness_normalization"] = normalization.Data["normalization"]
			result.Data["previous_timeline_id"] = timelineID
		}
	}
	if result.Data == nil {
		result.Data = map[string]any{}
	}
	// Normalization may already have committed a newer immutable version before
	// the read-only checker encounters an infrastructure error. Preserve that
	// exact ID in the return value so caller feedback and terminal truth never
	// fall back to the stale pre-normalization version.
	if agentexec.InterfaceString(result.Data["timeline_id"]) == "" {
		result.Data["timeline_id"] = checkedTimelineID
	}
	status := "passed"
	observation := compactJSON(result)
	issues := stopGateActionableIssues(result)
	if err != nil {
		status, observation = "hook_error", err.Error()
		issues = []map[string]any{{
			"code": "stop_gate_hook_error", "message": err.Error(),
			"recovery": "保留已完成事实并说明终验程序未完成；安全重试或等待 Harness 恢复。",
		}}
	} else if result.Status != string(rushestools.StatusSucceeded) {
		status = "blocked"
	}
	if status != "passed" || !deferPassed {
		_, finishErr := service.finishStopGate(
			ctx, draftID, checkedTimelineID, status, observation,
			"validation:"+checkedTimelineID, issues, run,
		)
		if finishErr != nil {
			err = errors.Join(err, finishErr)
		}
		run = stopGateRun{}
	}
	return result, run, err
}

func (service *Service) executeHarnessTimelineNormalization(
	ctx context.Context,
	draftID, timelineID string,
) (rushestools.ToolResult, error) {
	document, err := timeline.GetByID(ctx, service.database, draftID, timelineID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	normalizationNeeded, err := service.executor.StopGateTimelineNormalizationNeeded(
		ctx, draftID, document,
	)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	if !normalizationNeeded {
		return rushestools.ToolResult{
			Status:      string(rushestools.StatusSucceeded),
			Observation: "Stop Gate 未发现可安全合并的相邻主视频片段",
			Data: map[string]any{
				"timeline_id": timelineID, "timeline_version": document.Version,
				"changed": false, "merged_clip_count": 0,
			},
		}, nil
	}
	leaseSession := timelineEditLeaseSessionFromContext(ctx)
	if leaseSession == nil {
		return rushestools.ToolResult{}, fmt.Errorf("stop gate 时间线归一化缺少当前 Agent edit lease")
	}
	if err = leaseSession.ensure(ctx); err != nil {
		return rushestools.ToolResult{}, err
	}
	normalizationCtx := rushestools.WithTimelineMutationOrigin(ctx, "harness")
	return service.executor.NormalizeTimelineForStopGate(
		normalizationCtx, draftID, timelineID,
	)
}

func (service *Service) executeHarnessTimelineCheck(
	ctx context.Context,
	timelineID string,
) (rushestools.ToolResult, error) {
	// Suppress the registry's normal model-tool reporter. This check owns one distinct
	// Harness step and must not masquerade as a provider-authored tool call.
	executionCtx := rushestools.WithReporter(ctx, func(
		context.Context, string, string, any, any, error,
	) {
	})
	raw, err := service.ExecuteTool(
		executionCtx,
		"timeline.check",
		rushestools.TimelineCheckInput{TimelineID: timelineID},
	)
	result, ok := terminalTruthToolResult(raw)
	if err == nil && !ok {
		err = fmt.Errorf("timeline.check 返回类型异常: %T", raw)
	}
	if err == nil && agentexec.InterfaceString(result.Data["timeline_id"]) != timelineID {
		err = fmt.Errorf(
			"timeline.check 版本不匹配: want=%s got=%s",
			timelineID, agentexec.InterfaceString(result.Data["timeline_id"]),
		)
	}
	return result, err
}

// ensureTerminalTimelineTruth is a fail-safe for non-ReAct paths and provider failures.
// Normal ReAct terminal candidates are checked by the Stop Gate before display.
func (service *Service) ensureTerminalTimelineTruth(ctx context.Context, draftID string) error {
	state := terminalTimelineTruthFromContext(ctx)
	snapshot := state.snapshot()
	if snapshot.mutationSequence == 0 || snapshot.mutationProofInvalid ||
		(snapshot.checkSequence == snapshot.mutationSequence &&
			snapshot.checkTimelineID == snapshot.mutationTimelineID) {
		return nil
	}
	result, _, err := service.executeAutomaticTimelineCheck(
		ctx, draftID, snapshot.mutationTimelineID, false,
	)
	if err != nil {
		return err
	}
	state.recordTimelineCheckResult(
		agentexec.InterfaceString(result.Data["timeline_id"]), result.Status, result,
	)
	refreshed := state.snapshot()
	if refreshed.checkSequence != refreshed.mutationSequence ||
		refreshed.checkTimelineID != refreshed.mutationTimelineID {
		return &terminalReplyGuardError{
			kind: "timeline_check_missing", mutationTimelineID: snapshot.mutationTimelineID,
		}
	}
	return nil
}
