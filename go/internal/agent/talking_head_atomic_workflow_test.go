package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

// 该 fixture 固化 #140 的真实失败形态：主讲素材已被拆成 8 个当前 clip，
// transcript 同时包含句内重说、跨 4 句的前后同义重讲、显著气口和可覆盖台词。
// 工作流只组合纯读取与通用原子编辑，不把选择或坐标编译藏进另一个口播 wrapper。
func TestIssue140TalkingHeadFixtureUsesReadEvidenceAndAtomicEdits(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const (
		draftID = "draft_issue_140_atomic"
		aRollID = "asset_issue_140_aroll"
		bRollID = "asset_issue_140_keyboard"
	)
	agenttest.CreateAgentDraft(t, database, draftID)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, asset := range []struct {
		id       string
		filename string
		duration float64
		audio    bool
		relDir   string
	}{
		{aRollID, "Tim-Macbook-Neo-Talking.mp4", 60, true, "Aroll"},
		{bRollID, "键盘指纹与键帽特写.mp4", 4, false, "Broll/键盘"},
	} {
		probe, _ := json.Marshal(map[string]any{
			"duration_sec": asset.duration, "has_audio": asset.audio,
		})
		if _, err := database.Write().ExecContext(t.Context(), `
			INSERT INTO assets(
				asset_id,storage_mode,reference_path,kind,source,filename,hash,size,
				probe_json,ingest_status,understanding_status,usable
			) VALUES(?, 'reference', ?, 'video', 'local_path', ?, ?, 1, ?, 'ready', 'ready', 1)`,
			asset.id, "/tmp/"+asset.filename, asset.filename, asset.id, string(probe),
		); err != nil {
			t.Fatal(err)
		}
		if _, err := database.Write().ExecContext(t.Context(), `
			INSERT INTO draft_asset_links(draft_id,asset_id,rel_dir,linked_at)
			VALUES(?, ?, ?, ?)`, draftID, asset.id, asset.relDir, now,
		); err != nil {
			t.Fatal(err)
		}
	}
	summary, _ := json.Marshal(map[string]any{
		"asset_id": bRollID, "semantic_role": "b_roll",
		"overall": "键盘、同色键帽和指纹按键产品特写",
		"segments": []map[string]any{{
			"source_start_frame": 0, "source_end_frame": 90,
			"description": "手指按压键盘右上角指纹键并展示同色键帽",
			"tags":        []string{"键盘", "键帽", "指纹", "产品特写"},
			"quality":     "usable",
		}},
	})
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO material_summaries(
			summary_id,asset_id,version,status,summary_json,fingerprint,prompt_version,created_at
		) VALUES('summary_issue_140',?,1,'ready',?,'issue_140','v3',?)`,
		bRollID, string(summary), now,
	); err != nil {
		t.Fatal(err)
	}

	utterances := issue140AtomicUtterances()
	pauses := []map[string]any{{
		"pause_id":           "pause_issue_140_breath",
		"source_start_frame": 720, "source_end_frame": 780,
		"delete_start_frame": 720, "delete_end_frame": 780,
		"detection_method": "fixture_vad",
	}}
	transcriptResult, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: contracts.ActorAgent,
		ResultRows: reducer.ResultRows{Transcripts: []reducer.TranscriptRow{{
			ID: "transcript_issue_140", AssetID: aRollID, ProviderID: "issue-140-fixture",
			Utterances: utterances, VADSegments: pauses,
		}}},
	})
	if err != nil || transcriptResult.Status != reducer.StatusApplied {
		t.Fatalf("transcript status=%s err=%v", transcriptResult.Status, err)
	}

	selections := make([]timeline.Selection, 0, 8)
	for start := 0; start < 1560; start += 195 {
		selections = append(selections, timeline.Selection{
			AssetID: aRollID, AssetKind: "video", HasAudio: true,
			SourceStartFrame: start, SourceEndFrame: start + 195, Role: "a_roll",
		})
	}
	document, err := timeline.ComposeInitial(draftID, 1, selections)
	if err != nil {
		t.Fatal(err)
	}
	if len(timelineTrackClips(document, "visual_base")) != 8 {
		t.Fatalf("fixture 未形成至少 7 个当前 A-roll clip: %#v", document.Tracks[0])
	}
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	if persisted, persistErr := service.executor.PersistTimeline(
		t.Context(), draftID, document, "issue_140_fixture",
	); persistErr != nil || persisted.Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("persisted=%#v err=%v", persisted, persistErr)
	}
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	trace := []string{}

	inspectRaw, err := service.ExecuteTool(ctx, "timeline.inspect", rushestools.TimelineInspectInput{})
	if err != nil {
		t.Fatal(err)
	}
	trace = append(trace, "timeline.inspect")
	if inspectRaw.(rushestools.ToolResult).Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("timeline.inspect=%#v", inspectRaw)
	}
	speechRaw, err := service.ExecuteTool(ctx, "speech.search", rushestools.SpeechSearchInput{
		AssetID: aRollID, IncludeWords: true, MaxWords: 2000, MaxUtterances: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	trace = append(trace, "speech.search")
	speech := speechRaw.(rushestools.SpeechSearchResult)
	similarity := issue140KeyboardSimilarity(t, speech.SimilarPairs)
	repetition := issue140OpeningRepetition(t, speech.Repetitions)
	if len(speech.Pauses) != 1 {
		t.Fatalf("pause evidence=%#v", speech.Pauses)
	}
	searchRaw, err := service.ExecuteTool(ctx, "shot.search", rushestools.ShotSearchInput{
		Query: "键盘 同色键帽 指纹按键", SemanticRoles: []string{"b_roll"},
		MinDurationFrames: 45, Limit: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	trace = append(trace, "shot.search")
	search := searchRaw.(rushestools.ShotSearchResult)
	if len(search.Shots) != 1 || search.Shots[0].AssetID != bRollID {
		t.Fatalf("shot.search=%#v", search)
	}

	// 模型明确选择：跨句重说删除较早一遍；句内重说删除较早一遍；显著气口删除。
	successfulAtomicEdits := 0
	failedAtomicAttempts := 0
	for index, selected := range [][2]int{
		{similarity.EarlierSourceStartFrame, similarity.EarlierSourceEndFrame},
		{repetition.EarlierSourceStartFrame, repetition.EarlierSourceEndFrame},
		{speech.Pauses[0].DeleteStartFrame, speech.Pauses[0].DeleteEndFrame},
	} {
		if index == 1 {
			beforeFailure, latestErr := timeline.Latest(t.Context(), database, draftID)
			if latestErr != nil {
				t.Fatal(latestErr)
			}
			failedRaw, failedErr := service.ExecuteTool(ctx, "timeline.delete", rushestools.TimelineDeleteInput{
				"kind": "delete_clip", "timeline_clip_id": "clip_stale_issue_140",
			})
			if failedErr != nil {
				t.Fatal(failedErr)
			}
			failed := failedRaw.(rushestools.ToolResult)
			afterFailure, latestErr := timeline.Latest(t.Context(), database, draftID)
			if latestErr != nil {
				t.Fatal(latestErr)
			}
			if failed.Status != string(rushestools.StatusFailed) ||
				afterFailure.Version != beforeFailure.Version ||
				sourceCoverage(afterFailure, aRollID, similarity.EarlierSourceStartFrame, similarity.EarlierSourceEndFrame) != 0 {
				t.Fatalf(
					"失败原语污染已有成功版本: result=%#v version=%d->%d",
					failed, beforeFailure.Version, afterFailure.Version,
				)
			}
			failedAtomicAttempts++
			trace = append(trace, "timeline.delete:failed")
		}
		deleteIssue140SourceRange(t, service, ctx, draftID, aRollID, selected[0], selected[1])
		successfulAtomicEdits++
		trace = append(trace, "timeline.delete")
	}
	cleaned, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil {
		t.Fatal(err)
	}
	if sourceCoverage(cleaned, aRollID, similarity.EarlierSourceStartFrame, similarity.EarlierSourceEndFrame) != 0 ||
		sourceCoverage(cleaned, aRollID, similarity.LaterSourceStartFrame, similarity.LaterSourceEndFrame) !=
			similarity.LaterSourceEndFrame-similarity.LaterSourceStartFrame ||
		sourceCoverage(cleaned, aRollID, repetition.EarlierSourceStartFrame, repetition.EarlierSourceEndFrame) != 0 {
		t.Fatalf("相似/句内重说选择未精确落实: %#v", timelineTrackClips(cleaned, "visual_base"))
	}

	anchorStart, anchorEnd, ok := sourceRangeOnCurrentTimeline(cleaned, aRollID, 1320, 1440)
	if !ok || anchorEnd-anchorStart < 90 {
		t.Fatalf("保留指纹台词映射=%d-%d ok=%v", anchorStart, anchorEnd, ok)
	}
	insertRaw, err := service.ExecuteTool(ctx, "timeline.insert", rushestools.TimelineInsertInput{
		"kind": "insert_clip", "track_id": "visual_overlay",
		"asset_id": bRollID, "role": "b_roll",
		"source_start_frame": 0, "source_end_frame": 90,
		"timeline_start_frame": anchorStart,
		"metadata": map[string]any{
			"shot_id": search.Shots[0].ShotID, "anchor_source_start_frame": 1320,
			"anchor_source_end_frame": 1440,
		},
	})
	if err != nil || insertRaw.(rushestools.ToolResult).Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("insert B-roll=%#v err=%v", insertRaw, err)
	}
	successfulAtomicEdits++
	trace = append(trace, "timeline.insert")
	withBroll, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil {
		t.Fatal(err)
	}
	overlays := timelineTrackClips(withBroll, "visual_overlay")
	if len(overlays) != 1 || overlays[0].TimelineEndFrame-overlays[0].TimelineStartFrame < 45 {
		t.Fatalf("B-roll=%#v", overlays)
	}
	fadeRaw, err := service.ExecuteTool(ctx, "timeline.update", rushestools.TimelineUpdateInput{
		"kind": "set_clip_fades", "timeline_clip_id": overlays[0].TimelineClipID,
		"fade_in_frames": 7, "fade_out_frames": 7,
	})
	if err != nil || fadeRaw.(rushestools.ToolResult).Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("fade B-roll=%#v err=%v", fadeRaw, err)
	}
	successfulAtomicEdits++
	trace = append(trace, "timeline.update")
	checkRaw, err := service.ExecuteTool(ctx, "timeline.check", rushestools.TimelineCheckInput{})
	if err != nil || checkRaw.(rushestools.ToolResult).Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("timeline.check=%#v err=%v", checkRaw, err)
	}
	quality, ok := checkRaw.(rushestools.ToolResult).Data["speech_quality"].(map[string]any)
	if !ok ||
		quality["residual_breath_count"] != 0 ||
		quality["short_retained_island_count"] != 0 ||
		quality["short_b_roll_clip_count"] != 0 {
		t.Fatalf("口播质检未收敛: %#v", quality)
	}
	trace = append(trace, "timeline.check")

	final, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Version != 6 || !timeline.Validate(final).Valid {
		t.Fatalf("final version=%d report=%#v", final.Version, timeline.Validate(final))
	}
	visualCoverage := clipSourceRanges(timelineTrackClips(final, "visual_base"))
	audioCoverage := clipSourceRanges(timelineTrackClips(final, "original_audio"))
	if !reflect.DeepEqual(visualCoverage, audioCoverage) {
		t.Fatalf("派生原声音画范围漂移: visual=%v audio=%v", visualCoverage, audioCoverage)
	}
	if overlays = timelineTrackClips(final, "visual_overlay"); len(overlays) != 1 ||
		overlays[0].FadeInFrames != 7 || overlays[0].FadeOutFrames != 7 {
		t.Fatalf("final overlay=%#v", overlays)
	}
	var singleOpBatches int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM timeline_edit_batches
		WHERE draft_id=? AND json_array_length(operations_json)=1`, draftID,
	).Scan(&singleOpBatches); err != nil || singleOpBatches != 5 {
		t.Fatalf("single-op batches=%d err=%v", singleOpBatches, err)
	}
	for _, name := range trace {
		if name == "timeline.edit_talking_head" {
			t.Fatalf("trace 仍调用旧口播工具: %v", trace)
		}
	}
	if successfulAtomicEdits != 5 || failedAtomicAttempts != 1 {
		t.Fatalf(
			"原子重试统计不符: success=%d failed=%d trace=%v",
			successfulAtomicEdits, failedAtomicAttempts, trace,
		)
	}
	t.Logf(
		"#140 atomic trace: reads=3 successful_edits=%d failed_attempts=%d retry_scope=failed_primitive schema_runes=19254 trace=%v",
		successfulAtomicEdits, failedAtomicAttempts, trace,
	)
}

