package agentexec

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
	"github.com/nanzhi84/Rushes/go/internal/timeline"
)

const (
	userExportJobKind      = "render_final"
	userExportOrigin       = "user"
	userExportListLimit    = 20
	userExportApplyRetries = 4
)

var (
	ErrUserExportTimelineRequired = errors.New("用户导出需要 timeline_id")
	ErrUserExportNotFound         = errors.New("用户导出任务不存在")
	ErrUserExportNotRetryable     = errors.New("用户导出任务当前不可重试")
	ErrUserExportStaleTimeline    = errors.New("用户导出目标不是草稿当前时间线")
	ErrUserExportStateConflict    = errors.New("用户导出创建时草稿持续变化，请重试")
)

// UserExportValidationError 保留结构化校验报告，供 REST 层返回稳定的失败原因。
// 最终导出由用户动作触发，因此这里不包装成 ToolResult，也不会进入工具 Registry。
type UserExportValidationError struct {
	Report map[string]any
}

func (err *UserExportValidationError) Error() string {
	return "目标时间线未通过导出前校验"
}

type UserExportFailure struct {
	Code      string
	Message   string
	Retryable bool
}

// UserExportRecord 是持久 jobs 行的用户侧投影。timeline_id/version 来自入队时
// 固定的 payload，不会用草稿当前版本覆盖，因而刷新、失败与显式重试都保持同一目标。
type UserExportRecord struct {
	JobID           string
	Status          string
	TimelineID      string
	TimelineVersion int
	Orientation     string
	Progress        float64
	ExportID        string
	Profile         string
	Failure         *UserExportFailure
	Retryable       bool
	RetryOfJobID    string
	Attempts        int
	MaxRetries      int
	CreatedAt       string
	StartedAt       string
	FinishedAt      string
}

type userExportJob struct {
	Record             UserExportRecord
	RootIdempotencyKey string
}

// CreateUserExport 为用户显式创建最终导出。相同 timeline/orientation 的重复点击
// 返回同一条持久任务；失败任务只允许通过 RetryUserExport 显式派生新任务。
func (exec *Executor) CreateUserExport(
	ctx context.Context,
	draftID, timelineID, orientation string,
) (UserExportRecord, error) {
	timelineID = strings.TrimSpace(timelineID)
	if timelineID == "" {
		return UserExportRecord{}, ErrUserExportTimelineRequired
	}
	normalized, err := normalizeRenderOrientation(orientation)
	if err != nil {
		return UserExportRecord{}, err
	}
	draft, err := storage.GetDraft(ctx, exec.database.Read(), draftID)
	if err != nil {
		return UserExportRecord{}, err
	}
	document, err := timeline.GetByID(ctx, exec.database, draftID, timelineID)
	if err != nil {
		return UserExportRecord{}, err
	}
	if draft.TimelineCurrentVersion == nil || *draft.TimelineCurrentVersion != document.Version {
		return UserExportRecord{}, ErrUserExportStaleTimeline
	}
	if err := exec.ensureUserExportUnlocked(ctx, draftID, time.Now().UTC()); err != nil {
		return UserExportRecord{}, err
	}
	rootKey := userExportIdempotencyKey(draftID, document.TimelineID, normalized)
	if existing, found, findErr := exec.FindRenderJob(ctx, userExportJobKind, rootKey, true); findErr != nil {
		return UserExportRecord{}, findErr
	} else if found {
		return exec.UserExport(ctx, draftID, existing.ID)
	}
	if err := exec.validateUserExportTimeline(ctx, draftID, document); err != nil {
		return UserExportRecord{}, err
	}
	return exec.enqueueUserExport(ctx, draftID, document, normalized, rootKey, rootKey, "", true)
}

// RetryUserExport 只接受属于当前草稿的 user-origin final job，并从旧 payload 复制
// timeline_id/version/orientation。即使草稿当前已是 N+1，重试仍严格渲染原来的 N。
func (exec *Executor) RetryUserExport(
	ctx context.Context,
	draftID, jobID string,
) (UserExportRecord, error) {
	if err := exec.ensureUserExportUnlocked(ctx, draftID, time.Now().UTC()); err != nil {
		return UserExportRecord{}, err
	}
	job, err := exec.loadUserExport(ctx, draftID, strings.TrimSpace(jobID))
	if err != nil {
		return UserExportRecord{}, err
	}
	if job.Record.Status != "failed" && job.Record.Status != "cancelled" {
		return UserExportRecord{}, ErrUserExportNotRetryable
	}
	document, err := timeline.GetByID(ctx, exec.database, draftID, job.Record.TimelineID)
	if err != nil {
		return UserExportRecord{}, err
	}
	if document.Version != job.Record.TimelineVersion {
		return UserExportRecord{}, errors.New("用户导出任务的时间线版本不一致")
	}
	if err := exec.validateUserExportTimeline(ctx, draftID, document); err != nil {
		return UserExportRecord{}, err
	}
	rootKey := job.RootIdempotencyKey
	if rootKey == "" {
		rootKey = userExportIdempotencyKey(draftID, document.TimelineID, job.Record.Orientation)
	}
	retryKey := fmt.Sprintf("%s:retry:%s", rootKey, job.Record.JobID)
	if existing, found, findErr := exec.FindRenderJob(ctx, userExportJobKind, retryKey, false); findErr != nil {
		return UserExportRecord{}, findErr
	} else if found {
		return exec.UserExport(ctx, draftID, existing.ID)
	}
	return exec.enqueueUserExport(
		ctx, draftID, document, job.Record.Orientation, rootKey, retryKey, job.Record.JobID, false,
	)
}

