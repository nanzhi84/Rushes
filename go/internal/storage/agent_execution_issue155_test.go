package storage

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func openAgentExecutionTestDB(t *testing.T) *DB {
	t.Helper()
	database, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func seedAgentExecutionDraft(t *testing.T, database *DB, draftID string) {
	t.Helper()
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO drafts(draft_id,name,created_at,updated_at)
		VALUES(?,?,?,?)`, draftID, draftID,
		"2026-07-31T12:00:00.000000000Z", "2026-07-31T12:00:00.000000000Z",
	); err != nil {
		t.Fatal(err)
	}
}

func TestAgentEditLeaseStorageProjectionAndTimeFailures(t *testing.T) {
	t.Parallel()
	database := openAgentExecutionTestDB(t)
	seedAgentExecutionDraft(t, database, "draft-lease-storage")
	base := time.Date(2026, 7, 31, 12, 0, 0, 123, time.UTC)
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO agent_edit_leases(
			draft_id,turn_id,lease_token,acquired_at,heartbeat_at,expires_at
		) VALUES(?,?,?,?,?,?)`,
		"draft-lease-storage", "turn-storage", "token-storage",
		formatAgentEditLeaseTime(base), formatAgentEditLeaseTime(base.Add(time.Second)),
		formatAgentEditLeaseTime(base.Add(time.Minute)),
	); err != nil {
		t.Fatal(err)
	}

	lease, err := GetAgentEditLease(t.Context(), database.Read(), "draft-lease-storage")
	if err != nil || lease.DraftID != "draft-lease-storage" || lease.TurnID != "turn-storage" ||
		lease.LeaseToken != "token-storage" || !lease.LiveAt(base) || lease.LiveAt(base.Add(time.Minute)) {
		t.Fatalf("lease=%#v err=%v", lease, err)
	}
	live, err := GetLiveAgentEditLease(t.Context(), database.Read(), "draft-lease-storage", base)
	if err != nil || live.ExpiresAt != base.Add(time.Minute) {
		t.Fatalf("live=%#v err=%v", live, err)
	}
	if _, err := GetLiveAgentEditLease(
		t.Context(), database.Read(), "draft-lease-storage", base.Add(time.Minute),
	); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired live lookup err=%v", err)
	}
	if _, err := GetAgentEditLease(t.Context(), database.Read(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing lease err=%v", err)
	}

	columns := []string{"acquired_at", "heartbeat_at", "expires_at"}
	for _, column := range columns {
		if _, err := database.Write().ExecContext(t.Context(), `
			UPDATE agent_edit_leases
			SET acquired_at=?,heartbeat_at=?,expires_at=?
			WHERE draft_id=?`,
			formatAgentEditLeaseTime(base), formatAgentEditLeaseTime(base),
			formatAgentEditLeaseTime(base.Add(time.Minute)), "draft-lease-storage",
		); err != nil {
			t.Fatal(err)
		}
		if _, err := database.Write().ExecContext(t.Context(),
			"UPDATE agent_edit_leases SET "+column+"='not-a-time' WHERE draft_id=?",
			"draft-lease-storage",
		); err != nil {
			t.Fatal(err)
		}
		if _, err := GetAgentEditLease(t.Context(), database.Read(), "draft-lease-storage"); err == nil || !strings.Contains(err.Error(), "解析 edit lease 时间") {
			t.Fatalf("column=%s parse err=%v", column, err)
		}
	}

	tx, err := database.Read().BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := GetAgentEditLease(t.Context(), tx, "draft-lease-storage"); !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("closed transaction lease lookup err=%v", err)
	}
}

func TestAgentExecutionStorageRoundTripMissingAndQueryErrors(t *testing.T) {
	t.Parallel()
	database := openAgentExecutionTestDB(t)
	seedAgentExecutionDraft(t, database, "draft-agent-execution-storage")
	const startedAt = "2026-07-31T12:00:00.000000000Z"
	const finishedAt = "2026-07-31T12:01:00.000000000Z"
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO agent_turn_runs(
			turn_id,draft_id,source_item_id,kind,status,started_at,finished_at
		) VALUES
			('turn-running','draft-agent-execution-storage','source-running','chat','running',?,NULL),
			('turn-finished','draft-agent-execution-storage','source-finished','chat','finished',?,?);
		INSERT INTO agent_tool_receipts(
			invocation_key,draft_id,source_message_id,turn_id,tool_call_id,tool_name,
			argument_fingerprint,before_timeline_id,before_version,
			after_timeline_id,after_version,terminal_status,result_json,created_at
		) VALUES
			('invocation-with-before','draft-agent-execution-storage','source-finished',
			 'turn-finished','call-with-before','timeline.insert','fingerprint-a',
			 'draft-agent-execution-storage:v1',1,'draft-agent-execution-storage:v2',2,
			 'succeeded','{"status":"succeeded"}',?),
			('invocation-without-before','draft-agent-execution-storage','source-finished',
			 'turn-finished','call-without-before','timeline.insert','fingerprint-b',
			 NULL,0,'draft-agent-execution-storage:v1',1,
			 'succeeded','{"status":"succeeded"}',?)`,
		startedAt, startedAt, finishedAt, finishedAt, finishedAt,
	); err != nil {
		t.Fatal(err)
	}

	withBefore, err := GetAgentToolReceipt(t.Context(), database.Read(), "turn-finished", "call-with-before")
	if err != nil || withBefore.BeforeTimelineID == nil ||
		*withBefore.BeforeTimelineID != "draft-agent-execution-storage:v1" ||
		withBefore.AfterVersion != 2 || withBefore.ResultJSON != `{"status":"succeeded"}` {
		t.Fatalf("with before=%#v err=%v", withBefore, err)
	}
	withoutBefore, err := GetAgentToolReceipt(
		t.Context(), database.Read(), "turn-finished", "call-without-before",
	)
	if err != nil || withoutBefore.BeforeTimelineID != nil || withoutBefore.BeforeVersion != 0 {
		t.Fatalf("without before=%#v err=%v", withoutBefore, err)
	}
	if _, err := GetAgentToolReceipt(t.Context(), database.Read(), "turn-finished", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing receipt err=%v", err)
	}

	running, err := GetAgentTurnRunBySource(
		t.Context(), database.Read(), "draft-agent-execution-storage", "source-running", "chat",
	)
	if err != nil || running.Status != "running" || running.FinishedAt != nil {
		t.Fatalf("running=%#v err=%v", running, err)
	}
	finished, err := GetAgentTurnRunBySource(
		t.Context(), database.Read(), "draft-agent-execution-storage", "source-finished", "chat",
	)
	if err != nil || finished.Status != "finished" || finished.FinishedAt == nil ||
		*finished.FinishedAt != finishedAt {
		t.Fatalf("finished=%#v err=%v", finished, err)
	}
	if _, err := GetAgentTurnRunBySource(
		t.Context(), database.Read(), "draft-agent-execution-storage", "missing", "chat",
	); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing turn err=%v", err)
	}

	tx, err := database.Read().BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := GetAgentToolReceipt(t.Context(), tx, "turn-finished", "call-with-before"); !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("closed transaction receipt err=%v", err)
	}
	if _, err := GetAgentTurnRunBySource(
		t.Context(), tx, "draft-agent-execution-storage", "source-finished", "chat",
	); !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("closed transaction turn err=%v", err)
	}
}