func issue140AtomicUtterances() []map[string]any {
	return []map[string]any{
		{
			"utterance_id": "utt_issue_140_opening", "source_start_frame": 0,
			"source_end_frame": 180, "text": "柑橘色偏绿我不喜欢，柑橘色偏绿我不喜欢。",
			"words": []map[string]any{
				{"word_id": "opening_1", "source_start_frame": 0, "source_end_frame": 30, "text": "柑橘色"},
				{"word_id": "opening_2", "source_start_frame": 30, "source_end_frame": 60, "text": "偏绿"},
				{"word_id": "opening_3", "source_start_frame": 60, "source_end_frame": 90, "text": "我不喜欢", "punctuation": "，"},
				{"word_id": "opening_4", "source_start_frame": 90, "source_end_frame": 120, "text": "柑橘色"},
				{"word_id": "opening_5", "source_start_frame": 120, "source_end_frame": 150, "text": "偏绿"},
				{"word_id": "opening_6", "source_start_frame": 150, "source_end_frame": 180, "text": "我不喜欢", "punctuation": "。"},
			},
		},
		{"utterance_id": "utt_keyboard_early_1", "source_start_frame": 240, "source_end_frame": 330, "text": "这次键盘创新主要是同色设计。"},
		{"utterance_id": "utt_keyboard_early_2", "source_start_frame": 330, "source_end_frame": 420, "text": "键帽和机身保持完全相同的颜色。"},
		{"utterance_id": "utt_keyboard_early_3", "source_start_frame": 420, "source_end_frame": 510, "text": "但这块键盘依然没有背光。"},
		{"utterance_id": "utt_keyboard_early_4", "source_start_frame": 510, "source_end_frame": 600, "text": "实际打字手感比较扎实。"},
		{"utterance_id": "utt_bridge", "source_start_frame": 600, "source_end_frame": 720, "text": "接下来再看屏幕和接口。"},
		{"utterance_id": "utt_after_pause", "source_start_frame": 780, "source_end_frame": 900, "text": "我再完整讲一遍键盘体验。"},
		{"utterance_id": "utt_keyboard_late_1", "source_start_frame": 900, "source_end_frame": 990, "text": "这次键盘创新主要是同色设计。"},
		{"utterance_id": "utt_keyboard_late_2", "source_start_frame": 990, "source_end_frame": 1080, "text": "键帽和机身保持完全相同的颜色。"},
		{"utterance_id": "utt_keyboard_late_3", "source_start_frame": 1080, "source_end_frame": 1170, "text": "但这块键盘依然没有背光。"},
		{"utterance_id": "utt_keyboard_late_4", "source_start_frame": 1170, "source_end_frame": 1260, "text": "实际打字手感比较扎实。"},
		{"utterance_id": "utt_fingerprint_retained", "source_start_frame": 1320, "source_end_frame": 1440, "text": "指纹解锁按键仍然位于键盘右上角。"},
		{"utterance_id": "utt_outro", "source_start_frame": 1440, "source_end_frame": 1560, "text": "这就是完整的使用结论。"},
	}
}

