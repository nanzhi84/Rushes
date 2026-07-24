package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/tools"
)

func TestServerConstructionJSONSecurityAndHelperBranches(t *testing.T) {
	t.Parallel()
	if _, err := NewServer(Config{Port: 8000}); err == nil {
		t.Fatal("nil database should fail")
	}
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	for _, port := range []int{0, 65536} {
		if _, err := NewServer(Config{Database: database, Port: port}); err == nil {
			t.Fatalf("port %d should fail", port)
		}
	}
	server, err := NewServer(Config{Database: database, Port: 8000, FSRoots: []string{t.TempDir(), t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	if len(server.token) < 32 || len(GenerateToken()) < 32 {
		t.Fatal("generated token too short")
	}

	health := httptest.NewRecorder()
	server.Handler().ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health=%d", health.Code)
	}
	for _, body := range []string{"{", `{}` + `{}`} {
		request := httptest.NewRequest(http.MethodPost, "/api/drafts", strings.NewReader(body))
		request.Host = "127.0.0.1:8000"
		request.Header.Set("Authorization", "Bearer "+server.token)
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("body=%q status=%d response=%s", body, recorder.Code, recorder.Body.String())
		}
	}
	for _, authorization := range []string{"Basic token", "Bearer"} {
		request := apiRequest(t, http.MethodGet, "/api/drafts", nil)
		request.Header.Set("Authorization", authorization)
		recorder := httptest.NewRecorder()
		server.securityMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("authorization=%q status=%d", authorization, recorder.Code)
		}
	}
	validOrigin := apiRequest(t, http.MethodGet, "/api/drafts", nil)
	validOrigin.Header.Set("Authorization", "Bearer "+server.token)
	validOrigin.Header.Set("Origin", "http://127.0.0.1:8000")
	accepted := false
	server.securityMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { accepted = true })).ServeHTTP(httptest.NewRecorder(), validOrigin)
	if !accepted || !allowsQueryToken(httptest.NewRequest(http.MethodGet, "/api/media/x/source", nil)) ||
		allowsQueryToken(httptest.NewRequest(http.MethodPost, "/api/media/x/source", nil)) || isMutation(http.MethodGet) {
		t.Fatal("security helper mismatch")
	}
	if contentTypeForName("clip.mp4", "fallback") == "fallback" || contentTypeForName("clip.unknown", "fallback") != "fallback" {
		t.Fatal("content type mismatch")
	}
	if mapPointer(nil) != nil || mapPointer(map[string]any{}) == nil || pointerValue(nil) != nil {
		t.Fatal("pointer helper mismatch")
	}
	for _, value := range []any{float64(1), float32(2), 3, "bad"} {
		_, _ = numeric(value)
	}
}

