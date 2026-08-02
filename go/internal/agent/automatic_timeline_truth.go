package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino/compose"
	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

const automaticTimelineCheckDataKey = "automatic_timeline_check"

// newAutomaticTimelineTruthMiddleware turns every durably committed atomic timeline
// mutation into a closed ReAct loop: execute timeline.check for that exact version and
// attach the authoritative evidence to the mutation tool message before the next model
// call. timeline.check itself remains harness-only and is never exposed to the provider.
func newAutomaticTimelineTruthMiddleware(service *Service) compose.ToolMiddleware {
	return compose.ToolMiddleware{
		Invokable: func(next compose.InvokableToolEndpoint) compose.InvokableToolEndpoint {
			return func(ctx context.Context, input *compose.ToolInput) (*compose.ToolOutput, error) {
				output, err := next(ctx, input)
				if err != nil || output == nil || input == nil || !isTerminalTimelineMutation(input.Name) {
					return output, err
				}
				mutation, timelineID, committed := committedTimelineMutationResult(output.Result)
				if !committed {
					return output, nil
				}
				truth := terminalTimelineTruthFromContext(ctx)
				truth.recordMutationTimelineID(timelineID)
				draftID, draftErr := rushestools.DraftID(ctx)
				if draftErr != nil {
					return output, draftErr
				}
				check, checkErr := service.executeAutomaticTimelineCheck(ctx, draftID, timelineID)
				if errors.Is(checkErr, context.Canceled) || errors.Is(checkErr, context.DeadlineExceeded) {
					return output, checkErr
				}
				if checkErr == nil {
					truth.recordTimelineCheckResult(
						agentexec.InterfaceString(check.Data["timeline_id"]), check.Status,
					)
				}
				output.Result = attachAutomaticTimelineCheckEvidence(mutation, check, checkErr)
				return output, nil
			}
		},
	}
}

func committedTimelineMutationResult(raw string) (rushestools.ToolResult, string, bool) {
	var result rushestools.ToolResult
	if json.Unmarshal([]byte(raw), &result) != nil {
		return rushestools.ToolResult{}, "", false
	}
	timelineID := agentexec.InterfaceString(result.Data["timeline_id"])
	if !isValidTimelineVersionID(timelineID) {
		return result, timelineID, false
	}
	switch result.Status {
	case string(rushestools.StatusSucceeded):
		return result, timelineID, true
	case string(rushestools.StatusValidationFailed):
		if unchanged, _ := result.Data["current_timeline_unchanged"].(bool); unchanged {
			return result, timelineID, false
		}
		before, beforeOK := agentexec.NumericValue(result.Data["before_version"])
		after, afterOK := agentexec.NumericValue(result.Data["after_version"])
		return result, timelineID, beforeOK && afterOK && after > before
	default:
		return result, timelineID, false
	}
}

func attachAutomaticTimelineCheckEvidence(
	mutation rushestools.ToolResult,
	check rushestools.ToolResult,
	checkErr error,
) string {
	if mutation.Data == nil {
		mutation.Data = map[string]any{}
	}
	if checkErr != nil {
		check = rushestools.ToolResult{
			Status:      string(rushestools.StatusFailed),
			Observation: "Harness 未能完成自动时间线校验：" + checkErr.Error(),
			Data: map[string]any{
				"error_code": string(rushestools.ErrCodeToolExecutionError),
			},
		}
	}
	mutation.Data[automaticTimelineCheckDataKey] = check
	suffix := "Harness 已自动校验精确写入版本；请以 automatic_timeline_check 为终态证据。"
	if strings.TrimSpace(mutation.Observation) == "" {
		mutation.Observation = suffix
	} else {
		mutation.Observation += "\n" + suffix
	}
	encoded, _ := json.Marshal(mutation)
	return string(encoded)
}

func (service *Service) executeAutomaticTimelineCheck(
	ctx context.Context,
	draftID, timelineID string,
) (rushestools.ToolResult, error) {
	stepID := agentexec.RandomID("step")
	startedAt := time.Now()
	argsSummary := compactJSON(map[string]any{"timeline_id": timelineID})
	service.hub.Record(draftID, StreamEvent{
		"type": TurnStreamToolStepStarted, "step_id": stepID, "tool": "timeline.check",
		"args_summary": argsSummary, "harness_owned": true, "progress": 0,
	})
	service.hub.Record(draftID, StreamEvent{
		"type": TurnStreamToolStepProgress, "step_id": stepID, "tool": "timeline.check",
		"harness_owned": true, "progress": 0.5, "note": "正在校验精确时间线版本",
	})

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
	durationMS := time.Since(startedAt).Milliseconds()
	status := "succeeded"
	observation := compactJSON(result)
	if err != nil {
		status, observation = "failed", err.Error()
	} else if result.Status != string(rushestools.StatusSucceeded) {
		status = "failed"
	}
	service.hub.Record(draftID, StreamEvent{
		"type": TurnStreamToolStepFinished, "step_id": stepID, "tool": "timeline.check",
		"status": status, "observation": observation, "harness_owned": true,
		"progress": 1, "duration_ms": durationMS,
	})
	_ = service.persistToolTrace(
		context.WithoutCancel(ctx), draftID, stepID, "timeline.check", status,
		argsSummary, observation, "", "", map[string]any{
			"harness_owned": true, "progress": 1, "duration_ms": durationMS,
		},
	)
	return result, err
}

// ensureTerminalTimelineTruth is the fail-safe for paths that bypass the model tool
// middleware (for example confirmed replay). Normal ReAct mutations are already checked
// before the next provider call, so this is a no-op in the common path.
func (service *Service) ensureTerminalTimelineTruth(ctx context.Context, draftID string) error {
	state := terminalTimelineTruthFromContext(ctx)
	snapshot := state.snapshot()
	if snapshot.mutationSequence == 0 || snapshot.mutationProofInvalid ||
		(snapshot.checkSequence == snapshot.mutationSequence &&
			snapshot.checkTimelineID == snapshot.mutationTimelineID) {
		return nil
	}
	result, err := service.executeAutomaticTimelineCheck(ctx, draftID, snapshot.mutationTimelineID)
	if err != nil {
		return err
	}
	state.recordTimelineCheckResult(agentexec.InterfaceString(result.Data["timeline_id"]), result.Status)
	refreshed := state.snapshot()
	if refreshed.checkSequence != refreshed.mutationSequence ||
		refreshed.checkTimelineID != refreshed.mutationTimelineID {
		return &terminalReplyGuardError{
			kind: "timeline_check_missing", mutationTimelineID: snapshot.mutationTimelineID,
		}
	}
	return nil
}
