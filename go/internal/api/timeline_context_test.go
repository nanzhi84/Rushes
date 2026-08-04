package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/tools"
)

func TestAnnotateTimelineClipLabelsSkipsIncompleteContext(t *testing.T) {
	document := map[string]any{"tracks": []any{map[string]any{"clips": []any{
		map[string]any{"timeline_clip_id": "no_asset"},
		map[string]any{
			"timeline_clip_id": "bad_range", "asset_id": "asset_named",
			"source_start_frame": "invalid", "source_end_frame": float64(20),
		},
		map[string]any{
			"timeline_clip_id": "no_shot", "asset_id": "asset_named",
			"source_start_frame": 10, "source_end_frame": float64(20),
		},
	}}}}
	annotateTimelineClipLabels(document, timelineLabelLookup{
		filenameByAsset: map[string]string{"asset_named": "海滩.mov"},
		shotsByAsset:    map[string][]timelineShotLabel{},
	})
	clips := document["tracks"].([]any)[0].(map[string]any)["clips"].([]any)
	if _, exists := clips[0].(map[string]any)["asset_filename"]; exists {
		t.Fatal("clip without asset must not be annotated")
	}
	for _, index := range []int{1, 2} {
		clip := clips[index].(map[string]any)
		if clip["asset_filename"] != "海滩.mov" {
			t.Fatalf("clip=%#v", clip)
		}
		if _, exists := clip["shot_id"]; exists {
			t.Fatalf("incomplete or unmatched source range must not gain a shot: %#v", clip)
		}
	}
	if value, ok := intFromJSONNumber(int(7)); !ok || value != 7 {
		t.Fatalf("value=%d ok=%v", value, ok)
	}
}

func TestTimelineSemanticLabelAndMessageClipContextRoundTrip(t *testing.T) {
	t.Parallel()
	const (
		draftID  = "draft_semantic_context"
		assetID  = "asset_opaque_internal_id"
		clipID   = "clip_v1_001"
		shotID   = "shot_stable_001"
		semantic = "海边日落人物"
	)
	server, handler := testServer(t, t.TempDir(), 0)
	createDraftThroughAPI(t, handler, draftID)
	result, err := reducer.Apply(t.Context(), server.database, []contracts.Event{
		{Type: "AssetImported", Payload: map[string]any{
			"asset_id": assetID, "job_id": "job_semantic_context", "storage_mode": "reference",
			"reference_path": "/tmp/sunset.mov", "kind": "video", "source": "local_path",
			"filename": "海边混剪-05.mov", "hash": "semantic-context-hash", "size": 1,
			"ingest_status": "ready",
		}},
		{Type: "AssetLinked", DraftID: draftID, Payload: map[string]any{"asset_id": assetID}},
	}, reducer.Options{Actor: contracts.ActorUser})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("assets status=%s err=%v", result.Status, err)
	}
	if _, err := server.database.Write().ExecContext(t.Context(), `
		INSERT INTO shot_index_snapshots(
			index_snapshot_id,asset_content_hash,generation,analyzer_version,
			output_schema_version,source_asset_id,status,summary_json,created_at,published_at
		) VALUES('snapshot_semantic','semantic-context-hash',1,'test-v6',2,?,'ready','{}',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`, assetID); err != nil {
		t.Fatal(err)
	}
	if _, err := server.database.Write().ExecContext(t.Context(), `
		INSERT INTO shots(
			index_snapshot_id,shot_id,asset_content_hash,source_start_frame,source_end_frame,
			boundary_version,boundary_kind,representative_frames_json,description,tags_json,
			subjects_json,actions_json,setting_json,shot_scale,composition,lighting_json,
			mood_json,edit_hints_json,quality_json,search_text,search_tokens_json,
			deep_coverage_json,created_at,semantic_name
		) VALUES(
			'snapshot_semantic',?,'semantic-context-hash',0,90,1,'analysis_window','[]',
			'夕阳下人物站在海边','["海边"]','["人物"]','["站立"]','["海滩"]',
			'全景','居中','["日落"]','["平静"]','[]','{"quality":"good"}',
			'海边 日落 人物','["海边","日落","人物"]','[]',CURRENT_TIMESTAMP,?
		)`, shotID, semantic); err != nil {
		t.Fatal(err)
	}
	ctx := tools.WithTimelineMutationOrigin(tools.WithDraftID(t.Context(), draftID), "manual")
	if _, err := server.agent.ExecuteTool(ctx, "timeline.insert", tools.TimelineInsertInput{
		"kind": "insert_clip", "asset_id": assetID, "role": "a_roll",
		"source_start_frame": 0, "source_end_frame": 60,
	}); err != nil {
		t.Fatal(err)
	}

	timelineResponse := httptest.NewRecorder()
	handler.ServeHTTP(timelineResponse, apiRequest(t, http.MethodGet, "/api/drafts/"+draftID+"/timeline", nil))
	if timelineResponse.Code != http.StatusOK ||
		!strings.Contains(timelineResponse.Body.String(), `"semantic_name":"`+semantic+`"`) ||
		!strings.Contains(timelineResponse.Body.String(), `"asset_filename":"海边混剪-05.mov"`) ||
		!strings.Contains(timelineResponse.Body.String(), `"shot_id":"`+shotID+`"`) {
		t.Fatalf("timeline status=%d body=%s", timelineResponse.Code, timelineResponse.Body.String())
	}

	messageResponse := httptest.NewRecorder()
	handler.ServeHTTP(messageResponse, apiRequest(t, http.MethodPost,
		"/api/drafts/"+draftID+"/messages", map[string]any{
			"message_id": "message_with_clip_context", "content": "把我引用的这段放到高潮处",
			"context_refs": []any{map[string]any{
				"kind": "timeline_clip", "timeline_clip_id": clipID,
				"timeline_id": draftID + ":v1", "timeline_version": 1,
			}},
		}))
	if messageResponse.Code != http.StatusAccepted {
		t.Fatalf("message status=%d body=%s", messageResponse.Code, messageResponse.Body.String())
	}
	server.agent.Queue().JoinDraft(draftID)

	history := httptest.NewRecorder()
	handler.ServeHTTP(history, apiRequest(t, http.MethodGet, "/api/drafts/"+draftID+"/messages?limit=200", nil))
	body := history.Body.String()
	for _, expected := range []string{
		`"timeline_clip_id":"` + clipID + `"`, `"semantic_name":"` + semantic + `"`,
		`"asset_filename":"海边混剪-05.mov"`, `"shot_id":"` + shotID + `"`,
		`"timeline_start_frame":0`, `"timeline_end_frame":60`,
	} {
		if history.Code != http.StatusOK || !strings.Contains(body, expected) {
			t.Fatalf("history missing %s: status=%d body=%s", expected, history.Code, body)
		}
	}

	stale := httptest.NewRecorder()
	handler.ServeHTTP(stale, apiRequest(t, http.MethodPost,
		"/api/drafts/"+draftID+"/messages", map[string]any{
			"content": "过期引用", "context_refs": []any{map[string]any{
				"kind": "timeline_clip", "timeline_clip_id": clipID,
				"timeline_id": draftID + ":v0", "timeline_version": 1,
			}},
		}))
	if stale.Code != http.StatusConflict || !strings.Contains(stale.Body.String(), "message_context_stale") {
		t.Fatalf("stale status=%d body=%s", stale.Code, stale.Body.String())
	}
}
