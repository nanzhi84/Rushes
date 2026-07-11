package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	"github.com/nanzhi84/Rushes/go/internal/tools"
)

func (server *Server) GetDraftTimelineApiDraftsDraftIdTimelineGet(
	writer http.ResponseWriter,
	request *http.Request,
	draftID string,
	params GetDraftTimelineApiDraftsDraftIdTimelineGetParams,
) {
	if _, err := storage.GetDraft(request.Context(), server.database.Read(), draftID); errors.Is(err, storage.ErrNotFound) {
		writeNotFound(writer, "draft_not_found")
		return
	} else if err != nil {
		server.internalError(writer, err)
		return
	}
	response, err := server.draftTimelineResponse(request.Context(), draftID, params.Version)
	if errors.Is(err, storage.ErrNotFound) {
		writeNotFound(writer, "not_found")
		return
	}
	if err != nil {
		server.internalError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, response)
}

func (server *Server) ApplyTimelinePatchApiDraftsDraftIdTimelinePatchPost(
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
	var payload TimelinePatchRequest
	if err := decodeJSON(request, &payload); err != nil || len(payload.Op) == 0 {
		writeBadRequest(writer, "timeline_patch_invalid")
		return
	}
	ctx := tools.WithDraftID(request.Context(), draftID)
	raw, err := server.agent.ExecuteTool(ctx, "timeline.apply_patch", tools.TimelinePatchInput{Op: payload.Op})
	if err != nil {
		writeBadRequest(writer, "timeline_patch_invalid: "+err.Error())
		return
	}
	result, ok := raw.(tools.ToolResult)
	if !ok {
		server.internalError(writer, errors.New("timeline patch 返回类型无效"))
		return
	}
	if result.Status != "succeeded" {
		writeBadRequest(writer, "timeline_patch_validation_failed")
		return
	}
	// 手动剪辑成功后自动刷新预览。排队失败不回滚已落库的新版本，避免客户端
	// 重试时重复应用 patch；SSE/状态栏仍会如实呈现后续渲染状态。
	if _, renderErr := server.agent.ExecuteTool(ctx, "render.preview", tools.RenderPreviewInput{}); renderErr != nil {
		server.logger.Warn("时间线已修改，但预览刷新排队失败", "draft_id", draftID, "error", renderErr)
	}
	response, err := server.draftTimelineResponse(request.Context(), draftID, nil)
	if err != nil {
		server.internalError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, response)
}

func (server *Server) RestoreTimelineVersionApiDraftsDraftIdTimelineRestorePost(
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
	var payload TimelineRestoreRequest
	if err := decodeJSON(request, &payload); err != nil || payload.Version < 1 {
		writeBadRequest(writer, "timeline_restore_invalid")
		return
	}
	ctx := tools.WithDraftID(request.Context(), draftID)
	if _, err := server.agent.ExecuteTool(ctx, "timeline.restore_version", tools.TimelineRestoreInput{
		SourceVersion: payload.Version,
	}); errors.Is(err, storage.ErrNotFound) {
		writeNotFound(writer, "timeline_version_not_found")
		return
	} else if err != nil {
		writeBadRequest(writer, "timeline_restore_invalid: "+err.Error())
		return
	}
	validated, err := server.agent.ExecuteTool(ctx, "timeline.validate", tools.TimelineValidateInput{})
	if err != nil {
		server.internalError(writer, err)
		return
	}
	result, ok := validated.(tools.ToolResult)
	if !ok || result.Status != "succeeded" {
		writeBadRequest(writer, "timeline_restore_validation_failed")
		return
	}
	if _, renderErr := server.agent.ExecuteTool(ctx, "render.preview", tools.RenderPreviewInput{}); renderErr != nil {
		server.logger.Warn("时间线已恢复，但预览刷新排队失败", "draft_id", draftID, "error", renderErr)
	}
	response, err := server.draftTimelineResponse(request.Context(), draftID, nil)
	if err != nil {
		server.internalError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, response)
}

func (server *Server) draftTimelineResponse(
	ctx context.Context,
	draftID string,
	version *int,
) (DraftTimelineResponse, error) {
	var document timeline.Document
	var err error
	if version != nil {
		document, err = timeline.Get(ctx, server.database, draftID, *version)
	} else {
		document, err = timeline.Latest(ctx, server.database, draftID)
	}
	if err != nil {
		return DraftTimelineResponse{}, err
	}
	documentMap, err := timeline.ToMap(document)
	if err != nil {
		return DraftTimelineResponse{}, err
	}
	previewID, err := timeline.LatestPreviewID(ctx, server.database, draftID, document.Version)
	if err != nil {
		return DraftTimelineResponse{}, err
	}
	navigation, err := timeline.Navigation(ctx, server.database, draftID, document.Version)
	if err != nil {
		return DraftTimelineResponse{}, err
	}
	return DraftTimelineResponse{
		DraftId: draftID, TimelineVersion: document.Version, Timeline: documentMap,
		Summary: timeline.Inspect(document), PreviewId: previewID,
		ParentVersion: navigation.Parent, RedoVersion: navigation.Redo, LatestVersion: navigation.Latest,
	}, nil
}

func (server *Server) PreviewViewedApiDraftsDraftIdPreviewsPreviewIdViewedPost(
	writer http.ResponseWriter,
	request *http.Request,
	draftID, previewID string,
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
	result, err := reducer.Apply(request.Context(), server.database, []contracts.Event{{
		Type: "PreviewViewed", DraftID: draftID,
		Payload: map[string]any{"preview_id": previewID},
	}}, reducer.Options{Actor: contracts.ActorUser, BaseVersion: &draft.StateVersion})
	if err != nil {
		server.internalError(writer, err)
		return
	}
	if result.Status != reducer.StatusApplied {
		writeReducerResult(writer, result)
		return
	}
	draft, err = storage.GetDraft(request.Context(), server.database.Read(), draftID)
	if err != nil {
		server.internalError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, DraftMutationResponse{Draft: draftRecord(draft), EventIds: reducerEventIDs(result)})
}
