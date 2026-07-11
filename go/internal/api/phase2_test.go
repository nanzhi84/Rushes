package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/media"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
)

func TestImportLocalRecursiveDeduplicateAndInstantLink(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	nested := filepath.Join(root, "batch", "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		filepath.Join(root, "batch", "first.mp4"): "first",
		filepath.Join(nested, "second.jpg"):       "second",
		filepath.Join(nested, "skip.txt"):         "skip",
	} {
		if err := os.WriteFile(name, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	server, handler := testServer(t, root, 0)
	createDraftThroughAPI(t, handler, "draft_import_a")
	createDraftThroughAPI(t, handler, "draft_import_b")

	path := filepath.Join(root, "batch")
	first := httptest.NewRecorder()
	handler.ServeHTTP(first, apiRequest(t, http.MethodPost,
		"/api/drafts/draft_import_a/materials/import-local",
		map[string]any{"path": path, "storage_mode": "reference"}))
	if first.Code != http.StatusOK {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	var firstBody MaterialMutationResponse
	if err := json.Unmarshal(first.Body.Bytes(), &firstBody); err != nil {
		t.Fatal(err)
	}
	if firstBody.AssetIds == nil || len(*firstBody.AssetIds) != 2 || firstBody.Skipped == nil || len(*firstBody.Skipped) != 1 {
		t.Fatalf("first body=%s", first.Body.String())
	}
	var jobs int
	if err := server.database.Read().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM jobs WHERE kind='ingest'").Scan(&jobs); err != nil || jobs != 2 {
		t.Fatalf("jobs=%d err=%v", jobs, err)
	}

	duplicate := httptest.NewRecorder()
	handler.ServeHTTP(duplicate, apiRequest(t, http.MethodPost,
		"/api/drafts/draft_import_a/materials/import-local", map[string]any{"path": path}))
	if duplicate.Code != http.StatusOK || !strings.Contains(duplicate.Body.String(), "first.mp4") ||
		!strings.Contains(duplicate.Body.String(), "second.jpg") {
		t.Fatalf("duplicate status=%d body=%s", duplicate.Code, duplicate.Body.String())
	}
	if err := server.database.Read().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM jobs WHERE kind='ingest'").Scan(&jobs); err != nil || jobs != 2 {
		t.Fatalf("dedupe jobs=%d err=%v", jobs, err)
	}

	linked := httptest.NewRecorder()
	handler.ServeHTTP(linked, apiRequest(t, http.MethodPost,
		"/api/drafts/draft_import_b/materials/import-local", map[string]any{"path": path}))
	if linked.Code != http.StatusOK {
		t.Fatalf("linked status=%d body=%s", linked.Code, linked.Body.String())
	}
	if err := server.database.Read().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM jobs WHERE kind='ingest'").Scan(&jobs); err != nil || jobs != 2 {
		t.Fatalf("instant link jobs=%d err=%v", jobs, err)
	}
	var links int
	if err := server.database.Read().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM draft_asset_links WHERE draft_id='draft_import_b'").Scan(&links); err != nil || links != 2 {
		t.Fatalf("links=%d err=%v", links, err)
	}

	listed := httptest.NewRecorder()
	handler.ServeHTTP(listed, apiRequest(t, http.MethodGet, "/api/drafts/draft_import_b/materials", nil))
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), "batch/nested") {
		t.Fatalf("listed status=%d body=%s", listed.Code, listed.Body.String())
	}
}

