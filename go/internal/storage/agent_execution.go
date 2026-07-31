package storage

import (
	"context"
	"database/sql"
	"errors"
)

// AgentToolReceipt is the minimal durable proof that a model-visible timeline
// mutation committed. It is intentionally not a persisted workflow frame.
type AgentToolReceipt struct {
	InvocationKey       string
	DraftID             string
	SourceMessageID     string
	TurnID              string
	ToolCallID          string
	ToolName            string
	ArgumentFingerprint string
	BeforeTimelineID    *string
	BeforeVersion       int
	AfterTimelineID     string
	AfterVersion        int
	TerminalStatus      string
	ResultJSON          string
	CreatedAt           string
}

func GetAgentToolReceipt(
	ctx context.Context,
	query Querier,
	turnID, toolCallID string,
) (AgentToolReceipt, error) {
	var receipt AgentToolReceipt
	var beforeTimelineID sql.NullString
	err := query.QueryRowContext(ctx, `
		SELECT invocation_key,draft_id,source_message_id,turn_id,tool_call_id,tool_name,
			argument_fingerprint,before_timeline_id,before_version,
			after_timeline_id,after_version,terminal_status,result_json,created_at
		FROM agent_tool_receipts WHERE turn_id=? AND tool_call_id=?`,
		turnID, toolCallID,
	).Scan(
		&receipt.InvocationKey, &receipt.DraftID, &receipt.SourceMessageID, &receipt.TurnID,
		&receipt.ToolCallID, &receipt.ToolName, &receipt.ArgumentFingerprint,
		&beforeTimelineID, &receipt.BeforeVersion, &receipt.AfterTimelineID,
		&receipt.AfterVersion, &receipt.TerminalStatus, &receipt.ResultJSON,
		&receipt.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentToolReceipt{}, ErrNotFound
	}
	if err != nil {
		return AgentToolReceipt{}, err
	}
	if beforeTimelineID.Valid {
		receipt.BeforeTimelineID = &beforeTimelineID.String
	}
	return receipt, nil
}

type AgentTurnRun struct {
	TurnID       string
	DraftID      string
	SourceItemID string
	Kind         string
	Status       string
	StartedAt    string
	FinishedAt   *string
}

func GetAgentTurnRunBySource(
	ctx context.Context,
	query Querier,
	draftID, sourceItemID, kind string,
) (AgentTurnRun, error) {
	var run AgentTurnRun
	var finishedAt sql.NullString
	err := query.QueryRowContext(ctx, `
		SELECT turn_id,draft_id,source_item_id,kind,status,started_at,finished_at
		FROM agent_turn_runs WHERE draft_id=? AND source_item_id=? AND kind=?
		ORDER BY started_at DESC LIMIT 1`, draftID, sourceItemID, kind,
	).Scan(
		&run.TurnID, &run.DraftID, &run.SourceItemID, &run.Kind,
		&run.Status, &run.StartedAt, &finishedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentTurnRun{}, ErrNotFound
	}
	if err != nil {
		return AgentTurnRun{}, err
	}
	if finishedAt.Valid {
		run.FinishedAt = &finishedAt.String
	}
	return run, nil
}
