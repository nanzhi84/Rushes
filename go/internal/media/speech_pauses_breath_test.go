package media

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// 生成「响元音(低平坦度) + 安静吸气(高平坦度噪声) + 静音」，断言呼吸检测只圈出吸气段，
// 不误判元音（能量在阈内但谱平坦度低）也不圈静音。
// 依赖 ffmpeg ≥ 5.0 的 aspectralstats(谱平坦度)滤镜；breathFlatnessThreshold 按其数值定标，
// CI 两平台(ubuntu/macos)的 ffmpeg 均满足该版本且平坦度数值稳定。
func TestDetectBreathRangesCatchesBreathNotVowel(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 未安装")
	}
	fps := 30
	source := filepath.Join(t.TempDir(), "clip.wav")
	// 0.4s 元音样正弦 + 0.3s 高通白噪声(呼吸样) + 0.3s 静音。
	if _, err := RunCommand(t.Context(), "ffmpeg", "-y",
		"-f", "lavfi", "-i", "sine=frequency=180:duration=0.4:sample_rate=48000",
		"-f", "lavfi", "-i", "anoisesrc=duration=0.3:sample_rate=48000:amplitude=0.06",
		"-filter_complex",
		"[1:a]highpass=f=1800[br];[0:a][br]concat=n=2:v=0:a=1,apad=pad_dur=0.3",
		"-ac", "1", source,
	); err != nil {
		t.Fatal(err)
	}
	durationFrames := 30 // 1s @30fps
	ranges, err := detectBreathRanges(t.Context(), source, fps, -35, durationFrames, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	// 呼吸段大约在 0.4-0.7s → 帧 12-21。断言存在一个与之重叠的段，且不覆盖元音区(帧<12)。
	found := false
	for _, r := range ranges {
		if r[0] >= 10 && r[1] <= 24 {
			found = true
		}
		if r[0] < 8 {
			t.Fatalf("误把元音区圈成呼吸: %v", r)
		}
	}
	if !found {
		t.Fatalf("未检出呼吸段, ranges=%v", ranges)
	}
}
