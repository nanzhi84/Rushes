package api

import (
	"errors"
	"net/http"
	"strings"

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

const maxBatchDeleteDrafts = 100

// BatchDeleteDraftsApiDraftsDelete 将多个草稿作为一次原子 Reducer 写入软删除。
// 任一草稿不存在时不会提交其中任何一个 DraftTrashed 事件。
func (server *Server) BatchDeleteDraftsApiDraftsDelete(
	writer http.ResponseWriter,
	request *http.Request,
) {
	var payload DraftBatchDeleteRequest
	if err := decodeJSON(request, &payload); err != nil {
		writeBadRequest(writer, "invalid_json")
		return
	}
	if !payload.Confirm {
		writeJSON(writer, http.StatusConflict, map[string]any{
			"detail": map[string]string{"reason": "confirmation_required"},
		})
		return
	}
	if len(payload.DraftIds) == 0 {
		writeBadRequest(writer, "empty_draft_ids")
		return
	}
	if len(payload.DraftIds) > maxBatchDeleteDrafts {
		writeBadRequest(writer, "too_many_drafts")
		return
	}

	seen := make(map[string]struct{}, len(payload.DraftIds))
	activeDraftIDs := make([]string, 0, len(payload.DraftIds))
	for _, rawDraftID := range payload.DraftIds {
		draftID := strings.TrimSpace(rawDraftID)
		if draftID == "" || draftID != rawDraftID {
			writeBadRequest(writer, "invalid_draft_id")
			return
		}
		if _, exists := seen[draftID]; exists {
			writeBadRequest(writer, "duplicate_draft_id")
			return
		}
		seen[draftID] = struct{}{}

		draft, err := storage.GetDraft(request.Context(), server.database.Read(), draftID)
		if errors.Is(err, storage.ErrNotFound) {
			writeJSON(writer, http.StatusNotFound, map[string]any{
				"detail": map[string]string{"reason": "draft_not_found", "draft_id": draftID},
			})
			return
		}
		if err != nil {
			server.internalError(writer, err)
			return
		}
		if draft.Status == "trashed" {
			continue
		}
		if draft.Status != "active" {
			writeJSON(writer, http.StatusConflict, map[string]any{
				"detail": map[string]string{"reason": "draft_not_deletable", "draft_id": draftID},
			})
			return
		}
		activeDraftIDs = append(activeDraftIDs, draftID)
	}

	events := make([]contracts.Event, 0, len(activeDraftIDs))
	for _, draftID := range activeDraftIDs {
		events = append(events, contracts.Event{
			Type: "DraftTrashed", DraftID: draftID, Payload: map[string]any{},
		})
	}
	result, err := reducer.Apply(request.Context(), server.database, events, reducer.Options{
		Actor: contracts.ActorUser,
	})
	if err != nil {
		server.internalError(writer, err)
		return
	}
	if result.Status != reducer.StatusApplied {
		writeReducerResult(writer, result)
		return
	}
	writeJSON(writer, http.StatusOK, DraftBatchDeleteResponse{
		DeletedCount: len(activeDraftIDs), DeletedDraftIds: activeDraftIDs,
		EventIds: reducerEventIDs(result),
	})
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

func (server *Server) RenameDraftApiDraftsDraftIdPatch(
	writer http.ResponseWriter,
	request *http.Request,
	draftID string,
) {
	_, err := storage.GetDraft(request.Context(), server.database.Read(), draftID)
	if errors.Is(err, storage.ErrNotFound) {
		writeNotFound(writer, "draft_not_found")
		return
	}
	if err != nil {
		server.internalError(writer, err)
		return
	}
	var payload DraftUpdateRequest
	if err := decodeJSON(request, &payload); err != nil {
		writeBadRequest(writer, "invalid_json")
		return
	}
	name := strings.TrimSpace(payload.Name)
	if name == "" || len([]rune(name)) > 200 {
		writeBadRequest(writer, "invalid_name")
		return
	}
	result, err := reducer.Apply(request.Context(), server.database, []contracts.Event{{
		Type: "DraftRenamed", DraftID: draftID, Payload: map[string]any{"name": name},
	}}, reducer.Options{Actor: contracts.ActorUser})
	if err != nil {
		server.internalError(writer, err)
		return
	}
	if result.Status != reducer.StatusApplied {
		writeReducerResult(writer, result)
		return
	}
	updated, err := storage.GetDraft(request.Context(), server.database.Read(), draftID)
	if err != nil {
		server.internalError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, DraftMutationResponse{
		Draft: draftRecord(updated), EventIds: reducerEventIDs(result),
	})
}

func (server *Server) DeleteDraftApiDraftsDraftIdDelete(
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
	var payload ConfirmRequest
	if err := decodeJSON(request, &payload); err != nil {
		writeBadRequest(writer, "invalid_json")
		return
	}
	if payload.Confirm == nil || !*payload.Confirm {
		writeJSON(writer, http.StatusConflict, map[string]any{
			"detail": map[string]string{"reason": "confirmation_required"},
		})
		return
	}
	result, err := reducer.Apply(request.Context(), server.database, []contracts.Event{{
		Type: "DraftTrashed", DraftID: draftID, Payload: map[string]any{},
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

func (server *Server) CopyDraftApiDraftsDraftIdCopyPost(
	writer http.ResponseWriter,
	request *http.Request,
	draftID string,
) {
	source, err := storage.GetDraft(request.Context(), server.database.Read(), draftID)
	if errors.Is(err, storage.ErrNotFound) {
		writeNotFound(writer, "draft_not_found")
		return
	}
	if err != nil {
		server.internalError(writer, err)
		return
	}
	var payload DraftCopyRequest
	if err := decodeJSON(request, &payload); err != nil {
		writeBadRequest(writer, "invalid_json")
		return
	}
	targetID := newID("draft")
	if payload.DraftId != nil && strings.TrimSpace(*payload.DraftId) != "" {
		targetID = strings.TrimSpace(*payload.DraftId)
	}
	if _, err := storage.GetDraft(request.Context(), server.database.Read(), targetID); err == nil {
		writeJSON(writer, http.StatusConflict, map[string]any{
			"detail": map[string]string{"reason": "draft_already_exists"},
		})
		return
	} else if !errors.Is(err, storage.ErrNotFound) {
		server.internalError(writer, err)
		return
	}
	name := source.Name + " Copy"
	if payload.Name != nil && strings.TrimSpace(*payload.Name) != "" {
		name = strings.TrimSpace(*payload.Name)
	}
	result, err := reducer.Apply(request.Context(), server.database, []contracts.Event{{
		Type: "DraftCopied", DraftID: targetID,
		Payload: map[string]any{"source_draft_id": draftID, "name": name},
	}}, reducer.Options{Actor: contracts.ActorUser})
	if err != nil {
		server.internalError(writer, err)
		return
	}
	if result.Status != reducer.StatusApplied {
		writeReducerResult(writer, result)
		return
	}
	copied, err := storage.GetDraft(request.Context(), server.database.Read(), targetID)
	if err != nil {
		server.internalError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, DraftMutationResponse{
		Draft: draftRecord(copied), EventIds: reducerEventIDs(result),
	})
}

func (server *Server) DraftCostsApiDraftsDraftIdCostsGet(
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
	writeJSON(writer, http.StatusOK, DraftCostsResponse{
		DraftId: draftID,
		Costs: CostSummary{
			ByCapability: map[string]float32{}, ByProvider: map[string]float32{},
			ProviderCallCount: 0, TotalCostEstimate: 0,
		},
	})
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