func (exec *Executor) UserExport(
	ctx context.Context,
	draftID, jobID string,
) (UserExportRecord, error) {
	job, err := exec.loadUserExport(ctx, draftID, strings.TrimSpace(jobID))
	return job.Record, err
}

func (exec *Executor) ListUserExports(
	ctx context.Context,
	draftID string,
) ([]UserExportRecord, error) {
	if _, err := storage.GetDraft(ctx, exec.database.Read(), draftID); err != nil {
		return nil, err
	}
	rows, err := exec.database.Read().QueryContext(ctx, `
		SELECT job_id
		FROM jobs
		WHERE kind=? AND (draft_id=? OR requested_by_draft_id=?)
		AND json_extract(payload_json, '$.request_origin')=?
		ORDER BY rowid DESC LIMIT ?`,
		userExportJobKind, draftID, draftID, userExportOrigin, userExportListLimit,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	jobIDs := make([]string, 0, userExportListLimit)
	for rows.Next() {
		var jobID string
		if err := rows.Scan(&jobID); err != nil {
			return nil, err
		}
		jobIDs = append(jobIDs, jobID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	records := make([]UserExportRecord, 0, len(jobIDs))
	for _, jobID := range jobIDs {
		record, err := exec.UserExport(ctx, draftID, jobID)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}

func (exec *Executor) validateUserExportTimeline(
	ctx context.Context,
	draftID string,
	document timeline.Document,
) error {
	report, valid, err := exec.timelineValidationReport(ctx, draftID, document)
	if err != nil {
		return err
	}
	if !valid {
		return &UserExportValidationError{Report: report}
	}
	return nil
}

func (exec *Executor) enqueueUserExport(
	ctx context.Context,
	draftID string,
	document timeline.Document,
	orientation, rootKey, idempotencyKey, retryOfJobID string,
	requireCurrent bool,
) (UserExportRecord, error) {
	report, valid, err := exec.timelineValidationReport(ctx, draftID, document)
	if err != nil {
		return UserExportRecord{}, err
	}
	if !valid {
		return UserExportRecord{}, &UserExportValidationError{Report: report}
	}
	for attempt := 0; attempt < userExportApplyRetries; attempt++ {
		if existing, found, findErr := exec.FindRenderJob(
			ctx, userExportJobKind, idempotencyKey, false,
		); findErr != nil {
			return UserExportRecord{}, findErr
		} else if found {
			return exec.UserExport(ctx, draftID, existing.ID)
		}
		draft, draftErr := storage.GetDraft(ctx, exec.database.Read(), draftID)
		if draftErr != nil {
			return UserExportRecord{}, draftErr
		}
		if requireCurrent &&
			(draft.TimelineCurrentVersion == nil || *draft.TimelineCurrentVersion != document.Version) {
			return UserExportRecord{}, ErrUserExportStaleTimeline
		}
		now := time.Now().UTC()
		if leaseErr := exec.ensureUserExportUnlocked(ctx, draftID, now); leaseErr != nil {
			return UserExportRecord{}, leaseErr
		}
		jobID := RandomID("job")
		jobPayload := map[string]any{
			"request_origin":       userExportOrigin,
			"timeline_id":          document.TimelineID,
			"timeline_version":     document.Version,
			"orientation":          orientation,
			"root_idempotency_key": rootKey,
		}
		if retryOfJobID != "" {
			jobPayload["retry_of_job_id"] = retryOfJobID
		}
		var transactionValidationErr error
		result, applyErr := reducer.Apply(ctx, exec.database, []contracts.Event{{
			Type: "TimelineValidated", DraftID: draftID,
			Payload: map[string]any{
				"timeline_version": document.Version, "validation_report": report,
			},
		}, {
			Type: "JobEnqueued", DraftID: draftID,
			Payload: map[string]any{
				"job_id": jobID, "kind": userExportJobKind,
				"requested_by_draft_id": draftID, "idempotency_key": idempotencyKey,
				"job_payload": jobPayload,
				"next_run_at": now.Format(time.RFC3339Nano),
				"priority":    30, "max_retries": 0,
			},
		}}, reducer.Options{
			Actor: contracts.ActorUser, BaseVersion: &draft.StateVersion,
			Validate: func(validateCtx context.Context, tx *sql.Tx, _ []string) error {
				if _, leaseErr := storage.GetLiveAgentEditLease(
					validateCtx, tx, draftID, now,
				); leaseErr == nil {
					transactionValidationErr = storage.ErrTimelineLockedByAgent
					return transactionValidationErr
				} else if !errors.Is(leaseErr, storage.ErrNotFound) {
					transactionValidationErr = leaseErr
					return transactionValidationErr
				}
				if !requireCurrent {
					return nil
				}
				var currentVersion sql.NullInt64
				if queryErr := tx.QueryRowContext(validateCtx, `
					SELECT timeline_current_version FROM drafts WHERE draft_id=?`, draftID,
				).Scan(&currentVersion); queryErr != nil {
					transactionValidationErr = queryErr
					return transactionValidationErr
				}
				if !currentVersion.Valid || int(currentVersion.Int64) != document.Version {
					transactionValidationErr = ErrUserExportStaleTimeline
					return transactionValidationErr
				}
				return nil
			},
		})
		if applyErr == nil && result.Status == reducer.StatusApplied {
			return exec.UserExport(ctx, draftID, jobID)
		}
		if result.Status == reducer.StatusVersionConflict {
			continue
		}
		if result.Status == reducer.StatusValidationFailed {
			if transactionValidationErr != nil {
				return UserExportRecord{}, transactionValidationErr
			}
			return UserExportRecord{}, errors.New("用户导出事务校验失败")
		}
		if existing, found, findErr := exec.FindRenderJob(
			ctx, userExportJobKind, idempotencyKey, false,
		); findErr != nil {
			return UserExportRecord{}, errors.Join(applyErr, findErr)
		} else if found {
			return exec.UserExport(ctx, draftID, existing.ID)
		}
		return UserExportRecord{}, errors.Join(
			applyErr, fmt.Errorf("user export reducer status: %s", result.Status),
		)
	}
	return UserExportRecord{}, ErrUserExportStateConflict
}

func (exec *Executor) ensureUserExportUnlocked(
	ctx context.Context,
	draftID string,
	now time.Time,
) error {
	if _, err := storage.GetLiveAgentEditLease(
		ctx, exec.database.Read(), draftID, now,
	); err == nil {
		return storage.ErrTimelineLockedByAgent
	} else if !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	return nil
}

func (exec *Executor) loadUserExport(
	ctx context.Context,
	draftID, jobID string,
) (userExportJob, error) {
	if jobID == "" {
		return userExportJob{}, ErrUserExportNotFound
	}
	var status, payloadJSON, createdAt string
	var resultJSON, errorJSON, startedAt, finishedAt sql.NullString
	var progress sql.NullFloat64
	var attempts, maxRetries int
	err := exec.database.Read().QueryRowContext(ctx, `
		SELECT status, payload_json, result_json, error_json, progress,
		       attempts, max_retries, created_at, started_at, finished_at
		FROM jobs
		WHERE job_id=? AND kind=? AND (draft_id=? OR requested_by_draft_id=?)
		AND json_extract(payload_json, '$.request_origin')=?`,
		jobID, userExportJobKind, draftID, draftID, userExportOrigin,
	).Scan(
		&status, &payloadJSON, &resultJSON, &errorJSON, &progress,
		&attempts, &maxRetries, &createdAt, &startedAt, &finishedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return userExportJob{}, ErrUserExportNotFound
	}
	if err != nil {
		return userExportJob{}, err
	}
	payload := map[string]any{}
	if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
		return userExportJob{}, err
	}
	version, validVersion := positiveIntValue(payload["timeline_version"])
	timelineID, _ := payload["timeline_id"].(string)
	orientation, _ := payload["orientation"].(string)
	if !validVersion || strings.TrimSpace(timelineID) == "" {
		return userExportJob{}, errors.New("用户导出任务缺少固定时间线")
	}
	record := UserExportRecord{
		JobID: jobID, Status: status, TimelineID: timelineID,
		TimelineVersion: version, Orientation: orientation,
		Attempts: attempts, MaxRetries: maxRetries, CreatedAt: createdAt,
		StartedAt: startedAt.String, FinishedAt: finishedAt.String,
		Retryable: status == "failed" || status == "cancelled",
	}
	if progress.Valid {
		record.Progress = progress.Float64
	}
	if retryOf, _ := payload["retry_of_job_id"].(string); retryOf != "" {
		record.RetryOfJobID = retryOf
	}
	if resultJSON.Valid {
		result := map[string]any{}
		if json.Unmarshal([]byte(resultJSON.String), &result) == nil {
			bounded := boundedJobResult(result)
			record.ExportID, _ = bounded["artifact_id"].(string)
			record.Profile, _ = bounded["profile"].(string)
		}
	}
	if errorJSON.Valid {
		failure := map[string]any{}
		if json.Unmarshal([]byte(errorJSON.String), &failure) == nil {
			bounded := boundedJobFailure(failure)
			record.Failure = &UserExportFailure{
				Code:      InterfaceString(bounded["error_code"]),
				Message:   InterfaceString(bounded["message"]),
				Retryable: record.Retryable,
			}
		}
	}
	rootKey, _ := payload["root_idempotency_key"].(string)
	return userExportJob{Record: record, RootIdempotencyKey: rootKey}, nil
}

func userExportIdempotencyKey(draftID, timelineID, orientation string) string {
	return fmt.Sprintf("user_export:%s:%s:%s", draftID, timelineID, orientation)
}
