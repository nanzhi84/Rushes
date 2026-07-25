package agent

import (
	"context"
	"testing"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

// setupEvidenceCoordsDraft 建一个草稿：单条 A-roll 素材 + 持久化逐句索引 + 一条
// 测试夹具的单段 [0,300] 时间线再叠加 cuts，模拟「首剪把 A-roll 多次裁剪/拆分」
// 后的状态。返回 service、带 draft 的 ctx 与裁剪后的时间线文档。
func setupEvidenceCoordsDraft(
	t *testing.T,
	draftID, assetID string,
	utterances, pauses []map[string]any,
	cuts []map[string]any,
) (*Service, context.Context, timeline.Document) {
	t.Helper()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, draftID)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO assets(
			asset_id,storage_mode,reference_path,kind,source,filename,hash,size,
			probe_json,ingest_status,understanding_status,usable
		) VALUES(?, 'reference', ?, 'video', 'local_path', ?, ?, 1,
			'{"duration_sec":12,"has_audio":true}','ready','ready',1);`,
		assetID, "/tmp/"+assetID+".mp4", assetID+".mp4", assetID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO draft_asset_links(draft_id,asset_id,rel_dir,linked_at)
		VALUES(?, ?, 'Aroll', ?);`,
		draftID, assetID, now,
	); err != nil {
		t.Fatal(err)
	}
	result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: contracts.ActorAgent,
		ResultRows: reducer.ResultRows{Transcripts: []reducer.TranscriptRow{{
			ID: "transcript_" + assetID, AssetID: assetID, ProviderID: "sidecar-srt",
			Utterances: utterances, VADSegments: pauses,
		}}},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("transcript status=%s err=%v", result.Status, err)
	}
	document, err := agenttest.ComposeTimeline(draftID, 1, []agenttest.TimelineSelection{{
		AssetID: assetID, AssetKind: "video", SourceStartFrame: 0, SourceEndFrame: 300,
		Role: "a_roll", HasAudio: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, cut := range cuts {
		document, err = timeline.ApplyPatch(document, cut)
		if err != nil {
			t.Fatalf("apply cut %#v: %v", cut, err)
		}
	}
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	if persisted, persistErr := seedTimelineVersion(service,
		t.Context(), draftID, document, "fixture", nil); persistErr != nil || persisted.Status != "succeeded" {
		t.Fatalf("persist=%#v err=%v", persisted, persistErr)
	}
	return service, rushestools.WithDraftID(t.Context(), draftID), document
}

// clipBySourceRange 在主视频轨上按已裁剪源区间定位 clip，避免依赖裁剪后的 ID 命名。
func clipBySourceRange(t *testing.T, document timeline.Document, start, end int) string {
	t.Helper()
	for _, clip := range timelineTrackClips(document, "visual_base") {
		if clip.SourceStartFrame == start && clip.SourceEndFrame == end {
			return clip.TimelineClipID
		}
	}
	t.Fatalf("未找到源区间 [%d,%d] 的主视频 clip: %#v", start, end, timelineTrackClips(document, "visual_base"))
	return ""
}

func inspectClip(
	t *testing.T, service *Service, ctx context.Context, clipID string,
) rushestools.SpeechSearchResult {
	t.Helper()
	raw, err := service.ExecuteTool(ctx, "speech.search", rushestools.SpeechSearchInput{
		TimelineClipID: clipID, IncludeWords: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw.(rushestools.SpeechSearchResult)
}

func TestTalkingHeadEvidenceCoordsRepairsStraddlingEvidence(t *testing.T) {
	t.Parallel()
	const clipEnd = 130
	service, ctx, document := setupEvidenceCoordsDraft(t, "draft_ec_pause", "asset_ec_pause",
		[]map[string]any{
			{"utterance_id": "utt_keep", "source_start_frame": 0, "source_end_frame": 120,
				"text": "开头这一段口播内容需要完整保留下来继续讲。"},
		},
		[]map[string]any{
			{"pause_id": "pause_tail", "source_start_frame": 120, "source_end_frame": 148,
				"delete_start_frame": 122, "delete_end_frame": 145},
		},
		[]map[string]any{
			{"kind": "delete_range", "start_frame": clipEnd, "end_frame": clipEnd + 30},
			{"kind": "delete_range", "start_frame": 250, "end_frame": 270},
		},
	)
	clipID := clipBySourceRange(t, document, 0, clipEnd)
	inspect := inspectClip(t, service, ctx, clipID)
	var clampedPause *rushestools.SpeechPauseEvidence
	for index := range inspect.Pauses {
		if inspect.Pauses[index].PauseID == "pause_tail" {
			clampedPause = &inspect.Pauses[index]
		}
	}
	if clampedPause == nil || !clampedPause.Clamped || clampedPause.DeleteEndFrame != clipEnd {
		t.Fatalf("跨界气口未按 clip 裁剪并标注 clamped: %#v", inspect.Pauses)
	}
}
