package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/media"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/tools"
)

func TestTimelineEndpointPreviewLookupAndViewedMutation(t *testing.T) {
	t.Parallel()
	server, handler := testServer(t, t.TempDir(), 0)
	createDraftThroughAPI(t, handler, "draft_timeline_api")
	result, err := reducer.Apply(t.Context(), server.database, []contracts.Event{
		{Type: "AssetImported", Payload: map[string]any{
			"asset_id": "asset_timeline_api", "job_id": "job_asset", "storage_mode": "reference",
			"reference_path": "/tmp/clip.mp4", "kind": "video", "source": "local_path",
			"filename": "clip.mp4", "hash": "hash", "size": 1, "ingest_status": "ready",
		}},
		{Type: "AssetLinked", DraftID: "draft_timeline_api", Payload: map[string]any{"asset_id": "asset_timeline_api"}},
	}, reducer.Options{Actor: contracts.ActorUser})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("assets status=%s err=%v", result.Status, err)
	}
	ctx := tools.WithDraftID(t.Context(), "draft_timeline_api")
	if _, err := server.agent.ExecuteTool(ctx, "timeline.compose_initial", tools.ComposeInitialInput{
		Clips: []tools.ComposeClip{{
			AssetID: "asset_timeline_api", SourceStartFrame: 0, SourceEndFrame: 60, Role: "a_roll",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	store := media.NewObjectStore(server.database.Paths)
	object, err := store.Put(t.Context(), bytes.NewReader([]byte("preview")))
	if err != nil {
		t.Fatal(err)
	}
	result, err = reducer.Apply(t.Context(), server.database, []contracts.Event{{
		Type: "PreviewRendered", DraftID: "draft_timeline_api",
		Payload: map[string]any{
			"artifact_id": "preview_timeline_api", "timeline_version": 1,
			"object_hash": object.Hash, "object_size": object.Size,
			"quality":      map[string]any{"profile": "test"},
			"render_width": 360, "render_height": 640, "render_fps": 30,
			"expected_duration_sec": 2,
		},
	}}, reducer.Options{Actor: contracts.ActorJob, CreatedAt: time.Now().UTC()})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("preview status=%s err=%v", result.Status, err)
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, apiRequest(t, http.MethodGet,
		"/api/drafts/draft_timeline_api/timeline", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"timeline_version":1`) ||
		!strings.Contains(response.Body.String(), `"preview_id":"preview_timeline_api"`) ||
		!strings.Contains(response.Body.String(), `"visual_base"`) {
		t.Fatalf("timeline status=%d body=%s", response.Code, response.Body.String())
	}
	patched := httptest.NewRecorder()
	handler.ServeHTTP(patched, apiRequest(t, http.MethodPost,
		"/api/drafts/draft_timeline_api/timeline/patch", map[string]any{"op": map[string]any{
			"kind": "batch", "ops": []any{
				map[string]any{"kind": "split_clip", "timeline_clip_id": "clip_v1_001", "split_frame": 30},
				map[string]any{"kind": "adjust_gain", "timeline_clip_id": "clip_v1_001", "gain_db": -6},
			},
		}}))
	if patched.Code != http.StatusOK || !strings.Contains(patched.Body.String(), `"timeline_version":2`) ||
		!strings.Contains(patched.Body.String(), `"clip_v1_001_split_30"`) {
		t.Fatalf("patch status=%d body=%s", patched.Code, patched.Body.String())
	}
	var renderJobs int
	if err := server.database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM jobs WHERE draft_id='draft_timeline_api' AND kind='render_preview'`,
	).Scan(&renderJobs); err != nil || renderJobs != 0 {
		t.Fatalf("render jobs=%d err=%v", renderJobs, err)
	}
	var currentVersion, timelineRows int
	if err := server.database.Read().QueryRowContext(t.Context(), `
		SELECT timeline_current_version FROM drafts WHERE draft_id='draft_timeline_api'`,
	).Scan(&currentVersion); err != nil || currentVersion != 2 {
		t.Fatalf("current version=%d err=%v", currentVersion, err)
	}
	if err := server.database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM timeline_versions WHERE draft_id='draft_timeline_api'`,
	).Scan(&timelineRows); err != nil || timelineRows != 1 {
		t.Fatalf("timeline rows=%d err=%v", timelineRows, err)
	}
	var manualBatches, operationCount int
	var actor, origin string
	if err := server.database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*), actor, origin, json_array_length(operations_json)
		FROM timeline_edit_batches
		WHERE draft_id='draft_timeline_api' AND origin='manual'`,
	).Scan(&manualBatches, &actor, &origin, &operationCount); err != nil {
		t.Fatal(err)
	}
	if manualBatches != 1 || actor != string(contracts.ActorUser) || origin != "manual" || operationCount != 2 {
		t.Fatalf("manual batches=%d actor=%s origin=%s ops=%d", manualBatches, actor, origin, operationCount)
	}
	removedRestore := httptest.NewRecorder()
	handler.ServeHTTP(removedRestore, apiRequest(t, http.MethodPost,
		"/api/drafts/draft_timeline_api/timeline/restore", map[string]any{"version": 1}))
	if removedRestore.Code != http.StatusNotFound {
		t.Fatalf("removed restore status=%d body=%s", removedRestore.Code, removedRestore.Body.String())
	}
	viewed := httptest.NewRecorder()
	handler.ServeHTTP(viewed, apiRequest(t, http.MethodPost,
		"/api/drafts/draft_timeline_api/previews/preview_timeline_api/viewed", nil))
	if viewed.Code != http.StatusOK || !strings.Contains(viewed.Body.String(), `"last_viewed_preview_id":"preview_timeline_api"`) {
		t.Fatalf("viewed status=%d body=%s", viewed.Code, viewed.Body.String())
	}
}
