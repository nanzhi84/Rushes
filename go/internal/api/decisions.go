package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

func (server *Server) CurrentDecisionApiDraftsDraftIdDecisionsCurrentGet(
	writer http.ResponseWriter,
	request *http.Request,
	draftID string,
) {
	if _, err := storage.GetDraft(request.Context(), server.database.Read(), draftID); errors.Is(err, storage.ErrNotFound) {
		writeNotFound(writer, "draft_not_found")
		return
	} else if err != nil {
		server.internalError(writer, err)
		return
	}
	decision, err := storage.CurrentDecision(request.Context(), server.database.Read(), draftID)
	if errors.Is(err, storage.ErrNotFound) {
		writeJSON(writer, http.StatusOK, map[string]any{"decision": nil})
		return
	}
	if err != nil {
		server.internalError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"decision": decisionRecord(decision)})
}

func (server *Server) PendingDraftDecisionsApiDraftsDraftIdDecisionsPendingGet(
	writer http.ResponseWriter,
	request *http.Request,
	draftID string,
) {
	if _, err := storage.GetDraft(request.Context(), server.database.Read(), draftID); errors.Is(err, storage.ErrNotFound) {
		writeNotFound(writer, "draft_not_found")
		return
	} else if err != nil {
		server.internalError(writer, err)
		return
	}
	rows, err := storage.ListPendingDecisions(request.Context(), server.database.Read(), draftID)
	if err != nil {
		server.internalError(writer, err)
		return
	}
	decisions := make([]Decision, 0, len(rows))
	for _, row := range rows {
		decisions = append(decisions, decisionRecord(row))
	}
	writeJSON(writer, http.StatusOK, PendingDecisionsResponse{DraftId: draftID, Decisions: decisions})
}

func (server *Server) AnswerDecisionApiDecisionsDecisionIdAnswerPost(
	writer http.ResponseWriter,
	request *http.Request,
	decisionID string,
) {
	decision, err := storage.GetDecision(request.Context(), server.database.Read(), decisionID)
	if errors.Is(err, storage.ErrNotFound) {
		writeNotFound(writer, "decision_not_found")
		return
	}
	if err != nil {
		server.internalError(writer, err)
		return
	}
	if decision.Status != "pending" {
		writeJSON(writer, http.StatusConflict, map[string]any{"detail": map[string]string{"reason": "decision_not_pending"}})
		return
	}
	var payload DecisionAnswerRequest
	if err := decodeJSON(request, &payload); err != nil {
		writeBadRequest(writer, "invalid_json")
		return
	}
	if decision.DraftID == nil {
		writeBadRequest(writer, "workspace_decision_not_supported")
		return
	}
	if payload.DraftId != nil && *payload.DraftId != *decision.DraftID {
		writeBadRequest(writer, "decision_ownership_mismatch")
		return
	}
	draft, err := storage.GetDraft(request.Context(), server.database.Read(), *decision.DraftID)
	if err != nil {
		server.internalError(writer, err)
		return
	}
	optionID := ""
	if payload.Answer.OptionId != nil {
		optionID = *payload.Answer.OptionId
	}
	freeText := ""
	if payload.Answer.FreeText != nil {
		freeText = *payload.Answer.FreeText
	}
	answerPayload := map[string]any(nil)
	if payload.Answer.Payload != nil {
		answerPayload = *payload.Answer.Payload
	}
	answer, err := agentexec.AdjudicateDecisionAnswer(decision, optionID, freeText, answerPayload)
	if err != nil {
		var answerErr *agentexec.DecisionAnswerError
		if errors.As(err, &answerErr) {
			writeBadRequest(writer, answerErr.Reason)
			return
		}
		server.internalError(writer, err)
		return
	}
	answer["answered_via"] = payload.Answer.AnsweredVia
	replayID := ""
	consumedAt := ""
	if decision.PendingToolCall != nil {
		replayID, consumedAt = newID("replay"), time.Now().UTC().Format(time.RFC3339Nano)
	}
	result, err := reducer.Apply(request.Context(), server.database, []contracts.Event{{
		Type: "DecisionAnswered", DraftID: *decision.DraftID,
		Payload: map[string]any{
			"decision_id": decisionID, "scope_type": decision.ScopeType, "answer": answer,
			"replayed_tool_call_id": replayID, "consumed_at": consumedAt,
		},
	}}, reducer.Options{Actor: contracts.ActorUser, BaseVersion: &draft.StateVersion})
	if err != nil {
		server.internalError(writer, err)
		return
	}
	if result.Status != reducer.StatusApplied {
		writeReducerResult(writer, result)
		return
	}
	queueItemID := replayID
	if queueItemID == "" {
		queueItemID = newID("decision_resume")
	}
	replays := 0
	if server.agent.Queue().EnqueueUIObservation(
		*decision.DraftID, queueItemID, "decision_answered",
		map[string]any{"decision_id": decisionID, "answer": answer, "pending_tool_call": decision.PendingToolCall},
	) {
		replays = 1
	}
	writeJSON(writer, http.StatusOK, DecisionAnswerResponse{
		DecisionId: decisionID, EventIds: reducerEventIDs(result), ReplaysEnqueued: replays,
		Status: DecisionAnswerResponseStatus("answered"),
	})
}

