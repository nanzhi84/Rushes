package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

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
}

const jobColumns = `
job_id, kind, status, draft_id, requested_by_draft_id, asset_id,
payload_json, attempts, max_retries, priority, worker_id, started_at`

func Claim(ctx context.Context, database *storage.DB, workerID string, now time.Time) (*Job, error) {
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
	_, err = tx.ExecContext(ctx, `
		UPDATE jobs SET status='running', worker_id=?, started_at=?, heartbeat_at=?
		WHERE job_id=(
			SELECT job_id FROM jobs
			WHERE status='pending' AND next_run_at<=?
			ORDER BY priority, created_at, job_id LIMIT 1
		) AND status='pending'`, workerID, timestamp, timestamp, timestamp)
	if err != nil {
		return nil, err
	}
	var changed int
	if err := tx.QueryRowContext(ctx, "SELECT changes()").Scan(&changed); err != nil {
		return nil, err
	}
	if changed == 0 {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		committed = true
		return nil, nil
	}
	row := tx.QueryRowContext(ctx, `
		SELECT `+jobColumns+` FROM jobs
		WHERE status='running' AND worker_id=? AND started_at=? AND heartbeat_at=?
		ORDER BY priority, created_at, job_id LIMIT 1`, workerID, timestamp, timestamp)
	job, err := scanJob(row)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	committed = true
	return &job, nil
}

func GetJob(ctx context.Context, database *storage.DB, jobID string) (Job, error) {
	return scanJob(database.Read().QueryRowContext(ctx, "SELECT "+jobColumns+" FROM jobs WHERE job_id=?", jobID))
}

func scanJob(row interface{ Scan(...any) error }) (Job, error) {
	var job Job
	var draftID, requestedByDraftID, assetID, workerID, startedAt sql.NullString
	var payloadJSON string
	if err := row.Scan(
		&job.ID, &job.Kind, &job.Status, &draftID, &requestedByDraftID, &assetID,
		&payloadJSON, &job.Attempts, &job.MaxRetries, &job.Priority, &workerID, &startedAt,
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

func RecoverStale(
	ctx context.Context,
	database *storage.DB,
	now time.Time,
	timeout time.Duration,
) (int64, error) {
	cutoff := now.Add(-timeout).UTC().Format(time.RFC3339Nano)
	next := now.UTC().Format(time.RFC3339Nano)
	result, err := database.Write().ExecContext(ctx, `
		UPDATE jobs SET status='pending', worker_id=NULL, heartbeat_at=NULL,
		started_at=NULL, next_run_at=?
		WHERE status='running' AND (heartbeat_at IS NULL OR heartbeat_at<?)`, next, cutoff)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
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

func mustJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(data)
}
