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

// TestSpeechQualityReportSurfacesInValidateAndEdit 验证报告进两处：timeline.check
// 的 Data 与 edit_talking_head 成功 observation。
func TestSpeechQualityReportSurfacesInValidateAndEdit(t *testing.T) {
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
	document, err := timeline.ComposeInitial("draft_q6_surface", 1, []timeline.Selection{{
		AssetID: "asset_q6s", AssetKind: "video", SourceStartFrame: 0, SourceEndFrame: 300,
		Role: "a_roll", HasAudio: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if persisted, persistErr := service.executor.PersistTimeline(t.Context(), "draft_q6_surface", document, "fixture"); persistErr != nil || persisted.Status != "succeeded" {
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

	editRaw, err := service.ExecuteTool(ctx, "timeline.edit_talking_head", rushestools.TalkingHeadEditInput{
		ARollTimelineClipID: "clip_v1_001",
		RemoveWordRanges:    []rushestools.TalkingHeadWordRange{{StartWordID: "w_filler", EndWordID: "w_filler"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	edit := editRaw.(rushestools.ToolResult)
	if edit.Status != "succeeded" {
		t.Fatalf("edit=%#v", edit)
	}
	if _, ok := edit.Data["speech_quality"].(map[string]any); !ok || !strings.Contains(edit.Observation, "口播质检") {
		t.Fatalf("edit quality missing=%#v", edit)
	}
	if _, drifted := edit.Data["plan_drift"]; drifted {
		t.Fatalf("非确认重放不应出现 plan_drift: %#v", edit.Data)
	}
}

// TestTalkingHeadIslandOnMisspeakEvidenceRejectedWithCounterProposal 覆盖 Q2 语义
// 检查：即便孤岛时长 >=2 秒，只要它被口误证据过半覆盖，也被判为孤岛并给出并入
// 相邻删除的 counter-proposal；仅少量重叠的完整长句不受影响。
func TestTalkingHeadIslandOnMisspeakEvidenceRejectedWithCounterProposal(t *testing.T) {
	t.Parallel()
	utterance := agentexec.SpeechUtterance{
		ID: "utt", StartFrame: 0, EndFrame: 240, Text: "前面口误后面。",
		Words: []agentexec.SpeechWord{
			{ID: "w_lead", StartFrame: 0, EndFrame: 100, Text: "前面"},
			{ID: "w_bad", StartFrame: 100, EndFrame: 180, Text: "口误"},
			{ID: "w_tail", StartFrame: 180, EndFrame: 240, Text: "后面", Punctuation: "。"},
		},
	}
	deletions := []agentexec.TalkingHeadRange{{Start: 0, End: 100}, {Start: 180, End: 240}}
	island := []agentexec.TalkingHeadRange{{Start: 100, End: 180}}
	evidence := []agentexec.TalkingHeadEvidenceRange{{ID: "frag_bad", Start: 100, End: 180}}
	orphans := agentexec.TalkingHeadOrphanSpeechFragments(
		deletions, island, []agentexec.SpeechUtterance{utterance},
		map[string]struct{}{}, map[string]struct{}{}, nil,
		0, 240, agentexec.MinTalkingHeadRetainedIslandFrames, evidence,
	)
	if len(orphans) != 1 || orphans[0]["reason"] != "lands_on_misspeak_evidence" ||
		orphans[0]["duration_frames"] != 80 {
		t.Fatalf("orphans=%#v", orphans)
	}
	if matched, _ := orphans[0]["matched_evidence_ids"].([]string); len(matched) != 1 || matched[0] != "frag_bad" {
		t.Fatalf("matched=%#v", orphans[0]["matched_evidence_ids"])
	}
	proposals := agentexec.TalkingHeadIslandCounterProposals(orphans, []agentexec.SpeechUtterance{utterance})
	if len(proposals) != 1 || proposals[0]["merged_delete_source_start_frame"] != 0 ||
		proposals[0]["merged_delete_source_end_frame"] != 240 ||
		proposals[0]["island_start_word_id"] != "w_bad" || proposals[0]["island_end_word_id"] != "w_bad" ||
		proposals[0]["island_text"] != "口误" || proposals[0]["reason"] != "lands_on_misspeak_evidence" {
		t.Fatalf("proposals=%#v", proposals)
	}
	minority := []agentexec.TalkingHeadEvidenceRange{{ID: "frag_minor", Start: 100, End: 120}}
	clean := agentexec.TalkingHeadOrphanSpeechFragments(
		deletions, island, []agentexec.SpeechUtterance{utterance},
		map[string]struct{}{}, map[string]struct{}{}, nil,
		0, 240, agentexec.MinTalkingHeadRetainedIslandFrames, minority,
	)
	if len(clean) != 0 {
		t.Fatalf("仅少量重叠的 >=2 秒保留段不应被判为孤岛: %#v", clean)
	}
}

// TestTalkingHeadEditNarrowingIntoIslandReturnsCounterProposal 复现锚点行为：删除
// 在保留台词中夹出不足 2 秒的孤岛时，工具失败并给出 counter-proposal；采纳后编辑成功。
func TestTalkingHeadEditNarrowingIntoIslandReturnsCounterProposal(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_q2_island")
	utterances := []map[string]any{{
		"utterance_id": "utt", "source_start_frame": 0, "source_end_frame": 360, "text": "开场很长内容一呃这个很长内容二结尾。",
		"words": []map[string]any{
			{"word_id": "w_head", "source_start_frame": 0, "source_end_frame": 80, "text": "开场"},
			{"word_id": "w_a", "source_start_frame": 80, "source_end_frame": 200, "text": "很长内容一"},
			{"word_id": "w_garbage", "source_start_frame": 200, "source_end_frame": 230, "text": "呃这个"},
			{"word_id": "w_b", "source_start_frame": 230, "source_end_frame": 350, "text": "很长内容二"},
			{"word_id": "w_tail", "source_start_frame": 350, "source_end_frame": 360, "text": "结尾", "punctuation": "。"},
		},
	}}
	seedTalkingHeadQualityAsset(t, database, "draft_q2_island", "asset_q2i", 12, utterances, nil)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	document, err := timeline.ComposeInitial("draft_q2_island", 1, []timeline.Selection{{
		AssetID: "asset_q2i", AssetKind: "video", SourceStartFrame: 0, SourceEndFrame: 360,
		Role: "a_roll", HasAudio: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if persisted, persistErr := service.executor.PersistTimeline(t.Context(), "draft_q2_island", document, "fixture"); persistErr != nil || persisted.Status != "succeeded" {
		t.Fatalf("persisted=%#v err=%v", persisted, persistErr)
	}
	ctx := rushestools.WithDraftID(t.Context(), "draft_q2_island")

	strandRaw, err := service.ExecuteTool(ctx, "timeline.edit_talking_head", rushestools.TalkingHeadEditInput{
		ARollTimelineClipID: "clip_v1_001",
		RemoveWordRanges: []rushestools.TalkingHeadWordRange{
			{StartWordID: "w_a", EndWordID: "w_a"}, {StartWordID: "w_b", EndWordID: "w_b"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	strand := strandRaw.(rushestools.ToolResult)
	if strand.Status != "failed" || !strings.Contains(strand.Observation, "不足 2 秒") {
		t.Fatalf("strand=%#v", strand)
	}
	proposals, ok := strand.Data["island_counter_proposals"].([]map[string]any)
	if !ok || len(proposals) != 1 || proposals[0]["island_text"] != "呃这个" ||
		proposals[0]["merged_delete_source_start_frame"] != 80 ||
		proposals[0]["merged_delete_source_end_frame"] != 350 ||
		proposals[0]["island_start_word_id"] != "w_garbage" ||
		proposals[0]["island_end_word_id"] != "w_garbage" {
		t.Fatalf("counter-proposals=%#v", strand.Data["island_counter_proposals"])
	}

	adoptRaw, err := service.ExecuteTool(ctx, "timeline.edit_talking_head", rushestools.TalkingHeadEditInput{
		ARollTimelineClipID: "clip_v1_001",
		RemoveWordRanges: []rushestools.TalkingHeadWordRange{
			{StartWordID: "w_a", EndWordID: "w_a"},
			{StartWordID: "w_garbage", EndWordID: "w_garbage"},
			{StartWordID: "w_b", EndWordID: "w_b"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if adopt := adoptRaw.(rushestools.ToolResult); adopt.Status != "succeeded" {
		t.Fatalf("采纳 counter-proposal 后应成功: %#v", adopt)
	}
}

// TestTalkingHeadPlanDriftOnlyWithApprovedReplay 锁定 plan_drift 的门槛与文案：只有在
// 决策卡批准后的重放里、且实际保留了气口时才产出，并明示保留了几处。
func TestTalkingHeadPlanDriftOnlyWithApprovedReplay(t *testing.T) {
	t.Parallel()
	utterances := []agentexec.SpeechUtterance{
		{ID: "u1", StartFrame: 0, EndFrame: 60, Text: "前一句。"},
		{ID: "u2", StartFrame: 80, EndFrame: 140, Text: "后一句。"},
	}
	preserved := []agentexec.SpeechPause{{ID: "pause_x", StartFrame: 60, EndFrame: 80, DeleteStart: 62, DeleteEnd: 78}}
	if agentexec.TalkingHeadPlanDrift(t.Context(), preserved, utterances) != nil {
		t.Fatal("非确认重放不应产出 plan_drift")
	}
	replayCtx := agentexec.WithConfirmedToolReplay(t.Context())
	if agentexec.TalkingHeadPlanDrift(replayCtx, nil, utterances) != nil {
		t.Fatal("无漂移不应产出 plan_drift")
	}
	drift := agentexec.TalkingHeadPlanDrift(replayCtx, preserved, utterances)
	if drift == nil || drift["retained_pause_count"] != 1 || drift["approved_plan"] != true ||
		drift["summary"] != "与你批准的删除方案相比，为避免制造不足 2 秒的保留孤岛，本次实际保留了 1 处气口未删；请在回复中如实向用户说明这一偏差。" {
		t.Fatalf("drift=%#v", drift)
	}
	retained := drift["retained_pauses"].([]map[string]any)
	if retained[0]["pause_id"] != "pause_x" || retained[0]["previous_text"] != "前一句。" ||
		retained[0]["next_text"] != "后一句。" {
		t.Fatalf("retained=%#v", retained)
	}
}

// TestSpeechQualityReportEdgeCasesAndHelpers 覆盖短孤岛、已被 overlay 遮盖的接缝、
// 无 a_roll 转写等分支，以及口误证据汇总与纯函数边界。
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

func TestTalkingHeadMisspeakEvidenceClipsToSelectedRange(t *testing.T) {
	t.Parallel()
	repetitions := []rushestools.SpeechRepetitionEvidence{
		{
			RepetitionID: "rep_in", EarlierSourceStartFrame: 10, EarlierSourceEndFrame: 30,
			LaterSourceStartFrame: 40, LaterSourceEndFrame: 60,
		},
		{
			RepetitionID: "rep_out", EarlierSourceStartFrame: 200, EarlierSourceEndFrame: 220,
			LaterSourceStartFrame: 240, LaterSourceEndFrame: 260,
		},
	}
	fragments := []rushestools.SpeechFragmentEvidence{
		{FragmentID: "frag_in", SourceStartFrame: 70, SourceEndFrame: 90},
		{FragmentID: "frag_out", SourceStartFrame: 300, SourceEndFrame: 320},
	}
	evidence := agentexec.TalkingHeadMisspeakEvidence(
		repetitions, fragments, timeline.Clip{SourceStartFrame: 0, SourceEndFrame: 100},
	)
	got := map[string]agentexec.TalkingHeadRange{}
	for _, item := range evidence {
		got[item.ID] = agentexec.TalkingHeadRange{Start: item.Start, End: item.End}
	}
	if len(evidence) != 3 || got["rep_in:earlier"] != (agentexec.TalkingHeadRange{Start: 10, End: 30}) ||
		got["rep_in:later"] != (agentexec.TalkingHeadRange{Start: 40, End: 60}) ||
		got["frag_in"] != (agentexec.TalkingHeadRange{Start: 70, End: 90}) {
		t.Fatalf("evidence=%#v", evidence)
	}
	if agentexec.TalkingHeadIslandMisspeakMatches(agentexec.TalkingHeadRange{Start: 50, End: 50}, evidence) != nil {
		t.Fatal("零长孤岛不应匹配任何证据")
	}
}

// TestValidateTimelineSoftSkipsBrokenQualityReport 锁定 P2-1：validate 是只读诊断，
// 口播质检读取失败（此处注入 source_end<=source_start 的损坏 utterance，令
// speechQualityReport 直接报错）时应跳过附加 speech_quality，让结构合法的时间线仍
// 成功返回，而不是把诊断读取失败伪装成校验失败去诱导模型重试。
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

	document, err := timeline.ComposeInitial("draft_validate_softskip", 1, []timeline.Selection{
		{AssetID: "asset_broken_tx", AssetKind: "video", SourceStartFrame: 0, SourceEndFrame: 60, Role: "a_roll"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 前置断言：损坏 transcript 确实让质检报告器直接报错，否则这条软跳过测试没有意义。
	if _, qualityErr := service.executor.SpeechQualityReport(t.Context(), document); qualityErr == nil {
		t.Fatal("期望损坏 transcript 让 speechQualityReport 报错，但返回了 nil")
	}
	if _, err := service.executor.PersistTimeline(t.Context(), "draft_validate_softskip", document, "softskip_fixture"); err != nil {
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

// seedTalkingHeadAutoPreserveScenario 复用 auto-preserve 形态：删两个气口时
// pause_after_year 会把保留台词夹成不足 2 秒的孤岛，被防护保守撤回删除，从而在
// 确认后重放里产生 plan_drift。为每个 draft 用独立 asset，便于并行与多 draft 断言。
func seedTalkingHeadAutoPreserveScenario(
	t *testing.T, database *storage.DB, service *Service, draftID, assetID string,
) {
	t.Helper()
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO assets(
			asset_id,storage_mode,reference_path,kind,source,filename,hash,size,
			probe_json,ingest_status,understanding_status,usable
		) VALUES(?, 'reference', ?, 'video', 'local_path', ?, ?, 1,
			'{"duration_sec":4,"has_audio":true}', 'ready', 'ready', 1);`,
		assetID, "/tmp/"+assetID+".mp4", assetID+".mp4", assetID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write().ExecContext(t.Context(),
		`INSERT INTO draft_asset_links(draft_id,asset_id,rel_dir,linked_at) VALUES(?, ?, 'Aroll', ?);`,
		draftID, assetID, time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatal(err)
	}
	transcriptResult, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: contracts.ActorAgent,
		ResultRows: reducer.ResultRows{Transcripts: []reducer.TranscriptRow{{
			ID: "transcript_" + assetID, AssetID: assetID, ProviderID: "fixture-word-timestamps",
			Utterances: []map[string]any{{
				"utterance_id": "utt_year", "source_start_frame": 70,
				"source_end_frame": 104, "text": "是2015年。",
				"words": []map[string]any{
					{"word_id": "w_is", "source_start_frame": 70, "source_end_frame": 78, "text": "是"},
					{"word_id": "w_year", "source_start_frame": 91, "source_end_frame": 104, "text": "2015年", "punctuation": "。"},
				},
			}},
			VADSegments: []map[string]any{
				{"pause_id": "pause_before_year", "source_start_frame": 78, "source_end_frame": 91, "delete_start_frame": 78, "delete_end_frame": 91},
				{"pause_id": "pause_after_year", "source_start_frame": 104, "source_end_frame": 116, "delete_start_frame": 104, "delete_end_frame": 116},
			},
		}}},
	})
	if err != nil || transcriptResult.Status != reducer.StatusApplied {
		t.Fatalf("transcript status=%s err=%v", transcriptResult.Status, err)
	}
	document, err := timeline.ComposeInitial(draftID, 1, []timeline.Selection{{
		AssetID: assetID, AssetKind: "video",
		SourceStartFrame: 0, SourceEndFrame: 120, Role: "a_roll", HasAudio: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if persisted, persistErr := service.executor.PersistTimeline(t.Context(), draftID, document, "plan_drift_fixture"); persistErr != nil || persisted.Status != "succeeded" {
		t.Fatalf("persisted=%#v err=%v", persisted, persistErr)
	}
}

// TestReplayPendingEditTalkingHeadEmitsPlanDrift 锁定 P2-2 生产链路：
// replayPendingTool → executeReported → ExecuteTool → toolEditTalkingHead 的确认重放
// ctx 透传。summary 与 Data["plan_drift"] 在 toolEditTalkingHead 的同一 if drift != nil
// 分支一同写入，而 replayPendingTool 只回传 Observation，故 summary 出现在返回观察里即为
// 「Data["plan_drift"] 非空且确认重放 ctx 一路透传到 talkingHeadPlanDrift」的端到端证据。
func TestReplayPendingEditTalkingHeadEmitsPlanDrift(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)

	const draftID = "draft_plan_drift_replay"
	agenttest.CreateAgentDraft(t, database, draftID)
	seedTalkingHeadAutoPreserveScenario(t, database, service, draftID, "asset_plan_drift_replay")

	ctx := rushestools.WithDraftID(t.Context(), draftID)
	observation, err := service.replayPendingTool(ctx, QueueItem{
		DraftID: draftID,
		Payload: map[string]any{
			"pending_tool_call": map[string]any{
				"tool_name": "timeline.edit_talking_head",
				"arguments": map[string]any{
					"a_roll_timeline_clip_id": "clip_v1_001",
					"remove_pause_ids":        []any{"pause_before_year", "pause_after_year"},
				},
			},
			"answer": map[string]any{"option_id": "confirm"},
		},
	})
	if err != nil {
		t.Fatalf("replayPendingTool err=%v", err)
	}
	if !strings.Contains(observation, "为避免制造不足 2 秒的保留孤岛") ||
		!strings.Contains(observation, "如实向用户说明这一偏差") {
		t.Fatalf("确认重放未透传 plan_drift 偏差告知：%q", observation)
	}
}

// TestConfirmedToolReplayCtxGatesEditTalkingHeadPlanDrift 直接锁定 Data["plan_drift"]
// 的结构化形态与 ctx 门控：仅确认重放 ctx 才结构化产出 plan_drift；同样触发 auto-preserve
// 的普通调用不得输出，避免把工具的保守保留误当成用户批准方案。
func TestConfirmedToolReplayCtxGatesEditTalkingHeadPlanDrift(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)

	editInput := rushestools.TalkingHeadEditInput{
		ARollTimelineClipID: "clip_v1_001",
		RemovePauseIDs:      []string{"pause_before_year", "pause_after_year"},
	}

	const confirmedDraft = "draft_plan_drift_confirmed"
	agenttest.CreateAgentDraft(t, database, confirmedDraft)
	seedTalkingHeadAutoPreserveScenario(t, database, service, confirmedDraft, "asset_plan_drift_confirmed")
	confirmedCtx := agentexec.WithConfirmedToolReplay(rushestools.WithDraftID(t.Context(), confirmedDraft))
	confirmedRaw, err := service.executeReported(confirmedCtx, confirmedDraft, "timeline.edit_talking_head", editInput)
	if err != nil {
		t.Fatal(err)
	}
	confirmed := confirmedRaw.(rushestools.ToolResult)
	drift, driftOK := confirmed.Data["plan_drift"].(map[string]any)
	if confirmed.Status != "succeeded" || !driftOK || drift == nil {
		t.Fatalf("确认重放应结构化产出 plan_drift：result=%#v", confirmed)
	}
	retained, _ := drift["retained_pauses"].([]map[string]any)
	if drift["retained_pause_count"] != 1 || len(retained) != 1 || retained[0]["pause_id"] != "pause_after_year" {
		t.Fatalf("plan_drift 形态异常：%#v", drift)
	}

	const normalDraft = "draft_plan_drift_normal"
	agenttest.CreateAgentDraft(t, database, normalDraft)
	seedTalkingHeadAutoPreserveScenario(t, database, service, normalDraft, "asset_plan_drift_normal")
	normalRaw, err := service.executeReported(rushestools.WithDraftID(t.Context(), normalDraft), normalDraft, "timeline.edit_talking_head", editInput)
	if err != nil {
		t.Fatal(err)
	}
	normal := normalRaw.(rushestools.ToolResult)
	if _, present := normal.Data["plan_drift"]; present {
		t.Fatalf("非确认重放不应输出 plan_drift：%#v", normal.Data["plan_drift"])
	}
	// 前置校验这条非确认路径确实触发了 auto-preserve，否则负向断言没有意义。
	if normal.Data["auto_preserved_pause_count"] != 1 {
		t.Fatalf("非确认路径未触发 auto-preserve，负向断言失效：%#v", normal.Data)
	}
}
