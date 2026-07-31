package reducer

import (
	"context"
	"database/sql"
	"errors"
)

// AgentToolReceiptRow is written in the exact transaction that advances the
// current timeline pointer. It is the crash boundary between “mutation may be
// retried” and “terminal result must be reused”.
type AgentToolReceiptRow struct {
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
}

type AgentTurnRunStartRow struct {
	TurnID       string
	DraftID      string
	SourceItemID string
	Kind         string
}

type AgentTurnRunFinishRow struct {
	TurnID string
	Status string
}

func persistAgentToolReceipt(
	ctx context.Context,
	tx *sql.Tx,
	receipt *AgentToolReceiptRow,
	createdAt string,
) error {
	if receipt == nil {
		return nil
	}
	if receipt.InvocationKey == "" || receipt.DraftID == "" || receipt.SourceMessageID == "" || receipt.TurnID == "" ||
		receipt.ToolCallID == "" || receipt.ToolName == "" || receipt.ArgumentFingerprint == "" ||
		receipt.AfterTimelineID == "" || receipt.AfterVersion < 1 || receipt.TerminalStatus == "" ||
		receipt.ResultJSON == "" {
		return errors.New("agent tool receipt 字段不完整")
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO agent_tool_receipts(
			invocation_key,draft_id,source_message_id,turn_id,tool_call_id,tool_name,
			argument_fingerprint,before_timeline_id,before_version,
			after_timeline_id,after_version,terminal_status,result_json,created_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		receipt.InvocationKey, receipt.DraftID, receipt.SourceMessageID, receipt.TurnID,
		receipt.ToolCallID, receipt.ToolName, receipt.ArgumentFingerprint,
		receipt.BeforeTimelineID, receipt.BeforeVersion, receipt.AfterTimelineID,
		receipt.AfterVersion, receipt.TerminalStatus, receipt.ResultJSON, createdAt,
	)
	return err
}

func persistAgentTurnRunStart(
	ctx context.Context,
	tx *sql.Tx,
	run *AgentTurnRunStartRow,
	createdAt string,
) error {
	if run == nil {
		return nil
	}
	if run.TurnID == "" || run.DraftID == "" || run.SourceItemID == "" || run.Kind == "" {
		return errors.New("agent turn run start 字段不完整")
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO agent_turn_runs(
			turn_id,draft_id,source_item_id,kind,status,started_at
		) VALUES(?,?,?,?, 'running', ?)`,
		run.TurnID, run.DraftID, run.SourceItemID, run.Kind, createdAt,
	)
	return err
}

func persistAgentTurnRunFinish(
	ctx context.Context,
	tx *sql.Tx,
	run *AgentTurnRunFinishRow,
	createdAt string,
) error {
	if run == nil {
		return nil
	}
	if run.TurnID == "" || !validAgentTurnTerminalStatus(run.Status) {
		return errors.New("agent turn run finish 字段无效")
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE agent_turn_runs SET status=?,finished_at=?
		WHERE turn_id=? AND status='running'`, run.Status, createdAt, run.TurnID,
	)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return errors.New("agent turn run 不存在或已终态")
	}
	return nil
}

func interruptRunningAgentTurns(
	ctx context.Context,
	tx *sql.Tx,
	createdAt string,
) error {
	// The synthetic system row is durable UI/audit evidence only. It is not a
	// user message and therefore can never enter TurnQueue reconciliation.
	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO messages(message_id,draft_id,role,kind,content,created_at)
		SELECT 'turn_interrupted:' || turn_id,draft_id,'system','turn_interrupted',
			json_object(
				'turn_id',turn_id,
				'source_item_id',source_item_id,
				'message','上一次 AI 任务因服务重启中断。为避免重复编辑，请由用户明确继续。'
			),?
		FROM agent_turn_runs WHERE status='running'`, createdAt,
	); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `
		UPDATE agent_turn_runs SET status='interrupted',finished_at=? WHERE status='running'`,
		createdAt,
	)
	return err
}

func validAgentTurnTerminalStatus(status string) bool {
	switch status {
	case "finished", "waiting_user", "cancelled", "failed", "timeout", "lease_lost":
		return true
	default:
		return false
	}
}
