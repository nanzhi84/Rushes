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
		if err := applyConversationRewind(ctx, state, event.DraftID, checkpointID, checkpoint); err != nil {
			return err
		}
	}
	if err := invalidateDecisionsAfterCheckpoint(
		ctx, state.tx, event.DraftID, checkpoint.decisionBoundary,
	); err != nil {
		return err
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
	checkpoint rewindCheckpointState,
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
	if checkpoint.anchorMessageID.Valid {
		_, err := state.tx.ExecContext(ctx, `
			UPDATE drafts SET messages_tail_ref=CASE
				WHEN messages_tail_ref IS NULL THEN NULL
				WHEN messages_tail_ref IN (
					SELECT message_id FROM rewind_checkpoint_messages WHERE checkpoint_id=?
				) THEN messages_tail_ref
				ELSE ? END
			WHERE draft_id=?`, checkpointID, checkpoint.anchorMessageID.String, draftID)
		return err
	}
	_, err := state.tx.ExecContext(ctx, "UPDATE drafts SET messages_tail_ref=NULL WHERE draft_id=?", draftID)
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

func attachToolTraceToRewindCheckpoint(
	ctx context.Context,
	tx *sql.Tx,
	message MessageRow,
) error {
	var trace struct {
		Tool   string `json:"tool"`
		Status string `json:"status"`
	}
	if json.Unmarshal([]byte(message.Content), &trace) != nil || trace.Status != "succeeded" ||
		!isTimelineMutationTool(trace.Tool) {
		return nil
	}
	var version sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		"SELECT timeline_current_version FROM drafts WHERE draft_id=?", message.DraftID,
	).Scan(&version); err != nil {
		return err
	}
	if !version.Valid {
		return nil
	}
	var checkpointID string
	if err := tx.QueryRowContext(ctx, `
		SELECT checkpoint_id FROM rewind_checkpoints
		WHERE draft_id=? AND trigger_kind='timeline_write' AND timeline_version=?
		ORDER BY rowid DESC LIMIT 1`, message.DraftID, version.Int64,
	).Scan(&checkpointID); errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE rewind_checkpoints SET anchor_message_id=?,summary=? WHERE checkpoint_id=?`,
		message.ID, "工具批次 "+trace.Tool, checkpointID,
	); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO rewind_checkpoint_messages(checkpoint_id,message_id) VALUES(?,?)`,
		checkpointID, message.ID,
	)
	return err
}

func isTimelineMutationTool(name string) bool {
	switch name {
	// timeline.apply_patch 自 #100 起已从 LLM 工具面移除，此处保留用于历史事件回放识别：
	// 旧 trace/rewind checkpoint 仍以该工具名记录，回滚重放必须能认出它。
	case "timeline.compose_initial", "timeline.apply_patch", "timeline.apply_patches",
		"timeline.recut_to_beats", "timeline.edit_talking_head":
		return true
	default:
		return false
	}
}

func recordTimelineRewindCheckpoint(
	ctx context.Context,
	state *applyState,
	event contracts.Event,
	document map[string]any,
	triggerKind string,
) error {
	version := intFrom(event.Payload["timeline_version"], 0)
	timelineID := stringFrom(event.Payload["timeline_id"], fmt.Sprintf("%s:v%d", event.DraftID, version))
	anchorID, summary, err := latestActiveUserMessage(ctx, state.tx, event.DraftID)
	if err != nil {
		return err
	}
	patchID := stringFrom(event.Payload["patch_id"], "")
	if patchID != "" {
		summary = strings.TrimSpace(summary + " · 编辑批次 " + patchID)
	}
	return recordRewindCheckpoint(ctx, state.tx, rewindCheckpointInput{
		id: "rewind:timeline:" + timelineID, draftID: event.DraftID, triggerKind: triggerKind,
		anchorMessageID: anchorID, anchorTurnID: anchorID,
		timelineVersion: sql.NullInt64{Int64: int64(version), Valid: true},
		patchID:         sql.NullString{String: patchID, Valid: patchID != ""},
		summary:         truncateCheckpointSummary(summary), document: document, createdAt: state.createdAt,
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
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rewind_checkpoint_messages(checkpoint_id,message_id)
			SELECT ?,message_id FROM messages WHERE draft_id=? AND rewound_at IS NULL`,
			input.id, input.draftID); err != nil {
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

func latestActiveUserMessage(
	ctx context.Context,
	tx *sql.Tx,
	draftID string,
) (sql.NullString, string, error) {
	var messageID, content sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT message_id,content FROM messages
		WHERE draft_id=? AND role='user' AND rewound_at IS NULL
		ORDER BY rowid DESC LIMIT 1`, draftID).Scan(&messageID, &content)
	if errors.Is(err, sql.ErrNoRows) {
		return sql.NullString{}, "时间线编辑", nil
	}
	return messageID, content.String, err
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