func issue140KeyboardSimilarity(
	t *testing.T,
	pairs []rushestools.SpeechSimilarityEvidence,
) rushestools.SpeechSimilarityEvidence {
	t.Helper()
	for _, pair := range pairs {
		if pair.Method == "normalized_character_lcs_dice" &&
			strings.Contains(pair.EarlierText, "同色") &&
			strings.Contains(pair.EarlierText, "背光") &&
			strings.Contains(pair.LaterText, "同色") &&
			strings.Contains(pair.LaterText, "背光") {
			return pair
		}
	}
	t.Fatalf("未检出 #140 跨句键盘重说: %#v", pairs)
	return rushestools.SpeechSimilarityEvidence{}
}

func issue140OpeningRepetition(
	t *testing.T,
	repetitions []rushestools.SpeechRepetitionEvidence,
) rushestools.SpeechRepetitionEvidence {
	t.Helper()
	for _, repetition := range repetitions {
		if repetition.UtteranceID == "utt_issue_140_opening" &&
			repetition.EarlierSourceStartFrame == 0 &&
			repetition.LaterSourceStartFrame == 90 {
			return repetition
		}
	}
	t.Fatalf("未检出 #140 开头句内重说: %#v", repetitions)
	return rushestools.SpeechRepetitionEvidence{}
}

