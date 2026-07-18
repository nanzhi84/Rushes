package reducer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

const maxRewindCheckpoints = 50

type rewindCheckpointState struct {
	id               string
	anchorMessageID  sql.NullString
	anchorTurnID     sql.NullString
	timelineVersion  sql.NullInt64
	decisionBoundary int64
}

func applyTimelineRestored(ctx context.Context, state *applyState, event contracts.Event) error {
	checkpointID := stringFrom(event.Payload["checkpoint_id"], "")
	mode := stringFrom(event.Payload["mode"], "")
	if checkpointID == "" || (mode != "timeline" && mode != "conversation" && mode != "both") {
		return errors.New("TimelineVersionRestored checkpoint 或 mode 无效")
	}
	checkpoint, err := rewindCheckpointForRestore(ctx, state.tx, event.DraftID, checkpointID)
	if err != nil {
		return err
	}

	var document map[string]any
	resultVersion := checkpoint.timelineVersion
	if mode == "timeline" || mode == "both" {
		if !checkpoint.timelineVersion.Valid {
			return errors.New("目标检查点没有时间线版本")
		}
		newVersion := intFrom(event.Payload["timeline_version"], 0)
		if newVersion < 1 {
			return errors.New("TimelineVersionRestored 缺少新 timeline_version")
		}
		var raw string
		if err := state.tx.QueryRowContext(ctx, `
			SELECT document_json FROM timeline_versions WHERE draft_id=? AND version=?`,
			event.DraftID, checkpoint.timelineVersion.Int64,
		).Scan(&raw); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(raw), &document); err != nil {
			return fmt.Errorf("恢复目标时间线无法解析: %w", err)
		}
		timelineID := fmt.Sprintf("%s:v%d", event.DraftID, newVersion)
		document["version"] = newVersion
		document["draft_id"] = event.DraftID
		document["timeline_id"] = timelineID
		if _, err := state.tx.ExecContext(ctx, `
			INSERT INTO timeline_versions(
				timeline_id,draft_id,version,parent_version,created_by_patch_id,
				document_json,validation_report_json,created_at
			) VALUES(?,?,?,?,?,?,NULL,?)`,
			timelineID, event.DraftID, newVersion,
			checkpoint.timelineVersion.Int64, "rewind:"+checkpointID, mustJSON(document), state.createdAt,
		); err != nil {
			return err
		}
		if _, err := state.tx.ExecContext(ctx, `
			UPDATE drafts SET timeline_current_version=?,timeline_validated=0,
			preview_current_id=NULL,export_current_id=NULL
			WHERE draft_id=?`, newVersion, event.DraftID); err != nil {
			return err
		}
		resultVersion = sql.NullInt64{Int64: int64(newVersion), Valid: true}
	}

	if mode == "conversation" || mode == "both" {
		if err := applyConversationRewind(ctx, state, event.DraftID, checkpointID); err != nil {
			return err
		}
	}
	if err := invalidateDecisionsAfterCheckpoint(
		ctx, state.tx, event.DraftID, checkpoint.decisionBoundary,
	); err != nil {
		return err
	}
	// 回退遮蔽消息、作废决策之后,枚举被本次回退波及的长期记忆。只有动了对话
	// (conversation/both)才有区间可言;纯时间线回退不牵动任何记忆。
	if mode == "conversation" || mode == "both" {
		affected, err := collectRewindAffectedMemories(
			ctx, state.tx, event.DraftID, checkpoint.anchorMessageID, checkpoint.decisionBoundary,
		)
		if err != nil {
			return err
		}
		state.rewindAffectedMemories = affected
	}
	if err := state.touch(ctx, event.DraftID); err != nil {
		return err
	}
	restoreCheckpointID := stringFrom(event.Payload["restore_checkpoint_id"], "")
	if restoreCheckpointID == "" {
		return errors.New("TimelineVersionRestored 缺少 restore_checkpoint_id")
	}
	if document == nil && resultVersion.Valid {
		var raw string
		if err := state.tx.QueryRowContext(ctx, `
			SELECT document_json FROM timeline_versions WHERE draft_id=? AND version=?`,
			event.DraftID, resultVersion.Int64,
		).Scan(&raw); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(raw), &document); err != nil {
			return err
		}
	}
	summary := fmt.Sprintf("恢复检查点：%s（%s）", checkpointID, mode)
	return recordRewindCheckpoint(ctx, state.tx, rewindCheckpointInput{
		id: restoreCheckpointID, draftID: event.DraftID, triggerKind: "restore",
		anchorMessageID: checkpoint.anchorMessageID, anchorTurnID: checkpoint.anchorTurnID,
		timelineVersion: resultVersion,
		summary:         summary, document: document, createdAt: state.createdAt,
	})
}

