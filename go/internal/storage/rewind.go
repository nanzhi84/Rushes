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

// RewindAffectedMemory 是一次「编辑并重发」回退波及的一条长期记忆:其当前证据落在被
// 回退的对话区间内、且该记忆创建于同一区间内。key+statement 摘要供前端「撤回这些记忆」
// 卡片列出;记忆本身不随回退自动删除(证据无外键、刻意跨回退存活),撤回是用户显式动作。
type RewindAffectedMemory struct {
	Key       string `json:"key"`
	Statement string `json:"statement"`
}

type RewindRestoreResult struct {
	DraftID             string
	IdempotencyKey      string
	CheckpointID        string
	Mode                string
	NewMessageID        string
	TimelineVersion     *int
	RewoundMessageCount int
	CancelledJobs       int
	CancelledDecisions  int
	EventIDs            []int64
	AffectedMemories    []RewindAffectedMemory
}

func GetRewindRestoreResult(
	ctx context.Context,
	query Querier,
	draftID string,
	idempotencyKey string,
) (RewindRestoreResult, error) {
	var result RewindRestoreResult
	var timelineVersion sql.NullInt64
	var newMessageID sql.NullString
	var eventIDsJSON string
	var affectedMemoriesJSON string
	err := query.QueryRowContext(ctx, `
		SELECT draft_id,idempotency_key,checkpoint_id,mode,new_message_id,timeline_version,
			rewound_message_count,cancelled_jobs,cancelled_decisions,event_ids_json,
			COALESCE(affected_memories_json,'[]')
		FROM rewind_restore_requests WHERE draft_id=? AND idempotency_key=?`,
		draftID, idempotencyKey,
	).Scan(
		&result.DraftID, &result.IdempotencyKey, &result.CheckpointID, &result.Mode,
		&newMessageID, &timelineVersion, &result.RewoundMessageCount, &result.CancelledJobs,
		&result.CancelledDecisions, &eventIDsJSON, &affectedMemoriesJSON,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return RewindRestoreResult{}, ErrNotFound
	}
	if err != nil {
		return RewindRestoreResult{}, err
	}
	result.NewMessageID = newMessageID.String
	if timelineVersion.Valid {
		value := int(timelineVersion.Int64)
		result.TimelineVersion = &value
	}
	if err := json.Unmarshal([]byte(eventIDsJSON), &result.EventIDs); err != nil {
		return RewindRestoreResult{}, err
	}
	result.AffectedMemories = []RewindAffectedMemory{}
	if err := json.Unmarshal([]byte(affectedMemoriesJSON), &result.AffectedMemories); err != nil {
		return RewindRestoreResult{}, err
	}
	return result, nil
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
