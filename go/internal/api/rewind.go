package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/agent"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

func (server *Server) ListRewindCheckpointsApiDraftsDraftIdRewindCheckpointsGet(
	writer http.ResponseWriter,
	request *http.Request,
	draftID string,
) {
	if _, err := storage.GetDraft(request.Context(), server.database.Read(), draftID); errors.Is(err, storage.ErrNotFound) {
		writeNotFound(writer, "draft_not_found")
		return
	} else if err != nil {
		server.internalError(writer, err)
		return
	}
	rows, err := storage.ListRewindCheckpoints(request.Context(), server.database.Read(), draftID, 50)
	if err != nil {
		server.internalError(writer, err)
		return
	}
	checkpoints := make([]RewindCheckpoint, 0, len(rows))
	for index, row := range rows {
		var older *storage.RewindCheckpoint
		if index+1 < len(rows) {
			older = &rows[index+1]
		}
		checkpoints = append(checkpoints, rewindCheckpointRecord(row, older))
	}
	writeJSON(writer, http.StatusOK, RewindCheckpointsResponse{
		DraftId: draftID, Checkpoints: checkpoints,
	})
}

func (server *Server) RestoreRewindCheckpointApiDraftsDraftIdRewindPost(
	writer http.ResponseWriter,
	request *http.Request,
	draftID string,
) {
	if _, err := storage.GetDraft(request.Context(), server.database.Read(), draftID); errors.Is(err, storage.ErrNotFound) {
		writeNotFound(writer, "draft_not_found")
		return
	} else if err != nil {
		server.internalError(writer, err)
		return
	}
	var payload RewindRestoreRequest
	if err := decodeJSON(request, &payload); err != nil || !payload.Mode.Valid() ||
		payload.CheckpointId == "" || payload.IdempotencyKey == "" || len(payload.IdempotencyKey) > 128 {
		writeBadRequest(writer, "rewind_request_invalid")
		return
	}
	previous, err := storage.GetRewindRestoreResult(
		request.Context(), server.database.Read(), draftID, payload.IdempotencyKey,
	)
	if err == nil {
		if previous.CheckpointID != payload.CheckpointId || previous.Mode != string(payload.Mode) {
			writeJSON(writer, http.StatusConflict, map[string]any{
				"detail": map[string]string{"reason": "rewind_idempotency_key_reused"},
			})
			return
		}
		writeJSON(writer, http.StatusOK, rewindRestoreResponse(previous))
		return
	}
	if !errors.Is(err, storage.ErrNotFound) {
		server.internalError(writer, err)
		return
	}
	checkpoint, err := storage.GetRewindCheckpoint(
		request.Context(), server.database.Read(), draftID, payload.CheckpointId,
	)
	if errors.Is(err, storage.ErrNotFound) {
		writeNotFound(writer, "rewind_checkpoint_not_found")
		return
	}
	if err != nil {
		server.internalError(writer, err)
		return
	}
	mode := string(payload.Mode)
	if (mode == "timeline" || mode == "both") && checkpoint.TimelineVersion == nil {
		writeBadRequest(writer, "rewind_checkpoint_has_no_timeline")
		return
	}

	barrier, draining := server.beginRewindCancellation(draftID)
	if draining {
		writeJSON(writer, http.StatusConflict, map[string]any{
			"detail": map[string]string{"reason": "rewind_cancellation_timeout"},
		})
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
		writeJSON(writer, http.StatusConflict, map[string]any{
			"detail": map[string]string{"reason": "rewind_cancellation_timeout"},
		})
		return
	}
	defer server.releaseRewindCancellation(draftID, barrier)
	jobs, err := server.rewindCancellableJobs(cleanupCtx, draftID)
	if err != nil {
		server.internalError(writer, err)
		return
	}

	response, err := server.applyRewind(
		cleanupCtx, draftID, mode, payload.IdempotencyKey, checkpoint, jobs,
	)
	if errors.Is(err, storage.ErrNotFound) {
		writeNotFound(writer, "rewind_checkpoint_not_found")
		return
	}
	if errors.Is(err, reducer.ErrJobNotCancellable) {
		writeJSON(writer, http.StatusConflict, map[string]any{
			"detail": map[string]string{"reason": "rewind_job_state_changed"},
		})
		return
	}
	if errors.Is(err, reducer.ErrRewindRestoreDuplicate) {
		writeJSON(writer, http.StatusConflict, map[string]any{
			"detail": map[string]string{"reason": "rewind_idempotency_key_reused"},
		})
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
	writeJSON(writer, http.StatusOK, response)
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

func (server *Server) applyRewind(
	ctx context.Context,
	draftID string,
	mode string,
	idempotencyKey string,
	checkpoint storage.RewindCheckpoint,
	jobs []rewindJob,
) (RewindRestoreResponse, error) {
	draft, err := storage.GetDraft(ctx, server.database.Read(), draftID)
	if err != nil {
		return RewindRestoreResponse{}, err
	}
	newTimelineVersion := draft.TimelineCurrentVersion
	if mode == "timeline" || mode == "both" {
		var version int
		if err := server.database.Read().QueryRowContext(ctx, `
			SELECT COALESCE(MAX(version),0)+1 FROM timeline_versions WHERE draft_id=?`, draftID,
		).Scan(&version); err != nil {
			return RewindRestoreResponse{}, err
		}
		newTimelineVersion = &version
	}
	rewoundMessages, cancelledDecisions, err := server.rewindConversationImpact(ctx, draftID, mode, checkpoint)
	if err != nil {
		return RewindRestoreResponse{}, err
	}
	eventPayload := map[string]any{
		"checkpoint_id": checkpoint.ID, "mode": mode,
		"restore_checkpoint_id": newID("rewind"),
	}
	if newTimelineVersion != nil && (mode == "timeline" || mode == "both") {
		eventPayload["timeline_version"] = *newTimelineVersion
		eventPayload["source_version"] = *checkpoint.TimelineVersion
	}
	versionLabel := "无时间线"
	if newTimelineVersion != nil {
		versionLabel = fmt.Sprintf("时间线现为 v%d", *newTimelineVersion)
	}
	observationID := newID("msg")
	events := make([]contracts.Event, 0, len(jobs)+1)
	suppressions := make([]reducer.AgentJobObservationSuppressionRow, 0, len(jobs))
	for _, job := range jobs {
		eventDraftID := ""
		if job.draftID.Valid {
			eventDraftID = job.draftID.String
		}
		payload := map[string]any{
			"job_id": job.id, "kind": job.kind, "reason": "rewind_restored",
		}
		if job.requestedBy.Valid {
			payload["requested_by_draft_id"] = job.requestedBy.String
		}
		if job.assetID.Valid {
			payload["asset_id"] = job.assetID.String
		}
		events = append(events, contracts.Event{
			Type: "JobCancelled", DraftID: eventDraftID, Payload: payload,
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
			CheckpointID: checkpoint.ID, Mode: mode, TimelineVersion: newTimelineVersion,
			RewoundMessageCount: rewoundMessages, CancelledJobs: len(jobs),
			CancelledDecisions: cancelledDecisions,
		},
		ResultRows: reducer.ResultRows{
			Message: &reducer.MessageRow{
				ID: observationID, DraftID: draftID, Role: "system_observation", Kind: "rewind",
				Content: fmt.Sprintf("用户已回退到检查点 %s（%s）；%s，之后的编辑或对话已撤销。",
					checkpoint.ID, mode, versionLabel),
			},
			AgentJobObservationSuppressions: suppressions,
		},
	})
	if errors.Is(err, reducer.ErrRewindRestoreDuplicate) {
		previous, lookupErr := storage.GetRewindRestoreResult(ctx, server.database.Read(), draftID, idempotencyKey)
		if lookupErr != nil {
			return RewindRestoreResponse{}, lookupErr
		}
		if previous.CheckpointID != checkpoint.ID || previous.Mode != mode {
			return RewindRestoreResponse{}, err
		}
		return rewindRestoreResponse(previous), nil
	}
	if err != nil {
		return RewindRestoreResponse{}, err
	}
	if result.Status != reducer.StatusApplied {
		return RewindRestoreResponse{}, &rewindReducerResultError{result: result}
	}
	return RewindRestoreResponse{
		DraftId: draftID, CheckpointId: checkpoint.ID,
		Mode: RewindRestoreResponseMode(mode), Status: Restored,
		TimelineVersion: newTimelineVersion, RewoundMessageCount: rewoundMessages,
		CancelledJobs: len(jobs), CancelledDecisions: cancelledDecisions,
		EventIds: reducerEventIDs(result),
	}, nil
}

func rewindRestoreResponse(result storage.RewindRestoreResult) RewindRestoreResponse {
	eventIDs := make([]int, 0, len(result.EventIDs))
	for _, eventID := range result.EventIDs {
		eventIDs = append(eventIDs, int(eventID))
	}
	return RewindRestoreResponse{
		DraftId: result.DraftID, CheckpointId: result.CheckpointID,
		Mode: RewindRestoreResponseMode(result.Mode), Status: Restored,
		TimelineVersion: result.TimelineVersion, RewoundMessageCount: result.RewoundMessageCount,
		CancelledJobs: result.CancelledJobs, CancelledDecisions: result.CancelledDecisions,
		EventIds: eventIDs,
	}
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
	mode string,
	checkpoint storage.RewindCheckpoint,
) (int, int, error) {
	var messages, decisions int
	if mode == "conversation" || mode == "both" {
		if err := server.database.Read().QueryRowContext(ctx, `
			SELECT COUNT(*) FROM messages
			WHERE draft_id=? AND rewound_at IS NULL AND message_id NOT IN (
				SELECT message_id FROM rewind_checkpoint_messages WHERE checkpoint_id=?
			)`, draftID, checkpoint.ID,
		).Scan(&messages); err != nil {
			return 0, 0, err
		}
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

func rewindCheckpointRecord(row storage.RewindCheckpoint, older *storage.RewindCheckpoint) RewindCheckpoint {
	var anchorEventID *int
	if row.AnchorEventID != nil {
		value := int(*row.AnchorEventID)
		anchorEventID = &value
	}
	record := RewindCheckpoint{
		CheckpointId: row.ID, TriggerKind: RewindCheckpointTriggerKind(row.TriggerKind),
		AnchorMessageId: row.AnchorMessageID, AnchorTurnId: row.AnchorTurnID,
		AnchorEventId: anchorEventID, TimelineVersion: row.TimelineVersion,
		PatchId: row.PatchID, Summary: row.Summary,
		ClipCount: row.ClipCount, DurationFrames: row.DurationFrames, TrackCount: row.TrackCount,
		CreatedAt: row.CreatedAt,
	}
	if older != nil {
		record.ClipCountDelta = row.ClipCount - older.ClipCount
		record.DurationFramesDelta = row.DurationFrames - older.DurationFrames
		record.TrackCountDelta = row.TrackCount - older.TrackCount
	}
	return record
}