func rewindCheckpointForRestore(
	ctx context.Context,
	tx *sql.Tx,
	draftID string,
	checkpointID string,
) (rewindCheckpointState, error) {
	var checkpoint rewindCheckpointState
	checkpoint.id = checkpointID
	err := tx.QueryRowContext(ctx, `
		SELECT anchor_message_id,anchor_turn_id,timeline_version,decision_boundary
		FROM rewind_checkpoints WHERE draft_id=? AND checkpoint_id=?`, draftID, checkpointID,
	).Scan(
		&checkpoint.anchorMessageID, &checkpoint.anchorTurnID,
		&checkpoint.timelineVersion, &checkpoint.decisionBoundary,
	)
	return checkpoint, err
}

func applyConversationRewind(
	ctx context.Context,
	state *applyState,
	draftID string,
	checkpointID string,
) error {
	if _, err := state.tx.ExecContext(ctx, `
		UPDATE messages SET rewound_at=?,rewind_checkpoint_id=?
		WHERE draft_id=?`, state.createdAt, checkpointID, draftID); err != nil {
		return err
	}
	if _, err := state.tx.ExecContext(ctx, `
		UPDATE messages SET rewound_at=NULL,rewind_checkpoint_id=NULL
		WHERE draft_id=? AND message_id IN (
			SELECT message_id FROM rewind_checkpoint_messages WHERE checkpoint_id=?
		)`, draftID, checkpointID); err != nil {
		return err
	}
	if _, err := state.tx.ExecContext(ctx,
		"DELETE FROM agent_context_checkpoints WHERE draft_id=?", draftID,
	); err != nil {
		return err
	}
	// 上下文重置锚点若被本次回退遮蔽，则那次「清空上下文」一并被撤销，回到无重置
	// 状态（模型可见 X 之前的历史 + 新的 X′）；仍可见的锚点原样保留。
	_, err := state.tx.ExecContext(ctx, `
		UPDATE drafts SET messages_tail_ref=CASE
			WHEN messages_tail_ref IS NULL THEN NULL
			WHEN messages_tail_ref IN (
				SELECT message_id FROM rewind_checkpoint_messages WHERE checkpoint_id=?
			) THEN messages_tail_ref
			ELSE NULL END
		WHERE draft_id=?`, checkpointID, draftID)
	return err
}

func invalidateDecisionsAfterCheckpoint(
	ctx context.Context,
	tx *sql.Tx,
	draftID string,
	decisionBoundary int64,
) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE decisions SET status='cancelled',
		pending_tool_call_status=CASE
			WHEN pending_tool_call_json IS NULL OR pending_tool_call_json='null'
			THEN pending_tool_call_status ELSE 'discarded' END
		WHERE draft_id=? AND status='pending' AND rowid>?`,
		draftID, decisionBoundary); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `
		UPDATE drafts SET pending_decision_id=NULL
		WHERE draft_id=? AND pending_decision_id IN (
			SELECT decision_id FROM decisions WHERE draft_id=? AND status='cancelled' AND rowid>?
		)`, draftID, draftID, decisionBoundary)
	return err
}

type rewindCheckpointInput struct {
	id              string
	draftID         string
	triggerKind     string
	anchorMessageID sql.NullString
	anchorTurnID    sql.NullString
	timelineVersion sql.NullInt64
	patchID         sql.NullString
	summary         string
	document        map[string]any
	createdAt       string
}

func recordMessageRewindCheckpoint(
	ctx context.Context,
	tx *sql.Tx,
	message MessageRow,
	createdAt string,
) error {
	document, version, err := currentTimelineDocument(ctx, tx, message.DraftID)
	if err != nil {
		return err
	}
	return recordRewindCheckpoint(ctx, tx, rewindCheckpointInput{
		id: "rewind:message:" + message.ID, draftID: message.DraftID,
		triggerKind: "user_message", anchorMessageID: sql.NullString{String: message.ID, Valid: true},
		anchorTurnID:    sql.NullString{String: message.ID, Valid: true},
		timelineVersion: version, summary: truncateCheckpointSummary(message.Content),
		document: document, createdAt: createdAt,
	})
}

func recordRewindCheckpoint(ctx context.Context, tx *sql.Tx, input rewindCheckpointInput) error {
	if input.id == "" || input.draftID == "" || input.createdAt == "" {
		return errors.New("rewind checkpoint 字段不完整")
	}
	var decisionBoundary, jobBoundary, eventBoundary int64
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(rowid),0) FROM decisions").Scan(&decisionBoundary); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(rowid),0) FROM jobs").Scan(&jobBoundary); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(event_id),0) FROM event_log").Scan(&eventBoundary); err != nil {
		return err
	}
	clipCount, durationFrames, trackCount := timelineCheckpointStats(input.document)
	var timelineVersion any
	if input.timelineVersion.Valid {
		timelineVersion = input.timelineVersion.Int64
	}
	inserted, err := tx.ExecContext(ctx, `
		INSERT INTO rewind_checkpoints(
			checkpoint_id,draft_id,trigger_kind,anchor_message_id,anchor_turn_id,anchor_event_id,
			timeline_version,patch_id,decision_boundary,job_boundary,summary,clip_count,
			duration_frames,track_count,created_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(checkpoint_id) DO NOTHING`,
		input.id, input.draftID, input.triggerKind, nullableString(input.anchorMessageID.String),
		nullableString(input.anchorTurnID.String), eventBoundary, timelineVersion,
		nullableString(input.patchID.String), decisionBoundary, jobBoundary,
		input.summary, clipCount, durationFrames, trackCount, input.createdAt,
	)
	if err != nil {
		return err
	}
	if rows, rowsErr := inserted.RowsAffected(); rowsErr != nil {
		return rowsErr
	} else if rows == 1 {
		// 可见集边界取“锚点消息之前”：编辑重发消息 X 恢复到 X 发出前的状态，故
		// X 自身不进入其检查点可见集；IS NOT 兼容锚点为 NULL 的非消息检查点。
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rewind_checkpoint_messages(checkpoint_id,message_id)
			SELECT ?,message_id FROM messages
			WHERE draft_id=? AND rewound_at IS NULL AND message_id IS NOT ?`,
			input.id, input.draftID, input.anchorMessageID); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `
		DELETE FROM rewind_checkpoints WHERE draft_id=? AND rowid NOT IN (
			SELECT rowid FROM rewind_checkpoints WHERE draft_id=? ORDER BY rowid DESC LIMIT ?
		)`, input.draftID, input.draftID, maxRewindCheckpoints)
	return err
}

