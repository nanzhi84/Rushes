package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

func TestMemoryGovernanceEndpointsListDeleteAndClear(t *testing.T) {
	t.Parallel()
	server, handler := testServer(t, t.TempDir(), 0)
	seedAPIMemory(t, server, "pacing", "preference", "成片节奏偏快")
	seedAPIMemory(t, server, "subtitle_style", "correction", "字幕不要遮脸")

	listed := httptest.NewRecorder()
	handler.ServeHTTP(listed, apiRequest(t, http.MethodGet, "/api/memories", nil))
	if listed.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", listed.Code, listed.Body.String())
	}
	var response MemoriesResponse
	if err := json.Unmarshal(listed.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Memories) != 2 || response.Memories[0].MemoryKey != "subtitle_style" ||
		response.Memories[1].MemoryKey != "pacing" || response.Memories[0].SourceDraftId != "draft_memory_api" {
		t.Fatalf("memories=%#v", response.Memories)
	}
	for _, privateField := range []string{"evidence_kind", "evidence_id"} {
		if strings.Contains(listed.Body.String(), privateField) {
			t.Fatalf("REST response leaked %s: %s", privateField, listed.Body.String())
		}
	}

	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, apiRequest(t, http.MethodDelete, "/api/memories/missing_key", nil))
	if missing.Code != http.StatusNotFound || !strings.Contains(missing.Body.String(), "memory_not_found") {
		t.Fatalf("missing status=%d body=%s", missing.Code, missing.Body.String())
	}
	invalid := httptest.NewRecorder()
	handler.ServeHTTP(invalid, apiRequest(t, http.MethodDelete, "/api/memories/Bad-Key", nil))
	if invalid.Code != http.StatusBadRequest || !strings.Contains(invalid.Body.String(), "invalid_memory_key") {
		t.Fatalf("invalid status=%d body=%s", invalid.Code, invalid.Body.String())
	}

	deleted := httptest.NewRecorder()
	handler.ServeHTTP(deleted, apiRequest(t, http.MethodDelete, "/api/memories/pacing", nil))
	if deleted.Code != http.StatusOK || !strings.Contains(deleted.Body.String(), `"deleted_memory_keys":["pacing"]`) {
		t.Fatalf("delete status=%d body=%s", deleted.Code, deleted.Body.String())
	}

	unconfirmed := httptest.NewRecorder()
	handler.ServeHTTP(unconfirmed, apiRequest(t, http.MethodDelete, "/api/memories", map[string]any{"confirm": false}))
	if unconfirmed.Code != http.StatusConflict || !strings.Contains(unconfirmed.Body.String(), "confirmation_required") {
		t.Fatalf("unconfirmed status=%d body=%s", unconfirmed.Code, unconfirmed.Body.String())
	}
	malformed := httptest.NewRecorder()
	handler.ServeHTTP(malformed, apiRequest(t, http.MethodDelete, "/api/memories", json.RawMessage(`{"confirm":true,"extra":1}`)))
	if malformed.Code != http.StatusBadRequest || !strings.Contains(malformed.Body.String(), "invalid_json") {
		t.Fatalf("malformed status=%d body=%s", malformed.Code, malformed.Body.String())
	}

	cleared := httptest.NewRecorder()
	handler.ServeHTTP(cleared, apiRequest(t, http.MethodDelete, "/api/memories", map[string]any{"confirm": true}))
	if cleared.Code != http.StatusOK || !strings.Contains(cleared.Body.String(), `"deleted_memory_keys":["subtitle_style"]`) {
		t.Fatalf("clear status=%d body=%s", cleared.Code, cleared.Body.String())
	}
	empty := httptest.NewRecorder()
	handler.ServeHTTP(empty, apiRequest(t, http.MethodDelete, "/api/memories", map[string]any{"confirm": true}))
	if empty.Code != http.StatusOK || !strings.Contains(empty.Body.String(), `"deleted_count":0`) {
		t.Fatalf("empty clear status=%d body=%s", empty.Code, empty.Body.String())
	}
}

func TestMemoryGovernanceEndpointsReportStorageFailures(t *testing.T) {
	t.Parallel()
	server, handler := testServer(t, t.TempDir(), 0)
	if _, err := server.database.Write().ExecContext(t.Context(), "DROP TABLE user_memories"); err != nil {
		t.Fatal(err)
	}
	for _, request := range []*http.Request{
		apiRequest(t, http.MethodGet, "/api/memories", nil),
		apiRequest(t, http.MethodDelete, "/api/memories/pacing", nil),
		apiRequest(t, http.MethodDelete, "/api/memories", map[string]any{"confirm": true}),
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusInternalServerError {
			t.Fatalf("%s %s status=%d body=%s", request.Method, request.URL.Path, recorder.Code, recorder.Body.String())
		}
	}
}

func seedAPIMemory(t *testing.T, server *Server, key, kind, statement string) {
	t.Helper()
	const draftID = "draft_memory_api"
	if _, err := storage.GetDraft(t.Context(), server.database.Read(), draftID); err != nil {
		result, applyErr := reducer.Apply(t.Context(), server.database, []contracts.Event{{
			Type: "DraftCreated", DraftID: draftID, Payload: map[string]any{"name": "记忆 API"},
		}}, reducer.Options{Actor: contracts.ActorUser})
		if applyErr != nil || result.Status != reducer.StatusApplied {
			t.Fatalf("create draft result=%#v err=%v", result, applyErr)
		}
	}
	messageID := "message_" + key
	result, err := reducer.Apply(t.Context(), server.database, nil, reducer.Options{
		Actor: contracts.ActorUser,
		ResultRows: reducer.ResultRows{
			Message: &reducer.MessageRow{
				ID: messageID, DraftID: draftID, Role: "user", Kind: "user", Content: statement,
			},
			UserMemoryUpserts: []reducer.UserMemoryRow{{
				Key: key, Kind: kind, Statement: statement,
				EvidenceKind: storage.UserMemoryEvidenceMessage,
				EvidenceID:   messageID, SourceDraftID: draftID,
			}},
		},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("seed memory result=%#v err=%v", result, err)
	}
}
