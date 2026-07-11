package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

type Message struct {
	ID        string
	DraftID   string
	Role      string
	Kind      string
	Content   string
	CreatedAt string
}

func ListMessages(ctx context.Context, query Querier, draftID string, limit int) ([]Message, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := query.QueryContext(ctx, `
		SELECT message_id,draft_id,role,kind,content,created_at FROM (
			SELECT message_id,draft_id,role,kind,content,created_at
			FROM messages WHERE draft_id=? ORDER BY created_at DESC,message_id DESC LIMIT ?
		) ORDER BY created_at,message_id`, draftID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	messages := []Message{}
	for rows.Next() {
		var message Message
		if err := rows.Scan(&message.ID, &message.DraftID, &message.Role, &message.Kind, &message.Content, &message.CreatedAt); err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, rows.Err()
}

type Decision struct {
	ID                    string
	ScopeType             string
	DraftID               *string
	Type                  string
	Question              string
	Options               []map[string]any
	AllowFreeText         bool
	Status                string
	Answer                map[string]any
	PendingToolCall       map[string]any
	PendingToolCallStatus *string
	ConsumedAt            *string
	ReplayedToolCallID    *string
	Blocking              bool
	CreatedByToolCallID   *string
}

const decisionColumns = `
decision_id,scope_type,draft_id,type,question,options_json,allow_free_text,status,answer_json,
pending_tool_call_json,pending_tool_call_status,consumed_at,replayed_tool_call_id,
blocking,created_by_tool_call_id`

func GetDecision(ctx context.Context, query Querier, decisionID string) (Decision, error) {
	return scanDecision(query.QueryRowContext(ctx,
		"SELECT "+decisionColumns+" FROM decisions WHERE decision_id=?", decisionID))
}

func CurrentDecision(ctx context.Context, query Querier, draftID string) (Decision, error) {
	return scanDecision(query.QueryRowContext(ctx, `
		SELECT `+decisionColumns+` FROM decisions
		WHERE draft_id=? AND status='pending' AND blocking=1
		ORDER BY rowid LIMIT 1`, draftID))
}

func ListPendingDecisions(ctx context.Context, query Querier, draftID string) ([]Decision, error) {
	rows, err := query.QueryContext(ctx, `
		SELECT `+decisionColumns+` FROM decisions
		WHERE draft_id=? AND status='pending' ORDER BY rowid`, draftID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	decisions := []Decision{}
	for rows.Next() {
		decision, err := scanDecision(rows)
		if err != nil {
			return nil, err
		}
		decisions = append(decisions, decision)
	}
	return decisions, rows.Err()
}

func scanDecision(row rowScanner) (Decision, error) {
	var decision Decision
	var draftID, answerJSON, pendingJSON, pendingStatus, consumed, replayed, createdBy sql.NullString
	var optionsJSON string
	var allowFreeText, blocking int
	if err := row.Scan(
		&decision.ID, &decision.ScopeType, &draftID, &decision.Type, &decision.Question,
		&optionsJSON, &allowFreeText, &decision.Status, &answerJSON, &pendingJSON, &pendingStatus,
		&consumed, &replayed, &blocking, &createdBy,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Decision{}, ErrNotFound
		}
		return Decision{}, err
	}
	decision.DraftID = stringPointer(draftID)
	decision.PendingToolCallStatus = stringPointer(pendingStatus)
	decision.ConsumedAt = stringPointer(consumed)
	decision.ReplayedToolCallID = stringPointer(replayed)
	decision.CreatedByToolCallID = stringPointer(createdBy)
	decision.Blocking = blocking != 0
	decision.AllowFreeText = allowFreeText != 0
	_ = json.Unmarshal([]byte(optionsJSON), &decision.Options)
	if decision.Options == nil {
		decision.Options = []map[string]any{}
	}
	decision.Answer = decodeNullMap(answerJSON)
	decision.PendingToolCall = decodeNullMap(pendingJSON)
	return decision, nil
}
