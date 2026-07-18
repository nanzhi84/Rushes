package storage

import (
	"errors"
	"testing"
)

func TestRewindCheckpointQueriesHandleMissingRowsAndStorageFailures(t *testing.T) {
	t.Parallel()
	database, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if _, err := GetRewindCheckpoint(t.Context(), database.Read(), "missing-draft", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("缺失检查点应返回 ErrNotFound: %v", err)
	}
	if _, err := database.Write().ExecContext(t.Context(), "DROP TABLE rewind_checkpoints"); err != nil {
		t.Fatal(err)
	}
	if _, err := GetRewindCheckpoint(t.Context(), database.Read(), "missing-draft", "missing"); err == nil {
		t.Fatal("缺少 rewind_checkpoints 表时查询必须失败")
	}
	if _, err := scanRewindCheckpoint(failingRewindScanner{}); err == nil {
		t.Fatal("scan 错误必须透传")
	}
}

func TestRewindRestoreResultRejectsCorruptPersistedEventIDs(t *testing.T) {
	t.Parallel()
	database, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO drafts(draft_id,name,state_version,status,created_at,updated_at)
		VALUES('draft-corrupt-rewind-result','corrupt',0,'active','now','now');
		INSERT INTO rewind_restore_requests(
			draft_id,idempotency_key,checkpoint_id,mode,rewound_message_count,
			cancelled_jobs,cancelled_decisions,event_ids_json,created_at
		) VALUES('draft-corrupt-rewind-result','request','checkpoint','conversation',0,0,0,'{','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := GetRewindRestoreResult(
		t.Context(), database.Read(), "draft-corrupt-rewind-result", "request",
	); err == nil {
		t.Fatal("损坏的 event_ids_json 不得伪装成可重放的恢复响应")
	}
}

type failingRewindScanner struct{}

func (failingRewindScanner) Scan(...any) error {
	return errors.New("forced rewind scan failure")
}
