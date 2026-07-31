package reducer

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

func applyAgentExecutionRows(
	t *testing.T,
	database *storage.DB,
	rows ResultRows,
) (Result, error) {
	t.Helper()
	return Apply(t.Context(), database, nil, Options{
		Actor:      contracts.ActorAgent,
		CreatedAt:  time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC),
		ResultRows: rows,
	})
}

func TestAgentTurnRunAndToolReceiptDurableLifecycle(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	const draftID = "draft-agent-execution-lifecycle"
	createDraft(t, database, draftID)

	terminalStatuses := []string{
		"finished", "waiting_user", "cancelled", "failed", "timeout", "lease_lost",
	}
	for index, status := range terminalStatuses {
		turnID := "turn-" + status
		sourceID := "source-" + status
		result, err := applyAgentExecutionRows(t, database, ResultRows{
			AgentTurnRunStart: &AgentTurnRunStartRow{
				TurnID: turnID, DraftID: draftID, SourceItemID: sourceID, Kind: "chat",
			},
		})
		if err != nil || result.Status != StatusApplied {
			t.Fatalf("start %s result=%#v err=%v", status, result, err)
		}

		beforeTimelineID := draftID + ":v1"
		receipt := &AgentToolReceiptRow{
			InvocationKey: "invocation-" + status, DraftID: draftID,
			SourceMessageID: sourceID, TurnID: turnID,
			ToolCallID: "call-" + status, ToolName: "timeline.insert",
			ArgumentFingerprint: "fingerprint-" + status,
			BeforeVersion:       index, AfterTimelineID: draftID + ":v2",
			AfterVersion: 2, TerminalStatus: "succeeded",
			ResultJSON: `{"status":"succeeded"}`,
		}
		if index > 0 {
			receipt.BeforeTimelineID = &beforeTimelineID
		}
		result, err = applyAgentExecutionRows(t, database, ResultRows{
			AgentToolReceipt: receipt,
			AgentTurnRunFinish: &AgentTurnRunFinishRow{
				TurnID: turnID, Status: status,
			},
		})
		if err != nil || result.Status != StatusApplied {
			t.Fatalf("finish %s result=%#v err=%v", status, result, err)
		}
		storedRun, err := storage.GetAgentTurnRunBySource(
			t.Context(), database.Read(), draftID, sourceID, "chat",
		)
		if err != nil || storedRun.Status != status || storedRun.FinishedAt == nil {
			t.Fatalf("stored run %s=%#v err=%v", status, storedRun, err)
		}
		storedReceipt, err := storage.GetAgentToolReceipt(
			t.Context(), database.Read(), turnID, "call-"+status,
		)
		if err != nil || storedReceipt.TerminalStatus != "succeeded" ||
			storedReceipt.AfterTimelineID != draftID+":v2" {
			t.Fatalf("stored receipt %s=%#v err=%v", status, storedReceipt, err)
		}
	}
}