func TestDraftFSMaterialSummaryAndTimelineErrorContracts(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	clip := filepath.Join(root, "clip.mp4")
	image := filepath.Join(root, "poster.png")
	copyClip := filepath.Join(root, "copy.mov")
	unsupported := filepath.Join(root, "notes.txt")
	for path, data := range map[string][]byte{
		clip: []byte("video"), image: []byte("image"), copyClip: []byte("copy"), unsupported: []byte("text"),
	} {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	directory := filepath.Join(root, "batch")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	server, handler := testServer(t, root, 1)
	createDraftThroughAPI(t, handler, "draft_edges")

	for _, item := range []struct {
		method string
		path   string
		body   any
		status int
		reason string
	}{
		{http.MethodGet, "/api/drafts/missing", nil, 404, "draft_not_found"},
		{http.MethodGet, "/api/drafts/missing/timeline", nil, 404, "draft_not_found"},
		{http.MethodGet, "/api/drafts/draft_edges/timeline", nil, 404, "not_found"},
		{http.MethodPost, "/api/drafts/missing/materials/import-local", map[string]any{"path": clip}, 404, "draft_not_found"},
		{http.MethodPost, "/api/drafts/draft_edges/materials/import-local", map[string]any{}, 400, "missing_path"},
		{http.MethodPost, "/api/drafts/draft_edges/materials/import-local", map[string]any{"path": filepath.Join(server.fsRoots[0], "missing.mp4")}, 400, "path_not_found"},
		{http.MethodPost, "/api/drafts/draft_edges/materials/import-local", map[string]any{"path": unsupported}, 400, "unsupported_material_type"},
		{http.MethodPost, "/api/drafts/draft_edges/materials/import-local", map[string]any{"path": "/etc/passwd"}, 403, "path_escape"},
		{http.MethodPost, "/api/drafts/draft_edges/materials/import-local", map[string]any{"path": directory, "asset_id": "one"}, 400, "asset_id_requires_single_file"},
		{http.MethodGet, "/api/drafts/missing/materials", nil, 404, "draft_not_found"},
		{http.MethodGet, "/api/drafts/draft_edges/materials/missing/summary", nil, 404, "asset_not_linked"},
		{http.MethodPost, "/api/drafts/missing/previews/missing/viewed", nil, 404, "draft_not_found"},
		{http.MethodGet, "/api/drafts/missing/events?turn_stream_client_id=test-client", nil, 404, "draft_not_found"},
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, apiRequest(t, item.method, item.path, item.body))
		if recorder.Code != item.status || !strings.Contains(recorder.Body.String(), item.reason) {
			t.Fatalf("%s %s request=%#v want=%d/%s status=%d body=%s", item.method, item.path, item.body,
				item.status, item.reason, recorder.Code, recorder.Body.String())
		}
	}

	for _, mode := range []string{"files", "folder", "mixed"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, apiRequest(t, http.MethodPost, "/api/fs/pick", map[string]any{"mode": mode}))
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), mode) {
			t.Fatalf("picker mode=%s status=%d body=%s", mode, recorder.Code, recorder.Body.String())
		}
	}
	invalidPicker := httptest.NewRecorder()
	handler.ServeHTTP(invalidPicker, apiRequest(t, http.MethodPost, "/api/fs/pick", map[string]any{"mode": "bad"}))
	if invalidPicker.Code != http.StatusBadRequest {
		t.Fatalf("invalid picker=%d body=%s", invalidPicker.Code, invalidPicker.Body.String())
	}
	roots := httptest.NewRecorder()
	handler.ServeHTTP(roots, apiRequest(t, http.MethodGet, "/api/fs/roots", nil))
	if roots.Code != http.StatusOK || !strings.Contains(roots.Body.String(), root) {
		t.Fatalf("roots=%d body=%s", roots.Code, roots.Body.String())
	}

	imported := httptest.NewRecorder()
	handler.ServeHTTP(imported, apiRequest(t, http.MethodPost, "/api/drafts/draft_edges/materials/import-local", map[string]any{
		"paths": []string{clip, image}, "storage_mode": "reference",
	}))
	if imported.Code != http.StatusOK {
		t.Fatalf("import=%d body=%s", imported.Code, imported.Body.String())
	}
	var mutation MaterialMutationResponse
	if err := json.Unmarshal(imported.Body.Bytes(), &mutation); err != nil || mutation.AssetIds == nil || len(*mutation.AssetIds) != 2 {
		t.Fatalf("mutation=%#v err=%v", mutation, err)
	}
	assetID := (*mutation.AssetIds)[0]
	duplicate := httptest.NewRecorder()
	handler.ServeHTTP(duplicate, apiRequest(t, http.MethodPost, "/api/drafts/draft_edges/materials/import-local", map[string]any{"path": clip}))
	if duplicate.Code != http.StatusOK || !strings.Contains(duplicate.Body.String(), "clip.mp4") {
		t.Fatalf("duplicate=%d body=%s", duplicate.Code, duplicate.Body.String())
	}
	copied := httptest.NewRecorder()
	handler.ServeHTTP(copied, apiRequest(t, http.MethodPost, "/api/drafts/draft_edges/materials/import-local", map[string]any{
		"path": copyClip, "storage_mode": "copy",
	}))
	if copied.Code != http.StatusOK {
		t.Fatalf("copy=%d body=%s", copied.Code, copied.Body.String())
	}
	notReady := httptest.NewRecorder()
	handler.ServeHTTP(notReady, apiRequest(t, http.MethodGet, "/api/drafts/draft_edges/materials/"+assetID+"/summary", nil))
	if notReady.Code != http.StatusNotFound || !strings.Contains(notReady.Body.String(), "summary_not_ready") {
		t.Fatalf("not ready=%d body=%s", notReady.Code, notReady.Body.String())
	}
	if _, err := server.database.Write().ExecContext(t.Context(), `
		INSERT INTO material_summaries(summary_id,asset_id,version,status,summary_json,created_at)
		VALUES('summary_edge',?,1,'ready','{"overall":"ready"}',?)`, assetID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	ready := httptest.NewRecorder()
	handler.ServeHTTP(ready, apiRequest(t, http.MethodGet, "/api/drafts/draft_edges/materials/"+assetID+"/summary", nil))
	if ready.Code != http.StatusOK || !strings.Contains(ready.Body.String(), "ready") {
		t.Fatalf("ready=%d body=%s", ready.Code, ready.Body.String())
	}
	revalidated := httptest.NewRecorder()
	handler.ServeHTTP(revalidated, apiRequest(t, http.MethodPost, "/api/drafts/draft_edges/materials/revalidate", nil))
	if revalidated.Code != http.StatusOK || !strings.Contains(revalidated.Body.String(), "invalidated_asset_ids") {
		t.Fatalf("revalidate=%d body=%s", revalidated.Code, revalidated.Body.String())
	}

	draftEvents := httptest.NewRecorder()
	handler.ServeHTTP(draftEvents, apiRequest(t, http.MethodGet,
		"/api/drafts/draft_edges/events?last_event_id=0&turn_stream_client_id=test-client", nil))
	if draftEvents.Code != http.StatusOK || !strings.Contains(draftEvents.Body.String(), "event: DraftCreated") {
		t.Fatalf("draft events=%d body=%s", draftEvents.Code, draftEvents.Body.String())
	}
}

