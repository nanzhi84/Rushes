package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func TestTalkingHeadWorkflowUsesPersistentEvidenceAndAtomicSourceCorrectEdits(t *testing.T) {
	t.Parallel()
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_talking_head")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, fixture := range []struct {
		id       string
		filename string
		relDir   string
		duration int
	}{
		{id: "asset_aroll", filename: "第二节课实操-口播.mp4", relDir: "第二节课实操-口播", duration: 10},
		{id: "asset_broll_fingerprint", filename: "键盘指纹解锁特写.mp4", relDir: "Broll/键盘", duration: 4},
	} {
		if _, err := database.Write().ExecContext(t.Context(), `
			INSERT INTO assets(
				asset_id,storage_mode,reference_path,kind,source,filename,hash,size,
				probe_json,ingest_status,understanding_status,usable
			) VALUES(?, 'reference', ?, 'video', 'local_path', ?, ?, 1, ?, 'ready', 'ready', 1);
			INSERT INTO draft_asset_links(draft_id,asset_id,rel_dir,linked_at)
			VALUES('draft_talking_head', ?, ?, ?);`,
			fixture.id, "/tmp/"+fixture.filename, fixture.filename, fixture.id,
			fmtJSON(map[string]any{"duration_sec": fixture.duration, "has_audio": fixture.id == "asset_aroll"}),
			fixture.id, fixture.relDir, now,
		); err != nil {
			t.Fatal(err)
		}
	}
	brollSummary, _ := json.Marshal(map[string]any{
		"asset_id": "asset_broll_fingerprint", "semantic_role": "b_roll",
		"overall": "键盘右上角指纹识别区域的产品特写",
		"segments": []map[string]any{{
			"source_start_frame": 0, "source_end_frame": 40,
			"description": "手指按压键盘右上角的指纹解锁键，产品近景",
			"tags":        []string{"键盘", "指纹", "解锁", "产品特写"}, "quality": "usable",
		}},
	})
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO material_summaries(
			summary_id,asset_id,version,status,summary_json,fingerprint,prompt_version,created_at
		) VALUES('summary_broll','asset_broll_fingerprint',1,'ready',?,'fingerprint','v3',?)`,
		string(brollSummary), now,
	); err != nil {
		t.Fatal(err)
	}
	utterances := []map[string]any{
		{"utterance_id": "utt_intro", "source_start_frame": 0, "source_end_frame": 60, "text": "这台电脑提供橙色外观。"},
		{"utterance_id": "utt_duplicate", "source_start_frame": 60, "source_end_frame": 120, "text": "这台电脑也就是提供橙色外观。"},
		{"utterance_id": "utt_fingerprint", "source_start_frame": 135, "source_end_frame": 210, "text": "指纹解锁按键位于键盘右上角。", "words": []map[string]any{
			{"word_id": "w_fingerprint", "source_start_frame": 135, "source_end_frame": 165, "text": "指纹解锁"},
			{"word_id": "w_key", "source_start_frame": 165, "source_end_frame": 185, "text": "按键"},
			{"word_id": "w_position", "source_start_frame": 185, "source_end_frame": 210, "text": "位于键盘右上角", "punctuation": "。"},
		}},
		{"utterance_id": "utt_touchpad", "source_start_frame": 210, "source_end_frame": 300, "text": "触控板支持震动反馈。", "words": []map[string]any{
			{"word_id": "w_touchpad_short", "source_start_frame": 210, "source_end_frame": 220, "text": "触控板"},
			{"word_id": "w_touchpad_rest", "source_start_frame": 220, "source_end_frame": 300, "text": "支持震动反馈", "punctuation": "。"},
		}},
	}
	pauses := []map[string]any{{
		"pause_id": "pause_breath", "source_start_frame": 120, "source_end_frame": 135,
		"delete_start_frame": 123, "delete_end_frame": 132,
	}}
	result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: contracts.ActorAgent,
		ResultRows: reducer.ResultRows{Transcripts: []reducer.TranscriptRow{{
			ID: "transcript_aroll", AssetID: "asset_aroll", ProviderID: "sidecar-srt",
			Utterances: utterances, VADSegments: pauses,
		}}},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("transcript status=%s err=%v", result.Status, err)
	}
	document, err := timeline.ComposeInitial("draft_talking_head", 1, []timeline.Selection{{
		AssetID: "asset_aroll", AssetKind: "video", SourceStartFrame: 0, SourceEndFrame: 300,
		Role: "a_roll", HasAudio: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	if persisted, persistErr := service.persistTimeline(
		t.Context(), "draft_talking_head", document, "fixture",
	); persistErr != nil || persisted.Status != "succeeded" {
		t.Fatalf("persist=%#v err=%v", persisted, persistErr)
	}
	ctx := rushestools.WithDraftID(t.Context(), "draft_talking_head")

	inspectRaw, err := service.ExecuteTool(ctx, "speech.inspect", rushestools.SpeechInspectInput{
		TimelineClipID: "clip_v1_001", Query: "指纹解锁",
	})
	if err != nil {
		t.Fatal(err)
	}
	inspect := inspectRaw.(rushestools.SpeechInspectResult)
	if !inspect.CacheHit || inspect.ProviderID != "sidecar-srt" || len(inspect.Utterances) != 1 ||
		inspect.Utterances[0].UtteranceID != "utt_fingerprint" || len(inspect.Pauses) != 1 {
		t.Fatalf("inspect=%#v", inspect)
	}
	contextText, err := NewContextBuilder(database).Build(t.Context(), "draft_talking_head")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(contextText, `"speech_searchable":true`) ||
		!strings.Contains(contextText, `"utterance_count":4`) ||
		strings.Contains(contextText, "指纹解锁按键位于键盘右上角") {
		t.Fatalf("口播上下文索引或全文隔离无效: %s", contextText)
	}

	searchRaw, err := service.ExecuteTool(ctx, "media.search_shots", rushestools.ShotSearchInput{
		Query: "指纹解锁 键盘右上角", SemanticRoles: []string{"b_roll"}, Limit: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	search := searchRaw.(rushestools.ShotSearchResult)
	if len(search.Shots) != 1 || search.Shots[0].SemanticRole != "b_roll" {
		t.Fatalf("search=%#v", search)
	}
	assertTalkingHeadFailure(t, service, ctx, rushestools.TalkingHeadEditInput{}, "至少需要一个删除项")
	assertTalkingHeadFailure(t, service, ctx, rushestools.TalkingHeadEditInput{
		ARollTimelineClipID: "missing", RemovePauseIDs: []string{"pause_breath"},
	}, "不存在于主视频轨")
	assertTalkingHeadFailure(t, service, ctx, rushestools.TalkingHeadEditInput{
		ARollTimelineClipID: "clip_v1_001", RemoveUtteranceIDs: []string{"utt_missing"},
	}, "未知 ID")
	assertTalkingHeadFailure(t, service, ctx, rushestools.TalkingHeadEditInput{
		ARollTimelineClipID: "clip_v1_001", RemoveUtteranceIDs: []string{"utt_fingerprint"},
		BrollAssignments: []rushestools.TalkingHeadBrollAssignment{{
			ShotID: search.Shots[0].ShotID, StartUtteranceID: "utt_fingerprint",
		}},
	}, "引用了未知")
	assertTalkingHeadFailure(t, service, ctx, rushestools.TalkingHeadEditInput{
		ARollTimelineClipID: "clip_v1_001",
		BrollAssignments: []rushestools.TalkingHeadBrollAssignment{{
			ShotID: "shot_missing", StartUtteranceID: "utt_fingerprint",
		}},
	}, "不存在、已失效")
	assertTalkingHeadFailure(t, service, ctx, rushestools.TalkingHeadEditInput{
		ARollTimelineClipID: "clip_v1_001",
		BrollAssignments: []rushestools.TalkingHeadBrollAssignment{{
			ShotID: search.Shots[0].ShotID, StartWordID: "w_touchpad_short",
		}},
	}, "不足半秒")
	assertTalkingHeadFailure(t, service, ctx, rushestools.TalkingHeadEditInput{
		ARollTimelineClipID: "clip_v1_001",
		BrollAssignments: []rushestools.TalkingHeadBrollAssignment{{
			ShotID:           search.Shots[0].ShotID,
			StartUtteranceID: "utt_intro", EndUtteranceID: "utt_fingerprint",
		}},
	}, "语义窗口过宽")
	editRaw, err := service.ExecuteTool(ctx, "timeline.edit_talking_head", rushestools.TalkingHeadEditInput{
		ARollTimelineClipID: "clip_v1_001",
		RemoveUtteranceIDs:  []string{"utt_duplicate"},
		RemovePauseIDs:      []string{"pause_breath"},
		BrollAssignments: []rushestools.TalkingHeadBrollAssignment{{
			ShotID: search.Shots[0].ShotID, StartWordID: "w_fingerprint", EndWordID: "w_position",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	edit := editRaw.(rushestools.ToolResult)
	if edit.Status != "succeeded" {
		t.Fatalf("edit=%#v", edit)
	}
	removedPauseRanges, rangesOK := edit.Data["removed_pause_ranges"].([]agentexec.TalkingHeadRange)
	autoPreservedPauseIDs, preservedOK := edit.Data["auto_preserved_pause_ids"].([]string)
	if !rangesOK || !preservedOK || edit.Data["removed_pause_range_count"] != len(removedPauseRanges) ||
		edit.Data["removed_pause_range_count"] != 1 || len(autoPreservedPauseIDs) != 0 ||
		edit.Data["auto_preserved_pause_count"] != len(autoPreservedPauseIDs) {
		t.Fatalf("pause result contract=%#v", edit.Data)
	}
	latest, err := timeline.Latest(t.Context(), database, "draft_talking_head")
	if err != nil {
		t.Fatal(err)
	}
	if latest.DurationFrames != 228 || !timeline.Validate(latest).Valid {
		t.Fatalf("latest=%#v report=%#v", latest, timeline.Validate(latest))
	}
	wantSourceRanges := [][2]int{{0, 60}, {132, 300}}
	for _, trackID := range []string{"visual_base", "original_audio"} {
		clips := timelineTrackClips(latest, trackID)
		if len(clips) != len(wantSourceRanges) {
			t.Fatalf("track=%s clips=%#v", trackID, clips)
		}
		for index, want := range wantSourceRanges {
			if clips[index].SourceStartFrame != want[0] || clips[index].SourceEndFrame != want[1] {
				t.Fatalf("track=%s clip[%d]=%#v want=%v", trackID, index, clips[index], want)
			}
		}
	}
	overlays := timelineTrackClips(latest, "visual_overlay")
	if len(overlays) != 1 || overlays[0].AssetID != "asset_broll_fingerprint" ||
		overlays[0].Role != "b_roll" || overlays[0].TimelineStartFrame != 63 ||
		overlays[0].TimelineEndFrame != 103 ||
		overlays[0].Metadata["shot_id"] != search.Shots[0].ShotID ||
		overlays[0].Metadata["start_word_id"] != "w_fingerprint" ||
		overlays[0].Metadata["transcript_text"] != "指纹解锁按键位于键盘右上角。" ||
		overlays[0].Metadata["placement_policy"] != "fit_within_semantic_window" ||
		overlays[0].Metadata["anchor_timeline_end_frame"] != float64(138) {
		t.Fatalf("overlays=%#v", overlays)
	}
	assertTalkingHeadFailure(t, service, ctx, rushestools.TalkingHeadEditInput{
		ARollTimelineClipID: "clip_v1_001_after_132",
		BrollAssignments: []rushestools.TalkingHeadBrollAssignment{{
			ShotID: search.Shots[0].ShotID, StartUtteranceID: "utt_fingerprint",
		}},
	}, "与现有叠加轨片段重叠")
	postContext, err := NewContextBuilder(database).Build(t.Context(), "draft_talking_head")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(postContext, "utt_duplicate") ||
		!strings.Contains(postContext, `"removed_utterance_count":1`) ||
		!strings.Contains(postContext, `"kind":"b_roll_semantic_anchor"`) ||
		!strings.Contains(postContext, `"transcript_text":"指纹解锁按键位于键盘右上角。"`) {
		t.Fatalf("最近编辑历史未做语义压缩: %s", postContext)
	}
	remainingClipID := ""
	for _, clip := range timelineTrackClips(latest, "visual_base") {
		if clip.SourceStartFrame == 132 && clip.SourceEndFrame == 300 {
			remainingClipID = clip.TimelineClipID
			break
		}
	}
	if remainingClipID == "" {
		t.Fatal("未找到用于确认重放的剩余口播片段")
	}
	confirmRaw, err := service.ExecuteTool(ctx, "interaction.confirm_action", rushestools.ConfirmActionInput{
		Question: "确认删除末尾触控板台词？", ToolName: "timeline.edit_talking_head",
		Arguments: map[string]any{
			"a_roll_timeline_clip_id": remainingClipID,
			"remove_utterance_ids":    []any{"utt_touchpad"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	confirmResult := confirmRaw.(rushestools.ToolResult)
	if confirmResult.Status != "waiting" {
		t.Fatalf("confirm result=%#v", confirmResult)
	}
	decision, err := storage.GetDecision(
		t.Context(), database.Read(), confirmResult.Data["decision_id"].(string),
	)
	if err != nil {
		t.Fatal(err)
	}
	if decision.AllowFreeText {
		t.Fatal("破坏性操作确认卡不得允许自由文本")
	}
	beforeReplayVersion := latest.Version
	for _, answer := range []rushestools.DecisionAnswerInput{
		{DecisionID: decision.ID, FreeText: "就这样吧"},
		{DecisionID: decision.ID},
	} {
		if _, answerErr := service.ExecuteTool(ctx, "decision.answer", answer); answerErr == nil {
			t.Fatalf("未明确选择确认的答案必须被拒绝: %#v", answer)
		}
		unchanged, latestErr := timeline.Latest(t.Context(), database, "draft_talking_head")
		if latestErr != nil || unchanged.Version != beforeReplayVersion {
			t.Fatalf("无效确认答案修改了时间线: version=%d err=%v", unchanged.Version, latestErr)
		}
	}
	if !service.Queue().EnqueueUIObservation(
		"draft_talking_head", "forged_free_text", "decision_answered", map[string]any{
			"pending_tool_call": decision.PendingToolCall,
			"answer":            map[string]any{"free_text": "就这样吧"},
		},
	) {
		t.Fatal("自由文本重放观察未入队")
	}
	service.Queue().JoinDraft("draft_talking_head")
	unchanged, err := timeline.Latest(t.Context(), database, "draft_talking_head")
	if err != nil || unchanged.Version != beforeReplayVersion {
		t.Fatalf("自由文本队列重放修改了时间线: version=%d err=%v", unchanged.Version, err)
	}
	if _, err := service.ExecuteTool(ctx, "decision.answer", rushestools.DecisionAnswerInput{
		DecisionID: decision.ID, OptionID: "confirm",
	}); err != nil {
		t.Fatal(err)
	}
	decision, err = storage.GetDecision(t.Context(), database.Read(), decision.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !service.Queue().EnqueueUIObservation(
		"draft_talking_head", decision.ID, "decision_answered", map[string]any{
			"pending_tool_call": decision.PendingToolCall,
			"answer":            decision.Answer,
		},
	) {
		t.Fatal("确认答案未入队")
	}
	service.Queue().JoinDraft("draft_talking_head")
	replayedTimeline, err := timeline.Latest(t.Context(), database, "draft_talking_head")
	if err != nil {
		t.Fatal(err)
	}
	if replayedTimeline.Version != latest.Version+1 || replayedTimeline.DurationFrames >= latest.DurationFrames {
		t.Fatalf("talking head replay timeline=%#v before=%#v", replayedTimeline, latest)
	}
}

func TestTalkingHeadResultDoesNotCountAutoPreservedPauseAsRemovedRange(t *testing.T) {
	t.Parallel()
	const draftID = "draft_talking_head_auto_preserved_pause"
	database := agentTestDatabase(t)
	createAgentDraft(t, database, draftID)
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO assets(
			asset_id,storage_mode,reference_path,kind,source,filename,hash,size,
			probe_json,ingest_status,understanding_status,usable
		) VALUES(
			'asset_auto_preserved_pause','reference','/tmp/auto-preserved-pause.mp4',
			'video','local_path','auto-preserved-pause.mp4','asset_auto_preserved_pause',1,
			'{"duration_sec":4,"has_audio":true}','ready','ready',1
		);
		INSERT INTO draft_asset_links(draft_id,asset_id,rel_dir,linked_at)
		VALUES(?, 'asset_auto_preserved_pause', 'Aroll', ?);`,
		draftID, time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatal(err)
	}
	transcriptResult, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: contracts.ActorAgent,
		ResultRows: reducer.ResultRows{Transcripts: []reducer.TranscriptRow{{
			ID: "transcript_auto_preserved_pause", AssetID: "asset_auto_preserved_pause",
			ProviderID: "fixture-word-timestamps",
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
		AssetID: "asset_auto_preserved_pause", AssetKind: "video",
		SourceStartFrame: 0, SourceEndFrame: 120, Role: "a_roll", HasAudio: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	if persisted, persistErr := service.persistTimeline(t.Context(), draftID, document, "fixture"); persistErr != nil || persisted.Status != "succeeded" {
		t.Fatalf("persisted=%#v err=%v", persisted, persistErr)
	}
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	raw, err := service.ExecuteTool(ctx, "timeline.edit_talking_head", rushestools.TalkingHeadEditInput{
		ARollTimelineClipID: "clip_v1_001",
		RemovePauseIDs:      []string{"pause_before_year", "pause_after_year"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := raw.(rushestools.ToolResult)
	removedRanges, rangesOK := result.Data["removed_pause_ranges"].([]agentexec.TalkingHeadRange)
	removedIDs, removedOK := result.Data["removed_pause_ids"].([]string)
	autoPreservedIDs, preservedOK := result.Data["auto_preserved_pause_ids"].([]string)
	if result.Status != "succeeded" || !rangesOK || !removedOK || !preservedOK ||
		result.Data["removed_pause_range_count"] != 1 || len(removedRanges) != 1 ||
		len(removedIDs) != 1 || removedIDs[0] != "pause_before_year" ||
		result.Data["auto_preserved_pause_count"] != 1 || len(autoPreservedIDs) != 1 ||
		autoPreservedIDs[0] != "pause_after_year" ||
		!strings.Contains(result.Observation, "1 个独立气口区间") ||
		!strings.Contains(result.Observation, "已保守保留 1 个相邻气口") {
		t.Fatalf("auto-preserved pause result=%#v", result)
	}
}

func TestTalkingHeadAssignmentResolvesUniqueAnchorTextToWordFrames(t *testing.T) {
	t.Parallel()
	utterances := map[string]agentexec.SpeechUtterance{
		"utt_fingerprint": {
			ID: "utt_fingerprint", StartFrame: 100, EndFrame: 180,
			Text: "指纹识别解锁，然后指纹设置。",
		},
	}
	words := []agentexec.SpeechWord{
		{ID: "w1", StartFrame: 100, EndFrame: 122, Text: "指纹识别"},
		{ID: "w2", StartFrame: 122, EndFrame: 140, Text: "解锁"},
		{ID: "w3", StartFrame: 140, EndFrame: 152, Text: "然后"},
		{ID: "w4", StartFrame: 152, EndFrame: 168, Text: "指纹"},
		{ID: "w5", StartFrame: 168, EndFrame: 180, Text: "设置", Punctuation: "。"},
	}
	clip := timeline.Clip{SourceStartFrame: 0, SourceEndFrame: 300}
	resolved, err := agentexec.TalkingHeadAssignmentSourceRange(
		rushestools.TalkingHeadBrollAssignment{
			ShotID: "shot_fingerprint", StartUtteranceID: "utt_fingerprint",
			AnchorText: "指纹识别解锁",
		},
		utterances, words, map[string]struct{}{}, map[string]struct{}{}, clip,
	)
	if err != nil || resolved != (agentexec.TalkingHeadRange{Start: 100, End: 140}) {
		t.Fatalf("resolved=%#v err=%v", resolved, err)
	}
	_, err = agentexec.TalkingHeadAssignmentSourceRange(
		rushestools.TalkingHeadBrollAssignment{
			ShotID: "shot_fingerprint", StartUtteranceID: "utt_fingerprint",
			AnchorText: "指纹",
		},
		utterances, words, map[string]struct{}{}, map[string]struct{}{}, clip,
	)
	if err == nil || !strings.Contains(err.Error(), "不唯一") {
		t.Fatalf("ambiguous anchor err=%v", err)
	}
	_, err = agentexec.TalkingHeadAssignmentSourceRange(
		rushestools.TalkingHeadBrollAssignment{
			ShotID: "shot_fingerprint", StartUtteranceID: "utt_fingerprint",
			AnchorText: "指纹识别解锁",
		},
		utterances, words, map[string]struct{}{}, map[string]struct{}{"w2": {}}, clip,
	)
	if err == nil || !strings.Contains(err.Error(), "本次将删除") {
		t.Fatalf("removed anchor err=%v", err)
	}
}

func TestTalkingHeadHelperEdgeBranches(t *testing.T) {
	t.Parallel()
	clip := timeline.Clip{SourceStartFrame: 10, SourceEndFrame: 80}
	utterances := map[string]agentexec.SpeechUtterance{
		"u1":      {ID: "u1", StartFrame: 10, EndFrame: 40, Text: "第一第二第三"},
		"u2":      {ID: "u2", StartFrame: 50, EndFrame: 80, Text: "第四第五"},
		"outside": {ID: "outside", StartFrame: 0, EndFrame: 9, Text: "越界"},
	}
	words := []agentexec.SpeechWord{
		{ID: "w1", StartFrame: 10, EndFrame: 20, Text: "第一"},
		{ID: "w2", StartFrame: 20, EndFrame: 30, Text: "第二"},
		{ID: "w3", StartFrame: 30, EndFrame: 40, Text: "第三"},
		{ID: "w4", StartFrame: 50, EndFrame: 65, Text: "第四"},
		{ID: "w5", StartFrame: 65, EndFrame: 80, Text: "第五"},
		{ID: "w_out", StartFrame: 81, EndFrame: 90, Text: "越界"},
	}

	selectedUtterances, invalidUtterances := agentexec.SelectTalkingHeadUtterances(
		[]string{"u1", "u1", "missing", "outside", "u2"}, utterances, clip,
	)
	if len(selectedUtterances) != 2 || len(invalidUtterances) != 2 {
		t.Fatalf("selected=%#v invalid=%#v", selectedUtterances, invalidUtterances)
	}

	ranges, removedWords, invalidWordRanges := agentexec.SelectTalkingHeadWordRanges(
		[]rushestools.TalkingHeadWordRange{
			{StartWordID: "w1"},
			{StartWordID: "w1", EndWordID: "w2"},
			{StartWordID: "missing"},
			{StartWordID: "w2", EndWordID: "w1"},
			{StartWordID: "w_out"},
		},
		words,
		clip,
	)
	if len(ranges) != 1 || len(removedWords) != 2 || len(invalidWordRanges) != 3 {
		t.Fatalf("ranges=%#v removed=%#v invalid=%#v", ranges, removedWords, invalidWordRanges)
	}

	pauses := map[string]agentexec.SpeechPause{
		"p1":      {ID: "p1", StartFrame: 18, EndFrame: 28, DeleteStart: 20, DeleteEnd: 26, Method: "fixture"},
		"outside": {ID: "outside", StartFrame: 0, EndFrame: 9, DeleteStart: 1, DeleteEnd: 8, Method: "fixture"},
	}
	selectedPauses, invalidPauses := agentexec.SelectTalkingHeadPauses(
		[]string{"p1", "p1", "missing", "outside"}, pauses, clip,
	)
	if len(selectedPauses) != 1 || len(invalidPauses) != 2 {
		t.Fatalf("selected=%#v invalid=%#v", selectedPauses, invalidPauses)
	}

	if got := agentexec.SubtractTalkingHeadRanges(agentexec.TalkingHeadRange{Start: 5, End: 5}, nil); got != nil {
		t.Fatalf("invalid target=%#v", got)
	}
	residual := agentexec.SubtractTalkingHeadRanges(
		agentexec.TalkingHeadRange{Start: 10, End: 40},
		[]agentexec.TalkingHeadRange{{Start: 0, End: 5}, {Start: 20, End: 25}, {Start: 40, End: 50}},
	)
	if len(residual) != 2 || residual[0] != (agentexec.TalkingHeadRange{Start: 10, End: 20}) ||
		residual[1] != (agentexec.TalkingHeadRange{Start: 25, End: 40}) {
		t.Fatalf("residual=%#v", residual)
	}
	covered := agentexec.SubtractTalkingHeadRanges(
		agentexec.TalkingHeadRange{Start: 10, End: 40},
		[]agentexec.TalkingHeadRange{{Start: 20, End: 50}},
	)
	if len(covered) != 1 || covered[0] != (agentexec.TalkingHeadRange{Start: 10, End: 20}) {
		t.Fatalf("covered=%#v", covered)
	}

	pauseList := []agentexec.SpeechPause{
		{ID: "equal_late", StartFrame: 28, EndFrame: 38, DeleteStart: 30, DeleteEnd: 36, Method: "fixture"},
		{ID: "equal_early", StartFrame: 18, EndFrame: 28, DeleteStart: 20, DeleteEnd: 26, Method: "fixture"},
		{ID: "too_short", StartFrame: 40, EndFrame: 43, DeleteStart: 40, DeleteEnd: 42, Method: "fixture"},
		{ID: "overlap", StartFrame: 48, EndFrame: 60, DeleteStart: 50, DeleteEnd: 58, Method: "fixture"},
	}
	if got := agentexec.TalkingHeadRetainedPauseCandidates(pauseList, nil, clip, nil, 4, 0); got != nil {
		t.Fatalf("limit zero=%#v", got)
	}
	candidates := agentexec.TalkingHeadRetainedPauseCandidates(
		pauseList, []agentexec.TalkingHeadRange{{Start: 49, End: 59}}, clip, nil, 4, 1,
	)
	if len(candidates) != 1 || candidates[0].PauseID != "equal_early" {
		t.Fatalf("candidates=%#v", candidates)
	}
	if unresolved := agentexec.UnresolvedTalkingHeadPauseDecisions(
		pauseList, nil, clip, nil, map[string]struct{}{"equal_early": {}}, 4, 4,
	); len(unresolved) != 2 {
		t.Fatalf("unresolved=%#v", unresolved)
	}

	result := rushestools.ToolResult{}
	agentexec.AttachTalkingHeadUnreviewedEvidence(
		&result,
		[]rushestools.SpeechPauseEvidence{{PauseID: "p1"}},
		nil,
		nil,
	)
	if result.Data == nil || !strings.Contains(result.Observation, "另返回") {
		t.Fatalf("result=%#v", result)
	}
	agentexec.AttachTalkingHeadUnreviewedEvidence(&result, nil, nil, nil)

	repetition := rushestools.SpeechRepetitionEvidence{
		RepetitionID: "repeat", EarlierStartWordID: "w1", EarlierEndWordID: "w2",
		LaterStartWordID: "w3", LaterEndWordID: "w4",
	}
	repetitionInput, decidedRepetitions, invalidRepetitions := agentexec.ExpandTalkingHeadRepetitionDecisions(
		rushestools.TalkingHeadEditInput{
			RemoveWordRanges: []rushestools.TalkingHeadWordRange{{StartWordID: "w1", EndWordID: "w2"}},
			RepetitionDecisions: []rushestools.TalkingHeadRepetitionDecision{
				{RepetitionID: "repeat", Action: "remove_earlier"},
				{RepetitionID: "repeat", Action: "remove_later"},
				{RepetitionID: "missing", Action: "preserve"},
			},
		},
		map[string]rushestools.SpeechRepetitionEvidence{"repeat": repetition},
	)
	if len(repetitionInput.RemoveWordRanges) != 1 || len(decidedRepetitions) != 1 || len(invalidRepetitions) != 2 {
		t.Fatalf("input=%#v decided=%#v invalid=%#v", repetitionInput, decidedRepetitions, invalidRepetitions)
	}

	fragment := rushestools.SpeechFragmentEvidence{
		FragmentID: "fragment", StartWordID: "w1", EndWordID: "w2",
	}
	keepFragment := rushestools.SpeechFragmentEvidence{
		FragmentID: "keep", StartWordID: "w3", EndWordID: "w4",
	}
	fragmentExpansion := agentexec.ExpandTalkingHeadFragmentDecisions(
		rushestools.TalkingHeadEditInput{
			RemoveWordRanges: []rushestools.TalkingHeadWordRange{{StartWordID: "w1", EndWordID: "w2"}},
			ShortFragmentDecisions: []rushestools.TalkingHeadFragmentDecision{
				{FragmentID: "fragment", Action: "remove"},
				{FragmentID: "keep", Action: "preserve", Reason: "保留"},
				{FragmentID: "fragment", Action: "preserve"},
				{FragmentID: "missing", Action: "remove"},
			},
		},
		map[string]rushestools.SpeechFragmentEvidence{"fragment": fragment, "keep": keepFragment},
	)
	if len(fragmentExpansion.Input.RemoveWordRanges) != 1 ||
		len(fragmentExpansion.PreservedIDs) != 1 || fragmentExpansion.PreservedIDs[0] != "keep" ||
		fragmentExpansion.PreservedReasons["keep"] != "保留" || len(fragmentExpansion.Invalid) != 2 {
		t.Fatalf("expansion=%#v", fragmentExpansion)
	}

	assertAssignmentError := func(assignment rushestools.TalkingHeadBrollAssignment, removedUtterances, removed map[string]struct{}) {
		t.Helper()
		if _, err := agentexec.TalkingHeadAssignmentSourceRange(
			assignment, utterances, words, removedUtterances, removed, clip,
		); err == nil {
			t.Fatalf("assignment should fail: %#v", assignment)
		}
	}
	assertAssignmentError(rushestools.TalkingHeadBrollAssignment{}, nil, nil)
	assertAssignmentError(rushestools.TalkingHeadBrollAssignment{ShotID: "shot"}, nil, nil)
	assertAssignmentError(rushestools.TalkingHeadBrollAssignment{
		ShotID: "shot", StartWordID: "w1", StartUtteranceID: "u1",
	}, nil, nil)
	assertAssignmentError(rushestools.TalkingHeadBrollAssignment{
		ShotID: "shot", StartWordID: "w1", AnchorText: "第一",
	}, nil, nil)
	assertAssignmentError(rushestools.TalkingHeadBrollAssignment{
		ShotID: "shot", EndUtteranceID: "u1",
	}, nil, nil)
	assertAssignmentError(rushestools.TalkingHeadBrollAssignment{
		ShotID: "shot", StartUtteranceID: "missing",
	}, nil, nil)
	assertAssignmentError(rushestools.TalkingHeadBrollAssignment{
		ShotID: "shot", StartUtteranceID: "u2", EndUtteranceID: "u1",
	}, nil, nil)
	assertAssignmentError(rushestools.TalkingHeadBrollAssignment{
		ShotID: "shot", StartUtteranceID: "outside",
	}, nil, nil)
	assertAssignmentError(rushestools.TalkingHeadBrollAssignment{
		ShotID: "shot", EndWordID: "w1",
	}, nil, nil)
	assertAssignmentError(rushestools.TalkingHeadBrollAssignment{
		ShotID: "shot", StartWordID: "missing",
	}, nil, nil)
	assertAssignmentError(rushestools.TalkingHeadBrollAssignment{
		ShotID: "shot", StartWordID: "w2", EndWordID: "w1",
	}, nil, nil)
	assertAssignmentError(rushestools.TalkingHeadBrollAssignment{
		ShotID: "shot", StartWordID: "w1",
	}, nil, map[string]struct{}{"w1": {}})
	assertAssignmentError(rushestools.TalkingHeadBrollAssignment{
		ShotID: "shot", StartWordID: "w1",
	}, map[string]struct{}{"u1": {}}, nil)
	assertAssignmentError(rushestools.TalkingHeadBrollAssignment{
		ShotID: "shot", StartWordID: "w_out",
	}, nil, nil)

	if _, err := agentexec.TalkingHeadAnchorTextSourceRange("一", 10, 40, utterances, words, nil, nil); err == nil {
		t.Fatal("short anchor should fail")
	}
	if _, err := agentexec.TalkingHeadAnchorTextSourceRange("不存在", 10, 20, utterances, words, nil, nil); err == nil {
		t.Fatal("missing anchor should fail")
	}
	if _, err := agentexec.TalkingHeadAnchorTextSourceRange(
		"第一", 10, 40, utterances, words,
		map[string]struct{}{"u1": {}}, nil,
	); err == nil {
		t.Fatal("removed utterance anchor should fail")
	}
}

func TestTalkingHeadEditDeletesExactWordRangeWithoutSwallowingRetainedSpeech(t *testing.T) {
	t.Parallel()
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_talking_head_words")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO assets(
			asset_id,storage_mode,reference_path,kind,source,filename,hash,size,
			probe_json,ingest_status,understanding_status,usable
		) VALUES('asset_aroll_words','reference','/tmp/aroll-words.mp4','video','local_path',
			'aroll-words.mp4','asset_aroll_words',1,'{"duration_sec":4,"has_audio":true}',
			'ready','ready',1);
		INSERT INTO draft_asset_links(draft_id,asset_id,rel_dir,linked_at)
		VALUES('draft_talking_head_words','asset_aroll_words','Aroll',?);`, now); err != nil {
		t.Fatal(err)
	}
	words := []map[string]any{
		{"word_id": "w_today", "source_start_frame": 0, "source_end_frame": 15, "text": "今天"},
		{"word_id": "w_this_1", "source_start_frame": 15, "source_end_frame": 30, "text": "这个"},
		{"word_id": "w_this_2", "source_start_frame": 30, "source_end_frame": 45, "text": "这个"},
		{"word_id": "w_computer", "source_start_frame": 50, "source_end_frame": 70, "text": "电脑"},
		{"word_id": "w_good", "source_start_frame": 70, "source_end_frame": 100, "text": "很好用", "punctuation": "。"},
	}
	transcriptResult, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: contracts.ActorAgent,
		ResultRows: reducer.ResultRows{Transcripts: []reducer.TranscriptRow{{
			ID: "transcript_words", AssetID: "asset_aroll_words", ProviderID: "fake-asr+provider-timestamps",
			Utterances: []map[string]any{{
				"utterance_id": "utt_words", "source_start_frame": 0, "source_end_frame": 100,
				"text": "今天这个这个电脑很好用。", "words": words,
			}},
		}}},
	})
	if err != nil || transcriptResult.Status != reducer.StatusApplied {
		t.Fatalf("transcript status=%s err=%v", transcriptResult.Status, err)
	}
	document, err := timeline.ComposeInitial("draft_talking_head_words", 1, []timeline.Selection{{
		AssetID: "asset_aroll_words", AssetKind: "video", SourceStartFrame: 0,
		SourceEndFrame: 120, Role: "a_roll", HasAudio: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	if persisted, persistErr := service.persistTimeline(
		t.Context(), "draft_talking_head_words", document, "fixture",
	); persistErr != nil || persisted.Status != "succeeded" {
		t.Fatalf("persist=%#v err=%v", persisted, persistErr)
	}
	ctx := rushestools.WithDraftID(t.Context(), "draft_talking_head_words")
	inspectRaw, err := service.ExecuteTool(ctx, "speech.inspect", rushestools.SpeechInspectInput{
		TimelineClipID: "clip_v1_001", IncludeWords: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	inspect := inspectRaw.(rushestools.SpeechInspectResult)
	if !inspect.CacheHit || inspect.WordTotal != 5 || len(inspect.Utterances) != 1 ||
		len(inspect.Utterances[0].Words) != 5 {
		t.Fatalf("inspect=%#v", inspect)
	}
	invalidRaw, err := service.ExecuteTool(ctx, "timeline.edit_talking_head", rushestools.TalkingHeadEditInput{
		ARollTimelineClipID: "clip_v1_001",
		RemoveWordRanges: []rushestools.TalkingHeadWordRange{{
			StartWordID: "w_computer", EndWordID: "w_this_1",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	invalid := invalidRaw.(rushestools.ToolResult)
	if invalid.Status != "failed" || !strings.Contains(invalid.Observation, "未知 ID") {
		t.Fatalf("invalid=%#v", invalid)
	}
	editRaw, err := service.ExecuteTool(ctx, "timeline.edit_talking_head", rushestools.TalkingHeadEditInput{
		ARollTimelineClipID: "clip_v1_001",
		RemoveWordRanges: []rushestools.TalkingHeadWordRange{{
			StartWordID: "w_this_2", EndWordID: "w_this_2",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	edit := editRaw.(rushestools.ToolResult)
	if edit.Status != "succeeded" {
		t.Fatalf("edit=%#v", edit)
	}
	latest, err := timeline.Latest(t.Context(), database, "draft_talking_head_words")
	if err != nil {
		t.Fatal(err)
	}
	clips := timelineTrackClips(latest, "visual_base")
	if latest.DurationFrames != 105 || len(clips) != 2 ||
		clips[0].SourceStartFrame != 0 || clips[0].SourceEndFrame != 30 ||
		clips[1].SourceStartFrame != 45 || clips[1].SourceEndFrame != 120 {
		t.Fatalf("latest=%#v clips=%#v", latest, clips)
	}
	if got := edit.Data["removed_word_ids"]; strings.Contains(fmtJSON(got), "w_this_1") ||
		!strings.Contains(fmtJSON(got), "w_this_2") {
		t.Fatalf("removed_word_ids=%#v", got)
	}
}

func TestAbsorbTalkingHeadEdgeSliversKeepsSpeechAndRemovesSilentTail(t *testing.T) {
	deletions := []agentexec.TalkingHeadRange{{Start: 10, End: 20}, {Start: 90, End: 96}}
	retainedSpeech := []agentexec.TalkingHeadRange{{Start: 0, End: 9}, {Start: 21, End: 90}}
	got := agentexec.AbsorbTalkingHeadEdgeSlivers(deletions, retainedSpeech, 0, 100, 12)
	if len(got) != 2 || got[0].Start != 10 || got[1].End != 100 {
		t.Fatalf("silent tail was not absorbed without touching leading speech: %#v", got)
	}
	withTrailingSpeech := []agentexec.TalkingHeadRange{{Start: 0, End: 9}, {Start: 21, End: 90}, {Start: 98, End: 100}}
	got = agentexec.AbsorbTalkingHeadEdgeSlivers(deletions, withTrailingSpeech, 0, 100, 12)
	if got[1].End != 96 {
		t.Fatalf("trailing speech was swallowed: %#v", got)
	}
}

func TestTalkingHeadOrphanSpeechFragmentsExposeAdjacentPauseEvidence(t *testing.T) {
	t.Parallel()
	utterances := []agentexec.SpeechUtterance{{
		ID: "utt_year", StartFrame: 70, EndFrame: 104, Text: "是2015年。",
		Words: []agentexec.SpeechWord{
			{ID: "w_is", StartFrame: 70, EndFrame: 78, Text: "是"},
			{ID: "w_year", StartFrame: 91, EndFrame: 104, Text: "2015年", Punctuation: "。"},
		},
	}}
	retainedSpeech := []agentexec.TalkingHeadRange{{Start: 70, End: 78}, {Start: 91, End: 104}}
	pauses := []agentexec.SpeechPause{
		{ID: "pause_before_year", DeleteStart: 78, DeleteEnd: 91},
		{ID: "pause_after_year", DeleteStart: 104, DeleteEnd: 116},
	}
	fragments := agentexec.TalkingHeadOrphanSpeechFragments(
		[]agentexec.TalkingHeadRange{{Start: 78, End: 91}, {Start: 104, End: 116}},
		retainedSpeech, utterances, map[string]struct{}{}, map[string]struct{}{}, pauses,
		0, 120, 15, nil,
	)
	if len(fragments) != 1 || fragments[0]["source_start_frame"] != 91 ||
		fragments[0]["source_end_frame"] != 104 || fragments[0]["retained_text"] != "2015年。" ||
		!strings.Contains(fmtJSON(fragments[0]["adjacent_pause_ids"]), "pause_before_year") ||
		!strings.Contains(fmtJSON(fragments[0]["adjacent_pause_ids"]), "pause_after_year") {
		t.Fatalf("orphan fragments=%#v", fragments)
	}
	fragments = agentexec.TalkingHeadOrphanSpeechFragments(
		[]agentexec.TalkingHeadRange{{Start: 78, End: 91}},
		retainedSpeech, utterances, map[string]struct{}{}, map[string]struct{}{}, pauses[:1],
		0, 120, 15, nil,
	)
	if len(fragments) != 0 {
		t.Fatalf("撤回一侧气口后不应再形成孤立碎片: %#v", fragments)
	}
	orphanWord := agentexec.SpeechUtterance{
		ID: "utt_orphan_word", StartFrame: 120, EndFrame: 139, Text: "自己",
		Words: []agentexec.SpeechWord{{ID: "w_self", StartFrame: 120, EndFrame: 139, Text: "自己"}},
	}
	fragments = agentexec.TalkingHeadOrphanSpeechFragments(
		[]agentexec.TalkingHeadRange{{Start: 0, End: 120}, {Start: 139, End: 170}},
		[]agentexec.TalkingHeadRange{{Start: 120, End: 139}}, []agentexec.SpeechUtterance{orphanWord},
		map[string]struct{}{}, map[string]struct{}{}, nil,
		0, 200, agentexec.MinTalkingHeadRetainedIslandFrames, nil,
	)
	if len(fragments) != 1 || fragments[0]["retained_text"] != "自己" {
		t.Fatalf("不足 2 秒的单词语音岛必须显式解决: %#v", fragments)
	}
}

func TestProtectTalkingHeadOrphanFragmentsRetractsOnlyAdjacentPause(t *testing.T) {
	t.Parallel()
	utterance := agentexec.SpeechUtterance{
		ID: "utt_year", StartFrame: 70, EndFrame: 104, Text: "是2015年。",
		Words: []agentexec.SpeechWord{
			{ID: "w_is", StartFrame: 70, EndFrame: 78, Text: "是"},
			{ID: "w_year", StartFrame: 91, EndFrame: 104, Text: "2015年", Punctuation: "。"},
		},
	}
	pauses := []agentexec.SpeechPause{
		{ID: "pause_before_year", DeleteStart: 78, DeleteEnd: 91},
		{ID: "pause_after_year", DeleteStart: 104, DeleteEnd: 116},
	}
	effective, pauseRanges, deletions, autoPreserved, orphans := agentexec.ProtectTalkingHeadOrphanFragments(
		nil, pauses, pauses,
		[]agentexec.TalkingHeadRange{{Start: 70, End: 78}, {Start: 91, End: 104}},
		[]agentexec.SpeechUtterance{utterance}, map[string]struct{}{}, map[string]struct{}{},
		timeline.Clip{SourceStartFrame: 0, SourceEndFrame: 120}, nil,
	)
	if len(orphans) != 0 || len(effective) != 1 || effective[0].ID != "pause_before_year" ||
		len(autoPreserved) != 1 || autoPreserved[0].ID != "pause_after_year" ||
		len(pauseRanges) != 1 || pauseRanges[0] != (agentexec.TalkingHeadRange{Start: 78, End: 91}) ||
		len(deletions) != 1 || deletions[0] != (agentexec.TalkingHeadRange{Start: 78, End: 91}) {
		t.Fatalf(
			"effective=%#v pauseRanges=%#v deletions=%#v autoPreserved=%#v orphans=%#v",
			effective, pauseRanges, deletions, autoPreserved, orphans,
		)
	}

	semanticOnlyUtterance := agentexec.SpeechUtterance{
		ID: "utt_self", StartFrame: 120, EndFrame: 139, Text: "自己",
		Words: []agentexec.SpeechWord{{ID: "w_self", StartFrame: 120, EndFrame: 139, Text: "自己"}},
	}
	_, _, _, autoPreserved, orphans = agentexec.ProtectTalkingHeadOrphanFragments(
		[]agentexec.TalkingHeadRange{{Start: 0, End: 120}, {Start: 139, End: 170}}, nil, nil,
		[]agentexec.TalkingHeadRange{{Start: 120, End: 139}}, []agentexec.SpeechUtterance{semanticOnlyUtterance},
		map[string]struct{}{}, map[string]struct{}{},
		timeline.Clip{SourceStartFrame: 0, SourceEndFrame: 200}, nil,
	)
	if len(autoPreserved) != 0 || len(orphans) != 1 || orphans[0]["retained_text"] != "自己" {
		t.Fatalf("纯语义删除不能由工具擅自改写: autoPreserved=%#v orphans=%#v", autoPreserved, orphans)
	}
}

func TestRestartFragmentPreserveReasonMustQuoteExactEvidence(t *testing.T) {
	t.Parallel()
	fragment := rushestools.SpeechFragmentEvidence{
		Kind: "restart_prefix_before_repeated_take", Text: "但是没有同时",
		RestartAnchorText: "这次键盘苹果", JoinedContext: "但是没有同时这次键盘苹果也说",
	}
	for _, reason := range []string{
		"作为第二遍键盘段落的衔接",
		"但是没有同时是完整衔接，所以保留",
		"这次键盘苹果是下一句，所以保留",
	} {
		if agentexec.ValidRestartFragmentPreserveReason(fragment, reason) {
			t.Fatalf("模糊理由不应通过: %q", reason)
		}
	}
	if !agentexec.ValidRestartFragmentPreserveReason(
		fragment,
		"原文“但是没有同时”接到“这次键盘苹果”后仍表达同一转折条件，句法和语义均完整，因此保留。",
	) {
		t.Fatal("包含确切片段、重启锚点和完整解释的理由应通过")
	}
}

func TestExpandTalkingHeadFragmentDecisionsCreatesOneShotWordAndPreserveChoices(t *testing.T) {
	t.Parallel()
	fragments := map[string]rushestools.SpeechFragmentEvidence{
		"remove_me": {
			FragmentID: "remove_me", StartWordID: "w_bad_start", EndWordID: "w_bad_end",
		},
		"keep_me": {
			FragmentID: "keep_me", StartWordID: "w_keep_start", EndWordID: "w_keep_end",
		},
	}
	expansion := agentexec.ExpandTalkingHeadFragmentDecisions(rushestools.TalkingHeadEditInput{
		ShortFragmentDecisions: []rushestools.TalkingHeadFragmentDecision{
			{FragmentID: "remove_me", Action: "REMOVE"},
			{FragmentID: "keep_me", Action: "preserve", Reason: "两侧语义完整，应保留"},
		},
	}, fragments)
	if len(expansion.Invalid) != 0 || len(expansion.Input.RemoveWordRanges) != 1 ||
		expansion.Input.RemoveWordRanges[0].StartWordID != "w_bad_start" ||
		expansion.Input.RemoveWordRanges[0].EndWordID != "w_bad_end" ||
		len(expansion.PreservedIDs) != 1 || expansion.PreservedIDs[0] != "keep_me" ||
		expansion.PreservedReasons["keep_me"] != "两侧语义完整，应保留" {
		t.Fatalf("expansion=%#v", expansion)
	}

	invalidExpansion := agentexec.ExpandTalkingHeadFragmentDecisions(rushestools.TalkingHeadEditInput{
		ShortFragmentDecisions: []rushestools.TalkingHeadFragmentDecision{
			{FragmentID: "remove_me", Action: "remove"},
			{FragmentID: "remove_me", Action: "preserve"},
			{FragmentID: "missing", Action: "remove"},
			{FragmentID: "keep_me", Action: "guess"},
		},
	}, fragments)
	if len(invalidExpansion.Invalid) != 3 {
		t.Fatalf("重复、未知与非法 action 均应一次返回: %#v", invalidExpansion.Invalid)
	}
}

func TestExpandTalkingHeadRepetitionDecisionsCreatesExplicitWordChoices(t *testing.T) {
	t.Parallel()
	repetitions := map[string]rushestools.SpeechRepetitionEvidence{
		"rep_stutter": {
			RepetitionID:       "rep_stutter",
			EarlierStartWordID: "press_1", EarlierEndWordID: "press_1",
			LaterStartWordID: "press_2", LaterEndWordID: "press_2",
		},
		"rep_retake": {
			RepetitionID:       "rep_retake",
			EarlierStartWordID: "early_start", EarlierEndWordID: "early_end",
			LaterStartWordID: "later_start", LaterEndWordID: "later_end",
		},
		"rep_number": {RepetitionID: "rep_number"},
	}
	input, decided, invalid := agentexec.ExpandTalkingHeadRepetitionDecisions(
		rushestools.TalkingHeadEditInput{
			RepetitionDecisions: []rushestools.TalkingHeadRepetitionDecision{
				{RepetitionID: "rep_stutter", Action: "REMOVE_EARLIER"},
				{RepetitionID: "rep_retake", Action: "remove_later"},
				{RepetitionID: "rep_number", Action: "preserve"},
			},
		},
		repetitions,
	)
	if len(invalid) != 0 || len(decided) != 3 || len(input.RemoveWordRanges) != 2 ||
		input.RemoveWordRanges[0] != (rushestools.TalkingHeadWordRange{StartWordID: "press_1", EndWordID: "press_1"}) ||
		input.RemoveWordRanges[1] != (rushestools.TalkingHeadWordRange{StartWordID: "later_start", EndWordID: "later_end"}) {
		t.Fatalf("input=%#v decided=%#v invalid=%#v", input, decided, invalid)
	}
	_, _, invalid = agentexec.ExpandTalkingHeadRepetitionDecisions(
		rushestools.TalkingHeadEditInput{RepetitionDecisions: []rushestools.TalkingHeadRepetitionDecision{
			{RepetitionID: "rep_stutter", Action: "preserve"},
			{RepetitionID: "rep_stutter", Action: "remove_later"},
			{RepetitionID: "missing", Action: "preserve"},
			{RepetitionID: "rep_number", Action: "guess"},
		}},
		repetitions,
	)
	if len(invalid) != 3 {
		t.Fatalf("重复、未知与非法 action 均应一次返回: %#v", invalid)
	}
}

func TestExpandTalkingHeadPauseDecisionsRequiresExplicitRemoveOrPreserve(t *testing.T) {
	t.Parallel()
	pauses := map[string]agentexec.SpeechPause{
		"pause_remove":   {ID: "pause_remove", DeleteStart: 40, DeleteEnd: 70},
		"pause_preserve": {ID: "pause_preserve", DeleteStart: 90, DeleteEnd: 110},
	}
	input, decided, invalid := agentexec.ExpandTalkingHeadPauseDecisions(
		rushestools.TalkingHeadEditInput{
			PauseDecisions: []rushestools.TalkingHeadPauseDecision{
				{PauseID: "pause_remove", Action: "REMOVE", Reason: "两侧语义直接相连，是明显气口"},
				{PauseID: "pause_preserve", Action: "preserve", Reason: "这里需要保留表达停顿"},
			},
		},
		pauses,
	)
	if len(invalid) != 0 || len(decided) != 2 || len(input.RemovePauseIDs) != 1 ||
		input.RemovePauseIDs[0] != "pause_remove" {
		t.Fatalf("input=%#v decided=%#v invalid=%#v", input, decided, invalid)
	}

	_, _, invalid = agentexec.ExpandTalkingHeadPauseDecisions(
		rushestools.TalkingHeadEditInput{
			RemovePauseIDs: []string{"pause_preserve"},
			PauseDecisions: []rushestools.TalkingHeadPauseDecision{
				{PauseID: "pause_preserve", Action: "preserve"},
				{PauseID: "pause_remove", Action: "remove"},
				{PauseID: "pause_remove", Action: "preserve"},
				{PauseID: "missing", Action: "remove"},
				{PauseID: "pause_remove", Action: "guess"},
			},
		},
		pauses,
	)
	if len(invalid) != 4 {
		t.Fatalf("冲突、重复、未知与非法 action 均应一次返回: %#v", invalid)
	}
}

func TestTalkingHeadPausesSeparateRedundantSelectionsAndRankRetainedCandidates(t *testing.T) {
	t.Parallel()
	semantic := []agentexec.TalkingHeadRange{{Start: 10, End: 30}}
	pauses := []agentexec.SpeechPause{
		{ID: "pause_redundant", StartFrame: 12, EndFrame: 24, DeleteStart: 14, DeleteEnd: 22},
		{ID: "pause_retained_long", StartFrame: 40, EndFrame: 62, DeleteStart: 42, DeleteEnd: 60},
		{ID: "pause_retained_short", StartFrame: 70, EndFrame: 80, DeleteStart: 72, DeleteEnd: 78},
	}
	effective, residuals, redundant := agentexec.ResolveTalkingHeadPauseRanges(
		pauses[:2], semantic, agentexec.MinTalkingHeadPauseResidualFrames,
	)
	if len(effective) != 1 || effective[0].ID != "pause_retained_long" ||
		len(residuals) != 1 || residuals[0] != (agentexec.TalkingHeadRange{Start: 42, End: 60}) ||
		len(redundant) != 1 || redundant[0].ID != "pause_redundant" {
		t.Fatalf("effective=%#v residuals=%#v redundant=%#v", effective, residuals, redundant)
	}
	candidates := agentexec.TalkingHeadRetainedPauseCandidates(
		pauses, semantic, timeline.Clip{SourceStartFrame: 0, SourceEndFrame: 100},
		nil, agentexec.MinTalkingHeadPauseCandidateFrames, 8,
	)
	if len(candidates) != 1 || candidates[0].PauseID != "pause_retained_long" ||
		candidates[0].DeleteDurationFrames != 18 {
		t.Fatalf("candidates=%#v", candidates)
	}
	unresolved := agentexec.UnresolvedTalkingHeadPauseDecisions(
		pauses, semantic, timeline.Clip{SourceStartFrame: 0, SourceEndFrame: 100}, nil,
		map[string]struct{}{}, agentexec.MinTalkingHeadPauseCandidateFrames, 8,
	)
	if len(unresolved) != 1 || unresolved[0].PauseID != "pause_retained_long" {
		t.Fatalf("显著气口没有进入必答列表: %#v", unresolved)
	}
	unresolved = agentexec.UnresolvedTalkingHeadPauseDecisions(
		pauses, semantic, timeline.Clip{SourceStartFrame: 0, SourceEndFrame: 100}, nil,
		map[string]struct{}{"pause_retained_long": {}}, agentexec.MinTalkingHeadPauseCandidateFrames, 8,
	)
	if len(unresolved) != 0 {
		t.Fatalf("已明确 remove/preserve 的气口不应再次要求决定: %#v", unresolved)
	}
}

func TestTalkingHeadUnreviewedContentCandidatesAreNonBlockingEvidence(t *testing.T) {
	t.Parallel()
	result := rushestools.ToolResult{
		Status: "succeeded", Observation: "已完成局部编辑。", Data: map[string]any{},
	}
	agentexec.AttachTalkingHeadUnreviewedEvidence(
		&result,
		[]rushestools.SpeechPauseEvidence{{PauseID: "pause_outside_target"}},
		[]rushestools.SpeechRepetitionEvidence{{RepetitionID: "repeat_outside_target"}},
		[]rushestools.SpeechFragmentEvidence{{FragmentID: "fragment_outside_target"}},
	)
	if result.Status != "succeeded" ||
		!strings.Contains(result.Observation, "不影响本次合法修改成功") {
		t.Fatalf("未审阅内容候选不应把合法局部编辑变成失败: %#v", result)
	}
	if len(result.Data["unreviewed_pause_candidates"].([]rushestools.SpeechPauseEvidence)) != 1 ||
		len(result.Data["unreviewed_repetition_candidates"].([]rushestools.SpeechRepetitionEvidence)) != 1 ||
		len(result.Data["unreviewed_short_fragment_candidates"].([]rushestools.SpeechFragmentEvidence)) != 1 {
		t.Fatalf("未审阅内容证据没有完整返回: %#v", result.Data)
	}
}

func TestBridgeTalkingHeadRangesAbsorbsDetectedSilentIslandBetweenSemanticDeletes(t *testing.T) {
	t.Parallel()
	deletions := []agentexec.TalkingHeadRange{{Start: 918, End: 965}, {Start: 1017, End: 1571}}
	pauses := []agentexec.SpeechPause{{
		ID: "pause_between_takes", StartFrame: 959, EndFrame: 1017,
		DeleteStart: 961, DeleteEnd: 1015,
	}}
	got := agentexec.BridgeTalkingHeadRanges(deletions, nil, pauses, 12)
	if len(got) != 1 || got[0].Start != 918 || got[0].End != 1571 {
		t.Fatalf("已检测静音岛未并入相邻语义删除: %#v", got)
	}
	withoutEvidence := agentexec.BridgeTalkingHeadRanges(deletions, nil, nil, 12)
	if len(withoutEvidence) != 2 {
		t.Fatalf("没有静音证据的长非语音区间不应自动删除: %#v", withoutEvidence)
	}
	withSpeech := agentexec.BridgeTalkingHeadRanges(
		deletions, []agentexec.TalkingHeadRange{{Start: 980, End: 1000}}, pauses, 12,
	)
	if len(withSpeech) != 2 {
		t.Fatalf("包含保留语音的区间不应并入删除: %#v", withSpeech)
	}
}

func TestTalkingHeadRangeCoveredByRequiresFullCoverage(t *testing.T) {
	t.Parallel()
	ranges := []agentexec.TalkingHeadRange{{Start: 10, End: 20}, {Start: 20, End: 30}}
	if !agentexec.TalkingHeadRangeCoveredBy(agentexec.TalkingHeadRange{Start: 12, End: 28}, ranges) {
		t.Fatal("相邻删除范围合并后应完整覆盖候选")
	}
	if agentexec.TalkingHeadRangeCoveredBy(agentexec.TalkingHeadRange{Start: 5, End: 15}, ranges) {
		t.Fatal("部分重叠不能视为完整处理")
	}
}

func TestTalkingHeadEditRequiresARollRoleAndSpeechIndex(t *testing.T) {
	t.Parallel()
	database := agentTestDatabase(t)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	for _, fixture := range []struct {
		draftID string
		assetID string
		relDir  string
		role    string
		want    string
	}{
		{draftID: "draft_broll_rejected", assetID: "asset_broll_rejected", relDir: "Broll", role: "b_roll", want: "不能作为口播主干"},
		{draftID: "draft_aroll_unindexed", assetID: "asset_aroll_unindexed", relDir: "Aroll", role: "a_roll", want: "尚无持久化逐句索引"},
	} {
		createAgentDraft(t, database, fixture.draftID)
		if _, err := database.Write().ExecContext(t.Context(), `
			INSERT INTO assets(
				asset_id,storage_mode,reference_path,kind,source,filename,hash,size,
				probe_json,ingest_status,understanding_status,usable
			) VALUES(?, 'reference', ?, 'video', 'local_path', ?, ?, 1,
				'{"duration_sec":2,"has_audio":true}', 'ready', 'none', 1)`,
			fixture.assetID, "/tmp/"+fixture.assetID+".mp4", fixture.assetID+".mp4", fixture.assetID,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := database.Write().ExecContext(t.Context(), `
			INSERT INTO draft_asset_links(draft_id,asset_id,rel_dir,linked_at)
			VALUES(?, ?, ?, ?)`,
			fixture.draftID, fixture.assetID, fixture.relDir, time.Now().UTC().Format(time.RFC3339Nano),
		); err != nil {
			t.Fatal(err)
		}
		document, err := timeline.ComposeInitial(fixture.draftID, 1, []timeline.Selection{{
			AssetID: fixture.assetID, AssetKind: "video", SourceStartFrame: 0, SourceEndFrame: 60,
			Role: fixture.role, HasAudio: true,
		}})
		if err != nil {
			t.Fatal(err)
		}
		if persisted, err := service.persistTimeline(t.Context(), fixture.draftID, document, "fixture"); err != nil || persisted.Status != "succeeded" {
			t.Fatalf("persisted=%#v err=%v", persisted, err)
		}
		ctx := rushestools.WithDraftID(t.Context(), fixture.draftID)
		raw, err := service.ExecuteTool(ctx, "timeline.edit_talking_head", rushestools.TalkingHeadEditInput{
			ARollTimelineClipID: "clip_v1_001", RemovePauseIDs: []string{"pause"},
		})
		if err != nil {
			t.Fatal(err)
		}
		result := raw.(rushestools.ToolResult)
		if result.Status != "failed" || !strings.Contains(result.Observation, fixture.want) {
			t.Fatalf("fixture=%#v result=%#v", fixture, result)
		}
	}
}

func assertTalkingHeadFailure(
	t *testing.T,
	service *Service,
	ctx context.Context,
	input rushestools.TalkingHeadEditInput,
	want string,
) {
	t.Helper()
	before, err := timeline.Latest(t.Context(), service.database, "draft_talking_head")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := service.ExecuteTool(ctx, "timeline.edit_talking_head", input)
	if err != nil {
		t.Fatal(err)
	}
	result := raw.(rushestools.ToolResult)
	if result.Status != "failed" || !strings.Contains(result.Observation, want) {
		t.Fatalf("failure=%#v want=%q", result, want)
	}
	after, err := timeline.Latest(t.Context(), service.database, "draft_talking_head")
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != before.Version || after.TimelineID != before.TimelineID {
		t.Fatalf("失败调用修改了时间线: before=%s after=%s", before.TimelineID, after.TimelineID)
	}
}

func fmtJSON(value any) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func timelineTrackClips(document timeline.Document, trackID string) []timeline.Clip {
	for _, track := range document.Tracks {
		if track.TrackID == trackID {
			return track.Clips
		}
	}
	return nil
}
