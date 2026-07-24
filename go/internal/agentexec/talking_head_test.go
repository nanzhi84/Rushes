package agentexec

import "testing"

func TestTalkingHeadTranscriptTextFiltersOutsideSourceRange(t *testing.T) {
	t.Parallel()
	utterances := []SpeechUtterance{
		{ID: "before", StartFrame: 0, EndFrame: 10, Text: "之前"},
		{ID: "plain", StartFrame: 10, EndFrame: 20, Text: "开场"},
		{ID: "removed_utterance", StartFrame: 15, EndFrame: 25, Text: "整句移除"},
		{
			ID: "words", StartFrame: 20, EndFrame: 45,
			Words: []SpeechWord{
				{ID: "before_word", StartFrame: 0, EndFrame: 10, Text: "前"},
				{ID: "keep_1", StartFrame: 10, EndFrame: 20, Text: "你", Punctuation: "，"},
				{ID: "removed_word", StartFrame: 20, EndFrame: 30, Text: "不"},
				{ID: "keep_2", StartFrame: 30, EndFrame: 40, Text: "好", Punctuation: "！"},
				{ID: "after_word", StartFrame: 50, EndFrame: 60, Text: "后"},
			},
		},
		{
			ID: "empty_words", StartFrame: 20, EndFrame: 40,
			Words: []SpeechWord{{ID: "outside", StartFrame: 50, EndFrame: 60, Text: "空"}},
		},
		{ID: "after", StartFrame: 50, EndFrame: 60, Text: "之后"},
	}
	got := TalkingHeadTranscriptText(
		utterances,
		10,
		50,
	)
	if got != "开场整句移除你，不好！" {
		t.Fatalf("transcript=%q", got)
	}
}
