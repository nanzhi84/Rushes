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

var ErrInvalidAgentContextCheckpoint = errors.New("agent context checkpoint 无效")

type AgentContextCheckpoint struct {
	DraftID                   string
	WindowID                  string
	WindowNumber              int
	HistoryVersion            int
	BaseSnapshot              map[string]any
	BaseSnapshotHash          string
	Summary                   string
	CompactedThroughMessageID *string
	CreatedAt                 string
	UpdatedAt                 string
}

func ListMessages(ctx context.Context, query Querier, draftID string, limit int) ([]Message, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := query.QueryContext(ctx, `
		SELECT message_id,draft_id,role,kind,content,created_at FROM (
			SELECT m.message_id,m.draft_id,m.role,m.kind,m.content,m.created_at,m.rowid
			FROM messages m
			WHERE m.draft_id=? AND m.rowid >= COALESCE((
				SELECT anchor.rowid
				FROM drafts d JOIN messages anchor ON anchor.message_id=d.messages_tail_ref
				WHERE d.draft_id=?
			), 0)
			ORDER BY m.rowid DESC LIMIT ?
		) ORDER BY rowid`, draftID, draftID, limit)
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

// ListMessagesAfter returns the objective conversation tail visible to the
// current context window. The draft reset anchor and the checkpoint boundary
// are both honored, so compacted or explicitly cleared history cannot leak
// back into a later model call.
func ListMessagesAfter(
	ctx context.Context,
	query Querier,
	draftID string,
	afterMessageID *string,
	limit int,
) ([]Message, error) {
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	after := ""
	if afterMessageID != nil {
		after = *afterMessageID
	}
	rows, err := query.QueryContext(ctx, `
		SELECT m.message_id,m.draft_id,m.role,m.kind,m.content,m.created_at
		FROM messages m
		WHERE m.draft_id=?
		AND m.rowid >= COALESCE((
			SELECT anchor.rowid
			FROM drafts d JOIN messages anchor ON anchor.message_id=d.messages_tail_ref
			WHERE d.draft_id=?
		), 0)
		AND m.rowid > COALESCE((
			SELECT boundary.rowid FROM messages boundary
			WHERE boundary.draft_id=? AND boundary.message_id=?
		), 0)
		ORDER BY m.rowid LIMIT ?`, draftID, draftID, draftID, after, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	messages := []Message{}
	for rows.Next() {
		var message Message
		if err := rows.Scan(
			&message.ID, &message.DraftID, &message.Role, &message.Kind,
			&message.Content, &message.CreatedAt,
		); err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, rows.Err()
}

func GetAgentContextCheckpoint(
	ctx context.Context,
	query Querier,
	draftID string,
) (AgentContextCheckpoint, error) {
	var checkpoint AgentContextCheckpoint
	var snapshot string
	var compacted sql.NullString
	err := query.QueryRowContext(ctx, `
		SELECT draft_id,window_id,window_number,history_version,
		base_snapshot_json,base_snapshot_hash,summary,
		compacted_through_message_id,created_at,updated_at
		FROM agent_context_checkpoints WHERE draft_id=?`, draftID,
	).Scan(
		&checkpoint.DraftID, &checkpoint.WindowID, &checkpoint.WindowNumber,
		&checkpoint.HistoryVersion, &snapshot, &checkpoint.BaseSnapshotHash,
		&checkpoint.Summary, &compacted, &checkpoint.CreatedAt, &checkpoint.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentContextCheckpoint{}, ErrNotFound
	}
	if err != nil {
		return AgentContextCheckpoint{}, err
	}
	if err := json.Unmarshal([]byte(snapshot), &checkpoint.BaseSnapshot); err != nil {
		return AgentContextCheckpoint{}, errors.Join(ErrInvalidAgentContextCheckpoint, err)
	}
	checkpoint.CompactedThroughMessageID = stringPointer(compacted)
	return checkpoint, nil
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
