package media

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestAnalyzeSpeechPausesBreathRecallBenchmark 是 Q4「VAD 真实化」的量化对拍：
// 造一段口播样素材，含三个已知气口——两个真静音（两种检测都该抓到）和一个「有能量的
// 吸气」（RMS 高于 -35dB 静音阈、silencedetect 必漏，只有频谱呼吸检测能捞回）——
// 再用 Method 标签区分「纯 silencedetect 基线」与「呼吸增强」各自的召回，并断言两种检测
// 都不误圈响元音（精确率）。这样气口召回的提升是可复现、带地面真值的定量结论，不是口号。
//
// 地面真值时间线 @30fps（总长 2.2s / 66 帧）：
//
//	[0.00-0.50] 响元音正弦          帧 0-15
//	[0.50-0.80] 有能量吸气(高通噪声) 帧 15-24  ← GT 呼吸气口（silencedetect 漏）
//	[0.80-1.30] 响元音正弦          帧 24-39
//	[1.30-1.70] 真静音              帧 39-51  ← GT 静音气口
//	[1.70-2.20] 响元音正弦          帧 51-66
//
// 依赖 ffmpeg ≥ 5.0 的 aspectralstats(谱平坦度)滤镜；呼吸判据的平坦度阈值按其数值定标，
// CI 两平台(ubuntu/macos)的 ffmpeg 均满足该版本且数值稳定。
func TestAnalyzeSpeechPausesBreathRecallBenchmark(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 未安装")
	}
	const fps = 30
	source := filepath.Join(t.TempDir(), "talking_head.wav")
	// 三段响元音之间夹一个有能量吸气和一个真静音；concat 拼成 2.2s。
	if _, err := RunCommand(t.Context(), "ffmpeg", "-y",
		"-f", "lavfi", "-i", "sine=frequency=220:duration=0.5:sample_rate=48000",
		"-f", "lavfi", "-i", "anoisesrc=duration=0.3:sample_rate=48000:amplitude=0.08",
		"-f", "lavfi", "-i", "sine=frequency=240:duration=0.5:sample_rate=48000",
		"-f", "lavfi", "-i", "anullsrc=r=48000:cl=mono:duration=0.4",
		"-f", "lavfi", "-i", "sine=frequency=200:duration=0.5:sample_rate=48000",
		"-filter_complex",
		"[1:a]highpass=f=1800[br];[0:a][br][2:a][3:a][4:a]concat=n=5:v=0:a=1",
		"-ac", "1", source,
	); err != nil {
		t.Fatal(err)
	}

	analysis, err := AnalyzeSpeechPauses(t.Context(), source, fps, SpeechPauseOptions{
		ThresholdDB: -35, MinPauseFrames: 5, KeepEdgeFrames: 2,
		MaxPauses: 1000, IncludeBoundaries: false,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 地面真值气口（源帧闭开区间近似；留边界抖动余量用重叠判定，不比精确帧）。
	breathGT := [2]int{15, 24}
	silenceGT := [2]int{39, 51}
	// 三个响元音区不该出现任何气口（精确率）。
	voicedRegions := [][2]int{{2, 13}, {26, 37}, {53, 64}}

	overlaps := func(a, b [2]int) bool { return a[0] < b[1] && b[0] < a[1] }

	var breathHit, silenceHit bool
	var breathMethod, silenceMethod string
	for _, pause := range analysis.Pauses {
		span := [2]int{pause.SourceStartFrame, pause.SourceEndFrame}
		if overlaps(span, breathGT) {
			breathHit = true
			breathMethod = pause.Method
		}
		if overlaps(span, silenceGT) {
			silenceHit = true
			silenceMethod = pause.Method
		}
		for _, voiced := range voicedRegions {
			if overlaps(span, voiced) {
				t.Fatalf("误把响元音区 %v 圈成气口: span=%v method=%q", voiced, span, pause.Method)
			}
		}
	}

	// 呼吸增强：三个气口全召回，且吸气段带 rms_breath 标签。
	if !silenceHit {
		t.Fatalf("真静音气口漏检, pauses=%+v", analysis.Pauses)
	}
	if !breathHit {
		t.Fatalf("有能量吸气漏检, pauses=%+v", analysis.Pauses)
	}
	if !strings.Contains(breathMethod, "rms_breath") {
		t.Fatalf("吸气段应带 rms_breath 标签, 实得 method=%q", breathMethod)
	}
	if !strings.Contains(silenceMethod, "rms_silence") {
		t.Fatalf("静音段应带 rms_silence 标签, 实得 method=%q", silenceMethod)
	}
	if !strings.Contains(analysis.AnalysisMethod, "spectral-breath") {
		t.Fatalf("分析方法应声明 spectral-breath, 实得 %q", analysis.AnalysisMethod)
	}

	// 量化基线 vs 增强召回：基线只认带 rms_silence 的气口（silencedetect 独立能抓的），
	// 增强认所有气口。地面真值 2 个（吸气 + 静音）里，基线召回 1、增强召回 2。
	baselineRecall := 0
	enhancedRecall := 0
	for _, gt := range [][2]int{breathGT, silenceGT} {
		caughtBaseline, caughtEnhanced := false, false
		for _, pause := range analysis.Pauses {
			span := [2]int{pause.SourceStartFrame, pause.SourceEndFrame}
			if !overlaps(span, gt) {
				continue
			}
			caughtEnhanced = true
			// 基线口径：只有当这个气口是 silencedetect 独立圈出的（method 恰为 rms_silence，
			// 没有被呼吸段扩展合并）才算基线命中。
			if pause.Method == "rms_silence" {
				caughtBaseline = true
			}
		}
		if caughtBaseline {
			baselineRecall++
		}
		if caughtEnhanced {
			enhancedRecall++
		}
	}
	if enhancedRecall <= baselineRecall {
		t.Fatalf("呼吸增强未提升召回: 基线=%d 增强=%d", baselineRecall, enhancedRecall)
	}
	t.Logf("气口召回对拍 @%dfps: 基线(silencedetect)=%d/2, 呼吸增强=%d/2, 误检=0",
		fps, baselineRecall, enhancedRecall)
}
