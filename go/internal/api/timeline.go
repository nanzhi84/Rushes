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
) {
	if _, err := storage.GetDraft(request.Context(), server.database.Read(), draftID); errors.Is(err, storage.ErrNotFound) {
		writeNotFound(writer, "draft_not_found")
		return
	} else if err != nil {
		server.internalError(writer, err)
		return
	}
	response, err := server.draftTimelineResponse(request.Context(), draftID)
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
	ctx := tools.WithTimelineMutationOrigin(
		tools.WithDraftID(request.Context(), draftID),
		"manual",
	)
	operations := []map[string]any{map[string]any(payload.Op)}
	if kind, _ := payload.Op["kind"].(string); kind == "batch" {
		expanded, valid := timelinePatchOperations(payload.Op["ops"])
		if !valid || len(expanded) == 0 || len(expanded) > 100 {
			writeBadRequest(writer, "timeline_patch_invalid")
			return
		}
		operations = expanded
	}
	for _, operation := range operations {
		kind, _ := operation["kind"].(string)
		toolName, ok := tools.TimelineAtomicToolForKind(kind)
		if !ok {
			writeBadRequest(writer, "timeline_patch_invalid")
			return
		}
		input, decodeErr := server.agent.Tools().DecodeInput(toolName, operation)
		if decodeErr != nil {
			writeBadRequest(writer, "timeline_patch_invalid: "+decodeErr.Error())
			return
		}
		raw, executeErr := server.agent.ExecuteTool(ctx, toolName, input)
		if executeErr != nil {
			writeBadRequest(writer, "timeline_patch_invalid: "+executeErr.Error())
			return
		}
		result, resultOK := raw.(tools.ToolResult)
		if !resultOK {
			server.internalError(writer, errors.New("timeline patch 返回类型无效"))
			return
		}
		if result.Status != "succeeded" {
			writeBadRequest(writer, "timeline_patch_validation_failed")
			return
		}
	}
	// 手动编辑由浏览器 EditorSession 乐观更新，并由 Diffusion Studio Core 即时预览；
	// 这里只原子保存最新逻辑时间线。
	// 避免每次拖动后都排队一次完整 FFmpeg 渲染，最终预览/导出仍由显式工具触发。
	response, err := server.draftTimelineResponse(request.Context(), draftID)
	if err != nil {
		server.internalError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, response)
}

func timelinePatchOperations(value any) ([]map[string]any, bool) {
	raw, ok := value.([]any)
	if !ok {
		if typed, typedOK := value.([]map[string]any); typedOK {
			return typed, true
		}
		return nil, false
	}
	operations := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		operation, itemOK := item.(map[string]any)
		if !itemOK || len(operation) == 0 {
			return nil, false
		}
		operations = append(operations, operation)
	}
	return operations, true
}

func (server *Server) draftTimelineResponse(
	ctx context.Context,
	draftID string,
) (DraftTimelineResponse, error) {
	document, err := timeline.Latest(ctx, server.database, draftID)
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
	return DraftTimelineResponse{
		DraftId: draftID, TimelineVersion: document.Version, Timeline: documentMap,
		Summary: timeline.Inspect(document), PreviewId: previewID,
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
