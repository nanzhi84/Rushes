package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

func (server *Server) ListUserExportsApiDraftsDraftIdExportsGet(
	writer http.ResponseWriter,
	request *http.Request,
	draftID string,
) {
	records, err := server.agent.ListUserExports(request.Context(), draftID)
	if err != nil {
		server.writeUserExportError(writer, err)
		return
	}
	response := UserExportsResponse{Exports: make([]UserExportRecord, 0, len(records))}
	for _, record := range records {
		response.Exports = append(response.Exports, userExportResponse(record))
	}
	writeJSON(writer, http.StatusOK, response)
}

func (server *Server) CreateUserExportApiDraftsDraftIdExportsPost(
	writer http.ResponseWriter,
	request *http.Request,
	draftID string,
) {
	var payload UserExportCreateRequest
	if err := decodeJSON(request, &payload); err != nil {
		writeBadRequest(writer, "invalid_json")
		return
	}
	orientation := "auto"
	if payload.Orientation != nil {
		orientation = string(*payload.Orientation)
	}
	record, err := server.agent.CreateUserExport(
		request.Context(), draftID, payload.TimelineId, orientation,
	)
	if err != nil {
		server.writeUserExportError(writer, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, userExportResponse(record))
}

func (server *Server) RetryUserExportApiDraftsDraftIdExportsJobIdRetryPost(
	writer http.ResponseWriter,
	request *http.Request,
	draftID, jobID string,
) {
	record, err := server.agent.RetryUserExport(request.Context(), draftID, jobID)
	if err != nil {
		server.writeUserExportError(writer, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, userExportResponse(record))
}

func userExportResponse(record agentexec.UserExportRecord) UserExportRecord {
	response := UserExportRecord{
		Attempts: record.Attempts, CreatedAt: record.CreatedAt,
		JobId: record.JobID, MaxRetries: record.MaxRetries,
		Orientation: UserExportRecordOrientation(record.Orientation),
		Progress:    float32(max(0, min(record.Progress, 1))),
		Retryable:   record.Retryable, Status: UserExportRecordStatus(record.Status),
		TimelineId: record.TimelineID, TimelineVersion: record.TimelineVersion,
	}
	response.ExportId = nonEmptyStringPointer(record.ExportID)
	response.Profile = nonEmptyStringPointer(record.Profile)
	response.RetryOfJobId = nonEmptyStringPointer(record.RetryOfJobID)
	response.StartedAt = nonEmptyStringPointer(record.StartedAt)
	response.FinishedAt = nonEmptyStringPointer(record.FinishedAt)
	if record.Failure != nil {
		response.Error = &UserExportFailure{
			ErrorCode: record.Failure.Code,
			Message:   record.Failure.Message,
			Retryable: record.Failure.Retryable,
		}
	}
	return response
}

func (server *Server) writeUserExportError(writer http.ResponseWriter, err error) {
	var validationErr *agentexec.UserExportValidationError
	switch {
	case errors.Is(err, storage.ErrTimelineLockedByAgent):
		writeJSON(writer, http.StatusConflict, ErrorResponse{
			Detail: ErrorDetail{Reason: "agent_edit_lease_active"},
		})
	case errors.Is(err, agentexec.ErrUserExportStaleTimeline):
		writeJSON(writer, http.StatusConflict, ErrorResponse{
			Detail: ErrorDetail{Reason: "stale_timeline"},
		})
	case errors.Is(err, agentexec.ErrUserExportNotRetryable):
		writeJSON(writer, http.StatusConflict, ErrorResponse{
			Detail: ErrorDetail{Reason: "export_not_retryable"},
		})
	case errors.Is(err, agentexec.ErrUserExportStateConflict):
		writeJSON(writer, http.StatusConflict, ErrorResponse{
			Detail: ErrorDetail{Reason: "export_state_conflict"},
		})
	case errors.Is(err, agentexec.ErrUserExportNotFound):
		writeNotFound(writer, "export_job_not_found")
	case errors.Is(err, storage.ErrNotFound):
		writeNotFound(writer, "draft_or_timeline_not_found")
	case errors.Is(err, agentexec.ErrUserExportTimelineRequired):
		writeBadRequest(writer, "timeline_id_required")
	case errors.As(err, &validationErr):
		writeJSON(writer, http.StatusUnprocessableEntity, ErrorResponse{
			Detail: ErrorDetail{Reason: "timeline_validation_failed"},
		})
	case strings.Contains(err.Error(), "orientation 必须"):
		writeBadRequest(writer, "invalid_orientation")
	default:
		server.internalError(writer, err)
	}
}

func nonEmptyStringPointer(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
