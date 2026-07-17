package agent

import (
	"context"
	"math/rand"
	"strconv"
	"testing"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

// setupEvidenceCoordsDraft 建一个草稿：单条 A-roll 素材 + 持久化逐句索引 + 一条
// ComposeInitial([0,300]) 后再叠加 cuts 的时间线，模拟「首剪把 A-roll 多次裁剪/拆分」
// 后的状态。返回 service、带 draft 的 ctx 与裁剪后的时间线文档。
func setupEvidenceCoordsDraft(
	t *testing.T,
	draftID, assetID string,
	utterances, pauses []map[string]any,
	cuts []map[string]any,
) (*Service, context.Context, timeline.Document) {
	t.Helper()
	database := agentTestDatabase(t)
	createAgentDraft(t, database, draftID)
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
	document, err := timeline.ComposeInitial(draftID, 1, []timeline.Selection{{
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
	if persisted, persistErr := service.persistTimeline(
		t.Context(), draftID, document, "fixture",
	); persistErr != nil || persisted.Status != "succeeded" {
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
) rushestools.SpeechInspectResult {
	t.Helper()
	raw, err := service.ExecuteTool(ctx, "speech.inspect", rushestools.SpeechInspectInput{
		TimelineClipID: clipID, IncludeWords: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw.(rushestools.SpeechInspectResult)
}

func editTalkingHead(
	t *testing.T, service *Service, ctx context.Context, input rushestools.TalkingHeadEditInput,
) rushestools.ToolResult {
	t.Helper()
	raw, err := service.ExecuteTool(ctx, "timeline.edit_talking_head", input)
	if err != nil {
		t.Fatal(err)
	}
	return raw.(rushestools.ToolResult)
}

// TestTalkingHeadEvidenceCoordsRepairsStraddlingEvidence 复现锚点案例第二轮的三个
// 修复调用（pause 删除 / fragment 删除 / repetition 删除）：首剪把 A-roll 裁剪成
// 「前半保留后半已删」后，跨已删帧的证据必须能被成功删除，不再因坐标系不一致而失败。
func TestTalkingHeadEvidenceCoordsRepairsStraddlingEvidence(t *testing.T) {
	t.Parallel()

	// 两刀裁剪：中段删 [C,C+30] 把基础 clip 断成两段（前半 [0,C] 保留、[C,C+30]
	// 已删），再删尾部 [250,270] 使其成为「多次裁剪」状态。目标始终是 [0,C]。
	tailCut := map[string]any{"kind": "delete_range", "start_frame": 250, "end_frame": 270}

	t.Run("pause", func(t *testing.T) {
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
				tailCut,
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
		edit := editTalkingHead(t, service, ctx, rushestools.TalkingHeadEditInput{
			ARollTimelineClipID: clipID, RemovePauseIDs: []string{"pause_tail"},
		})
		if edit.Status != "succeeded" {
			t.Fatalf("跨界气口删除应成功: %#v", edit)
		}
	})

	t.Run("fragment", func(t *testing.T) {
		t.Parallel()
		const clipEnd = 235
		service, ctx, document := setupEvidenceCoordsDraft(t, "draft_ec_fragment", "asset_ec_fragment",
			[]map[string]any{
				{"utterance_id": "utt_keep", "source_start_frame": 0, "source_end_frame": 180,
					"text": "前面这一整段正常口播内容全部保留下来继续往下讲。"},
				{"utterance_id": "utt_frag", "source_start_frame": 195, "source_end_frame": 290,
					"text": "但是没有同时。", "words": []map[string]any{
						{"word_id": "fb_but", "source_start_frame": 195, "source_end_frame": 210, "text": "但是"},
						{"word_id": "fb_no", "source_start_frame": 210, "source_end_frame": 240, "text": "没"},
						{"word_id": "fb_have", "source_start_frame": 265, "source_end_frame": 275, "text": "有"},
						{"word_id": "fb_same", "source_start_frame": 275, "source_end_frame": 290, "text": "同时", "punctuation": "。"},
					}},
			},
			[]map[string]any{
				{"pause_id": "pause_ff", "source_start_frame": 240, "source_end_frame": 265,
					"delete_start_frame": 242, "delete_end_frame": 263},
			},
			[]map[string]any{
				{"kind": "delete_range", "start_frame": clipEnd, "end_frame": clipEnd + 30},
				tailCut,
			},
		)
		clipID := clipBySourceRange(t, document, 0, clipEnd)
		inspect := inspectClip(t, service, ctx, clipID)
		fragmentID := ""
		for _, fragment := range inspect.ShortFragments {
			if fragment.StartWordID == "fb_but" {
				fragmentID = fragment.FragmentID
			}
		}
		if fragmentID == "" {
			t.Fatalf("未检测到跨界的重启前缀短片段: %#v", inspect.ShortFragments)
		}
		edit := editTalkingHead(t, service, ctx, rushestools.TalkingHeadEditInput{
			ARollTimelineClipID: clipID,
			ShortFragmentDecisions: []rushestools.TalkingHeadFragmentDecision{
				{FragmentID: fragmentID, Action: "remove"},
			},
		})
		if edit.Status != "succeeded" {
			t.Fatalf("跨界短片段删除应成功: %#v", edit)
		}
	})

	t.Run("repetition", func(t *testing.T) {
		t.Parallel()
		const clipEnd = 150
		service, ctx, document := setupEvidenceCoordsDraft(t, "draft_ec_repeat", "asset_ec_repeat",
			[]map[string]any{
				{"utterance_id": "utt_intro", "source_start_frame": 0, "source_end_frame": 35,
					"text": "先做一个开头的介绍作为保留内容。"},
				{"utterance_id": "utt_repeat", "source_start_frame": 40, "source_end_frame": 170,
					"text": "这个柑橘色看起来偏绿反正这个这个柑橘色看起来偏绿。",
					"words": []map[string]any{
						{"word_id": "re_this", "source_start_frame": 40, "source_end_frame": 50, "text": "这个"},
						{"word_id": "re_color", "source_start_frame": 50, "source_end_frame": 60, "text": "柑橘色"},
						{"word_id": "re_look", "source_start_frame": 60, "source_end_frame": 75, "text": "看起来"},
						{"word_id": "re_green", "source_start_frame": 75, "source_end_frame": 90, "text": "偏绿"},
						{"word_id": "re_filler", "source_start_frame": 95, "source_end_frame": 105, "text": "反正"},
						{"word_id": "rl_this1", "source_start_frame": 110, "source_end_frame": 120, "text": "这个"},
						{"word_id": "rl_this2", "source_start_frame": 120, "source_end_frame": 130, "text": "这个"},
						{"word_id": "rl_color", "source_start_frame": 130, "source_end_frame": 140, "text": "柑橘色"},
						{"word_id": "rl_look", "source_start_frame": 140, "source_end_frame": 155, "text": "看起来"},
						{"word_id": "rl_green", "source_start_frame": 155, "source_end_frame": 170, "text": "偏绿", "punctuation": "。"},
					}},
			},
			nil,
			[]map[string]any{
				{"kind": "delete_range", "start_frame": clipEnd, "end_frame": clipEnd + 30},
				tailCut,
			},
		)
		clipID := clipBySourceRange(t, document, 0, clipEnd)
		inspect := inspectClip(t, service, ctx, clipID)
		repetitionID := ""
		for _, repetition := range inspect.Repetitions {
			if repetition.Kind == "repeated_phrase" {
				repetitionID = repetition.RepetitionID
			}
		}
		if repetitionID == "" {
			t.Fatalf("未检测到跨界的句内重复短语: %#v", inspect.Repetitions)
		}
		edit := editTalkingHead(t, service, ctx, rushestools.TalkingHeadEditInput{
			ARollTimelineClipID: clipID,
			RepetitionDecisions: []rushestools.TalkingHeadRepetitionDecision{
				{RepetitionID: repetitionID, Action: "remove_later"},
			},
		})
		if edit.Status != "succeeded" {
			t.Fatalf("跨界句内重复删除应成功: %#v", edit)
		}
	})
}

// TestTalkingHeadEvidenceCoordsInspectEditContract 遍历多种「多次裁剪/拆分」时间线，
// 断言 speech.inspect(clip) 返回的每条证据都能被 edit_talking_head(同 clip) 的选择器
// 接受——彻底消除「inspect 给的 ID edit 不认」。
func TestTalkingHeadEvidenceCoordsInspectEditContract(t *testing.T) {
	t.Parallel()
	utterances := []map[string]any{
		{"utterance_id": "utt_a", "source_start_frame": 0, "source_end_frame": 70,
			"text": "第一句台词。", "words": []map[string]any{
				{"word_id": "a1", "source_start_frame": 0, "source_end_frame": 25, "text": "第一"},
				{"word_id": "a2", "source_start_frame": 25, "source_end_frame": 50, "text": "句"},
				{"word_id": "a3", "source_start_frame": 50, "source_end_frame": 70, "text": "台词", "punctuation": "。"},
			}},
		{"utterance_id": "utt_b", "source_start_frame": 80, "source_end_frame": 170,
			"text": "第二句稍微长一点的台词内容。", "words": []map[string]any{
				{"word_id": "b1", "source_start_frame": 80, "source_end_frame": 110, "text": "第二句"},
				{"word_id": "b2", "source_start_frame": 110, "source_end_frame": 140, "text": "稍微长一点"},
				{"word_id": "b3", "source_start_frame": 140, "source_end_frame": 170, "text": "的台词内容", "punctuation": "。"},
			}},
		{"utterance_id": "utt_c", "source_start_frame": 185, "source_end_frame": 300,
			"text": "第三句结尾的台词也要覆盖到位。", "words": []map[string]any{
				{"word_id": "c1", "source_start_frame": 185, "source_end_frame": 220, "text": "第三句"},
				{"word_id": "c2", "source_start_frame": 220, "source_end_frame": 260, "text": "结尾的台词"},
				{"word_id": "c3", "source_start_frame": 260, "source_end_frame": 300, "text": "也要覆盖到位", "punctuation": "。"},
			}},
	}
	pauses := []map[string]any{
		{"pause_id": "pause_ab", "source_start_frame": 70, "source_end_frame": 80,
			"delete_start_frame": 72, "delete_end_frame": 78},
		{"pause_id": "pause_bc", "source_start_frame": 170, "source_end_frame": 185,
			"delete_start_frame": 172, "delete_end_frame": 183},
	}

	// 表驱动覆盖典型形态；随机化（固定种子）再补充多种裁剪拆分组合。
	cutTable := [][]map[string]any{
		{}, // 未裁剪基线
		{{"kind": "delete_range", "start_frame": 130, "end_frame": 160}},                                                                              // 中段删，断成两段
		{{"kind": "split_clip", "timeline_clip_id": "clip_v1_001", "split_frame": 100}},                                                               // 纯拆分
		{{"kind": "delete_range", "start_frame": 220, "end_frame": 300}},                                                                              // 尾部裁剪
		{{"kind": "delete_range", "start_frame": 0, "end_frame": 40}},                                                                                 // 头部裁剪
		{{"kind": "split_clip", "timeline_clip_id": "clip_v1_001", "split_frame": 100}, {"kind": "delete_range", "start_frame": 40, "end_frame": 75}}, // 拆分后再删中段
		{{"kind": "delete_range", "start_frame": 60, "end_frame": 90}, {"kind": "delete_range", "start_frame": 150, "end_frame": 175}},                // 多次裁剪
	}
	random := rand.New(rand.NewSource(20260717))
	for iteration := 0; iteration < 24; iteration++ {
		cutTable = append(cutTable, randomEvidenceCuts(random))
	}

	for index, cuts := range cutTable {
		draftID := "draft_ec_contract_" + strconv.Itoa(index)
		service, ctx, document := setupEvidenceCoordsDraft(
			t, draftID, "asset_ec_contract_"+strconv.Itoa(index), utterances, pauses, cuts,
		)
		transcript, err := storage.LatestTranscript(t.Context(), service.database.Read(), "asset_ec_contract_"+strconv.Itoa(index))
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := decodeSpeechUtterances(transcript.Utterances)
		if err != nil {
			t.Fatal(err)
		}
		decodedPauses, err := decodeSpeechPauses(transcript.VADSegments)
		if err != nil {
			t.Fatal(err)
		}
		clampedPauses := clampSpeechPausesToWordBoundaries("asset_ec_contract_"+strconv.Itoa(index), decodedPauses, decoded)
		utteranceByID := make(map[string]speechUtterance, len(decoded))
		wordSequence := []speechWord{}
		for _, utterance := range decoded {
			utteranceByID[utterance.ID] = utterance
			wordSequence = append(wordSequence, utterance.Words...)
		}
		pauseByID := make(map[string]speechPause, len(clampedPauses))
		for _, pause := range clampedPauses {
			pauseByID[pause.ID] = pause
		}
		for _, clip := range timelineTrackClips(document, "visual_base") {
			inspect := inspectClip(t, service, ctx, clip.TimelineClipID)
			utteranceIDs := make([]string, 0, len(inspect.Utterances))
			wordRanges := []rushestools.TalkingHeadWordRange{}
			for _, utterance := range inspect.Utterances {
				utteranceIDs = append(utteranceIDs, utterance.UtteranceID)
				for _, word := range utterance.Words {
					wordRanges = append(wordRanges, rushestools.TalkingHeadWordRange{StartWordID: word.WordID})
				}
			}
			pauseIDs := make([]string, 0, len(inspect.Pauses))
			for _, pause := range inspect.Pauses {
				pauseIDs = append(pauseIDs, pause.PauseID)
			}
			if _, invalid := selectTalkingHeadUtterances(utteranceIDs, utteranceByID, clip); len(invalid) > 0 {
				t.Fatalf("cut#%d clip=%s inspect utterance 被 edit 拒收: %#v", index, clip.TimelineClipID, invalid)
			}
			if _, _, invalid := selectTalkingHeadWordRanges(wordRanges, wordSequence, clip); len(invalid) > 0 {
				t.Fatalf("cut#%d clip=%s inspect word 被 edit 拒收: %#v", index, clip.TimelineClipID, invalid)
			}
			if _, invalid := selectTalkingHeadPauses(pauseIDs, pauseByID, clip); len(invalid) > 0 {
				t.Fatalf("cut#%d clip=%s inspect pause 被 edit 拒收: %#v", index, clip.TimelineClipID, invalid)
			}
		}
	}
}

// TestTalkingHeadEvidenceCoordsRelocatesInvalidEvidence 断言当证据完全落在目标
// clip 之外时，失败 observation 会指出它当前实际所属的 clip；未知 ID 则不给提示。
func TestTalkingHeadEvidenceCoordsRelocatesInvalidEvidence(t *testing.T) {
	t.Parallel()
	service, ctx, document := setupEvidenceCoordsDraft(t, "draft_ec_relocate", "asset_ec_relocate",
		[]map[string]any{
			{"utterance_id": "utt_first", "source_start_frame": 0, "source_end_frame": 140,
				"text": "第一段保留在前一个片段里的内容。"},
			{"utterance_id": "utt_second", "source_start_frame": 200, "source_end_frame": 260,
				"text": "第二段内容。", "words": []map[string]any{
					{"word_id": "s1", "source_start_frame": 200, "source_end_frame": 225, "text": "第二段"},
					{"word_id": "s2", "source_start_frame": 225, "source_end_frame": 260, "text": "内容", "punctuation": "。"},
				}},
		},
		[]map[string]any{
			{"pause_id": "pause_second", "source_start_frame": 265, "source_end_frame": 290,
				"delete_start_frame": 267, "delete_end_frame": 288},
		},
		[]map[string]any{
			{"kind": "split_clip", "timeline_clip_id": "clip_v1_001", "split_frame": 150},
		},
	)
	firstClip := clipBySourceRange(t, document, 0, 150)
	secondClip := clipBySourceRange(t, document, 150, 300)
	edit := editTalkingHead(t, service, ctx, rushestools.TalkingHeadEditInput{
		ARollTimelineClipID: firstClip,
		RemoveUtteranceIDs:  []string{"utt_second", "utt_unknown"},
		RemoveWordRanges:    []rushestools.TalkingHeadWordRange{{StartWordID: "s1"}},
		RemovePauseIDs:      []string{"pause_second"},
	})
	if edit.Status != "failed" {
		t.Fatalf("落在其他 clip 的证据应判失败: %#v", edit)
	}
	hints, ok := edit.Data["evidence_current_clips"].([]map[string]any)
	if !ok || len(hints) != 3 {
		t.Fatalf("应为三条已知但错位的证据给出 clip 提示: %#v", edit.Data["evidence_current_clips"])
	}
	for _, hint := range hints {
		if hint["current_timeline_clip_id"] != secondClip {
			t.Fatalf("错位证据的 clip 提示应指向后一个片段 %s: %#v", secondClip, hint)
		}
	}
	invalidUtterances, _ := edit.Data["invalid_utterance_ids"].([]string)
	if len(invalidUtterances) != 2 {
		t.Fatalf("未知 ID 与错位 ID 都应记为非法: %#v", invalidUtterances)
	}
}

// TestTalkingHeadSourceRangeClipPicksOverlappingSameAssetClip 校验 clip 归属查询只在
// 主视频轨、同素材范围内选择重叠最多的 clip，素材不匹配或轨道不对都跳过。
func TestTalkingHeadSourceRangeClipPicksOverlappingSameAssetClip(t *testing.T) {
	t.Parallel()
	document := timeline.Document{Tracks: []timeline.Track{
		{TrackID: "visual_base", Clips: []timeline.Clip{
			{TimelineClipID: "c_head", AssetID: "a", SourceStartFrame: 0, SourceEndFrame: 100},
			{TimelineClipID: "c_tail", AssetID: "a", SourceStartFrame: 150, SourceEndFrame: 300},
			{TimelineClipID: "c_other", AssetID: "b", SourceStartFrame: 100, SourceEndFrame: 150},
		}},
		{TrackID: "visual_overlay", Clips: []timeline.Clip{
			{TimelineClipID: "c_overlay", AssetID: "a", SourceStartFrame: 200, SourceEndFrame: 260},
		}},
	}}
	if id, ok := talkingHeadSourceRangeClip(document, "a", 200, 260); !ok || id != "c_tail" {
		t.Fatalf("与 [200,260] 重叠最多的同素材主视频 clip 应为 c_tail: clip=%q ok=%v", id, ok)
	}
	if id, ok := talkingHeadSourceRangeClip(document, "missing", 200, 260); ok || id != "" {
		t.Fatalf("素材不在主视频轨时应返回空: clip=%q ok=%v", id, ok)
	}
}

// randomEvidenceCuts 用固定种子的随机源生成 1-3 刀裁剪，制造多样的「多次裁剪」状态。
func randomEvidenceCuts(random *rand.Rand) []map[string]any {
	cuts := []map[string]any{}
	duration := 300
	count := 1 + random.Intn(3)
	for attempt := 0; attempt < count && duration > 80; attempt++ {
		span := 20 + random.Intn(40)
		if span >= duration-40 {
			continue
		}
		start := random.Intn(duration - span)
		cuts = append(cuts, map[string]any{
			"kind": "delete_range", "start_frame": start, "end_frame": start + span,
		})
		duration -= span
	}
	return cuts
}
