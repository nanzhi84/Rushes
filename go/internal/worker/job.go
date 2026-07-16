package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

type Job struct {
	ID                 string
	Kind               string
	Status             string
	DraftID            *string
	RequestedByDraftID *string
	AssetID            *string
	Payload            map[string]any
	Attempts           int
	MaxRetries         int
	Priority           int
	WorkerID           *string
	StartedAt          *string
	HeartbeatAt        *string
}

const jobColumns = `
job_id, kind, status, draft_id, requested_by_draft_id, asset_id,
payload_json, attempts, max_retries, priority, worker_id, started_at, heartbeat_at`

func Claim(ctx context.Context, database *storage.DB, workerID string, now time.Time) (*Job, error) {
	return ClaimMatching(ctx, database, workerID, now, ClaimFilter{})
}

type ClaimFilter struct {
	IncludeKinds []string
	ExcludeKinds []string
}

func ClaimMatching(
	ctx context.Context,
	database *storage.DB,
	workerID string,
	now time.Time,
	filter ClaimFilter,
) (*Job, error) {
	if len(filter.IncludeKinds) > 0 && len(filter.ExcludeKinds) > 0 {
		return nil, errors.New("job claim filter 不能同时 include 与 exclude")
	}
	predicate, kindArgs := claimKindPredicate(filter)
	timestamp := now.UTC().Format(time.RFC3339Nano)
	tx, err := database.Write().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	arguments := []any{workerID, timestamp, timestamp, timestamp}
	arguments = append(arguments, kindArgs...)
	row := tx.QueryRowContext(ctx, `
		UPDATE jobs SET status='running', worker_id=?, started_at=?, heartbeat_at=?
		WHERE job_id=(
			SELECT job_id FROM jobs
			WHERE status='pending' AND next_run_at<=?`+predicate+`
			ORDER BY priority, created_at, job_id LIMIT 1
		) AND status='pending'
		RETURNING `+jobColumns, arguments...)
	job, err := scanJob(row)
	if errors.Is(err, storage.ErrNotFound) {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		committed = true
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	committed = true
	return &job, nil
}

func claimKindPredicate(filter ClaimFilter) (string, []any) {
	kinds := filter.IncludeKinds
	operator := "IN"
	if len(kinds) == 0 {
		kinds = filter.ExcludeKinds
		operator = "NOT IN"
	}
	if len(kinds) == 0 {
		return "", nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(kinds)), ",")
	arguments := make([]any, len(kinds))
	for index, kind := range kinds {
		arguments[index] = kind
	}
	return " AND kind " + operator + " (" + placeholders + ")", arguments
}

func GetJob(ctx context.Context, database *storage.DB, jobID string) (Job, error) {
	return scanJob(database.Read().QueryRowContext(ctx, "SELECT "+jobColumns+" FROM jobs WHERE job_id=?", jobID))
}

func scanJob(row interface{ Scan(...any) error }) (Job, error) {
	var job Job
	var draftID, requestedByDraftID, assetID, workerID, startedAt, heartbeatAt sql.NullString
	var payloadJSON string
	if err := row.Scan(
		&job.ID, &job.Kind, &job.Status, &draftID, &requestedByDraftID, &assetID,
		&payloadJSON, &job.Attempts, &job.MaxRetries, &job.Priority, &workerID, &startedAt, &heartbeatAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Job{}, storage.ErrNotFound
		}
		return Job{}, err
	}
	job.DraftID = nullStringPointer(draftID)
	job.RequestedByDraftID = nullStringPointer(requestedByDraftID)
	job.AssetID = nullStringPointer(assetID)
	job.WorkerID = nullStringPointer(workerID)
	job.StartedAt = nullStringPointer(startedAt)
	job.HeartbeatAt = nullStringPointer(heartbeatAt)
	if err := json.Unmarshal([]byte(payloadJSON), &job.Payload); err != nil {
		return Job{}, fmt.Errorf("job %s payload 无效: %w", job.ID, err)
	}
	return job, nil
}

