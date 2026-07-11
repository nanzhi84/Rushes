package api

import (
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
	object, err := store.PutBytes(t.Context(), []byte("preview"))
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
		"/api/drafts/draft_timeline_api/timeline?version=1", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"timeline_version":1`) ||
		!strings.Contains(response.Body.String(), `"preview_id":"preview_timeline_api"`) ||
		!strings.Contains(response.Body.String(), `"visual_base"`) {
		t.Fatalf("timeline status=%d body=%s", response.Code, response.Body.String())
	}
	viewed := httptest.NewRecorder()
	handler.ServeHTTP(viewed, apiRequest(t, http.MethodPost,
		"/api/drafts/draft_timeline_api/previews/preview_timeline_api/viewed", nil))
	if viewed.Code != http.StatusOK || !strings.Contains(viewed.Body.String(), `"last_viewed_preview_id":"preview_timeline_api"`) {
		t.Fatalf("viewed status=%d body=%s", viewed.Code, viewed.Body.String())
	}
}
