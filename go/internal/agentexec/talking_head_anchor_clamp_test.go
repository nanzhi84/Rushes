package agentexec

import (
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

// TestTalkingHeadAssignmentSourceRangeClampsToClip 验证 B-roll 锚定改交集裁剪（#134 裁决 a）：
// 跨 clip 源边界的语义锚点不再整体拒绝（旧「严格包含」），而是裁剪到 clip 内的交集成功解析；
// 只有完全落在 clip 之外才判无交集非法。与 Q1 删除路径的交集口径一致，消除「删得动、锚不上」。
func TestTalkingHeadAssignmentSourceRangeClampsToClip(t *testing.T) {
	// clip 覆盖源 [100,200)。
	clip := timeline.Clip{
		AssetID: "a1", AssetKind: "video", SourceStartFrame: 100, SourceEndFrame: 200,
		TimelineStartFrame: 0, TimelineEndFrame: 100, PlaybackRate: 1,
	}
	utterances := map[string]SpeechUtterance{
		"u_tail": {ID: "u_tail", StartFrame: 180, EndFrame: 250, Text: "跨尾界"}, // 尾越界
		"u_head": {ID: "u_head", StartFrame: 60, EndFrame: 140, Text: "跨头界"},  // 头越界
		"u_out":  {ID: "u_out", StartFrame: 210, EndFrame: 260, Text: "完全在外"}, // 全越界
	}
	words := []SpeechWord{{ID: "w_cross", StartFrame: 180, EndFrame: 240, Text: "跨界词"}}
	none := map[string]struct{}{}

	cases := []struct {
		name        string
		assignment  rushestools.TalkingHeadBrollAssignment
		wantStart   int
		wantEnd     int
		wantInvalid bool
	}{
		{"尾越界 utterance 裁剪", rushestools.TalkingHeadBrollAssignment{ShotID: "s1", StartUtteranceID: "u_tail"}, 180, 200, false},
		{"头越界 utterance 裁剪", rushestools.TalkingHeadBrollAssignment{ShotID: "s1", StartUtteranceID: "u_head"}, 100, 140, false},
		{"跨界 word 裁剪", rushestools.TalkingHeadBrollAssignment{ShotID: "s1", StartWordID: "w_cross"}, 180, 200, false},
		{"完全越界判非法", rushestools.TalkingHeadBrollAssignment{ShotID: "s1", StartUtteranceID: "u_out"}, 0, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := TalkingHeadAssignmentSourceRange(tc.assignment, utterances, words, none, none, clip)
			if tc.wantInvalid {
				if err == nil {
					t.Fatalf("应判无交集非法，却成功 got=%+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("跨界锚点应裁剪成功，err=%v", err)
			}
			if got.Start != tc.wantStart || got.End != tc.wantEnd {
				t.Fatalf("裁剪结果=%+v, want [%d,%d)", got, tc.wantStart, tc.wantEnd)
			}
		})
	}
}