func Heartbeat(ctx context.Context, database *storage.DB, job Job, now time.Time) (bool, error) {
	result, err := database.Write().ExecContext(ctx, `
		UPDATE jobs SET heartbeat_at=?
		WHERE job_id=? AND worker_id=? AND started_at=? AND status='running'`,
		now.UTC().Format(time.RFC3339Nano), job.ID, value(job.WorkerID), value(job.StartedAt))
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

func ReleaseClaim(ctx context.Context, database *storage.DB, job Job) (bool, error) {
	result, err := database.Write().ExecContext(ctx, `
		UPDATE jobs SET status='pending', worker_id=NULL, heartbeat_at=NULL, started_at=NULL
		WHERE job_id=? AND worker_id=? AND started_at=? AND status='running'`,
		job.ID, value(job.WorkerID), value(job.StartedAt))
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

func RecoverStale(
	ctx context.Context,
	database *storage.DB,
	now time.Time,
	timeout time.Duration,
) (int64, error) {
	cutoff := now.Add(-timeout).UTC().Format(time.RFC3339Nano)
	next := now.UTC().Format(time.RFC3339Nano)
	rows, err := database.Read().QueryContext(ctx, `
		SELECT `+jobColumns+` FROM jobs
		WHERE status='running' AND (heartbeat_at IS NULL OR heartbeat_at<?)
		ORDER BY job_id`, cutoff)
	if err != nil {
		return 0, err
	}
	stale := []Job{}
	for rows.Next() {
		job, scanErr := scanJob(rows)
		if scanErr != nil {
			_ = rows.Close()
			return 0, scanErr
		}
		stale = append(stale, job)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}

	var recovered int64
	for _, job := range stale {
		attempts := job.Attempts + 1
		if attempts > job.MaxRetries {
			failure := map[string]any{
				"error_code":   "stale_recovery_exhausted",
				"message":      "job 心跳过期且重试额度已耗尽",
				"worker_id":    value(job.WorkerID),
				"heartbeat_at": value(job.HeartbeatAt),
			}
			result, applyErr := reducer.Apply(ctx, database, []contracts.Event{{
				Type: "JobFailed", DraftID: value(job.DraftID),
				Payload: map[string]any{
					"job_id": job.ID, "kind": job.Kind, "asset_id": value(job.AssetID),
					"requested_by_draft_id": value(job.RequestedByDraftID),
					"worker_id":             value(job.WorkerID), "started_at": value(job.StartedAt),
					"heartbeat_at": value(job.HeartbeatAt), "attempts": attempts,
					"progress": 1.0, "error": failure,
				},
			}}, reducer.Options{Actor: contracts.ActorJob, CreatedAt: now})
			if errors.Is(applyErr, reducer.ErrJobCancelled) || errors.Is(applyErr, reducer.ErrJobClaimLost) {
				continue
			}
			if applyErr != nil {
				return recovered, applyErr
			}
			if result.Status != reducer.StatusApplied {
				return recovered, fmt.Errorf("stale job %s 终态写入失败: %s", job.ID, result.Status)
			}
			if len(result.AppliedEvents) == 1 {
				recovered++
			}
			continue
		}

		// 未正常释放 claim 的进程退出会消耗一次重试额度；这是防止
		// OOM/SIGKILL 类任务无限崩溃重领的有意取舍。
		result, updateErr := database.Write().ExecContext(ctx, `
			UPDATE jobs SET status='pending', worker_id=NULL, heartbeat_at=NULL,
			started_at=NULL, attempts=?, next_run_at=?
			WHERE job_id=? AND status='running'
			AND worker_id IS ? AND started_at IS ? AND heartbeat_at IS ?
			AND (heartbeat_at IS NULL OR heartbeat_at<?)`,
			attempts, next, job.ID, pointerDatabaseValue(job.WorkerID),
			pointerDatabaseValue(job.StartedAt), pointerDatabaseValue(job.HeartbeatAt), cutoff)
		if updateErr != nil {
			return recovered, updateErr
		}
		changed, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			return recovered, rowsErr
		}
		recovered += changed
	}
	return recovered, nil
}

func ScheduleRetry(
	ctx context.Context,
	database *storage.DB,
	job Job,
	now time.Time,
	failure map[string]any,
) (bool, error) {
	attempts := job.Attempts + 1
	delay := RetryDelay(attempts)
	result, err := database.Write().ExecContext(ctx, `
		UPDATE jobs SET status='pending', worker_id=NULL, heartbeat_at=NULL,
		started_at=NULL, attempts=?, next_run_at=?, error_json=?
		WHERE job_id=? AND worker_id=? AND started_at=? AND status='running'`, attempts,
		now.Add(delay).UTC().Format(time.RFC3339Nano), mustJSON(failure), job.ID,
		value(job.WorkerID), value(job.StartedAt))
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

func RetryDelay(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	seconds := 1 << min(attempts-1, 6)
	if seconds > 60 {
		seconds = 60
	}
	return time.Duration(seconds) * time.Second
}

func nullStringPointer(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}

func pointerDatabaseValue(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func mustJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(data)
}