func deleteIssue140SourceRange(
	t *testing.T,
	service *Service,
	ctx context.Context,
	draftID string,
	assetID string,
	sourceStart int,
	sourceEnd int,
) {
	t.Helper()
	document, err := timeline.Latest(t.Context(), service.database, draftID)
	if err != nil {
		t.Fatal(err)
	}
	start, end, ok := sourceRangeOnCurrentTimeline(document, assetID, sourceStart, sourceEnd)
	if !ok || end <= start {
		t.Fatalf("source range %d-%d 无当前时间线映射", sourceStart, sourceEnd)
	}
	raw, err := service.ExecuteTool(ctx, "timeline.delete", rushestools.TimelineDeleteInput{
		"kind": "delete_range", "start_frame": start, "end_frame": end,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result := raw.(rushestools.ToolResult); result.Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("delete source %d-%d timeline %d-%d result=%#v", sourceStart, sourceEnd, start, end, result)
	}
}

func sourceCoverage(document timeline.Document, assetID string, start int, end int) int {
	coverage := 0
	for _, clip := range timelineTrackClips(document, "visual_base") {
		if clip.AssetID != assetID {
			continue
		}
		coverage += max(0, min(end, clip.SourceEndFrame)-max(start, clip.SourceStartFrame))
	}
	return coverage
}

func clipSourceRanges(clips []timeline.Clip) [][2]int {
	result := make([][2]int, 0, len(clips))
	for _, clip := range clips {
		result = append(result, [2]int{clip.SourceStartFrame, clip.SourceEndFrame})
	}
	return result
}