func TestDecisionEndpointConflictOwnershipPendingAndNullBranches(t *testing.T) {
	t.Parallel()
	server, handler := testServer(t, t.TempDir(), 0)
	createDraftThroughAPI(t, handler, "draft_decision_edges")
	current := httptest.NewRecorder()
	handler.ServeHTTP(current, apiRequest(t, http.MethodGet, "/api/drafts/draft_decision_edges/decisions/current", nil))
	if current.Code != http.StatusOK || !strings.Contains(current.Body.String(), `"decision":null`) {
		t.Fatalf("current=%d body=%s", current.Code, current.Body.String())
	}
	ctx := tools.WithDraftID(t.Context(), "draft_decision_edges")
	result, err := server.agent.ExecuteTool(ctx, "interaction.ask_user", tools.AskUserInput{
		Question: "存在关键冲突，是否继续？", DecisionType: "critical",
	})
	if err != nil {
		t.Fatal(err)
	}
	decisionID := result.(tools.ToolResult).Data["decision_id"].(string)
	pending := httptest.NewRecorder()
	handler.ServeHTTP(pending, apiRequest(t, http.MethodGet, "/api/drafts/draft_decision_edges/decisions/pending", nil))
	if pending.Code != http.StatusOK || !strings.Contains(pending.Body.String(), decisionID) {
		t.Fatalf("pending=%d body=%s", pending.Code, pending.Body.String())
	}
	wrongOwner := httptest.NewRecorder()
	handler.ServeHTTP(wrongOwner, apiRequest(t, http.MethodPost, "/api/decisions/"+decisionID+"/answer", map[string]any{
		"draft_id": "wrong", "answer": map[string]any{"answered_via": "button", "option_id": "yes"},
	}))
	if wrongOwner.Code != http.StatusBadRequest || !strings.Contains(wrongOwner.Body.String(), "decision_ownership_mismatch") {
		t.Fatalf("wrong owner=%d body=%s", wrongOwner.Code, wrongOwner.Body.String())
	}
	answer := httptest.NewRecorder()
	handler.ServeHTTP(answer, apiRequest(t, http.MethodPost, "/api/decisions/"+decisionID+"/answer", map[string]any{
		"draft_id": "draft_decision_edges", "answer": map[string]any{"answered_via": "natural_language", "free_text": "继续"},
	}))
	if answer.Code != http.StatusOK || !strings.Contains(answer.Body.String(), `"replays_enqueued":1`) {
		t.Fatalf("answer=%d body=%s", answer.Code, answer.Body.String())
	}
	server.agent.Queue().JoinDraft("draft_decision_edges")
	stored, err := storage.GetDecision(t.Context(), server.database.Read(), decisionID)
	if err != nil || stored.PendingToolCall != nil || stored.PendingToolCallStatus != nil {
		t.Fatalf("普通选择不应留下待重放工具状态: decision=%#v err=%v", stored, err)
	}
	var continued int
	if err := server.database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM messages
		WHERE draft_id='draft_decision_edges' AND role='assistant'`,
	).Scan(&continued); err != nil || continued != 1 {
		t.Fatalf("回答后应自动续跑一次: count=%d err=%v", continued, err)
	}
	again := httptest.NewRecorder()
	handler.ServeHTTP(again, apiRequest(t, http.MethodPost, "/api/decisions/"+decisionID+"/answer", map[string]any{
		"answer": map[string]any{"answered_via": "button", "option_id": "yes"},
	}))
	if again.Code != http.StatusConflict || !strings.Contains(again.Body.String(), "decision_not_pending") {
		t.Fatalf("again=%d body=%s", again.Code, again.Body.String())
	}
	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, apiRequest(t, http.MethodPost, "/api/decisions/missing/answer", map[string]any{
		"answer": map[string]any{"answered_via": "button", "option_id": "yes"},
	}))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing=%d body=%s", missing.Code, missing.Body.String())
	}
}

func TestMediaHeadWrappersMissingAndRangeHelpers(t *testing.T) {
	t.Parallel()
	server, handler := testServer(t, t.TempDir(), 0)
	for _, path := range []string{
		"/api/media/missing/source", "/api/media/missing/proxy", "/api/media/missing/thumbnail",
		"/api/media/preview/missing", "/api/media/export/missing",
	} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, apiRequest(t, method, path, nil))
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("%s %s status=%d body=%s", method, path, recorder.Code, recorder.Body.String())
			}
		}
	}
	if _, _, _, err := parseRange("bytes=1", 10); err == nil {
		t.Fatal("range without dash should fail")
	}
	file := filepath.Join(server.database.Paths.Temporary, "empty.bin")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	serveRange(recorder, httptest.NewRequest(http.MethodGet, "/", nil), file, "application/octet-stream")
	if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Length") != "0" {
		t.Fatalf("empty status=%d headers=%v", recorder.Code, recorder.Header())
	}
	directory := filepath.Join(server.database.Paths.Temporary, "directory")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	directoryResponse := httptest.NewRecorder()
	serveRange(directoryResponse, httptest.NewRequest(http.MethodGet, "/", nil), directory, "x")
	if directoryResponse.Code != http.StatusNotFound {
		t.Fatalf("directory status=%d", directoryResponse.Code)
	}
	badJSON := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{"known":1,"unknown":2}`))
	var payload struct {
		Known int `json:"known"`
	}
	if err := decodeJSON(badJSON, &payload); err == nil {
		t.Fatal("unknown field should fail")
	}
}

