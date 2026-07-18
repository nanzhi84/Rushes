package agentexec

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/media"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

type fakeSpeechRecognizer struct {
	calls        int
	noWordsCalls map[int]bool
}

func (recognizer *fakeSpeechRecognizer) Recognize(
	_ context.Context, request contracts.SpeechRecognitionRequest,
) (contracts.SpeechRecognitionResult, error) {
	recognizer.calls++
	if info, err := os.Stat(request.AudioPath); err != nil || info.Size() == 0 {
		return contracts.SpeechRecognitionResult{}, os.ErrInvalid
	}
	if recognizer.noWordsCalls[recognizer.calls] {
		return contracts.SpeechRecognitionResult{}, contracts.ErrSpeechNoWords
	}
	return contracts.SpeechRecognitionResult{
		Text: "第一句口播。第二句口播！", Language: "zh", Emotion: "neutral", ProviderID: "fake-asr",
	}, nil
}

func TestSpeechInspectSkipsOnlyChunksWithoutWords(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_speech_partial")
	audio := createSpeechFixtureAudioDuration(t, database.Paths.Temporary, "partial", 30)
	insertSpeechFixtureAsset(t, database, "draft_speech_partial", "asset_speech_partial", audio)
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	recognizer := &fakeSpeechRecognizer{noWordsCalls: map[int]bool{1: true}}
	exec.SetSpeechRecognizer(recognizer)
	result, err := exec.ToolInspectSpeech(t.Context(), "draft_speech_partial", rushestools.SpeechInspectInput{
		AssetID: "asset_speech_partial", Language: "zh",
	})
	if err != nil {
		t.Fatal(err)
	}
	if recognizer.calls != 2 || result.UtteranceTotal != 2 ||
		result.ProviderID != "fake-asr+local-frame-alignment" {
		t.Fatalf("calls=%d result=%#v", recognizer.calls, result)
	}
}

