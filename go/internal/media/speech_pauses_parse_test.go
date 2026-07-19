package media

import (
	"path/filepath"
	"testing"
)

// detectBreathRanges 在 ffmpeg 失败(源不存在)时返回错误，让上层退回纯 silencedetect。
func TestDetectBreathRangesErrorFallsBack(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.wav")
	if _, err := detectBreathRanges(t.Context(), missing, 30, -35, 30, 5, 2); err == nil {
		t.Fatal("源不存在时应返回错误")
	}
}

// parseBreathFrames 直接喂 ametadata=print 样式字节，覆盖：命中呼吸窗、过响窗跳过、
// 过于纯音(低平坦度)窗跳过、缺 flatness 行跳过、末窗刷到区间末。
func TestParseBreathFramesWindows(t *testing.T) {
	const fps = 30
	// floor=-35, ceiling=-35+22=-13：呼吸带 (-35,-13]。
	output := []byte(
		"frame:0 pts:0 pts_time:0.000000\n" +
			"lavfi.astats.Overall.RMS_level=-28.0\n" + // 在带内
			"lavfi.aspectralstats.1.flatness=0.60\n" + // 噪声样 → 呼吸
			"frame:1 pts:1600 pts_time:0.100000\n" +
			"lavfi.astats.Overall.RMS_level=-5.0\n" + // 过响 → 跳过
			"lavfi.aspectralstats.1.flatness=0.60\n" +
			"frame:2 pts:3200 pts_time:0.200000\n" +
			"lavfi.astats.Overall.RMS_level=-28.0\n" + // 带内但
			"lavfi.aspectralstats.1.flatness=0.02\n" + // 太纯音 → 跳过
			"frame:3 pts:4800 pts_time:0.300000\n" +
			"lavfi.astats.Overall.RMS_level=-28.0\n" + // 缺 flatness 行 → okFlat=false 跳过
			"frame:4 pts:6400 pts_time:0.400000\n" +
			"lavfi.astats.Overall.RMS_level=-28.0\n" + // 末窗命中，endSec=start+1/fps
			"lavfi.aspectralstats.1.flatness=0.60\n",
	)
	flags := parseBreathFrames(output, fps, -35, -13, 20)
	// 窗0 [0.0,0.1)→帧[0,3) 应命中；过响/纯音/缺行窗对应帧应不命中；末窗 0.4→帧12 命中。
	for _, frame := range []int{0, 1, 2} {
		if !flags[frame] {
			t.Errorf("呼吸窗帧 %d 应命中", frame)
		}
	}
	for _, frame := range []int{3, 4, 5, 6, 7, 8, 9, 10} {
		if flags[frame] {
			t.Errorf("过响/纯音/缺行窗帧 %d 不应命中", frame)
		}
	}
	if !flags[12] {
		t.Error("末窗帧 12 应命中")
	}
}
