package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBatchDeleteDraftsIsAtomicAndUsesReducer(t *testing.T) {
	t.Parallel()
	server, handler := testServer(t, t.TempDir(), 0)
	for _, draftID := range []string{"draft_batch_a", "draft_batch_b", "draft_batch_keep"} {
		createDraftThroughAPI(t, handler, draftID)
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, apiRequest(t, http.MethodDelete, "/api/drafts", map[string]any{
		"draft_ids": []string{"draft_batch_a", "draft_batch_b"},
		"confirm":   true,
	}))
	if response.Code != http.StatusOK {
		t.Fatalf("batch delete status=%d body=%s", response.Code, response.Body.String())
	}
	var deleted DraftBatchDeleteResponse
	if err := json.Unmarshal(response.Body.Bytes(), &deleted); err != nil {
		t.Fatal(err)
	}
	if deleted.DeletedCount != 2 || len(deleted.DeletedDraftIds) != 2 || len(deleted.EventIds) != 2 {
		t.Fatalf("batch delete response=%#v", deleted)
	}

	listed := httptest.NewRecorder()
	handler.ServeHTTP(listed, apiRequest(t, http.MethodGet, "/api/drafts", nil))
	if listed.Code != http.StatusOK || strings.Contains(listed.Body.String(), "draft_batch_a") ||
		strings.Contains(listed.Body.String(), "draft_batch_b") ||
		!strings.Contains(listed.Body.String(), "draft_batch_keep") {
		t.Fatalf("draft list status=%d body=%s", listed.Code, listed.Body.String())
	}

	for _, draftID := range []string{"draft_batch_a", "draft_batch_b"} {
		var status string
		if err := server.database.Read().QueryRowContext(t.Context(),
			"SELECT status FROM drafts WHERE draft_id=?", draftID,
		).Scan(&status); err != nil || status != "trashed" {
			t.Fatalf("draft=%s status=%s err=%v", draftID, status, err)
		}
	}
	var trashedEvents int
	if err := server.database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM event_log
		WHERE event_type='DraftTrashed' AND draft_id IN ('draft_batch_a', 'draft_batch_b')`,
	).Scan(&trashedEvents); err != nil || trashedEvents != 2 {
		t.Fatalf("trashed events=%d err=%v", trashedEvents, err)
	}
}

func TestBatchDeleteDraftsRejectsMissingDraftWithoutPartialWrite(t *testing.T) {
	t.Parallel()
	server, handler := testServer(t, t.TempDir(), 0)
	createDraftThroughAPI(t, handler, "draft_batch_existing")

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, apiRequest(t, http.MethodDelete, "/api/drafts", map[string]any{
		"draft_ids": []string{"draft_batch_existing", "draft_batch_missing"},
		"confirm":   true,
	}))
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "draft_batch_missing") {
		t.Fatalf("batch delete status=%d body=%s", response.Code, response.Body.String())
	}

	var status string
	if err := server.database.Read().QueryRowContext(t.Context(),
		"SELECT status FROM drafts WHERE draft_id='draft_batch_existing'",
	).Scan(&status); err != nil || status != "active" {
		t.Fatalf("existing draft status=%s err=%v", status, err)
	}
	var eventCount int
	if err := server.database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM event_log
		WHERE event_type='DraftTrashed' AND draft_id='draft_batch_existing'`,
	).Scan(&eventCount); err != nil || eventCount != 0 {
		t.Fatalf("partial trash events=%d err=%v", eventCount, err)
	}
}

