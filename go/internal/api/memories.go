package api

import (
	"net/http"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

func (server *Server) ListMemoriesApiMemoriesGet(
	writer http.ResponseWriter,
	request *http.Request,
) {
	memories, err := storage.ListUserMemories(request.Context(), server.database.Read())
	if err != nil {
		server.internalError(writer, err)
		return
	}
	records := make([]MemoryRecord, 0, len(memories))
	for _, memory := range memories {
		records = append(records, memoryRecord(memory))
	}
	writeJSON(writer, http.StatusOK, MemoriesResponse{Memories: records})
}

func (server *Server) DeleteMemoryApiMemoriesMemoryKeyDelete(
	writer http.ResponseWriter,
	request *http.Request,
	memoryKey string,
) {
	if !storage.ValidUserMemoryKey(memoryKey) {
		writeBadRequest(writer, "invalid_memory_key")
		return
	}
	removed, err := server.removeMemories(request, []string{memoryKey})
	if err != nil {
		server.internalError(writer, err)
		return
	}
	if len(removed) == 0 {
		writeNotFound(writer, "memory_not_found")
		return
	}
	writeJSON(writer, http.StatusOK, MemoryMutationResponse{
		DeletedCount: len(removed), DeletedMemoryKeys: removed,
	})
}

func (server *Server) ClearMemoriesApiMemoriesDelete(
	writer http.ResponseWriter,
	request *http.Request,
) {
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
	result, err := reducer.Apply(request.Context(), server.database, nil, reducer.Options{
		Actor: contracts.ActorUser,
		ResultRows: reducer.ResultRows{
			UserMemoryClearAll: true,
		},
	})
	if err != nil {
		server.internalError(writer, err)
		return
	}
	if result.Status != reducer.StatusApplied || result.UserMemory == nil {
		server.internalError(writer, reducer.ErrUserMemoryInput)
		return
	}
	removed := result.UserMemory.RemovedKeys
	writeJSON(writer, http.StatusOK, MemoryMutationResponse{
		DeletedCount: len(removed), DeletedMemoryKeys: removed,
	})
}

func (server *Server) removeMemories(request *http.Request, keys []string) ([]string, error) {
	result, err := reducer.Apply(request.Context(), server.database, nil, reducer.Options{
		Actor: contracts.ActorUser,
		ResultRows: reducer.ResultRows{
			UserMemoryRemoveKeys: keys,
		},
	})
	if err != nil {
		return nil, err
	}
	if result.Status != reducer.StatusApplied || result.UserMemory == nil {
		return nil, reducer.ErrUserMemoryInput
	}
	return result.UserMemory.RemovedKeys, nil
}

func memoryRecord(memory storage.UserMemory) MemoryRecord {
	return MemoryRecord{
		MemoryKey: memory.Key, Kind: MemoryRecordKind(memory.Kind), Statement: memory.Statement,
		SourceDraftId: memory.SourceDraftID, CreatedAt: memory.CreatedAt,
		LastConfirmedAt: memory.LastConfirmedAt,
	}
}
