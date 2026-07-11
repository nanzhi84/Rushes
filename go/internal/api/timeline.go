package api

import (
	"errors"
	"net/http"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
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
	var document timeline.Document
	var err error
	if params.Version != nil {
		document, err = timeline.Get(request.Context(), server.database, draftID, *params.Version)
	} else {
		document, err = timeline.Latest(request.Context(), server.database, draftID)
	}
	if errors.Is(err, storage.ErrNotFound) {
		writeNotFound(writer, "not_found")
		return
	}
	if err != nil {
		server.internalError(writer, err)
		return
	}
	documentMap, err := timeline.ToMap(document)
	if err != nil {
		server.internalError(writer, err)
		return
	}
	previewID, err := timeline.LatestPreviewID(request.Context(), server.database, draftID, document.Version)
	if err != nil {
		server.internalError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, DraftTimelineResponse{
		DraftId: draftID, TimelineVersion: document.Version, Timeline: documentMap,
		Summary: timeline.Inspect(document), PreviewId: previewID,
	})
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
