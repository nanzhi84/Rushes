package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

type RewindCheckpoint struct {
	ID               string
	DraftID          string
	TriggerKind      string
	AnchorMessageID  *string
	AnchorTurnID     *string
	AnchorEventID    *int64
	TimelineVersion  *int
	PatchID          *string
	DecisionBoundary int64
	JobBoundary      int64
	Summary          string
	ClipCount        int
	DurationFrames   int
	TrackCount       int
	CreatedAt        string
}

type RewindRestoreResult struct {
	DraftID             string
	IdempotencyKey      string
	CheckpointID        string
	Mode                string
	TimelineVersion     *int
	RewoundMessageCount int
	CancelledJobs       int
	CancelledDecisions  int
	EventIDs            []int64
}

func GetRewindRestoreResult(
	ctx context.Context,
	query Querier,
	draftID string,
	idempotencyKey string,
) (RewindRestoreResult, error) {
	var result RewindRestoreResult
	var timelineVersion sql.NullInt64
	var eventIDsJSON string
	err := query.QueryRowContext(ctx, `
		SELECT draft_id,idempotency_key,checkpoint_id,mode,timeline_version,
			rewound_message_count,cancelled_jobs,cancelled_decisions,event_ids_json
		FROM rewind_restore_requests WHERE draft_id=? AND idempotency_key=?`,
		draftID, idempotencyKey,
	).Scan(
		&result.DraftID, &result.IdempotencyKey, &result.CheckpointID, &result.Mode,
		&timelineVersion, &result.RewoundMessageCount, &result.CancelledJobs,
		&result.CancelledDecisions, &eventIDsJSON,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return RewindRestoreResult{}, ErrNotFound
	}
	if err != nil {
		return RewindRestoreResult{}, err
	}
	if timelineVersion.Valid {
		value := int(timelineVersion.Int64)
		result.TimelineVersion = &value
	}
	if err := json.Unmarshal([]byte(eventIDsJSON), &result.EventIDs); err != nil {
		return RewindRestoreResult{}, err
	}
	return result, nil
}

func ListRewindCheckpoints(
	ctx context.Context,
	query Querier,
	draftID string,
	limit int,
) ([]RewindCheckpoint, error) {
	if limit <= 0 || limit > 50 {
		limit = 50
	}
	rows, err := query.QueryContext(ctx, `
		SELECT checkpoint_id,draft_id,trigger_kind,anchor_message_id,anchor_turn_id,anchor_event_id,
			timeline_version,patch_id,decision_boundary,job_boundary,summary,clip_count,
			duration_frames,track_count,created_at
		FROM rewind_checkpoints WHERE draft_id=?
		ORDER BY rowid DESC LIMIT ?`, draftID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	checkpoints := make([]RewindCheckpoint, 0, limit)
	for rows.Next() {
		checkpoint, scanErr := scanRewindCheckpoint(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		checkpoints = append(checkpoints, checkpoint)
	}
	return checkpoints, rows.Err()
}

func GetRewindCheckpoint(
	ctx context.Context,
	query Querier,
	draftID string,
	checkpointID string,
) (RewindCheckpoint, error) {
	row := query.QueryRowContext(ctx, `
		SELECT checkpoint_id,draft_id,trigger_kind,anchor_message_id,anchor_turn_id,anchor_event_id,
			timeline_version,patch_id,decision_boundary,job_boundary,summary,clip_count,
			duration_frames,track_count,created_at
		FROM rewind_checkpoints WHERE draft_id=? AND checkpoint_id=?`, draftID, checkpointID)
	checkpoint, err := scanRewindCheckpoint(row)
	if errors.Is(err, sql.ErrNoRows) {
		return RewindCheckpoint{}, ErrNotFound
	}
	return checkpoint, err
}

func CountRewoundMessages(ctx context.Context, query Querier, draftID string) (int, error) {
	var count int
	err := query.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM messages WHERE draft_id=? AND rewound_at IS NOT NULL`, draftID,
	).Scan(&count)
	return count, err
}

type rewindCheckpointScanner interface {
	Scan(...any) error
}

func scanRewindCheckpoint(scanner rewindCheckpointScanner) (RewindCheckpoint, error) {
	var checkpoint RewindCheckpoint
	var anchorMessage, anchorTurn, patchID sql.NullString
	var anchorEvent, timelineVersion sql.NullInt64
	err := scanner.Scan(
		&checkpoint.ID, &checkpoint.DraftID, &checkpoint.TriggerKind,
		&anchorMessage, &anchorTurn, &anchorEvent, &timelineVersion, &patchID,
		&checkpoint.DecisionBoundary, &checkpoint.JobBoundary, &checkpoint.Summary,
		&checkpoint.ClipCount, &checkpoint.DurationFrames, &checkpoint.TrackCount,
		&checkpoint.CreatedAt,
	)
	if err != nil {
		return RewindCheckpoint{}, err
	}
	checkpoint.AnchorMessageID = stringPointer(anchorMessage)
	checkpoint.AnchorTurnID = stringPointer(anchorTurn)
	checkpoint.PatchID = stringPointer(patchID)
	if anchorEvent.Valid {
		value := anchorEvent.Int64
		checkpoint.AnchorEventID = &value
	}
	if timelineVersion.Valid {
		value := int(timelineVersion.Int64)
		checkpoint.TimelineVersion = &value
	}
	return checkpoint, nil
}