func currentTimelineDocument(
	ctx context.Context,
	tx *sql.Tx,
	draftID string,
) (map[string]any, sql.NullInt64, error) {
	var version sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		"SELECT timeline_current_version FROM drafts WHERE draft_id=?", draftID,
	).Scan(&version); err != nil {
		return nil, version, err
	}
	if !version.Valid {
		return nil, version, nil
	}
	var raw string
	if err := tx.QueryRowContext(ctx, `
		SELECT document_json FROM timeline_versions WHERE draft_id=? AND version=?`,
		draftID, version.Int64,
	).Scan(&raw); err != nil {
		return nil, version, err
	}
	var document map[string]any
	if err := json.Unmarshal([]byte(raw), &document); err != nil {
		return nil, version, err
	}
	return document, version, nil
}

func timelineCheckpointStats(document map[string]any) (int, int, int) {
	if document == nil {
		return 0, 0, 0
	}
	tracks := sliceValue(document["tracks"])
	clipCount := 0
	for _, rawTrack := range tracks {
		clipCount += len(sliceValue(mapValue(rawTrack)["clips"]))
	}
	return clipCount, intFrom(document["duration_frames"], 0), len(tracks)
}

func truncateCheckpointSummary(value string) string {
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) <= 120 {
		return value
	}
	runes := []rune(value)
	return string(runes[:117]) + "…"
}

// collectRewindAffectedMemories 在回退事务内枚举「被这次编辑并重发波及」的长期记忆:
// 记忆创建于回退区间内(created_at >= 锚点消息 created_at),且其当前证据也落在同一区间
// 内(证据消息已被本次回退遮蔽,或证据决策创建于检查点决策边界之后)。两条同时成立才计
// 入:只重发某条较早消息不牵动更早已成立的偏好;证据仍指向可见历史的记忆也保留。记忆本
// 身不随回退删除(证据无外键、刻意跨回退存活),清单仅用于提示用户显式「撤回这些记忆」。
// 锚点缺失或消息已不存在(非消息检查点)时无区间可言,返回空。
func collectRewindAffectedMemories(
	ctx context.Context,
	tx *sql.Tx,
	draftID string,
	anchorMessageID sql.NullString,
	decisionBoundary int64,
) ([]storage.RewindAffectedMemory, error) {
	affected := []storage.RewindAffectedMemory{}
	if !anchorMessageID.Valid {
		return affected, nil
	}
	var anchorCreatedAt string
	err := tx.QueryRowContext(ctx,
		"SELECT created_at FROM messages WHERE message_id=? AND draft_id=?",
		anchorMessageID.String, draftID,
	).Scan(&anchorCreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return affected, nil
	}
	if err != nil {
		return nil, err
	}
	// user_memories.created_at 是定宽零填充格式(9 位纳秒),messages.created_at 是
	// RFC3339Nano(去尾零、变宽)。把锚点时间戳规整到记忆格式后再比,才能让字典序等价
	// 于时间序;两个变宽格式直接比会在尾零处错判。
	boundary, err := normalizeUserMemoryTimestamp(anchorCreatedAt)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT memory_key,statement FROM user_memories
		WHERE source_draft_id=? AND created_at>=? AND (
			(evidence_kind='user_message' AND EXISTS(
				SELECT 1 FROM messages m
				WHERE m.message_id=user_memories.evidence_id AND m.draft_id=?
					AND m.rewound_at IS NOT NULL))
			OR
			(evidence_kind='decision_answer' AND EXISTS(
				SELECT 1 FROM decisions d
				WHERE d.decision_id=user_memories.evidence_id AND d.draft_id=?
					AND d.rowid>?))
		)
		ORDER BY memory_key ASC`,
		draftID, boundary, draftID, draftID, decisionBoundary,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var memory storage.RewindAffectedMemory
		if err := rows.Scan(&memory.Key, &memory.Statement); err != nil {
			return nil, err
		}
		affected = append(affected, memory)
	}
	return affected, rows.Err()
}