func TestMediaRangesAcrossSourceProxyThumbnailPreviewAndExport(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	server, handler := testServer(t, root, 0)
	createDraftThroughAPI(t, handler, "draft_media")
	store := media.NewObjectStore(server.database.Paths)
	object, err := store.PutBytes(t.Context(), []byte("0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := reducer.Apply(t.Context(), server.database, []contracts.Event{
		{Type: "AssetImported", Payload: map[string]any{
			"asset_id": "asset_media", "job_id": "job_media", "storage_mode": "copy",
			"object_hash": object.Hash, "object_size": object.Size, "kind": "video",
			"source": "local_path", "filename": "clip.mp4", "hash": object.Hash,
			"size": object.Size, "ingest_status": "imported",
		}},
		{Type: "AssetLinked", DraftID: "draft_media", Payload: map[string]any{"asset_id": "asset_media"}},
		{Type: "AssetProbed", Payload: map[string]any{
			"asset_id": "asset_media", "probe": map[string]any{"duration_sec": 1},
			"thumbnail_object_hash": object.Hash, "thumbnail_object_size": object.Size,
		}},
		{Type: "ProxyGenerated", Payload: map[string]any{
			"asset_id": "asset_media", "proxy_object_hash": object.Hash,
			"proxy_object_size": object.Size,
		}},
	}, reducer.Options{Actor: contracts.ActorJob, CreatedAt: time.Now().UTC()})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("apply status=%s err=%v", result.Status, err)
	}
	created := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := server.database.Write().ExecContext(t.Context(), `
		INSERT INTO previews(preview_id,draft_id,timeline_version,object_hash,quality_json,created_at)
		VALUES('preview_media','draft_media',1,?,'{}',?);
		INSERT INTO exports(export_id,draft_id,timeline_version,object_hash,quality_json,created_at)
		VALUES('export_media','draft_media',1,?,'{}',?)`, object.Hash, created, object.Hash, created); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		method      string
		path        string
		rangeHeader string
		status      int
		body        string
		length      string
		content     string
	}{
		{"source full", http.MethodGet, "/api/media/asset_media/source", "", 200, "0123456789", "10", ""},
		{"source partial", http.MethodGet, "/api/media/asset_media/source", "bytes=2-5", 206, "2345", "4", "bytes 2-5/10"},
		{"source suffix", http.MethodGet, "/api/media/asset_media/source", "bytes=-3", 206, "789", "3", "bytes 7-9/10"},
		{"proxy head", http.MethodHead, "/api/media/asset_media/proxy", "bytes=0-1", 206, "", "2", "bytes 0-1/10"},
		{"thumbnail", http.MethodGet, "/api/media/asset_media/thumbnail", "", 200, "0123456789", "10", ""},
		{"preview", http.MethodGet, "/api/media/preview/preview_media", "bytes=1-", 206, "123456789", "9", "bytes 1-9/10"},
		{"export", http.MethodGet, "/api/media/export/export_media", "bytes=0-99", 206, "0123456789", "10", "bytes 0-9/10"},
	}
	for _, item := range tests {
		item := item
		t.Run(item.name, func(t *testing.T) {
			request := apiRequest(t, item.method, item.path+"?token="+url.QueryEscape(testToken), nil)
			request.Header.Del("Authorization")
			if item.rangeHeader != "" {
				request.Header.Set("Range", item.rangeHeader)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != item.status || recorder.Body.String() != item.body ||
				recorder.Header().Get("Content-Length") != item.length ||
				recorder.Header().Get("Content-Range") != item.content {
				t.Fatalf("status=%d body=%q headers=%v", recorder.Code, recorder.Body.String(), recorder.Header())
			}
		})
	}
	for _, invalid := range []string{"items=0-1", "bytes=99-", "bytes=0-1,3-4", "bytes=-0", "bytes=nope"} {
		request := apiRequest(t, http.MethodGet, "/api/media/asset_media/source", nil)
		request.Header.Set("Range", invalid)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusRequestedRangeNotSatisfiable ||
			recorder.Header().Get("Content-Range") != "bytes */10" ||
			!strings.Contains(recorder.Body.String(), "invalid_range") {
			t.Fatalf("range=%q status=%d body=%s headers=%v", invalid, recorder.Code, recorder.Body.String(), recorder.Header())
		}
	}
}

func createDraftThroughAPI(t *testing.T, handler http.Handler, draftID string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, apiRequest(t, http.MethodPost, "/api/drafts", map[string]any{
		"draft_id": draftID, "name": draftID,
	}))
	if recorder.Code != http.StatusOK {
		t.Fatalf("create draft status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
