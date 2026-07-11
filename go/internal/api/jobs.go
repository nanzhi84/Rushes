package api

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
)

func (server *Server) CancelJobApiJobsJobIdCancelPost(
	writer http.ResponseWriter,
	request *http.Request,
	jobID string,
) {
	var kind, status string
	var draftID, requestedByDraftID, assetID sql.NullString
	err := server.database.Read().QueryRowContext(request.Context(), `
		SELECT kind, status, draft_id, requested_by_draft_id, asset_id
		FROM jobs WHERE job_id=?`, jobID,
	).Scan(&kind, &status, &draftID, &requestedByDraftID, &assetID)
	if errors.Is(err, sql.ErrNoRows) {
		writeNotFound(writer, "job_not_found")
		return
	}
	if err != nil {
		server.internalError(writer, err)
		return
	}
	var payload JobCancelRequest
	if request.ContentLength != 0 {
		if err := decodeJSON(request, &payload); err != nil {
			writeBadRequest(writer, "invalid_json")
			return
		}
	}
	if status == string(Cancelled) {
		writeJSON(writer, http.StatusOK, JobCancelResponse{
			EventIds: []int{}, JobId: jobID, Status: Cancelled,
		})
		return
	}
	if status != "pending" && status != "running" {
		writeJSON(writer, http.StatusConflict, map[string]any{
			"detail": map[string]string{"reason": "job_not_cancellable"},
		})
		return
	}
	eventDraftID := ""
	if draftID.Valid {
		eventDraftID = draftID.String
	}
	eventPayload := map[string]any{"job_id": jobID, "kind": kind}
	if requestedByDraftID.Valid {
		eventPayload["requested_by_draft_id"] = requestedByDraftID.String
	}
	if assetID.Valid {
		eventPayload["asset_id"] = assetID.String
	}
	if payload.Reason != nil {
		eventPayload["reason"] = *payload.Reason
	}
	result, err := reducer.Apply(request.Context(), server.database, []contracts.Event{{
		Type: "JobCancelled", DraftID: eventDraftID, Payload: eventPayload,
	}}, reducer.Options{Actor: contracts.ActorUser})
	if errors.Is(err, reducer.ErrJobNotCancellable) {
		writeJSON(writer, http.StatusConflict, map[string]any{
			"detail": map[string]string{"reason": "job_not_cancellable"},
		})
		return
	}
	if err != nil {
		server.internalError(writer, err)
		return
	}
	if result.Status != reducer.StatusApplied {
		writeReducerResult(writer, result)
		return
	}
	writeJSON(writer, http.StatusOK, JobCancelResponse{
		EventIds: reducerEventIDs(result), JobId: jobID, Status: Cancelled,
	})
}
