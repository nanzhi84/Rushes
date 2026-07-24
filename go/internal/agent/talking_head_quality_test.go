package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func seedTalkingHeadQualityAsset(
	t *testing.T,
	database *storage.DB,
	draftID, assetID string,
	durationSec int,
	utterances, pauses []map[string]any,
) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO assets(
			asset_id,storage_mode,reference_path,kind,source,filename,hash,size,
			probe_json,ingest_status,understanding_status,usable
		) VALUES(?, 'reference', ?, 'video', 'local_path', ?, ?, 1, ?, 'ready', 'ready', 1);`,
		assetID, "/tmp/"+assetID+".mp4", assetID+".mp4", assetID,
		fmtJSON(map[string]any{"duration_sec": durationSec, "has_audio": true}),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write().ExecContext(t.Context(),
		`INSERT INTO draft_asset_links(draft_id,asset_id,rel_dir,linked_at) VALUES(?, ?, 'Aroll', ?);`,
		draftID, assetID, now,
	); err != nil {
		t.Fatal(err)
	}
	result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: contracts.ActorAgent,
		ResultRows: reducer.ResultRows{Transcripts: []reducer.TranscriptRow{{
			ID: "transcript_" + assetID, AssetID: assetID, ProviderID: "fixture-word-timestamps",
			Utterances: utterances, VADSegments: pauses,
		}}},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("transcript status=%s err=%v", result.Status, err)
	}
}

// TestSpeechQualityReportMatchesAnchorShapedTimeline 用与锚点案例 v6 同形态的合成
// 时间线锁定报告输出：6 处残留气口、1 个未遮盖硬接缝、2 段过短 B-roll、0 个短孤岛。
func TestSpeechQualityReportMatchesAnchorShapedTimeline(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_q6_report")
	utterances := []map[string]any{
		{"utterance_id": "u1", "source_start_frame": 0, "source_end_frame": 150, "text": "第一段。"},
		{"utterance_id": "u2", "source_start_frame": 150, "source_end_frame": 300, "text": "第二段。"},
		{"utterance_id": "u3", "source_start_frame": 400, "source_end_frame": 550, "text": "第三段。"},
		{"utterance_id": "u4", "source_start_frame": 550, "source_end_frame": 700, "text": "第四段。"},
	}
	pause := func(id string, start, end int) map[string]any {
		return map[string]any{
			"pause_id": id, "source_start_frame": start, "source_end_frame": end,
			"delete_start_frame": start + 2, "delete_end_frame": end - 2,
		}
	}
	pauses := []map[string]any{
		pause("p1", 50, 62), pause("p2", 120, 132), pause("p3", 200, 215),
		pause("p4", 450, 462), pause("p5", 520, 535), pause("p6", 640, 650),
	}
	seedTalkingHeadQualityAsset(t, database, "draft_q6_report", "asset_q6", 24, utterances, pauses)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)

	document := timeline.Empty("draft_q6_report", 1)
	document.FPS = 30
	document.DurationFrames = 600
	document.Tracks[0].Clips = []timeline.Clip{
		{TimelineClipID: "a1", TrackID: "visual_base", AssetID: "asset_q6", AssetKind: "video", Role: "a_roll",
			TimelineStartFrame: 0, TimelineEndFrame: 300, SourceStartFrame: 0, SourceEndFrame: 300, PlaybackRate: 1},
		{TimelineClipID: "a2", TrackID: "visual_base", AssetID: "asset_q6", AssetKind: "video", Role: "a_roll",
			TimelineStartFrame: 300, TimelineEndFrame: 600, SourceStartFrame: 400, SourceEndFrame: 700, PlaybackRate: 1},
	}
	document.Tracks[1].Clips = []timeline.Clip{
		{TimelineClipID: "b1", TrackID: "visual_overlay", AssetID: "asset_broll", AssetKind: "video", Role: "b_roll",
			TimelineStartFrame: 50, TimelineEndFrame: 80, SourceStartFrame: 0, SourceEndFrame: 30, PlaybackRate: 1,
			Metadata: map[string]any{"b_roll_filename": "broll_a.mp4"}},
		{TimelineClipID: "b2", TrackID: "visual_overlay", AssetID: "asset_broll", AssetKind: "video", Role: "b_roll",
			TimelineStartFrame: 500, TimelineEndFrame: 530, SourceStartFrame: 0, SourceEndFrame: 30, PlaybackRate: 1,
			Metadata: map[string]any{"b_roll_filename": "broll_b.mp4"}},
	}

	report, err := service.executor.SpeechQualityReport(t.Context(), document)
	if err != nil {
		t.Fatal(err)
	}
	if report["a_roll_present"] != true || report["residual_breath_count"] != 6 ||
		report["short_retained_island_count"] != 0 || report["uncovered_a_roll_seam_count"] != 1 ||
		report["short_b_roll_clip_count"] != 2 {
		t.Fatalf("report counts=%#v", report)
	}
	breaths := report["residual_breaths"].([]map[string]any)
	wantBreathStarts := []int{50, 120, 200, 350, 420, 540}
	for index, breath := range breaths {
		if breath["timeline_start_frame"].(int) != wantBreathStarts[index] {
			t.Fatalf("breath[%d]=%#v want start %d", index, breath, wantBreathStarts[index])
		}
	}
	seams := report["uncovered_a_roll_seams"].([]map[string]any)
	if seams[0]["timeline_frame"].(int) != 300 || seams[0]["previous_text"] != "第二段。" ||
		seams[0]["next_text"] != "第三段。" {
		t.Fatalf("seam=%#v", seams[0])
	}
	shortBroll := report["short_b_roll_clips"].([]map[string]any)
	if shortBroll[0]["duration_frames"].(int) != 30 || shortBroll[0]["b_roll_filename"] != "broll_a.mp4" ||
		shortBroll[1]["duration_frames"].(int) != 30 {
		t.Fatalf("short b-roll=%#v", shortBroll)
	}
}

// TestSpeechQualityReportSurfacesInTimelineCheck 验证客观口播报告进入纯读检查结果。
func TestSpeechQualityReportSurfacesInTimelineCheck(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_q6_surface")
	utterances := []map[string]any{{
		"utterance_id": "utt", "source_start_frame": 0, "source_end_frame": 300, "text": "很长的开头内容呃很长的结尾内容。",
		"words": []map[string]any{
			{"word_id": "w_head", "source_start_frame": 0, "source_end_frame": 100, "text": "很长的开头内容"},
			{"word_id": "w_filler", "source_start_frame": 100, "source_end_frame": 110, "text": "呃"},
			{"word_id": "w_tail", "source_start_frame": 110, "source_end_frame": 300, "text": "很长的结尾内容", "punctuation": "。"},
		},
	}}
	seedTalkingHeadQualityAsset(t, database, "draft_q6_surface", "asset_q6s", 10, utterances, nil)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	document, err := agenttest.ComposeTimeline("draft_q6_surface", 1, []agenttest.TimelineSelection{{
		AssetID: "asset_q6s", AssetKind: "video", SourceStartFrame: 0, SourceEndFrame: 300,
		Role: "a_roll", HasAudio: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if persisted, persistErr := seedTimelineVersion(service, t.Context(), "draft_q6_surface", document, "fixture", nil); persistErr != nil || persisted.Status != "succeeded" {
		t.Fatalf("persisted=%#v err=%v", persisted, persistErr)
	}
	ctx := rushestools.WithDraftID(t.Context(), "draft_q6_surface")

	validateRaw, err := service.ExecuteTool(ctx, "timeline.check", rushestools.TimelineCheckInput{})
	if err != nil {
		t.Fatal(err)
	}
	validate := validateRaw.(rushestools.ToolResult)
	quality, ok := validate.Data["speech_quality"].(map[string]any)
	if !ok || quality["a_roll_present"] != true || !strings.Contains(validate.Observation, "口播质检") {
		t.Fatalf("validate=%#v", validate)
	}
}

func TestSpeechQualityReportEdgeCasesAndHelpers(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_q6_edge")
	utterances := []map[string]any{
		{"utterance_id": "u1", "source_start_frame": 0, "source_end_frame": 300, "text": "长段一。"},
		{"utterance_id": "u2", "source_start_frame": 400, "source_end_frame": 430, "text": "岛。"},
		{"utterance_id": "u3", "source_start_frame": 500, "source_end_frame": 800, "text": "长段三。"},
	}
	seedTalkingHeadQualityAsset(t, database, "draft_q6_edge", "asset_edge", 30, utterances, nil)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)

	document := timeline.Empty("draft_q6_edge", 1)
	document.FPS = 30
	document.DurationFrames = 630
	document.Tracks[0].Clips = []timeline.Clip{
		{TimelineClipID: "c1", TrackID: "visual_base", AssetID: "asset_edge", AssetKind: "video", Role: "a_roll",
			TimelineStartFrame: 0, TimelineEndFrame: 300, SourceStartFrame: 0, SourceEndFrame: 300, PlaybackRate: 1},
		{TimelineClipID: "c2", TrackID: "visual_base", AssetID: "asset_edge", AssetKind: "video", Role: "a_roll",
			TimelineStartFrame: 300, TimelineEndFrame: 330, SourceStartFrame: 400, SourceEndFrame: 430, PlaybackRate: 1},
		{TimelineClipID: "c3", TrackID: "visual_base", AssetID: "asset_edge", AssetKind: "video", Role: "a_roll",
			TimelineStartFrame: 330, TimelineEndFrame: 630, SourceStartFrame: 500, SourceEndFrame: 800, PlaybackRate: 1},
	}
	document.Tracks[1].Clips = []timeline.Clip{
		{TimelineClipID: "cover", TrackID: "visual_overlay", AssetID: "asset_broll", AssetKind: "video", Role: "b_roll",
			TimelineStartFrame: 250, TimelineEndFrame: 310, SourceStartFrame: 0, SourceEndFrame: 60, PlaybackRate: 1,
			Metadata: map[string]any{"b_roll_filename": "cover.mp4"}},
	}
	report, err := service.executor.SpeechQualityReport(t.Context(), document)
	if err != nil {
		t.Fatal(err)
	}
	if report["short_retained_island_count"] != 1 || report["uncovered_a_roll_seam_count"] != 1 ||
		report["short_b_roll_clip_count"] != 0 || report["residual_breath_count"] != 0 {
		t.Fatalf("edge report=%#v", report)
	}
	island := report["short_retained_islands"].([]map[string]any)[0]
	if island["timeline_start_frame"].(int) != 300 || island["timeline_end_frame"].(int) != 330 ||
		island["duration_frames"].(int) != 30 || island["text"] != "岛。" {
		t.Fatalf("island=%#v", island)
	}
	if seam := report["uncovered_a_roll_seams"].([]map[string]any)[0]; seam["timeline_frame"].(int) != 330 {
		t.Fatalf("uncovered seam should be the one at 330: %#v", seam)
	}

	// 主视轨素材没有转写时报告判定不含 a_roll。
	noTranscript := timeline.Empty("draft_q6_edge", 1)
	noTranscript.Tracks[0].Clips = []timeline.Clip{{
		TimelineClipID: "x", TrackID: "visual_base", AssetID: "asset_no_transcript", AssetKind: "video",
		TimelineStartFrame: 0, TimelineEndFrame: 90, SourceStartFrame: 0, SourceEndFrame: 90, PlaybackRate: 1,
	}}
	empty, err := service.executor.SpeechQualityReport(t.Context(), noTranscript)
	if err != nil || empty["a_roll_present"] != false || agentexec.TalkingHeadQualitySummary(empty) != "" {
		t.Fatalf("empty report=%#v err=%v", empty, err)
	}

	if agentexec.FrameSeconds(45, 0) != 1.5 || agentexec.FrameSeconds(9, 30) != 0.3 {
		t.Fatalf("frameSeconds edge wrong: %v %v", agentexec.FrameSeconds(45, 0), agentexec.FrameSeconds(9, 30))
	}
}

func TestValidateTimelineSoftSkipsBrokenQualityReport(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_validate_softskip")
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO assets(asset_id,storage_mode,reference_path,kind,source,filename,hash,size,probe_json,ingest_status,understanding_status,usable)
		VALUES('asset_broken_tx','reference','/tmp/broken.mp4','video','local_path','broken.mp4','hash_broken',1,'{}','ready','done',1);
		INSERT INTO transcripts(transcript_id,asset_id,provider_id,raw_preserved,utterances_json,vad_segments_json)
		VALUES('transcript_broken','asset_broken_tx','fixture',0,
			'[{"utterance_id":"utt_broken","source_start_frame":100,"source_end_frame":50,"text":"坏"}]','[]')
	`); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)

	document, err := agenttest.ComposeTimeline("draft_validate_softskip", 1, []agenttest.TimelineSelection{
		{AssetID: "asset_broken_tx", AssetKind: "video", SourceStartFrame: 0, SourceEndFrame: 60, Role: "a_roll"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 前置断言：损坏 transcript 确实让质检报告器直接报错，否则这条软跳过测试没有意义。
	if _, qualityErr := service.executor.SpeechQualityReport(t.Context(), document); qualityErr == nil {
		t.Fatal("期望损坏 transcript 让 speechQualityReport 报错，但返回了 nil")
	}
	if _, err := seedTimelineVersion(service, t.Context(), "draft_validate_softskip", document, "softskip_fixture", nil); err != nil {
		t.Fatal(err)
	}

	validatedRaw, err := service.ExecuteTool(
		rushestools.WithDraftID(t.Context(), "draft_validate_softskip"), "timeline.check", rushestools.TimelineCheckInput{},
	)
	if err != nil {
		t.Fatalf("validate 应软跳过质检读取失败，却返回错误：%v", err)
	}
	validated, ok := validatedRaw.(rushestools.ToolResult)
	if !ok {
		t.Fatalf("timeline.check 返回类型异常: %T", validatedRaw)
	}
	if validated.Status != "succeeded" {
		t.Fatalf("validate status=%q，期望 succeeded（结构合法的时间线不应因质检读取失败降级）", validated.Status)
	}
	if _, present := validated.Data["speech_quality"]; present {
		t.Fatalf("质检读取失败时不应附加 speech_quality，实际存在：%#v", validated.Data["speech_quality"])
	}
}
