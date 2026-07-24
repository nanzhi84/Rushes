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
		server.writeTimelinePatchFailure(
			writer, request.Context(), draftID, http.StatusBadRequest,
			"timeline_patch_invalid", 0, 0,
		)
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
			server.writeTimelinePatchFailure(
				writer, request.Context(), draftID, http.StatusBadRequest,
				"timeline_patch_invalid", 0, 0,
			)
			return
		}
		operations = expanded
	}
	type invocation struct {
		name  string
		input any
	}
	invocations := make([]invocation, 0, len(operations))
	for index, operation := range operations {
		kind, _ := operation["kind"].(string)
		toolName, ok := tools.TimelineAtomicToolForKind(kind)
		if !ok {
			server.writeTimelinePatchFailure(
				writer, request.Context(), draftID, http.StatusBadRequest,
				"timeline_patch_invalid", 0, index,
			)
			return
		}
		input, decodeErr := server.agent.Tools().DecodeInput(toolName, operation)
		if decodeErr != nil {
			server.writeTimelinePatchFailure(
				writer, request.Context(), draftID, http.StatusBadRequest,
				"timeline_patch_invalid: "+decodeErr.Error(), 0, index,
			)
			return
		}
		invocations = append(invocations, invocation{name: toolName, input: input})
	}
	for index, current := range invocations {
		raw, executeErr := server.agent.ExecuteTool(ctx, current.name, current.input)
		if executeErr != nil {
			server.writeTimelinePatchFailure(
				writer, request.Context(), draftID, http.StatusBadRequest,
				"timeline_patch_invalid: "+executeErr.Error(), index, index,
			)
			return
		}
		result, resultOK := raw.(tools.ToolResult)
		if !resultOK {
			server.writeTimelinePatchFailure(
				writer, request.Context(), draftID, http.StatusInternalServerError,
				"timeline_patch_result_invalid", index, index,
			)
			return
		}
		if result.Status != "succeeded" {
			server.writeTimelinePatchFailure(
				writer, request.Context(), draftID, http.StatusBadRequest,
				"timeline_patch_validation_failed", index, index,
			)
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

func (server *Server) writeTimelinePatchFailure(
	writer http.ResponseWriter,
	ctx context.Context,
	draftID string,
	status int,
	reason string,
	appliedCount int,
	failedIndex int,
) {
	var latest any
	if response, err := server.draftTimelineResponse(ctx, draftID); err == nil {
		latest = response
	}
	writeJSON(writer, status, map[string]any{
		"detail": map[string]any{
			"reason":        reason,
			"applied_count": appliedCount,
			"failed_index":  failedIndex,
			"latest":        latest,
		},
	})
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
