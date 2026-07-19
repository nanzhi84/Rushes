package media

import (
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/timeline"
)

// TestBaseAudioSeams 验证同素材波纹删除接缝的识别：源不连续的首尾相接算接缝，
// 源连续的相接不算，时间线有空档的相接也不算，跨素材不算。
func TestBaseAudioSeams(t *testing.T) {
	clip := func(id, asset string, tlStart, tlEnd, srcStart, srcEnd int) timeline.Clip {
		return timeline.Clip{
			TimelineClipID: id, TrackID: "visual_base", AssetID: asset, AssetKind: "video",
			TimelineStartFrame: tlStart, TimelineEndFrame: tlEnd,
			SourceStartFrame: srcStart, SourceEndFrame: srcEnd, PlaybackRate: 1,
		}
	}
	document := timeline.Document{Tracks: []timeline.Track{{TrackID: "visual_base", Clips: []timeline.Clip{
		// a|b 源不连续(60!=100) 且时间线首尾相接 → 接缝。
		clip("a", "asset1", 0, 60, 0, 60),
		clip("b", "asset1", 60, 120, 100, 160),
		// b|c 源连续(160==160) → 非接缝。
		clip("c", "asset1", 120, 180, 160, 220),
		// c|d 跨素材 → 非接缝。
		clip("d", "asset2", 180, 240, 0, 60),
	}}}}
	seams := baseAudioSeams(document)
	if got := seams["a"]; !got.Out || got.In {
		t.Errorf("a 应只有尾接缝, got=%+v", got)
	}
	if got := seams["b"]; !got.In || got.Out {
		t.Errorf("b 应只有头接缝, got=%+v", got)
	}
	if _, ok := seams["c"]; ok {
		t.Errorf("c 源连续不应成接缝, got=%+v", seams["c"])
	}
	if _, ok := seams["d"]; ok {
		t.Errorf("d 跨素材不应成接缝, got=%+v", seams["d"])
	}
	if got := baseAudioSeams(timeline.Document{}); len(got) != 0 {
		t.Errorf("无主视觉轨应返回空接缝表, got=%v", got)
	}
	if !sameAssetRippleSeam(document.Tracks[0].Clips[0], document.Tracks[0].Clips[1]) {
		t.Error("a→b 应判为接缝")
	}
	if sameAssetRippleSeam(document.Tracks[0].Clips[1], document.Tracks[0].Clips[2]) {
		t.Error("b→c 源连续不应判为接缝")
	}
}

// TestAudioFilterEmitsSeamDeclick 结构性验证：接缝片段的滤镜链带 12ms 微淡，且只在
// 对应端没有显式淡化时才加；无接缝时不加。
func TestAudioFilterEmitsSeamDeclick(t *testing.T) {
	document := timeline.Document{FPS: 30}
	base := timeline.Clip{TimelineStartFrame: 0, TimelineEndFrame: 60, SourceStartFrame: 0, SourceEndFrame: 60, PlaybackRate: 1}

	withSeam := audioFilter(0, base, 0, document, "s", audioSeam{In: true, Out: true})
	if !strings.Contains(withSeam, "afade=t=in:st=0:d=0.012000") {
		t.Errorf("接缝头应有 12ms 淡入: %s", withSeam)
	}
	if !strings.Contains(withSeam, "afade=t=out:st=1.988000:d=0.012000") {
		t.Errorf("接缝尾应有 12ms 淡出: %s", withSeam)
	}

	noSeam := audioFilter(0, base, 0, document, "n", audioSeam{})
	if strings.Contains(noSeam, "d=0.012000") {
		t.Errorf("无接缝不应加去咔哒微淡: %s", noSeam)
	}

	// 已有显式淡化的一端不再叠加接缝微淡（那端本就从静音起落）。
	explicit := base
	explicit.FadeInFrames = 6
	withExplicitIn := audioFilter(0, explicit, 0, document, "e", audioSeam{In: true, Out: true})
	if strings.Contains(withExplicitIn, "afade=t=in:st=0:d=0.012000") {
		t.Errorf("已有显式淡入时不应叠加接缝淡入: %s", withExplicitIn)
	}
	if !strings.Contains(withExplicitIn, "afade=t=out:st=1.988000:d=0.012000") {
		t.Errorf("尾端接缝淡出仍应保留: %s", withExplicitIn)
	}
}

// TestSeamDeclickReducesRMSJump 行为验证：对同一个波纹接缝拼接，加上 audioFilter 会发出的
// 那对 12ms 微淡后，接缝处的宽带瞬变(咔哒声的能量)显著下降。用高通后的峰值电平作咔哒代理。
func TestSeamDeclickReducesRMSJump(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 未安装")
	}
	dir := t.TempDir()
	tone := filepath.Join(dir, "tone.wav")
	// 123.4Hz 连续音(低频便于高通把本体压掉)；取 [0,0.2) 与 [0.4,0.6) 硬拼即模拟波纹接缝
	// (源不连续→相位跳变→咔哒)。
	if _, err := RunCommand(t.Context(), "ffmpeg", "-y",
		"-f", "lavfi", "-i", "sine=frequency=123.4:duration=1.0:sample_rate=48000", "-ac", "1", tone,
	); err != nil {
		t.Fatal(err)
	}

	// 高通后峰值电平(dB)：越接近 0 说明瞬变越强。
	seamPeakDB := func(declick bool) float64 {
		aTail, bHead := "", ""
		if declick {
			aTail = ",afade=t=out:st=0.188:d=0.012"
			bHead = ",afade=t=in:st=0:d=0.012"
		}
		filter := "[0:a]atrim=start=0:end=0.2,asetpts=PTS-STARTPTS" + aTail + "[a];" +
			"[0:a]atrim=start=0.4:end=0.6,asetpts=PTS-STARTPTS" + bHead + "[b];" +
			"[a][b]concat=n=2:v=0:a=1,highpass=f=6000,highpass=f=6000,astats=metadata=1:measure_overall=Peak_level,ametadata=print:file=-"
		result, err := RunCommand(t.Context(), "ffmpeg", "-hide_banner", "-nostats", "-loglevel", "error",
			"-i", tone, "-filter_complex", filter, "-f", "null", "-")
		if err != nil {
			t.Fatalf("declick=%v render: %v", declick, err)
		}
		peakPattern := regexp.MustCompile(`lavfi\.astats\.Overall\.Peak_level=(-?[0-9.]+)`)
		// ametadata=print 逐帧打印累计 Overall.Peak_level，取最后一个才是整段(含接缝)的峰值。
		matches := peakPattern.FindAllSubmatch(result.Stdout, -1)
		if len(matches) == 0 {
			t.Fatalf("declick=%v 未解析到 Peak_level: %s", declick, result.Stdout)
		}
		peak, err := strconv.ParseFloat(string(matches[len(matches)-1][1]), 64)
		if err != nil {
			t.Fatalf("declick=%v Peak_level 解析失败: %v", declick, err)
		}
		return peak
	}

	hard := seamPeakDB(false)
	soft := seamPeakDB(true)
	// 微淡把接缝瞬变的高频峰值明显压下去（至少 6dB）。
	if soft >= hard-6 {
		t.Fatalf("去咔哒未显著降低接缝瞬变: 硬拼=%.1fdB 去咔哒=%.1fdB", hard, soft)
	}
	t.Logf("接缝瞬变高频峰值: 硬拼=%.1fdB → 去咔哒=%.1fdB (降 %.1fdB)", hard, soft, hard-soft)
}
