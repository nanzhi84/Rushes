package api

import (
	"errors"
	"net/http"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

func (server *Server) CreateDraftApiDraftsPost(writer http.ResponseWriter, request *http.Request) {
	var payload DraftCreateRequest
	if err := decodeJSON(request, &payload); err != nil {
		writeBadRequest(writer, "invalid_json")
		return
	}
	draftID := newID("draft")
	if payload.DraftId != nil && *payload.DraftId != "" {
		draftID = *payload.DraftId
	}
	name := "未命名草稿"
	if payload.Name != nil && *payload.Name != "" {
		name = *payload.Name
	}
	brief := map[string]any{"goal": ""}
	if payload.Brief != nil {
		brief = *payload.Brief
	}
	if payload.Goal != nil {
		brief["goal"] = *payload.Goal
	}
	defaults := map[string]any{}
	if payload.Defaults != nil {
		defaults = *payload.Defaults
	}
	result, err := reducer.Apply(request.Context(), server.database, []contracts.Event{{
		Type: "DraftCreated", DraftID: draftID,
		Payload: map[string]any{"name": name, "brief": brief, "defaults": defaults},
	}}, reducer.Options{Actor: contracts.ActorUser})
	if err != nil {
		server.internalError(writer, err)
		return
	}
	if result.Status != reducer.StatusApplied {
		writeReducerResult(writer, result)
		return
	}
	draft, err := storage.GetDraft(request.Context(), server.database.Read(), draftID)
	if err != nil {
		server.internalError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, DraftMutationResponse{
		Draft: draftRecord(draft), EventIds: reducerEventIDs(result),
	})
}

func (server *Server) ListDraftsApiDraftsGet(writer http.ResponseWriter, request *http.Request) {
	drafts, err := storage.ListDrafts(request.Context(), server.database.Read())
	if err != nil {
		server.internalError(writer, err)
		return
	}
	items := make([]DraftListItem, 0, len(drafts))
	for _, draft := range drafts {
		count, err := storage.DraftMaterialCount(request.Context(), server.database.Read(), draft.ID)
		if err != nil {
			server.internalError(writer, err)
			return
		}
		covers, err := storage.DraftAssetIDs(request.Context(), server.database.Read(), draft.ID, 3)
		if err != nil {
			server.internalError(writer, err)
			return
		}
		items = append(items, DraftListItem{
			DraftId: draft.ID, Name: draft.Name, Status: draft.Status,
			UpdatedAt: draft.UpdatedAt, MaterialCount: count, CoverAssetIds: covers,
		})
	}
	writeJSON(writer, http.StatusOK, DraftListResponse{Drafts: items})
}

func (server *Server) GetDraftApiDraftsDraftIdGet(
	writer http.ResponseWriter,
	request *http.Request,
	draftID string,
) {
	draft, err := storage.GetDraft(request.Context(), server.database.Read(), draftID)
	if errors.Is(err, storage.ErrNotFound) {
		writeNotFound(writer, "draft_not_found")
		return
	}
	if err != nil {
		server.internalError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, DraftResponse{Draft: draftRecord(draft)})
}

func draftRecord(draft storage.Draft) DraftRecord {
	return DraftRecord{
		AudioPlan: nil, Brief: draft.Brief, ContentPlan: mapPointer(draft.ContentPlan),
		CreatedAt: draft.CreatedAt, CutPlan: nil, Defaults: draft.Defaults,
		DraftId: draft.ID, ExportCurrentId: draft.ExportCurrentID,
		LastError: mapPointer(draft.LastError), LastViewedPreviewId: draft.LastViewedPreviewID,
		MessagesTailRef: draft.MessagesTailRef, Name: draft.Name,
		PendingDecisionId: draft.PendingDecisionID, PostprocessPlan: nil,
		PreviewCurrentId: draft.PreviewCurrentID, RoughCutApproved: false,
		RoughCutApprovedVersion: nil, RunningJobs: draft.RunningJobs,
		ScratchMemory: draft.ScratchMemory, StateVersion: draft.StateVersion,
		Status: draft.Status, TimelineCurrentVersion: draft.TimelineCurrentVersion,
		TimelineValidated: draft.TimelineValidated, UpdatedAt: draft.UpdatedAt,
	}
}

func mapPointer(value map[string]any) *map[string]any {
	if value == nil {
		return nil
	}
	return &value
}

func reducerEventIDs(result reducer.Result) []int {
	ids := make([]int, 0, len(result.AppliedEvents))
	for _, event := range result.AppliedEvents {
		ids = append(ids, int(event.ID))
	}
	return ids
}

func writeReducerResult(writer http.ResponseWriter, result reducer.Result) {
	if result.Status == reducer.StatusVersionConflict && result.Conflict != nil {
		writeJSON(writer, http.StatusConflict, map[string]any{
			"detail": map[string]any{"reason": "version_conflict", "conflict": result.Conflict},
		})
		return
	}
	writeJSON(writer, http.StatusConflict, map[string]any{
		"detail": map[string]string{"reason": string(result.Status)},
	})
}

func (server *Server) internalError(writer http.ResponseWriter, err error) {
	server.logger.Error("API 内部错误", "error", err)
	writeJSON(writer, http.StatusInternalServerError, map[string]any{
		"detail": map[string]string{"reason": "internal_error"},
	})
}
