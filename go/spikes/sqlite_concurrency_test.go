package spikes

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestModerncSQLiteReducerSSEWorkerConcurrency(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "workspace.db")
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_txlock=immediate"

	writeDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writeDB.Close() }()
	writeDB.SetMaxOpenConns(1)

	readDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = readDB.Close() }()
	readDB.SetMaxOpenConns(runtime.NumCPU())

	_, err = writeDB.Exec(`
		CREATE TABLE event_log (
			event_id INTEGER PRIMARY KEY AUTOINCREMENT,
			event_type TEXT NOT NULL,
			payload TEXT NOT NULL
		);
		CREATE TABLE jobs (
			job_id INTEGER PRIMARY KEY AUTOINCREMENT,
			status TEXT NOT NULL,
			created_at INTEGER NOT NULL
		);
	`)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	var busyErrors atomic.Int64
	var otherErrors atomic.Int64
	recordError := func(err error) {
		if err == nil {
			return
		}
		lower := strings.ToLower(err.Error())
		if strings.Contains(lower, "busy") || strings.Contains(lower, "locked") {
			busyErrors.Add(1)
			return
		}
		otherErrors.Add(1)
	}

	var wg sync.WaitGroup
	wg.Add(3)

	// reducer：每批事件在同一 IMMEDIATE 事务内提交。
	go func() {
		defer wg.Done()
		for batch := range 80 {
			tx, err := writeDB.BeginTx(ctx, nil)
			if err != nil {
				recordError(err)
				return
			}
			for event := range 8 {
				_, err = tx.ExecContext(ctx,
					"INSERT INTO event_log(event_type, payload) VALUES (?, ?)",
					"ReducerBatch", fmt.Sprintf(`{"batch":%d,"event":%d}`, batch, event),
				)
				if err != nil {
					break
				}
			}
			if err == nil {
				err = tx.Commit()
			} else {
				_ = tx.Rollback()
			}
			recordError(err)
		}
	}()

	// worker：和 reducer 共用单写连接，事务内原子 claim。
	go func() {
		defer wg.Done()
		for index := range 160 {
			_, err := writeDB.ExecContext(ctx,
				"INSERT INTO jobs(status, created_at) VALUES ('pending', ?)", index,
			)
			if err != nil {
				recordError(err)
				return
			}
			_, err = writeDB.ExecContext(ctx, `
				UPDATE jobs SET status='running'
				WHERE job_id=(
					SELECT job_id FROM jobs WHERE status='pending'
					ORDER BY created_at, job_id LIMIT 1
				) AND status='pending'
			`)
			if err != nil {
				recordError(err)
				return
			}
		}
	}()

	// SSE：独立读池持续尾随 event_log。
	go func() {
		defer wg.Done()
		for range 800 {
			var latest int64
			err := readDB.QueryRowContext(ctx,
				"SELECT COALESCE(MAX(event_id), 0) FROM event_log",
			).Scan(&latest)
			if err != nil {
				recordError(err)
				return
			}
		}
	}()

	wg.Wait()
	if count := busyErrors.Load(); count != 0 {
		t.Fatalf("SQLITE_BUSY/locked 泄漏 %d 次", count)
	}
	if count := otherErrors.Load(); count != 0 {
		t.Fatalf("其他并发错误 %d 次", count)
	}

	var events, running int
	if err := readDB.QueryRow("SELECT COUNT(*) FROM event_log").Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := readDB.QueryRow("SELECT COUNT(*) FROM jobs WHERE status='running'").Scan(&running); err != nil {
		t.Fatal(err)
	}
	if events != 640 || running != 160 {
		t.Fatalf("events=%d running=%d", events, running)
	}
}