func decisionRecord(value storage.Decision) Decision {
	options := make([]DecisionOption, 0, len(value.Options))
	for _, option := range value.Options {
		optionID, _ := option["option_id"].(string)
		label, _ := option["label"].(string)
		description, _ := option["description"].(string)
		var descriptionPointer *string
		if description != "" {
			descriptionPointer = &description
		}
		options = append(options, DecisionOption{OptionId: optionID, Label: label, Description: descriptionPointer})
	}
	var answer *DecisionAnswer
	if value.Answer != nil {
		answer = &DecisionAnswer{AnsweredVia: DecisionAnswerAnsweredVia(stringValue(value.Answer["answered_via"]))}
		if optionID := stringValue(value.Answer["option_id"]); optionID != "" {
			answer.OptionId = &optionID
		}
		if freeText := stringValue(value.Answer["free_text"]); freeText != "" {
			answer.FreeText = &freeText
		}
	}
	var pending *PendingToolCall
	if value.PendingToolCall != nil {
		pending = &PendingToolCall{
			ToolName:            stringValue(value.PendingToolCall["tool_name"]),
			Arguments:           mapValueAPI(value.PendingToolCall["arguments"]),
			IdempotencyKey:      stringValue(value.PendingToolCall["idempotency_key"]),
			ArgumentFingerprint: stringValue(value.PendingToolCall["argument_fingerprint"]),
		}
	}
	blocking := value.Blocking
	allowFreeText := value.AllowFreeText
	status := DecisionStatus(value.Status)
	return Decision{
		DecisionId: value.ID, ScopeType: DecisionScopeType(value.ScopeType), DraftId: value.DraftID,
		Type: DecisionType(value.Type), Question: value.Question, Options: &options,
		Status: &status, Answer: answer, PendingToolCall: pending,
		PendingToolCallStatus: decisionPendingStatus(value.PendingToolCallStatus),
		ConsumedAt:            value.ConsumedAt, ReplayedToolCallId: value.ReplayedToolCallID,
		Blocking: &blocking, AllowFreeText: &allowFreeText, CreatedByToolCallId: value.CreatedByToolCallID,
	}
}

func decisionPendingStatus(value *string) *DecisionPendingToolCallStatus {
	if value == nil {
		return nil
	}
	converted := DecisionPendingToolCallStatus(*value)
	return &converted
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func mapValueAPI(value any) map[string]any {
	mapping, _ := value.(map[string]any)
	if mapping == nil {
		mapping = map[string]any{}
	}
	return mapping
}
