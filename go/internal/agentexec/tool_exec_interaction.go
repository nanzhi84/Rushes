package agentexec

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func (exec *Executor) toolAskUser(
	ctx context.Context,
	draftID string,
	input rushestools.AskUserInput,
	pending map[string]any,
) (rushestools.ToolResult, error) {
	decisionType := NormalizeDecisionType(input.DecisionType)
	if len(pending) == 0 {
		if decisionType != "critical" {
			return rushestools.ToolResult{
				Status:      string(rushestools.StatusFailed),
				Observation: "该问题未声明为无法安全推断的关键分歧。可逆的镜头取舍、删保项、气口、B-roll、节奏、字幕、转场、调色和 BGM 等细节必须由 Agent 根据证据自主决定并继续执行，不得创建首剪或 EDL 审批卡。",
				Data: map[string]any{
					"autonomous_decision_required": true,
					"recovery":                     "采用有证据支持的安全默认值继续完成任务；完成后让用户通过增量反馈或 Rewind 调整。",
				},
			}, nil
		}
		if utf8.RuneCountInString(input.Question) > 240 || len(input.Options) > 3 {
			return rushestools.ToolResult{
				Status:      string(rushestools.StatusFailed),
				Observation: "关键决策卡只能聚焦一个核心分歧，问题不得超过 240 个字符，结构化方向不得超过 3 个；不要附带方案清单或逐项审批。",
				Data: map[string]any{
					"autonomous_decision_required": false,
					"recovery":                     "只保留那个无法从素材、上下文或安全默认值推断，且会实质改变成片目标的核心问题。",
				},
			}, nil
		}
	}
	draft, err := storage.GetDraft(ctx, exec.database.Read(), draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	decisionID := RandomID("decision")
	options := make([]map[string]any, 0, len(input.Options))
	for _, option := range input.Options {
		storedOption := map[string]any{
			"option_id": option.OptionID, "label": option.Label, "description": option.Description,
		}
		options = append(options, storedOption)
	}
	blocking := true
	if input.Blocking != nil {
		blocking = *input.Blocking
	}
	allowFreeText := true
	if input.AllowFreeText != nil {
		allowFreeText = *input.AllowFreeText
	}
	var pendingPayload any
	var pendingStatus any
	if len(pending) > 0 {
		pendingPayload = pending
		pendingStatus = "pending"
	}
	resultRows := reducer.ResultRows{}
	if blocking {
		resultRows.AgentJobObservationDelivery = PendingJobObservationDelivery(ctx)
	}
	var result reducer.Result
	applyDecision := func() (bool, error) {
		var applyErr error
		result, applyErr = reducer.Apply(ctx, exec.database, []contracts.Event{{
			Type: "DecisionCreated", DraftID: draftID,
			Payload: map[string]any{
				"decision_id": decisionID, "scope_type": "draft", "type": decisionType,
				"question": input.Question, "options": options, "blocking": blocking,
				"allow_free_text": allowFreeText, "pending_tool_call": pendingPayload,
				"pending_tool_call_status": pendingStatus,
				"created_by_tool_call_id":  nullableToolCallID(ctx),
			},
		}}, reducer.Options{
			Actor: contracts.ActorAgent, BaseVersion: &draft.StateVersion, ResultRows: resultRows,
		})
		return applyErr == nil && result.Status == reducer.StatusApplied, applyErr
	}
	if blocking {
		_, err = CommitDurableTerminal(ctx, applyDecision)
	} else {
		_, err = applyDecision()
	}
	if err != nil || result.Status != reducer.StatusApplied {
		return rushestools.ToolResult{}, errors.Join(err, fmt.Errorf("reducer status: %s", result.Status))
	}
	if resultRows.AgentJobObservationDelivery != nil {
		MarkJobObservationDelivered(ctx)
	}
	MarkDecisionCreatedThisTurn(ctx, decisionID, blocking)
	if !blocking {
		return rushestools.ToolResult{
			Status:      string(rushestools.StatusSucceeded),
			Observation: "已创建非阻塞决策卡；当前任务可以继续执行。",
			Data: map[string]any{
				"decision_id": decisionID, "decision_type": decisionType,
				"turn_should_end": false,
			},
		}, nil
	}
	return rushestools.ToolResult{
		Status:      string(rushestools.StatusWaiting),
		Observation: "已创建决策卡。本回合必须停止调用工具；等待真实用户回答后，系统会自动继续。",
		Data: map[string]any{
			"decision_id": decisionID, "decision_type": decisionType,
			"turn_should_end": true,
		},
	}, nil
}

func (exec *Executor) ToolDecisionAnswer(
	ctx context.Context,
	draftID string,
	input rushestools.DecisionAnswerInput,
) (rushestools.ToolResult, error) {
	if decisionCreatedThisTurn(ctx, input.DecisionID) {
		return rushestools.ToolResult{
			Status:      "failed",
			Observation: "不能回答本回合由 interaction.ask_user 刚创建的决策；请结束本回合并等待真实用户回答。",
			Data: map[string]any{
				"decision_id": input.DecisionID, "current_turn_unchanged": true,
				"turn_should_end": true,
			},
		}, nil
	}
	draft, err := storage.GetDraft(ctx, exec.database.Read(), draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	decision, err := storage.GetDecision(ctx, exec.database.Read(), input.DecisionID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	if decision.DraftID == nil || *decision.DraftID != draftID || decision.ScopeType != "draft" {
		return rushestools.ToolResult{}, errors.New("决策不属于当前草稿")
	}
	if decision.Status != "pending" {
		return rushestools.ToolResult{}, errors.New("决策已不在待回答状态")
	}
	answer, err := AdjudicateDecisionAnswer(decision, input.OptionID, input.FreeText, input.Payload)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	result, err := reducer.Apply(ctx, exec.database, []contracts.Event{{
		Type: "DecisionAnswered", DraftID: draftID,
		Payload: map[string]any{
			"decision_id": input.DecisionID, "scope_type": "draft",
			"answer": answer,
		},
	}}, reducer.Options{Actor: contracts.ActorUser, BaseVersion: &draft.StateVersion})
	if err != nil || result.Status != reducer.StatusApplied {
		return rushestools.ToolResult{}, errors.Join(err, fmt.Errorf("reducer status: %s", result.Status))
	}
	return rushestools.ToolResult{Status: string(rushestools.StatusSucceeded), Observation: "决策已回答"}, nil
}

// DecisionAnswerError reports a stable API reason for rejected answer content.
type DecisionAnswerError struct {
	Reason  string
	Message string
}

func (err *DecisionAnswerError) Error() string { return err.Message }

// AdjudicateDecisionAnswer is the single answer-content gate used by Agent and REST.
// Server-owned option payload fields always win over untrusted caller fields.
func AdjudicateDecisionAnswer(
	decision storage.Decision,
	optionValue string,
	freeTextValue string,
	payload map[string]any,
) (map[string]any, error) {
	optionID := strings.TrimSpace(optionValue)
	freeText := strings.TrimSpace(freeTextValue)
	if optionID == "" && freeText == "" {
		return nil, &DecisionAnswerError{
			Reason: "decision_answer_empty", Message: "决策答案必须提供 option_id 或 free_text",
		}
	}
	answerPayload := payload
	if optionID != "" {
		option, exists := decisionOption(decision, optionID)
		if !exists {
			return nil, &DecisionAnswerError{
				Reason: "decision_option_not_found", Message: "option_id 不属于该决策的可选项",
			}
		}
		if optionPayload, ok := option["payload"].(map[string]any); ok {
			answerPayload = make(map[string]any, len(payload)+len(optionPayload))
			for key, value := range payload {
				answerPayload[key] = value
			}
			for key, value := range optionPayload {
				answerPayload[key] = value
			}
		}
	}
	if freeText != "" && !decision.AllowFreeText {
		return nil, &DecisionAnswerError{
			Reason: "decision_free_text_not_allowed", Message: "该决策不允许自由文本答案",
		}
	}
	return map[string]any{
		"option_id": optionID, "free_text": freeText,
		"payload": answerPayload, "answered_via": "agent",
	}, nil
}

func decisionOption(decision storage.Decision, optionID string) (map[string]any, bool) {
	for _, option := range decision.Options {
		if InterfaceString(option["option_id"]) == optionID {
			return option, true
		}
	}
	return nil, false
}