func TestAgentExecutionRowsRejectInvalidAndDuplicateTransitions(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	const draftID = "draft-agent-execution-errors"
	createDraft(t, database, draftID)

	for name, rows := range map[string]ResultRows{
		"invalid start": {
			AgentTurnRunStart: &AgentTurnRunStartRow{TurnID: "turn-incomplete", DraftID: draftID},
		},
		"invalid finish status": {
			AgentTurnRunFinish: &AgentTurnRunFinishRow{TurnID: "turn-missing", Status: "running"},
		},
		"invalid receipt": {
			AgentToolReceipt: &AgentToolReceiptRow{InvocationKey: "incomplete"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := applyAgentExecutionRows(t, database, rows); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}

	start := ResultRows{AgentTurnRunStart: &AgentTurnRunStartRow{
		TurnID: "turn-once", DraftID: draftID, SourceItemID: "source-once", Kind: "chat",
	}}
	if _, err := applyAgentExecutionRows(t, database, start); err != nil {
		t.Fatal(err)
	}
	if _, err := applyAgentExecutionRows(t, database, start); err == nil {
		t.Fatal("duplicate turn start must fail")
	}
	if _, err := applyAgentExecutionRows(t, database, ResultRows{
		AgentTurnRunFinish: &AgentTurnRunFinishRow{TurnID: "turn-absent", Status: "failed"},
	}); err == nil || !strings.Contains(err.Error(), "不存在或已终态") {
		t.Fatalf("missing finish err=%v", err)
	}
	finish := ResultRows{AgentTurnRunFinish: &AgentTurnRunFinishRow{
		TurnID: "turn-once", Status: "finished",
	}}
	if _, err := applyAgentExecutionRows(t, database, finish); err != nil {
		t.Fatal(err)
	}
	if _, err := applyAgentExecutionRows(t, database, finish); err == nil ||
		!strings.Contains(err.Error(), "不存在或已终态") {
		t.Fatalf("duplicate finish err=%v", err)
	}

	receipt := &AgentToolReceiptRow{
		InvocationKey: "invocation-once", DraftID: draftID,
		SourceMessageID: "source-once", TurnID: "turn-once",
		ToolCallID: "call-once", ToolName: "timeline.insert",
		ArgumentFingerprint: "fingerprint-once", BeforeVersion: 0,
		AfterTimelineID: draftID + ":v1", AfterVersion: 1,
		TerminalStatus: "succeeded", ResultJSON: `{"status":"succeeded"}`,
	}
	if _, err := applyAgentExecutionRows(t, database, ResultRows{AgentToolReceipt: receipt}); err != nil {
		t.Fatal(err)
	}
	if _, err := applyAgentExecutionRows(t, database, ResultRows{AgentToolReceipt: receipt}); err == nil {
		t.Fatal("duplicate receipt must fail")
	}
}

func TestInterruptRunningAgentTurnsIsDurableAndIdempotent(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	const draftID = "draft-interrupt-agent-turns"
	createDraft(t, database, draftID)
	for _, turnID := range []string{"turn-running-a", "turn-running-b", "turn-finished"} {
		if _, err := applyAgentExecutionRows(t, database, ResultRows{
			AgentTurnRunStart: &AgentTurnRunStartRow{
				TurnID: turnID, DraftID: draftID, SourceItemID: "source-" + turnID, Kind: "chat",
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := applyAgentExecutionRows(t, database, ResultRows{
		AgentTurnRunFinish: &AgentTurnRunFinishRow{TurnID: "turn-finished", Status: "finished"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := applyAgentExecutionRows(t, database, ResultRows{
		InterruptRunningAgentTurns: true,
	}); err != nil {
		t.Fatal(err)
	}
	// A second startup reconciliation must neither duplicate audit messages nor
	// mutate already-terminal runs.
	if _, err := applyAgentExecutionRows(t, database, ResultRows{
		InterruptRunningAgentTurns: true,
	}); err != nil {
		t.Fatal(err)
	}
	var interrupted, finished, auditMessages int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT
			(SELECT COUNT(*) FROM agent_turn_runs WHERE draft_id=? AND status='interrupted'),
			(SELECT COUNT(*) FROM agent_turn_runs WHERE draft_id=? AND status='finished'),
			(SELECT COUNT(*) FROM messages WHERE draft_id=? AND kind='turn_interrupted')`,
		draftID, draftID, draftID,
	).Scan(&interrupted, &finished, &auditMessages); err != nil {
		t.Fatal(err)
	}
	if interrupted != 2 || finished != 1 || auditMessages != 2 {
		t.Fatalf("interrupted=%d finished=%d messages=%d", interrupted, finished, auditMessages)
	}
}

func TestAgentExecutionPersistencePropagatesTransactionAndUpdateErrors(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	const draftID = "draft-agent-execution-sql-errors"
	createDraft(t, database, draftID)
	if _, err := applyAgentExecutionRows(t, database, ResultRows{
		AgentTurnRunStart: &AgentTurnRunStartRow{
			TurnID: "turn-trigger", DraftID: draftID, SourceItemID: "source-trigger", Kind: "chat",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write().ExecContext(t.Context(), `
		CREATE TRIGGER reject_agent_turn_update
		BEFORE UPDATE ON agent_turn_runs
		BEGIN SELECT RAISE(ABORT, 'turn update rejected'); END`,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := applyAgentExecutionRows(t, database, ResultRows{
		AgentTurnRunFinish: &AgentTurnRunFinishRow{TurnID: "turn-trigger", Status: "failed"},
	}); err == nil || !strings.Contains(err.Error(), "turn update rejected") {
		t.Fatalf("triggered finish err=%v", err)
	}
	if _, err := database.Write().ExecContext(t.Context(), "DROP TRIGGER reject_agent_turn_update"); err != nil {
		t.Fatal(err)
	}

	if _, err := database.Write().ExecContext(t.Context(), `
		CREATE TRIGGER reject_interrupted_turn_update
		BEFORE UPDATE ON agent_turn_runs
		BEGIN SELECT RAISE(ABORT, 'interrupt update rejected'); END`,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := applyAgentExecutionRows(t, database, ResultRows{
		InterruptRunningAgentTurns: true,
	}); err == nil || !strings.Contains(err.Error(), "interrupt update rejected") {
		t.Fatalf("triggered interrupt err=%v", err)
	}
	var auditMessages int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM messages WHERE draft_id=? AND kind='turn_interrupted'`, draftID,
	).Scan(&auditMessages); err != nil || auditMessages != 0 {
		t.Fatalf("failed interrupt leaked messages=%d err=%v", auditMessages, err)
	}

	tx, err := database.Write().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := persistAgentTurnRunStart(t.Context(), tx, &AgentTurnRunStartRow{
		TurnID: "closed-start", DraftID: draftID, SourceItemID: "closed-source", Kind: "chat",
	}, "2026-07-31T12:00:00Z"); !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("closed start err=%v", err)
	}
	if err := persistAgentToolReceipt(t.Context(), tx, &AgentToolReceiptRow{
		InvocationKey: "closed-receipt", DraftID: draftID, SourceMessageID: "closed-source",
		TurnID: "turn-trigger", ToolCallID: "closed-call", ToolName: "timeline.insert",
		ArgumentFingerprint: "closed-fingerprint", AfterTimelineID: draftID + ":v1",
		AfterVersion: 1, TerminalStatus: "succeeded", ResultJSON: `{}`,
	}, "2026-07-31T12:00:00Z"); !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("closed receipt err=%v", err)
	}
	if err := interruptRunningAgentTurns(t.Context(), tx, "2026-07-31T12:00:00Z"); !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("closed interrupt err=%v", err)
	}
}

func TestAgentEditLeaseMutationValidationAndRareRenewalBoundaries(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	const draftID = "draft-agent-lease-boundaries"
	createDraft(t, database, draftID)
	for _, mutation := range []AgentEditLeaseMutation{
		{Operation: AgentEditLeaseAcquire, DraftID: draftID, TurnID: "turn", LeaseToken: "", TTL: time.Second},
		{Operation: AgentEditLeaseRenew, DraftID: draftID, TurnID: "turn", LeaseToken: "token", TTL: 0},
		{Operation: AgentEditLeaseRelease, DraftID: draftID, TurnID: "", LeaseToken: "token"},
		{Operation: AgentEditLeaseOperation("unknown")},
	} {
		if _, err := mutateTestLease(t, database, mutation); err == nil {
			t.Fatalf("mutation %#v must fail", mutation)
		}
	}

	acquired, err := mutateTestLease(t, database, AgentEditLeaseMutation{
		Operation: AgentEditLeaseAcquire, DraftID: draftID,
		TurnID: "turn-default-now", LeaseToken: "token-default-now", TTL: time.Minute,
	})
	if err != nil || acquired.AgentEditLease == nil || acquired.AgentEditLease.Lease == nil ||
		acquired.AgentEditLease.Lease.AcquiredAt.IsZero() {
		t.Fatalf("default now acquire=%#v err=%v", acquired, err)
	}
	if _, err := mutateTestLease(t, database, AgentEditLeaseMutation{
		Operation: AgentEditLeaseRelease, DraftID: draftID,
		TurnID: "turn-default-now", LeaseToken: "token-default-now",
	}); err != nil {
		t.Fatal(err)
	}

	base := time.Date(2026, 7, 31, 13, 0, 0, 0, time.UTC)
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO agent_edit_leases(
			draft_id,turn_id,lease_token,acquired_at,heartbeat_at,expires_at
		) VALUES(?,?,?,?,?,?)`, draftID, "turn-malformed", "token-malformed", "bad-acquired-at",
		editLeaseTimestamp(base), editLeaseTimestamp(base.Add(time.Minute)),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := mutateTestLease(t, database, AgentEditLeaseMutation{
		Operation: AgentEditLeaseRenew, DraftID: draftID,
		TurnID: "turn-malformed", LeaseToken: "token-malformed", Now: base, TTL: time.Minute,
	}); err == nil || !strings.Contains(err.Error(), "解析 edit lease 时间") {
		t.Fatalf("malformed renewed lease err=%v", err)
	}
	if _, err := database.Write().ExecContext(t.Context(),
		"DELETE FROM agent_edit_leases WHERE draft_id=?", draftID,
	); err != nil {
		t.Fatal(err)
	}

	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO agent_edit_leases(
			draft_id,turn_id,lease_token,acquired_at,heartbeat_at,expires_at
		) VALUES(?,?,?,?,?,?);
		CREATE TRIGGER steal_renewed_agent_lease
		AFTER UPDATE ON agent_edit_leases
		BEGIN UPDATE agent_edit_leases SET lease_token='token-stolen' WHERE draft_id=NEW.draft_id; END`,
		draftID, "turn-owned", "token-owned", editLeaseTimestamp(base),
		editLeaseTimestamp(base), editLeaseTimestamp(base.Add(time.Minute)),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := mutateTestLease(t, database, AgentEditLeaseMutation{
		Operation: AgentEditLeaseRenew, DraftID: draftID,
		TurnID: "turn-owned", LeaseToken: "token-owned", Now: base, TTL: time.Minute,
	}); !errors.Is(err, storage.ErrAgentEditLeaseLost) {
		t.Fatalf("stolen renewed lease err=%v", err)
	}

	tx, err := database.Write().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []AgentEditLeaseMutation{
		{Operation: AgentEditLeaseAcquire, DraftID: draftID, TurnID: "turn", LeaseToken: "token", TTL: time.Second},
		{Operation: AgentEditLeaseRenew, DraftID: draftID, TurnID: "turn", LeaseToken: "token", TTL: time.Second},
		{Operation: AgentEditLeaseRelease, DraftID: draftID, TurnID: "turn", LeaseToken: "token"},
		{Operation: AgentEditLeaseExpireStale},
	} {
		if _, err := persistAgentEditLeaseMutation(t.Context(), tx, mutation); !errors.Is(err, sql.ErrTxDone) {
			t.Fatalf("closed tx mutation=%s err=%v", mutation.Operation, err)
		}
	}
}

func TestTimelineWriteAdmissionLegacyAndQueryErrorBranches(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	const draftID = "draft-timeline-admission-branches"
	createDraft(t, database, draftID)
	event := contracts.Event{Type: "TimelineVersionCreated", DraftID: draftID}

	tx, err := database.Write().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateTimelineWriteAdmission(
		t.Context(), tx, nil, contracts.ActorJob, nil,
	); err != nil {
		t.Fatalf("non-pointer events err=%v", err)
	}
	if err := validateTimelineWriteAdmission(
		t.Context(), tx, []contracts.Event{event}, contracts.ActorJob, nil,
	); err != nil {
		t.Fatalf("legacy infrastructure write without lease err=%v", err)
	}
	if err := validateTimelineWriteAdmission(
		t.Context(), tx, []contracts.Event{event}, contracts.ActorUser,
		&TimelineWriteAdmission{Origin: "agent", TurnID: "turn", LeaseToken: "token"},
	); !errors.Is(err, storage.ErrAgentEditLeaseLost) {
		t.Fatalf("user claiming agent origin err=%v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := validateTimelineWriteAdmission(
		t.Context(), tx, []contracts.Event{event}, contracts.ActorJob, nil,
	); !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("closed transaction admission err=%v", err)
	}
}
