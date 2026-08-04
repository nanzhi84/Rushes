package agent

import (
	"strings"
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/tools"
)

func TestHumanizeFinalReplyReferencesUsesSemanticNamesAndClickableClip(t *testing.T) {
	const (
		draftID = "draft_human_reply"
		assetID = "asset_opaque_reply"
		shotID  = "shot_stable_reply"
		clipID  = "clip_v1_001"
	)
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, draftID)
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{
		{Type: "AssetImported", Payload: map[string]any{
			"asset_id": assetID, "job_id": "job_human_reply", "storage_mode": "reference",
			"reference_path": "/tmp/sunset.mov", "kind": "video", "source": "local_path",
			"filename": "海边混剪-05.mov", "hash": "human-reply-content", "size": 1,
			"ingest_status": "ready",
		}},
		{Type: "AssetLinked", DraftID: draftID, Payload: map[string]any{"asset_id": assetID}},
	}, reducer.Options{Actor: contracts.ActorUser})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("assets status=%s err=%v", result.Status, err)
	}
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO shot_index_snapshots(
			index_snapshot_id,asset_content_hash,generation,analyzer_version,
			output_schema_version,source_asset_id,status,summary_json,created_at,published_at
		) VALUES('snapshot_human_reply','human-reply-content',1,'test-v6',2,?,'ready','{}',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`, assetID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO shots(
			index_snapshot_id,shot_id,asset_content_hash,source_start_frame,source_end_frame,
			boundary_version,boundary_kind,representative_frames_json,description,tags_json,
			subjects_json,actions_json,setting_json,shot_scale,composition,lighting_json,
			mood_json,edit_hints_json,quality_json,search_text,search_tokens_json,
			deep_coverage_json,created_at,semantic_name
		) VALUES(
			'snapshot_human_reply',?,'human-reply-content',0,90,1,'analysis_window','[]',
			'夕阳下人物站在海边','[]','[]','[]','[]','全景','居中','[]','[]','[]','{}',
			'海边日落人物','["海边日落人物"]','[]',CURRENT_TIMESTAMP,'海边日落人物'
		)`, shotID); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	document, err := agenttest.ComposeTimeline(draftID, 1, []agenttest.TimelineSelection{{
		AssetID: assetID, AssetKind: "video", SourceStartFrame: 0, SourceEndFrame: 60,
		Role: "a_roll",
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := tools.WithTimelineMutationOrigin(t.Context(), "manual")
	if persisted, persistErr := seedTimelineVersion(
		service, ctx, draftID, document, "human_reply_fixture", nil,
	); persistErr != nil || persisted.Status != "succeeded" {
		t.Fatalf("timeline=%#v err=%v", persisted, persistErr)
	}

	got := service.humanizeFinalReplyReferences(t.Context(), draftID,
		"已把 "+assetID+" 的 "+shotID+" 放到 "+clipID+"。")
	for _, opaque := range []string{assetID, shotID} {
		if strings.Contains(got, opaque) {
			t.Fatalf("reply leaked %s: %s", opaque, got)
		}
	}
	for _, visible := range []string{
		"《海边混剪-05.mov》", "「海边日落人物」",
		"[「海边日落人物」0.00–2.00 秒](#timeline-clip=clip_v1_001)",
	} {
		if !strings.Contains(got, visible) {
			t.Fatalf("reply missing %q: %s", visible, got)
		}
	}
}