func TestSpeechInspectBuildsSidecarTranscriptThenReusesCache(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_speech_sidecar")
	audio := createSpeechFixtureAudio(t, database.Paths.Temporary, "sidecar")
	srt := audio[:len(audio)-len(filepath.Ext(audio))] + ".srt"
	if err := os.WriteFile(srt, []byte(
		"1\n00:00:00,100 --> 00:00:00,800\n第一句口播\n\n"+
			"2\n00:00:01,000 --> 00:00:01,800\n第一句口播\n\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	insertSpeechFixtureAsset(t, database, "draft_speech_sidecar", "asset_speech_sidecar", audio)
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.ToolInspectSpeech(
		t.Context(), "draft_speech_sidecar", rushestools.SpeechInspectInput{},
	); err == nil {
		t.Fatal("缺少 asset_id/timeline_clip_id 应失败")
	}
	if _, err := exec.ToolInspectSpeech(t.Context(), "draft_speech_sidecar", rushestools.SpeechInspectInput{
		AssetID: "missing",
	}); err == nil {
		t.Fatal("未知素材应失败")
	}
	first, err := exec.ToolInspectSpeech(t.Context(), "draft_speech_sidecar", rushestools.SpeechInspectInput{
		AssetID: "asset_speech_sidecar", IncludeSimilar: BoolPointer(true), IncludePauses: BoolPointer(false),
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.CacheHit || first.ProviderID != "sidecar-srt" || first.UtteranceTotal != 2 ||
		len(first.SimilarPairs) != 1 || len(first.Pauses) != 0 {
		t.Fatalf("first=%#v", first)
	}
	for _, required := range []string{
		"repetition_decisions", "按可安全删除时长从长到短", "previous_context",
		"pause_decisions", "short_fragment_decisions",
	} {
		if !strings.Contains(first.UsageNote, required) {
			t.Fatalf("usage note missing %q: %s", required, first.UsageNote)
		}
	}
	second, err := exec.ToolInspectSpeech(t.Context(), "draft_speech_sidecar", rushestools.SpeechInspectInput{
		AssetID: "asset_speech_sidecar", Query: "第一句", MaxUtterances: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !second.CacheHit || len(second.Utterances) != 1 || !second.Truncated {
		t.Fatalf("second=%#v", second)
	}
	refreshed, err := exec.ToolInspectSpeech(t.Context(), "draft_speech_sidecar", rushestools.SpeechInspectInput{
		AssetID: "asset_speech_sidecar", ForceRefresh: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.CacheHit || refreshed.TranscriptID == first.TranscriptID {
		t.Fatalf("refreshed=%#v first=%#v", refreshed, first)
	}
	if _, err := storage.LatestTranscript(t.Context(), database.Read(), "asset_speech_sidecar"); err != nil {
		t.Fatal(err)
	}
}

func TestSpeechInspectUsesChunkedRecognizerWithoutSidecar(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_speech_asr")
	audio := createSpeechFixtureAudio(t, database.Paths.Temporary, "asr")
	insertSpeechFixtureAsset(t, database, "draft_speech_asr", "asset_speech_asr", audio)
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.ToolInspectSpeech(t.Context(), "draft_speech_asr", rushestools.SpeechInspectInput{
		AssetID: "asset_speech_asr",
	}); err == nil {
		t.Fatal("无 sidecar 且未配置 ASR 应失败")
	}
	recognizer := &fakeSpeechRecognizer{}
	exec.SetSpeechRecognizer(recognizer)
	result, err := exec.ToolInspectSpeech(t.Context(), "draft_speech_asr", rushestools.SpeechInspectInput{
		AssetID: "asset_speech_asr", Language: "zh",
	})
	if err != nil {
		t.Fatal(err)
	}
	if recognizer.calls != 1 || result.ProviderID != "fake-asr+local-frame-alignment" ||
		result.UtteranceTotal != 2 || result.Utterances[0].Language != "zh" ||
		result.Utterances[0].Emotion != "neutral" {
		t.Fatalf("calls=%d result=%#v", recognizer.calls, result)
	}
}

func TestSpeechEvidenceHelpersKeepStableRangesAndRejectInvalidRows(t *testing.T) {
	t.Parallel()
	pauses := []SpeechPause{{StartFrame: 90, EndFrame: 120}, {StartFrame: 210, EndFrame: 240}}
	chunks := BuildASRChunks(300, pauses, 180)
	if len(chunks) != 3 || chunks[0] != [2]int{0, 105} || chunks[1] != [2]int{105, 225} ||
		chunks[2] != [2]int{225, 300} {
		t.Fatalf("chunks=%#v", chunks)
	}
	utterances := AlignRecognizedClauses("asset", "第一句。第二句！第三句", "zh", "neutral", 30, 120)
	if len(utterances) != 3 || utterances[0].StartFrame != 30 ||
		utterances[2].EndFrame != 120 || utterances[0].ID == utterances[1].ID {
		t.Fatalf("utterances=%#v", utterances)
	}
	encodedUtterances := EncodeSpeechUtterances(utterances)
	decodedUtterances, err := DecodeSpeechUtterances(encodedUtterances)
	if err != nil || len(decodedUtterances) != len(utterances) {
		t.Fatalf("decoded=%#v err=%v", decodedUtterances, err)
	}
	encodedPauses := EncodeSpeechPauses([]SpeechPause{{
		ID: "pause", StartFrame: 40, EndFrame: 55, DeleteStart: 42, DeleteEnd: 53,
	}})
	if decoded, err := DecodeSpeechPauses(encodedPauses); err != nil || len(decoded) != 1 {
		t.Fatalf("decoded=%#v err=%v", decoded, err)
	}
	if _, err := DecodeSpeechUtterances([]map[string]any{{"utterance_id": "bad"}}); err == nil {
		t.Fatal("无效 utterance 应失败")
	}
	if _, err := DecodeSpeechPauses([]map[string]any{{"pause_id": "bad"}}); err == nil {
		t.Fatal("无效 pause 应失败")
	}
	timestamped := AlignTimestampedRecognition("asset", contracts.SpeechRecognitionResult{
		Text: "第一句。第二句。", Language: "zh", ProviderID: "fun-asr",
		Segments: []contracts.SpeechRecognitionSegment{{
			Text: "第一句。第二句。", BeginMilliseconds: 100, EndMilliseconds: 1900,
			Words: []contracts.SpeechRecognitionWord{
				{Text: "第一句", BeginMilliseconds: 100, EndMilliseconds: 800, Punctuation: "。"},
				{Text: "第二句", BeginMilliseconds: 1100, EndMilliseconds: 1900, Punctuation: "。"},
			},
		}},
	}, 30, 120)
	if len(timestamped) != 2 || timestamped[0].StartFrame != 33 ||
		timestamped[0].EndFrame != 54 || timestamped[1].StartFrame != 63 ||
		timestamped[1].EndFrame != 87 || len(timestamped[0].Words) != 1 ||
		timestamped[0].Words[0].ID == "" || timestamped[0].Words[0].Text != "第一句" {
		t.Fatalf("timestamped=%#v", timestamped)
	}
	encodedTimestamped := EncodeSpeechUtterances(timestamped)
	if !TranscriptHasWordSchema(encodedTimestamped) {
		t.Fatalf("词级 schema 未持久化: %#v", encodedTimestamped)
	}
	decodedTimestamped, err := DecodeSpeechUtterances(encodedTimestamped)
	if err != nil || len(decodedTimestamped) != 2 || len(decodedTimestamped[1].Words) != 1 ||
		decodedTimestamped[1].Words[0].Punctuation != "。" {
		t.Fatalf("decoded timestamped=%#v err=%v", decodedTimestamped, err)
	}
	wordGaps := DeriveASRWordGaps("asset", timestamped)
	if len(wordGaps) != 1 || wordGaps[0].StartFrame != 54 || wordGaps[0].EndFrame != 63 ||
		wordGaps[0].DeleteStart != 56 || wordGaps[0].DeleteEnd != 61 ||
		wordGaps[0].Method != "asr_word_gap" {
		t.Fatalf("word gaps=%#v", wordGaps)
	}
	mergedPauses := MergeSpeechPauses("asset", append(wordGaps, SpeechPause{
		StartFrame: 55, EndFrame: 62, DeleteStart: 57, DeleteEnd: 60, Method: "rms_silence",
	}))
	if len(mergedPauses) != 1 || mergedPauses[0].Method != "asr_word_gap+rms_silence" ||
		mergedPauses[0].ID == "" {
		t.Fatalf("merged pauses=%#v", mergedPauses)
	}
	pauseEvidence := rushestools.SpeechPauseEvidence{
		SourceStartFrame: mergedPauses[0].StartFrame, SourceEndFrame: mergedPauses[0].EndFrame,
	}
	PopulateSpeechPauseContext(&pauseEvidence, timestamped)
	if pauseEvidence.PreviousText != "第一句。" || pauseEvidence.NextText != "第二句。" ||
		pauseEvidence.PreviousWordID == "" || pauseEvidence.NextWordID == "" {
		t.Fatalf("pause context=%#v", pauseEvidence)
	}
	if pauseEvidence.PreviousContext != "第一句。" || pauseEvidence.NextContext != "第二句。" ||
		pauseEvidence.JoinedContext != "第一句。第二句。" ||
		pauseEvidence.PreviousContextStartWordID == "" || pauseEvidence.NextContextEndWordID == "" {
		t.Fatalf("pause local word context=%#v", pauseEvidence)
	}
	if got := AlignTimestampedRecognition("asset", contracts.SpeechRecognitionResult{
		Text: "这是完整的第一句和第二句。", Segments: []contracts.SpeechRecognitionSegment{{
			Text: "第二句。", BeginMilliseconds: 1000, EndMilliseconds: 1800,
		}},
	}, 0, 90); len(got) != 0 {
		t.Fatalf("不完整 sentence 时间戳必须回退全文对齐: %#v", got)
	}
}

func TestRankSpeechPauseEvidenceSurfacesLongestCandidatesAndReportsTruncation(t *testing.T) {
	t.Parallel()
	values := []rushestools.SpeechPauseEvidence{
		{PauseID: "early_short", SourceStartFrame: 10, DeleteDurationFrames: 5},
		{PauseID: "late_long", SourceStartFrame: 900, DeleteDurationFrames: 29},
		{PauseID: "middle_long", SourceStartFrame: 500, DeleteDurationFrames: 22},
		{PauseID: "same_length_earlier", SourceStartFrame: 400, DeleteDurationFrames: 22},
	}
	ranked, total, truncated := RankSpeechPauseEvidence(values, 3)
	if total != 4 || !truncated || len(ranked) != 3 ||
		ranked[0].PauseID != "late_long" || ranked[1].PauseID != "same_length_earlier" ||
		ranked[2].PauseID != "middle_long" {
		t.Fatalf("ranked=%#v total=%d truncated=%v", ranked, total, truncated)
	}
	many := make([]rushestools.SpeechPauseEvidence, 150)
	for index := range many {
		many[index] = rushestools.SpeechPauseEvidence{
			PauseID: fmt.Sprintf("pause_%03d", index), DeleteDurationFrames: index + 1,
		}
	}
	defaultRanked, defaultTotal, defaultTruncated := RankSpeechPauseEvidence(
		append([]rushestools.SpeechPauseEvidence(nil), many...), 0,
	)
	if defaultTotal != 150 || !defaultTruncated || len(defaultRanked) != 24 {
		t.Fatalf(
			"default ranked=%d total=%d truncated=%v",
			len(defaultRanked), defaultTotal, defaultTruncated,
		)
	}
	capped, cappedTotal, cappedTruncated := RankSpeechPauseEvidence(
		append([]rushestools.SpeechPauseEvidence(nil), many...), 101,
	)
	if cappedTotal != 150 || !cappedTruncated || len(capped) != 100 {
		t.Fatalf("capped ranked=%d total=%d truncated=%v", len(capped), cappedTotal, cappedTruncated)
	}
}

func TestIntraUtteranceSpeechRepetitionsExposeRepeatedTakesAndAdjacentWords(t *testing.T) {
	t.Parallel()
	words := []SpeechWord{
		{ID: "w_early_this", StartFrame: 10, EndFrame: 20, Text: "这个"},
		{ID: "w_early_color", StartFrame: 20, EndFrame: 30, Text: "柑橘色"},
		{ID: "w_early_look", StartFrame: 30, EndFrame: 45, Text: "看起来"},
		{ID: "w_early_green", StartFrame: 45, EndFrame: 60, Text: "偏绿"},
		{ID: "w_filler", StartFrame: 70, EndFrame: 80, Text: "反正"},
		{ID: "w_later_this_1", StartFrame: 90, EndFrame: 100, Text: "这个"},
		{ID: "w_later_this_2", StartFrame: 100, EndFrame: 110, Text: "这个"},
		{ID: "w_later_color", StartFrame: 110, EndFrame: 120, Text: "柑橘色"},
		{ID: "w_later_look", StartFrame: 120, EndFrame: 135, Text: "看起来"},
		{ID: "w_later_green", StartFrame: 135, EndFrame: 150, Text: "偏绿", Punctuation: "。"},
	}
	got := IntraUtteranceSpeechRepetitions("asset_repeat", []SpeechUtterance{{
		ID: "utt_repeat", StartFrame: 10, EndFrame: 150,
		Text: "这个柑橘色看起来偏绿，反正这个这个柑橘色看起来偏绿。", Words: words,
	}}, 12)
	phraseFound, adjacentFound := false, false
	for _, evidence := range got {
		if evidence.RepetitionID == "" {
			t.Fatalf("句内重复证据必须提供稳定 repetition_id: %#v", evidence)
		}
		if !strings.Contains(evidence.Evidence, "判断") {
			t.Fatalf("句内重复证据必须引导模型结合上下文判断: %#v", evidence)
		}
		switch evidence.Kind {
		case "repeated_phrase":
			if evidence.MatchedCharacters >= 8 && evidence.EarlierStartWordID == "w_early_this" &&
				evidence.LaterStartWordID == "w_later_this_2" &&
				strings.Contains(evidence.ContextText, "反正") {
				phraseFound = true
			}
		case "adjacent_word_repeat":
			if evidence.EarlierStartWordID == "w_later_this_1" && evidence.LaterStartWordID == "w_later_this_2" {
				adjacentFound = true
			}
		}
	}
	if !phraseFound || !adjacentFound {
		t.Fatalf("句内重复证据不完整: %#v", got)
	}
}

func TestSpeechInspectResultSerializesDecisionEvidenceBeforeUtterances(t *testing.T) {
	t.Parallel()
	encoded, err := json.Marshal(rushestools.SpeechInspectResult{
		Repetitions:    []rushestools.SpeechRepetitionEvidence{{RepetitionID: "repeat_1"}},
		ShortFragments: []rushestools.SpeechFragmentEvidence{{FragmentID: "fragment_1"}},
		Pauses:         []rushestools.SpeechPauseEvidence{{PauseID: "pause_1"}},
		Utterances:     []rushestools.SpeechUtteranceEvidence{{UtteranceID: "utterance_1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	utterancesAt := strings.Index(text, `"utterances"`)
	if utterancesAt < 0 {
		t.Fatalf("serialized result missing utterances: %s", text)
	}
	for _, field := range []string{`"intra_utterance_repetitions"`, `"short_speech_fragments"`, `"pauses"`} {
		at := strings.Index(text, field)
		if at < 0 || at >= utterancesAt {
			t.Fatalf("decision evidence %s must precede utterances: %s", field, text)
		}
	}
}

func TestIntraUtteranceSpeechRepetitionsReserveLimitedOutputForAdjacentWords(t *testing.T) {
	t.Parallel()
	utterances := []SpeechUtterance{
		{ID: "utt_phrase", Text: "abcdef过渡abcdef", Words: []SpeechWord{
			{ID: "phrase_early_a", StartFrame: 0, EndFrame: 5, Text: "abc"},
			{ID: "phrase_early_b", StartFrame: 5, EndFrame: 10, Text: "def"},
			{ID: "phrase_bridge", StartFrame: 10, EndFrame: 15, Text: "过渡"},
			{ID: "phrase_late_a", StartFrame: 15, EndFrame: 20, Text: "abc"},
			{ID: "phrase_late_b", StartFrame: 20, EndFrame: 25, Text: "def"},
		}},
		{ID: "utt_stutter", Text: "纯物理压压的方式", Words: []SpeechWord{
			{ID: "normal", StartFrame: 30, EndFrame: 40, Text: "纯物理"},
			{ID: "press_1", StartFrame: 40, EndFrame: 41, Text: "压"},
			{ID: "press_2", StartFrame: 41, EndFrame: 46, Text: "压"},
			{ID: "ending", StartFrame: 46, EndFrame: 55, Text: "的方式"},
		}},
	}
	got := IntraUtteranceSpeechRepetitions("asset_repeat_priority", utterances, 1)
	if len(got) != 1 || got[0].Kind != "adjacent_word_repeat" ||
		got[0].EarlierStartWordID != "press_1" || got[0].LaterStartWordID != "press_2" {
		t.Fatalf("有限结果必须优先暴露相邻重复词: %#v", got)
	}
}

func TestShortLeadingSpeechFragmentsExposeInterruptedFalseStart(t *testing.T) {
	t.Parallel()
	utterance := SpeechUtterance{
		ID: "utt_retry", StartFrame: 1017, EndFrame: 1459,
		Text: "但是没有同时这次键盘苹果也说做了一些新的创新。",
		Words: []SpeechWord{
			{ID: "w_but", StartFrame: 1017, EndFrame: 1027, Text: "但是"},
			{ID: "w_not", StartFrame: 1027, EndFrame: 1049, Text: "没"},
			{ID: "w_have", StartFrame: 1064, EndFrame: 1067, Text: "有"},
			{ID: "w_same", StartFrame: 1067, EndFrame: 1074, Text: "同时"},
			{ID: "w_retry", StartFrame: 1074, EndFrame: 1083, Text: "这次"},
			{ID: "w_keyboard", StartFrame: 1083, EndFrame: 1095, Text: "键盘", Punctuation: "。"},
		},
	}
	pauses := []SpeechPause{{
		ID: "pause_false_start", StartFrame: 1037, EndFrame: 1064,
		DeleteStart: 1039, DeleteEnd: 1062,
	}}
	got := ShortLeadingSpeechFragments("asset", []SpeechUtterance{utterance}, pauses, 12)
	if len(got) != 1 || got[0].Text != "但是没" || got[0].StartWordID != "w_but" ||
		got[0].EndWordID != "w_not" || got[0].NextContext != "有同时这次键盘。" ||
		got[0].JoinedContext != "但是没有同时这次键盘。" ||
		got[0].PauseDurationFrames != 27 || got[0].FragmentID == "" {
		t.Fatalf("short fragments=%#v", got)
	}
}

func TestClampSpeechPausesNeverCutsASRWords(t *testing.T) {
	t.Parallel()
	utterances := []SpeechUtterance{{
		ID: "utt", StartFrame: 0, EndFrame: 100,
		Words: []SpeechWord{
			{ID: "w1", StartFrame: 5, EndFrame: 10, Text: "前"},
			{ID: "w2", StartFrame: 20, EndFrame: 30, Text: "后"},
			{ID: "w3", StartFrame: 50, EndFrame: 60, Text: "中"},
		},
	}}
	got := ClampSpeechPausesToWordBoundaries("asset", []SpeechPause{
		{ID: "p1", StartFrame: 8, EndFrame: 25, DeleteStart: 8, DeleteEnd: 25, Method: "rms_silence"},
		{ID: "p2", StartFrame: 40, EndFrame: 80, DeleteStart: 40, DeleteEnd: 80, Method: "rms_silence"},
		{ID: "tiny", StartFrame: 31, EndFrame: 34, DeleteStart: 31, DeleteEnd: 34, Method: "rms_silence"},
	}, utterances)
	if len(got) != 3 {
		t.Fatalf("safe pauses=%#v", got)
	}
	want := [][2]int{{10, 20}, {40, 50}, {60, 80}}
	for index, pause := range got {
		if pause.DeleteStart != want[index][0] || pause.DeleteEnd != want[index][1] ||
			pause.ID == "" || !strings.Contains(pause.Method, "word_boundary_clamped") {
			t.Fatalf("pause[%d]=%#v want=%v", index, pause, want[index])
		}
		for _, word := range utterances[0].Words {
			if SourceRangesOverlap(pause.DeleteStart, pause.DeleteEnd, word.StartFrame, word.EndFrame) {
				t.Fatalf("pause %#v cuts word %#v", pause, word)
			}
		}
	}
}

func TestShortLeadingSpeechFragmentsExpandToRepeatedTakeRestart(t *testing.T) {
	t.Parallel()
	earlier := SpeechUtterance{
		ID: "utt_earlier", StartFrame: 353, EndFrame: 446,
		Text: "那这次键盘苹果也说做了创新是什么创新呢？",
	}
	retry := SpeechUtterance{
		ID: "utt_retry", StartFrame: 1017, EndFrame: 1459,
		Text: "但是没有同时这次键盘苹果也说做了一些新的创新。",
		Words: []SpeechWord{
			{ID: "w_but", StartFrame: 1017, EndFrame: 1027, Text: "但是"},
			{ID: "w_not", StartFrame: 1027, EndFrame: 1049, Text: "没"},
			{ID: "w_have", StartFrame: 1064, EndFrame: 1067, Text: "有"},
			{ID: "w_same", StartFrame: 1067, EndFrame: 1071, Text: "同"},
			{ID: "w_time", StartFrame: 1071, EndFrame: 1074, Text: "时"},
			{ID: "w_retry", StartFrame: 1074, EndFrame: 1083, Text: "这次"},
			{ID: "w_keyboard", StartFrame: 1083, EndFrame: 1098, Text: "键盘"},
			{ID: "w_apple", StartFrame: 1098, EndFrame: 1105, Text: "苹果"},
			{ID: "w_also", StartFrame: 1105, EndFrame: 1109, Text: "也"},
		},
	}
	pauses := []SpeechPause{{
		ID: "pause_false_start", StartFrame: 1037, EndFrame: 1064,
		DeleteStart: 1039, DeleteEnd: 1062,
	}}
	got := ShortLeadingSpeechFragments("asset", []SpeechUtterance{earlier, retry}, pauses, 12)
	if len(got) != 1 || got[0].Kind != "restart_prefix_before_repeated_take" ||
		got[0].Text != "但是没有同时" || got[0].EndWordID != "w_time" ||
		got[0].NextContextStartWordID != "w_retry" ||
		got[0].PreviousContext != "那这次键盘苹果也说做了创新是什么创新呢？" ||
		got[0].JoinedContext != "但是没有同时这次键盘苹果也" ||
		got[0].RestartAnchorText != "这次键盘苹果" ||
		got[0].MatchedEarlierUtteranceID != "utt_earlier" ||
		!strings.Contains(got[0].MatchedEarlierText, "这次键盘苹果") {
		t.Fatalf("restart fragment=%#v", got)
	}
}

func TestShortLeadingSpeechFragmentsExposeEarlierTakeBeforeRepeatedPhraseRestart(t *testing.T) {
	t.Parallel()
	utterance := SpeechUtterance{
		ID: "utt_color_retry", StartFrame: 0, EndFrame: 120,
		Text: "柑橘色我觉得最爱的颜色反正可以自己这个全新的柑橘色我觉得",
		Words: []SpeechWord{
			{ID: "early_color", StartFrame: 0, EndFrame: 10, Text: "柑橘色"},
			{ID: "early_think", StartFrame: 10, EndFrame: 20, Text: "我觉得"},
			{ID: "tail_color", StartFrame: 20, EndFrame: 40, Text: "最爱的颜色"},
			{ID: "tail_self", StartFrame: 40, EndFrame: 60, Text: "反正可以自己"},
			{ID: "restart_this", StartFrame: 72, EndFrame: 80, Text: "这个"},
			{ID: "restart_new", StartFrame: 80, EndFrame: 90, Text: "全新的"},
			{ID: "later_color", StartFrame: 90, EndFrame: 100, Text: "柑橘色"},
			{ID: "later_think", StartFrame: 100, EndFrame: 110, Text: "我觉得"},
		},
	}
	pauses := []SpeechPause{{
		ID: "pause_before_restart", StartFrame: 60, EndFrame: 72,
		DeleteStart: 62, DeleteEnd: 70,
	}}

	got := ShortLeadingSpeechFragments("asset_color", []SpeechUtterance{utterance}, pauses, 12)
	if len(got) != 1 || got[0].Kind != "earlier_take_before_repeated_phrase_restart" ||
		got[0].Text != "柑橘色我觉得最爱的颜色反正可以自己" ||
		got[0].StartWordID != "early_color" || got[0].EndWordID != "tail_self" ||
		got[0].PauseID != "pause_before_restart" ||
		got[0].NextContext != "这个全新的柑橘色我觉得" ||
		got[0].RestartAnchorText != "柑橘色我觉得" ||
		got[0].MatchedEarlierText != "柑橘色我觉得" {
		t.Fatalf("retake tail fragment=%#v", got)
	}
}

func TestSimilarSpeechPairsExposeRepeatedMultiUtteranceTakes(t *testing.T) {
	t.Parallel()
	values := []SpeechUtterance{
		{ID: "utt_color", StartFrame: 10, EndFrame: 344, Text: "这个柑橘色看起来有点偏绿。"},
		{ID: "utt_keyboard_intro", StartFrame: 353, EndFrame: 446, Text: "那这次键盘苹果也说做了创新是什么创新呢？"},
		{ID: "utt_keycaps", StartFrame: 457, EndFrame: 632, Text: "它把键盘的键帽颜色靠近了外表颜色，这就是最大的创新了。"},
		{ID: "utt_backlight", StartFrame: 641, EndFrame: 715, Text: "然后它没有了背光，这点比较可惜。"},
		{ID: "utt_typing", StartFrame: 720, EndFrame: 912, Text: "打字手感没有问题，回弹也不错，但是没有背光晚上打字会比较难受。"},
		{ID: "utt_false_start", StartFrame: 918, EndFrame: 965, Text: "那另外只有一百呃。"},
		{ID: "utt_keyboard_retry", StartFrame: 1017, EndFrame: 1459, Text: "这次键盘苹果也说做了一些新的创新，核心创新就是采用同色系设计，键盘键帽颜色和外观接近，但是它没有了背光，这一点比较可惜。"},
		{ID: "utt_typing_retry", StartFrame: 1464, EndFrame: 1571, Text: "打字手感方面没有什么问题，回弹也都不错。"},
		{ID: "utt_fingerprint", StartFrame: 1583, EndFrame: 1721, Text: "电源键提供指纹识别。"},
	}
	pairs := SimilarSpeechPairs(values, 24)
	found := false
	for _, pair := range pairs {
		if pair.Method != "normalized_character_lcs_dice" ||
			pair.EarlierUtteranceID != "utt_keyboard_intro" ||
			pair.EarlierEndUtteranceID != "utt_typing" ||
			pair.LaterUtteranceID != "utt_keyboard_retry" ||
			pair.LaterEndUtteranceID != "utt_typing_retry" {
			continue
		}
		if pair.Similarity < 0.46 || pair.MatchedCharacters < 18 ||
			pair.EarlierSourceStartFrame != 353 || pair.LaterSourceEndFrame != 1571 ||
			!strings.Contains(pair.EarlierText, "没有了背光") ||
			!strings.Contains(pair.LaterText, "没有了背光") {
			t.Fatalf("连续重说证据不完整: %#v", pair)
		}
		found = true
		break
	}
	if !found {
		t.Fatalf("未发现跨句重说块: %#v", pairs)
	}
}

func createSpeechFixtureAudio(t *testing.T, directory, name string) string {
	return createSpeechFixtureAudioDuration(t, directory, name, 2)
}

func createSpeechFixtureAudioDuration(
	t *testing.T, directory, name string, durationSeconds int,
) string {
	t.Helper()
	path := filepath.Join(directory, name+".wav")
	if _, err := media.RunCommand(
		t.Context(), "ffmpeg", "-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", fmt.Sprintf(
			"sine=frequency=440:sample_rate=16000:duration=%d", durationSeconds,
		),
		"-c:a", "pcm_s16le", path,
	); err != nil {
		t.Fatal(err)
	}
	return path
}

func insertSpeechFixtureAsset(
	t *testing.T, database *storage.DB, draftID, assetID, path string,
) {
	t.Helper()
	probe, err := media.ProbeFile(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	probeJSON, _ := json.Marshal(probe)
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO assets(
			asset_id,storage_mode,reference_path,kind,source,filename,hash,size,
			probe_json,ingest_status,understanding_status,usable
		) VALUES(?, 'reference', ?, 'audio', 'local_path', ?, ?, 1, ?, 'ready', 'none', 1)`,
		assetID, path, filepath.Base(path), assetID, string(probeJSON),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO draft_asset_links(draft_id,asset_id,rel_dir,linked_at)
		VALUES(?, ?, 'Aroll', ?)`,
		draftID, assetID, time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatal(err)
	}
}
