package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/agent"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

// ResendMessageApiDraftsDraftIdMessagesMessageIdResendPost 编辑并重发：把对话与
// 时间线一起回退到消息 X 发出之前，再以新内容 X′ 开启新回合。回退与新消息创建落在
// 同一条幂等结果里（rewind_restore_requests.new_message_id），同 key 重放返回相同
// X′、不再二次回退或重复入队；同 key 不同参数返回 409。
func (server *Server) ResendMessageApiDraftsDraftIdMessagesMessageIdResendPost(
	writer http.ResponseWriter,
	request *http.Request,
	draftID string,
	messageID string,
) {
	if _, err := storage.GetDraft(request.Context(), server.database.Read(), draftID); errors.Is(err, storage.ErrNotFound) {
		writeNotFound(writer, "draft_not_found")
		return
	} else if err != nil {
		server.internalError(writer, err)
		return
	}
	var payload MessageResendRequest
	if err := decodeJSON(request, &payload); err != nil ||
		payload.IdempotencyKey == "" || len(payload.IdempotencyKey) > 128 {
		writeBadRequest(writer, "resend_request_invalid")
		return
	}
	content := strings.TrimSpace(payload.Content)
	if content == "" {
		writeBadRequest(writer, "empty_message")
		return
	}
	checkpointID := rewindMessageCheckpointID(messageID)

	// 幂等重放：命中已提交结果直接回放，绝不再次回退或入队。
	if handled, err := server.resendReplay(
		writer, request.Context(), draftID, checkpointID, payload.IdempotencyKey, content,
	); err != nil {
		server.internalError(writer, err)
		return
	} else if handled {
		return
	}

	checkpoint, err := storage.GetRewindCheckpoint(
		request.Context(), server.database.Read(), draftID, checkpointID,
	)
	if errors.Is(err, storage.ErrNotFound) {
		writeNotFound(writer, "resend_checkpoint_unavailable")
		return
	}
	if err != nil {
		server.internalError(writer, err)
		return
	}
	editable, exists, err := server.resendMessageEditable(request.Context(), draftID, messageID)
	if err != nil {
		server.internalError(writer, err)
		return
	}
	if !exists {
		writeNotFound(writer, "resend_message_not_found")
		return
	}
	if !editable {
		writeConflict(writer, "resend_message_not_editable")
		return
	}

	barrier, draining := server.beginRewindCancellation(draftID)
	if draining {
		writeConflict(writer, "resend_in_progress")
		return
	}
	if barrier == nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]any{
			"detail": map[string]string{"reason": "turn_queue_closed"},
		})
		return
	}
	cleanupCtx, cancelCleanup := turnCancellationContext(request.Context())
	defer cancelCleanup()
	waitCtx, cancelWait := context.WithTimeout(context.Background(), 500*time.Millisecond)
	finished := barrier.Wait(waitCtx)
	cancelWait()
	if !finished {
		server.releaseRewindWhenDrained(draftID, barrier)
		writeConflict(writer, "resend_cancellation_timeout")
		return
	}
	// 屏障归本请求所有：所有出错分支经 defer 释放，成功分支在入队前显式释放。
	releaseBarrier := sync.OnceFunc(func() { server.releaseRewindCancellation(draftID, barrier) })
	defer releaseBarrier()

	jobs, err := server.rewindCancellableJobs(cleanupCtx, draftID)
	if err != nil {
		server.internalError(writer, err)
		return
	}
	newMessageID := newID("msg")
	response, err := server.applyResend(
		cleanupCtx, draftID, checkpoint, newMessageID, content, payload.IdempotencyKey, jobs,
	)
	if errors.Is(err, reducer.ErrRewindRestoreDuplicate) {
		// 并发同 key 重发已抢先提交并入队：回放其结果，本次不再入队。
		releaseBarrier()
		if handled, replayErr := server.resendReplay(
			writer, request.Context(), draftID, checkpointID, payload.IdempotencyKey, content,
		); replayErr != nil {
			server.internalError(writer, replayErr)
		} else if !handled {
			server.internalError(writer, err)
		}
		return
	}
	if errors.Is(err, storage.ErrNotFound) {
		writeNotFound(writer, "resend_message_not_found")
		return
	}
	if errors.Is(err, reducer.ErrJobNotCancellable) {
		writeConflict(writer, "resend_job_state_changed")
		return
	}
	var reducerResultError *rewindReducerResultError
	if errors.As(err, &reducerResultError) {
		writeReducerResult(writer, reducerResultError.result)
		return
	}
	if err != nil {
		server.internalError(writer, err)
		return
	}
	// 回退与新消息已提交；释放屏障后方可为 X′ 入队新回合。
	releaseBarrier()
	if !server.agent.Queue().EnqueueUserMessage(draftID, response.MessageId, content) {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]any{
			"detail": map[string]string{"reason": "turn_queue_closed"},
		})
		return
	}
	writeJSON(writer, http.StatusAccepted, response)
}