func TestBatchDeleteDraftsValidationAndIdempotency(t *testing.T) {
	t.Parallel()
	server, handler := testServer(t, t.TempDir(), 0)
	createDraftThroughAPI(t, handler, "draft_batch_old")
	createDraftThroughAPI(t, handler, "draft_batch_new")

	trashed := httptest.NewRecorder()
	handler.ServeHTTP(trashed, apiRequest(t, http.MethodDelete, "/api/drafts/draft_batch_old", map[string]any{
		"confirm": true,
	}))
	if trashed.Code != http.StatusOK {
		t.Fatalf("prepare trashed draft status=%d body=%s", trashed.Code, trashed.Body.String())
	}

	idempotent := httptest.NewRecorder()
	handler.ServeHTTP(idempotent, apiRequest(t, http.MethodDelete, "/api/drafts", map[string]any{
		"draft_ids": []string{"draft_batch_old", "draft_batch_new"},
		"confirm":   true,
	}))
	var result DraftBatchDeleteResponse
	if idempotent.Code != http.StatusOK || json.Unmarshal(idempotent.Body.Bytes(), &result) != nil ||
		result.DeletedCount != 1 || len(result.EventIds) != 1 ||
		len(result.DeletedDraftIds) != 1 || result.DeletedDraftIds[0] != "draft_batch_new" {
		t.Fatalf("idempotent delete status=%d response=%#v body=%s", idempotent.Code, result, idempotent.Body.String())
	}
	var oldEvents int
	if err := server.database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM event_log
		WHERE event_type='DraftTrashed' AND draft_id='draft_batch_old'`,
	).Scan(&oldEvents); err != nil || oldEvents != 1 {
		t.Fatalf("old draft events=%d err=%v", oldEvents, err)
	}

	cases := []struct {
		name   string
		body   map[string]any
		status int
		reason string
	}{
		{"需要确认", map[string]any{"draft_ids": []string{"draft_batch_new"}, "confirm": false}, http.StatusConflict, "confirmation_required"},
		{"空数组", map[string]any{"draft_ids": []string{}, "confirm": true}, http.StatusBadRequest, "empty_draft_ids"},
		{"重复 ID", map[string]any{"draft_ids": []string{"draft_batch_new", "draft_batch_new"}, "confirm": true}, http.StatusBadRequest, "duplicate_draft_id"},
		{"空白 ID", map[string]any{"draft_ids": []string{" "}, "confirm": true}, http.StatusBadRequest, "invalid_draft_id"},
	}
	for _, item := range cases {
		item := item
		t.Run(item.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, apiRequest(t, http.MethodDelete, "/api/drafts", item.body))
			if response.Code != item.status || !strings.Contains(response.Body.String(), item.reason) {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestBatchDeleteDraftsRetryAfterAllDraftsWereAlreadyTrashed(t *testing.T) {
	t.Parallel()
	_, handler := testServer(t, t.TempDir(), 0)
	createDraftThroughAPI(t, handler, "draft_batch_retry")

	payload := map[string]any{
		"draft_ids": []string{"draft_batch_retry"},
		"confirm":   true,
	}
	first := httptest.NewRecorder()
	handler.ServeHTTP(first, apiRequest(t, http.MethodDelete, "/api/drafts", payload))
	if first.Code != http.StatusOK {
		t.Fatalf("first batch delete status=%d body=%s", first.Code, first.Body.String())
	}

	retry := httptest.NewRecorder()
	handler.ServeHTTP(retry, apiRequest(t, http.MethodDelete, "/api/drafts", payload))
	var result DraftBatchDeleteResponse
	if retry.Code != http.StatusOK || json.Unmarshal(retry.Body.Bytes(), &result) != nil ||
		result.DeletedCount != 0 || len(result.DeletedDraftIds) != 0 || len(result.EventIds) != 0 {
		t.Fatalf("retry status=%d response=%#v body=%s", retry.Code, result, retry.Body.String())
	}
}

func TestBatchDeleteDraftsRejectsMalformedOversizedAndInactiveRequests(t *testing.T) {
	t.Parallel()
	server, handler := testServer(t, t.TempDir(), 0)
	createDraftThroughAPI(t, handler, "draft_batch_inactive")
	if _, err := server.database.Write().ExecContext(t.Context(),
		"UPDATE drafts SET status='processing' WHERE draft_id='draft_batch_inactive'",
	); err != nil {
		t.Fatal(err)
	}

	malformedRequest := apiRequest(t, http.MethodDelete, "/api/drafts", nil)
	malformedRequest.Body = io.NopCloser(strings.NewReader("{"))
	malformedRequest.ContentLength = 1

	tooManyIDs := make([]string, maxBatchDeleteDrafts+1)
	for index := range tooManyIDs {
		tooManyIDs[index] = fmt.Sprintf("draft_batch_%03d", index)
	}

	cases := []struct {
		name    string
		request *http.Request
		status  int
		reason  string
	}{
		{"非法 JSON", malformedRequest, http.StatusBadRequest, "invalid_json"},
		{"超过单次上限", apiRequest(t, http.MethodDelete, "/api/drafts", map[string]any{
			"draft_ids": tooManyIDs,
			"confirm":   true,
		}), http.StatusBadRequest, "too_many_drafts"},
		{"不可删除状态", apiRequest(t, http.MethodDelete, "/api/drafts", map[string]any{
			"draft_ids": []string{"draft_batch_inactive"},
			"confirm":   true,
		}), http.StatusConflict, "draft_not_deletable"},
	}
	for _, item := range cases {
		item := item
		t.Run(item.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, item.request)
			if response.Code != item.status || !strings.Contains(response.Body.String(), item.reason) {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestBatchDeleteDraftsReturnsInternalErrorWhenReducerCannotWrite(t *testing.T) {
	t.Parallel()
	server, handler := testServer(t, t.TempDir(), 0)
	createDraftThroughAPI(t, handler, "draft_batch_write_error")
	if err := server.database.Write().Close(); err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, apiRequest(t, http.MethodDelete, "/api/drafts", map[string]any{
		"draft_ids": []string{"draft_batch_write_error"},
		"confirm":   true,
	}))
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "internal_error") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