func TestReducerResponseFormatting(t *testing.T) {
	t.Parallel()
	base := 1
	conflict := httptest.NewRecorder()
	writeReducerResult(conflict, reducer.Result{Status: reducer.StatusVersionConflict, Conflict: &reducer.VersionConflict{
		DraftID: "d", ExpectedBaseVersion: &base, ActualStateVersion: 2,
	}})
	if conflict.Code != http.StatusConflict || !strings.Contains(conflict.Body.String(), "version_conflict") {
		t.Fatalf("conflict=%d body=%s", conflict.Code, conflict.Body.String())
	}
	validation := httptest.NewRecorder()
	writeReducerResult(validation, reducer.Result{Status: reducer.StatusValidationFailed})
	if validation.Code != http.StatusConflict || !strings.Contains(validation.Body.String(), "validation_failed") {
		t.Fatalf("validation=%d body=%s", validation.Code, validation.Body.String())
	}
	eventIDs := reducerEventIDs(reducer.Result{AppliedEvents: []reducer.AppliedEvent{{ID: 1}, {ID: 2}}})
	if len(eventIDs) != 2 || eventIDs[1] != 2 {
		t.Fatalf("event ids=%v", eventIDs)
	}
}

func TestConversationDecisionAndMediaStateErrors(t *testing.T) {
	t.Parallel()
	server, handler := testServer(t, t.TempDir(), 0)
	createDraftThroughAPI(t, handler, "draft_state_errors")
	for _, item := range []struct {
		method string
		path   string
		body   any
		status int
		reason string
	}{
		{http.MethodPost, "/api/drafts/missing/messages", map[string]any{"content": "x"}, 404, "draft_not_found"},
		{http.MethodPost, "/api/drafts/draft_state_errors/messages", map[string]any{"content": "   "}, 400, "empty_message"},
		{http.MethodGet, "/api/drafts/missing/messages", nil, 404, "draft_not_found"},
		{http.MethodPost, "/api/drafts/missing/turn/cancel", nil, 404, "draft_not_found"},
		{http.MethodGet, "/api/drafts/missing/decisions/current", nil, 404, "draft_not_found"},
		{http.MethodGet, "/api/drafts/missing/decisions/pending", nil, 404, "draft_not_found"},
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, apiRequest(t, item.method, item.path, item.body))
		if recorder.Code != item.status || !strings.Contains(recorder.Body.String(), item.reason) {
			t.Fatalf("%s %s status=%d body=%s", item.method, item.path, recorder.Code, recorder.Body.String())
		}
	}
	invalidMessage := httptest.NewRecorder()
	handler.ServeHTTP(invalidMessage, apiRequest(t, http.MethodPost, "/api/drafts/draft_state_errors/messages", json.RawMessage(`{"content":"x","unknown":1}`)))
	if invalidMessage.Code != http.StatusBadRequest {
		t.Fatalf("invalid message=%d body=%s", invalidMessage.Code, invalidMessage.Body.String())
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := server.database.Write().ExecContext(t.Context(), `
		INSERT INTO decisions(decision_id,scope_type,draft_id,type,question,options_json,allow_free_text,status,blocking)
		VALUES('workspace_decision','workspace',NULL,'generic','workspace?','[]',1,'pending',0);
		INSERT INTO assets(asset_id,storage_mode,reference_path,kind,source,filename,hash,size,ingest_status,usable)
		VALUES('asset_invalid','reference','/missing','video','local','invalid.mp4','invalid',1,'ready',0),
		      ('asset_not_ready','reference',NULL,'audio','local','audio.bin','audio',1,'ready',1),
		      ('asset_bad_hash','copy',NULL,'video','local','copy.bin','copy',1,'ready',1);
		INSERT INTO objects(hash,rel_path,size,created_at) VALUES('short','short',1,?);
		INSERT INTO previews(preview_id,draft_id,timeline_version,object_hash,quality_json,created_at)
		VALUES('preview_short','draft_state_errors',1,'short','{}',?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := server.database.Write().Exec("UPDATE assets SET object_hash='short' WHERE asset_id='asset_bad_hash'"); err != nil {
		t.Fatal(err)
	}
	workspaceAnswer := httptest.NewRecorder()
	handler.ServeHTTP(workspaceAnswer, apiRequest(t, http.MethodPost, "/api/decisions/workspace_decision/answer", map[string]any{
		"answer": map[string]any{"answered_via": "button", "option_id": "yes"},
	}))
	if workspaceAnswer.Code != http.StatusBadRequest || !strings.Contains(workspaceAnswer.Body.String(), "workspace_decision_not_supported") {
		t.Fatalf("workspace answer=%d body=%s", workspaceAnswer.Code, workspaceAnswer.Body.String())
	}
	for path, reason := range map[string]string{
		"/api/media/asset_invalid/source":      "reference_invalidated",
		"/api/media/asset_not_ready/proxy":     "proxy_not_ready",
		"/api/media/asset_not_ready/thumbnail": "thumbnail_not_ready",
		"/api/media/asset_bad_hash/source":     "source_not_ready",
		"/api/media/preview/preview_short":     "preview_not_ready",
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, apiRequest(t, http.MethodGet, path, nil))
		if recorder.Code != http.StatusNotFound || !strings.Contains(recorder.Body.String(), reason) {
			t.Fatalf("path=%s status=%d body=%s", path, recorder.Code, recorder.Body.String())
		}
	}

	answer := map[string]any{"answered_via": "button", "option_id": "yes", "free_text": "ok"}
	pendingStatus := "pending"
	draftID := "draft_state_errors"
	recorded := decisionRecord(storage.Decision{
		ID: "d", ScopeType: "draft", DraftID: &draftID, Type: "generic", Question: "q",
		Options: []map[string]any{{"option_id": "yes", "label": "Yes", "description": "desc"}},
		Answer:  answer, PendingToolCall: map[string]any{
			"tool_name": "render.start", "arguments": "bad", "idempotency_key": "key", "argument_fingerprint": "fp",
		}, PendingToolCallStatus: &pendingStatus, Status: "answered",
	})
	if recorded.Answer == nil || recorded.Answer.FreeText == nil || recorded.PendingToolCall == nil ||
		recorded.Options == nil || len(*recorded.Options) != 1 {
		t.Fatalf("recorded=%#v", recorded)
	}
	if decisionPendingStatus(nil) != nil || stringValue(1) != "" || len(mapValueAPI("bad")) != 0 {
		t.Fatal("decision helper mismatch")
	}
}

func TestClosedDatabaseReturnsInternalErrorsAcrossAdapters(t *testing.T) {
	t.Parallel()
	server, _ := testServer(t, t.TempDir(), 0)
	if err := server.database.Close(); err != nil {
		t.Fatal(err)
	}
	type invocation func(http.ResponseWriter, *http.Request)
	get := apiRequest(t, http.MethodGet, "/", nil)
	post := apiRequest(t, http.MethodPost, "/", map[string]any{"content": "x", "answer": map[string]any{"answered_via": "button"}})
	invocations := []invocation{
		server.ListDraftsApiDraftsGet,
		func(w http.ResponseWriter, r *http.Request) { server.GetDraftApiDraftsDraftIdGet(w, r, "d") },
		func(w http.ResponseWriter, r *http.Request) { server.CreateDraftApiDraftsPost(w, r) },
		func(w http.ResponseWriter, r *http.Request) {
			server.ListMaterialsApiDraftsDraftIdMaterialsGet(w, r, "d")
		},
		func(w http.ResponseWriter, r *http.Request) {
			server.GetMaterialSummaryApiDraftsDraftIdMaterialsAssetIdSummaryGet(w, r, "d", "a")
		},
		func(w http.ResponseWriter, r *http.Request) {
			server.GetDraftTimelineApiDraftsDraftIdTimelineGet(w, r, "d")
		},
		func(w http.ResponseWriter, r *http.Request) {
			server.PreviewViewedApiDraftsDraftIdPreviewsPreviewIdViewedPost(w, r, "d", "p")
		},
		func(w http.ResponseWriter, r *http.Request) {
			server.CurrentDecisionApiDraftsDraftIdDecisionsCurrentGet(w, r, "d")
		},
		func(w http.ResponseWriter, r *http.Request) {
			server.PendingDraftDecisionsApiDraftsDraftIdDecisionsPendingGet(w, r, "d")
		},
		func(w http.ResponseWriter, r *http.Request) {
			server.AnswerDecisionApiDecisionsDecisionIdAnswerPost(w, r, "d")
		},
		func(w http.ResponseWriter, r *http.Request) {
			server.EnqueueMessageApiDraftsDraftIdMessagesPost(w, r, "d")
		},
		func(w http.ResponseWriter, r *http.Request) {
			server.ListDraftMessagesApiDraftsDraftIdMessagesGet(w, r, "d", ListDraftMessagesApiDraftsDraftIdMessagesGetParams{})
		},
		func(w http.ResponseWriter, r *http.Request) {
			server.CancelCurrentTurnApiDraftsDraftIdTurnCancelPost(w, r, "d")
		},
		func(w http.ResponseWriter, r *http.Request) {
			server.DraftEventsApiDraftsDraftIdEventsGet(w, r, "d", DraftEventsApiDraftsDraftIdEventsGetParams{
				TurnStreamClientId: "edge-case-client",
			})
		},
		func(w http.ResponseWriter, r *http.Request) { server.MediaSourceApiMediaAssetIdSourceGet(w, r, "a") },
		func(w http.ResponseWriter, r *http.Request) {
			server.MediaPreviewApiMediaPreviewPreviewIdGet(w, r, "p")
		},
	}
	for index, invoke := range invocations {
		recorder := httptest.NewRecorder()
		request := get.Clone(context.Background())
		switch index {
		case 2:
			request = apiRequest(t, http.MethodPost, "/", map[string]any{})
		case 6, 9, 10, 12:
			request = post.Clone(context.Background())
		}
		invoke(recorder, request)
		if recorder.Code != http.StatusInternalServerError {
			t.Fatalf("invocation[%d] status=%d body=%s", index, recorder.Code, recorder.Body.String())
		}
	}
}

func TestFilesystemAndMaterialHelpers(t *testing.T) {
	t.Parallel()
	if len(defaultFSRoots()) == 0 {
		t.Fatal("default roots empty")
	}
	for _, item := range []struct {
		name string
		kind string
		ok   bool
	}{
		{"a.MP4", "video", true}, {"a.wav", "audio", true}, {"a.jpeg", "image", true},
		{"a.woff2", "font", true}, {"a.txt", "", false},
	} {
		kind, ok := materialKind(item.name)
		if kind != item.kind || ok != item.ok {
			t.Fatalf("name=%s kind=%s ok=%v", item.name, kind, ok)
		}
	}
	path := filepath.Join(t.TempDir(), "hash.mp4")
	if err := os.WriteFile(path, []byte("hash"), 0o644); err != nil {
		t.Fatal(err)
	}
	hash, err := hashFile(t.Context(), path)
	if err != nil || len(hash) != 64 {
		t.Fatalf("hash=%s err=%v", hash, err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := hashFile(cancelled, path); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel hash err=%v", err)
	}

	lookupOK := func(string) (string, error) { return "/usr/bin/osascript", nil }
	if paths, available := nativePickerWith(t.Context(), "files", "linux", lookupOK, nil); available || len(paths) != 0 {
		t.Fatalf("non-darwin paths=%v available=%v", paths, available)
	}
	if _, available := nativePickerWith(t.Context(), "files", "darwin",
		func(string) (string, error) { return "", errors.New("missing") }, nil); available {
		t.Fatal("缺少 osascript 时 picker 不应可用")
	}
	for _, item := range []struct {
		mode              string
		chooseFiles       bool
		chooseDirectories bool
	}{
		{mode: "files", chooseFiles: true},
		{mode: "folder", chooseDirectories: true},
		{mode: "mixed", chooseFiles: true, chooseDirectories: true},
	} {
		var script string
		paths, available := nativePickerWith(t.Context(), item.mode, "darwin", lookupOK,
			func(_ context.Context, value string) ([]byte, error) {
				script = value
				return []byte(" /tmp/a.mp4 \n\n/tmp/b.mov\n"), nil
			})
		filesFlag := "panel.setCanChooseFiles(" + map[bool]string{true: "true", false: "false"}[item.chooseFiles] + ")"
		directoriesFlag := "panel.setCanChooseDirectories(" + map[bool]string{true: "true", false: "false"}[item.chooseDirectories] + ")"
		activationPolicy := "application.setActivationPolicy($.NSApplicationActivationPolicyAccessory)"
		activation := "application.activateIgnoringOtherApps(true)"
		if !available || len(paths) != 2 || !strings.Contains(script, filesFlag) ||
			!strings.Contains(script, directoriesFlag) || !strings.Contains(script, activationPolicy) ||
			!strings.Contains(script, activation) {
			t.Fatalf("mode=%s paths=%v available=%v script=%q", item.mode, paths, available, script)
		}
	}
	for _, item := range []struct {
		output    string
		err       error
		available bool
	}{{"User canceled (-128)", errors.New("exit 1"), true}, {"", context.Canceled, true}, {"boom", errors.New("exit 2"), false}} {
		paths, available := nativePickerWith(t.Context(), "files", "darwin", lookupOK,
			func(context.Context, string) ([]byte, error) { return []byte(item.output), item.err })
		if available != item.available || len(paths) != 0 {
			t.Fatalf("output=%q available=%v paths=%v", item.output, available, paths)
		}
	}
	cancelledContext, cancelPicker := context.WithCancel(t.Context())
	cancelPicker()
	if paths, available := nativePickerWith(cancelledContext, "files", "darwin", lookupOK,
		func(context.Context, string) ([]byte, error) { return nil, errors.New("signal: killed") }); !available || len(paths) != 0 {
		t.Fatalf("cancelled picker paths=%v available=%v", paths, available)
	}
}

func TestFSPickerAllowsOnlyOneModalRequest(t *testing.T) {
	database, err := storage.Open(t.Context(), filepath.Join(t.TempDir(), "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	entered := make(chan struct{})
	release := make(chan struct{})
	calls := 0
	server, err := NewServer(Config{
		Database: database,
		Token:    testToken,
		Port:     8000,
		FSRoots:  []string{t.TempDir()},
		Picker: func(ctx context.Context, mode string) ([]string, bool) {
			calls++
			close(entered)
			select {
			case <-release:
				return []string{"/tmp/" + mode}, true
			case <-ctx.Done():
				return []string{}, true
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	handler := server.Handler()
	firstRequest := apiRequest(t, http.MethodPost, "/api/fs/pick", map[string]any{"mode": "mixed"})
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, firstRequest)
		firstDone <- recorder
	}()
	<-entered

	second := httptest.NewRecorder()
	handler.ServeHTTP(second, apiRequest(t, http.MethodPost, "/api/fs/pick", map[string]any{"mode": "mixed"}))
	if second.Code != http.StatusOK || !strings.Contains(second.Body.String(), `"paths":[]`) || calls != 1 {
		t.Fatalf("second picker status=%d body=%s calls=%d", second.Code, second.Body.String(), calls)
	}

	close(release)
	select {
	case first := <-firstDone:
		if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), "/tmp/mixed") {
			t.Fatalf("first picker status=%d body=%s", first.Code, first.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("first picker did not finish")
	}
}