// resendReplay 回放同一幂等键已提交的重发结果：命中且参数一致写 202，参数不同写
// 409，未命中返回 handled=false 让调用方继续。
func (server *Server) resendReplay(
	writer http.ResponseWriter,
	ctx context.Context,
	draftID string,
	checkpointID string,
	idempotencyKey string,
	content string,
) (bool, error) {
	previous, err := storage.GetRewindRestoreResult(ctx, server.database.Read(), draftID, idempotencyKey)
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	sameContent, err := server.messageContentMatches(ctx, previous.NewMessageID, content)
	if err != nil {
		return false, err
	}
	if previous.CheckpointID != checkpointID || !sameContent {
		writeConflict(writer, "resend_idempotency_key_reused")
		return true, nil
	}
	// 收敛「回退已提交但入队丢失」:同 key 重放时补入队 X′(空闲守卫防并发双入队)。
	if err := server.ensureResendTurnEnqueued(ctx, draftID, previous.NewMessageID, content); err != nil {
		return false, err
	}
	writeJSON(writer, http.StatusAccepted, resendResponse(draftID, previous))
	return true, nil
}

// ensureResendTurnEnqueued 收敛崩溃/关停窗口:reducer 事务已提交但 X′ 的回合从未
// 入队(其后无非 user 消息即视为「未开始回合」),且队列空闲时补入队。X′ 已有回合
// 产物或队列非空闲则不动;EnqueueUserMessageIfIdle 的空闲守卫保证并发重试单入队。
func (server *Server) ensureResendTurnEnqueued(
	ctx context.Context,
	draftID string,
	messageID string,
	content string,
) error {
	if messageID == "" {
		return nil
	}
	var hasTurnOutput bool
	if err := server.database.Read().QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM messages
			WHERE draft_id=? AND role != 'user'
			AND rowid > COALESCE((SELECT rowid FROM messages WHERE message_id=?), 0)
		)`, draftID, messageID,
	).Scan(&hasTurnOutput); err != nil {
		return err
	}
	if hasTurnOutput {
		return nil
	}
	server.agent.Queue().EnqueueUserMessageIfIdle(draftID, messageID, content)
	return nil
}

// resendMessageEditable 报告目标是否为仍可见的用户消息（未被遮蔽）。
func (server *Server) resendMessageEditable(
	ctx context.Context,
	draftID string,
	messageID string,
) (editable bool, exists bool, err error) {
	var role string
	var rewoundAt sql.NullString
	scanErr := server.database.Read().QueryRowContext(ctx,
		"SELECT role,rewound_at FROM messages WHERE draft_id=? AND message_id=?", draftID, messageID,
	).Scan(&role, &rewoundAt)
	if errors.Is(scanErr, sql.ErrNoRows) {
		return false, false, nil
	}
	if scanErr != nil {
		return false, false, scanErr
	}
	return role == "user" && !rewoundAt.Valid, true, nil
}

// messageContentMatches 比较已存消息（去空白后）与本次内容是否一致，用于同 key
// 不同 content 的冲突判定。
func (server *Server) messageContentMatches(
	ctx context.Context,
	messageID string,
	content string,
) (bool, error) {
	if messageID == "" {
		return false, nil
	}
	var stored string
	err := server.database.Read().QueryRowContext(ctx,
		"SELECT content FROM messages WHERE message_id=?", messageID,
	).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return stored == content, nil
}

func rewindMessageCheckpointID(messageID string) string {
	return "rewind:message:" + messageID
}

func resendResponse(draftID string, previous storage.RewindRestoreResult) MessageResendResponse {
	return MessageResendResponse{
		DraftId:                 draftID,
		MessageId:               previous.NewMessageID,
		Status:                  Resent,
		RestoredTimelineVersion: previous.TimelineVersion,
		RewoundMessageCount:     previous.RewoundMessageCount,
	}
}

func (server *Server) beginRewindCancellation(
	draftID string,
) (*agent.DraftCancellationBarrier, bool) {
	server.rewindMu.Lock()
	defer server.rewindMu.Unlock()
	if server.rewindDrain[draftID] != nil {
		return nil, true
	}
	barrier, _ := server.agent.Queue().BeginDraftCancellation(draftID)
	if barrier != nil {
		server.rewindDrain[draftID] = barrier
	}
	return barrier, false
}

func (server *Server) releaseRewindWhenDrained(
	draftID string,
	barrier *agent.DraftCancellationBarrier,
) {
	go func() {
		_ = barrier.WaitForDrainOrQueueClose()
		server.releaseRewindCancellation(draftID, barrier)
	}()
}

func (server *Server) releaseRewindCancellation(
	draftID string,
	barrier *agent.DraftCancellationBarrier,
) {
	server.rewindMu.Lock()
	defer server.rewindMu.Unlock()
	if server.rewindDrain[draftID] != barrier {
		return
	}
	delete(server.rewindDrain, draftID)
	barrier.Release()
}

// applyResend 在同一 reducer 事务内完成回退（时间线恢复 / 会话软遮蔽 / 失效边界后
// 决策 / 取消可取消 job）与新用户消息 X′ 的创建，并把回退元数据与 new_message_id 记入
// 幂等结果。mode 内部化：检查点无时间线版本时只回退会话。
func (server *Server) applyResend(
	ctx context.Context,
	draftID string,
	checkpoint storage.RewindCheckpoint,
	newMessageID string,
	content string,
	idempotencyKey string,
	jobs []rewindJob,
) (MessageResendResponse, error) {
	draft, err := storage.GetDraft(ctx, server.database.Read(), draftID)
	if err != nil {
		return MessageResendResponse{}, err
	}
	mode := "both"
	if checkpoint.TimelineVersion == nil {
		mode = "conversation"
	}
	var newTimelineVersion *int
	if mode == "both" {
		var version int
		if err := server.database.Read().QueryRowContext(ctx, `
			SELECT COALESCE(MAX(version),0)+1 FROM timeline_versions WHERE draft_id=?`, draftID,
		).Scan(&version); err != nil {
			return MessageResendResponse{}, err
		}
		newTimelineVersion = &version
	}
	rewoundMessages, cancelledDecisions, err := server.rewindConversationImpact(ctx, draftID, checkpoint)
	if err != nil {
		return MessageResendResponse{}, err
	}
	eventPayload := map[string]any{
		"checkpoint_id": checkpoint.ID, "mode": mode,
		"restore_checkpoint_id": newID("rewind"),
	}
	if newTimelineVersion != nil {
		eventPayload["timeline_version"] = *newTimelineVersion
		eventPayload["source_version"] = *checkpoint.TimelineVersion
	}
	events := make([]contracts.Event, 0, len(jobs)+1)
	suppressions := make([]reducer.AgentJobObservationSuppressionRow, 0, len(jobs))
	for _, job := range jobs {
		eventDraftID := ""
		if job.draftID.Valid {
			eventDraftID = job.draftID.String
		}
		jobPayload := map[string]any{
			"job_id": job.id, "kind": job.kind, "reason": "rewind_restored",
		}
		if job.requestedBy.Valid {
			jobPayload["requested_by_draft_id"] = job.requestedBy.String
		}
		if job.assetID.Valid {
			jobPayload["asset_id"] = job.assetID.String
		}
		events = append(events, contracts.Event{
			Type: "JobCancelled", DraftID: eventDraftID, Payload: jobPayload,
		})
		suppressions = append(suppressions, reducer.AgentJobObservationSuppressionRow{JobID: job.id})
	}
	events = append(events, contracts.Event{
		Type: "TimelineVersionRestored", DraftID: draftID, Payload: eventPayload,
	})
	result, err := reducer.Apply(ctx, server.database, events, reducer.Options{
		Actor: contracts.ActorUser, BaseVersion: &draft.StateVersion,
		RewindRestore: &reducer.RewindRestore{
			DraftID: draftID, IdempotencyKey: idempotencyKey,
			CheckpointID: checkpoint.ID, Mode: mode, NewMessageID: newMessageID,
			TimelineVersion: newTimelineVersion, RewoundMessageCount: rewoundMessages,
			CancelledJobs: len(jobs), CancelledDecisions: cancelledDecisions,
		},
		ResultRows: reducer.ResultRows{
			Message: &reducer.MessageRow{
				ID: newMessageID, DraftID: draftID, Role: "user", Kind: "user", Content: content,
			},
			AgentJobObservationSuppressions: suppressions,
		},
	})
	if err != nil {
		return MessageResendResponse{}, err
	}
	if result.Status != reducer.StatusApplied {
		return MessageResendResponse{}, &rewindReducerResultError{result: result}
	}
	return MessageResendResponse{
		DraftId: draftID, MessageId: newMessageID, Status: Resent,
		RestoredTimelineVersion: newTimelineVersion, RewoundMessageCount: rewoundMessages,
	}, nil
}

type rewindReducerResultError struct {
	result reducer.Result
}

func (failure *rewindReducerResultError) Error() string {
	return fmt.Sprintf("rewind reducer status: %s", failure.result.Status)
}

func (server *Server) rewindConversationImpact(
	ctx context.Context,
	draftID string,
	checkpoint storage.RewindCheckpoint,
) (int, int, error) {
	var messages, decisions int
	if err := server.database.Read().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM messages
		WHERE draft_id=? AND rewound_at IS NULL AND message_id NOT IN (
			SELECT message_id FROM rewind_checkpoint_messages WHERE checkpoint_id=?
		)`, draftID, checkpoint.ID,
	).Scan(&messages); err != nil {
		return 0, 0, err
	}
	if err := server.database.Read().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM decisions
		WHERE draft_id=? AND rowid>? AND status='pending'`, draftID, checkpoint.DecisionBoundary,
	).Scan(&decisions); err != nil {
		return 0, 0, err
	}
	return messages, decisions, nil
}

type rewindJob struct {
	id, kind                      string
	draftID, requestedBy, assetID sql.NullString
}

func (server *Server) rewindCancellableJobs(ctx context.Context, draftID string) ([]rewindJob, error) {
	rows, err := server.database.Read().QueryContext(ctx, `
		SELECT job_id,kind,draft_id,requested_by_draft_id,asset_id
		FROM jobs
		WHERE COALESCE(requested_by_draft_id,draft_id)=?
		AND status IN ('pending','running') ORDER BY rowid`, draftID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	jobs := []rewindJob{}
	for rows.Next() {
		var job rewindJob
		if err := rows.Scan(&job.id, &job.kind, &job.draftID, &job.requestedBy, &job.assetID); err != nil {
			return nil, err
		}
		if agent.IsAgentWaitedJobKind(job.kind) {
			jobs = append(jobs, job)
		}
	}
	return jobs, rows.Err()
}

func writeConflict(writer http.ResponseWriter, reason string) {
	writeJSON(writer, http.StatusConflict, map[string]any{
		"detail": map[string]string{"reason": reason},
	})
}
