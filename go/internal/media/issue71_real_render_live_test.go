package media

import (
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
)

const (
	defaultIssue71ARoll = "/Users/yoryon/影视飓风剪辑课/课程1-2 第二节课/第二节课-练习素材包/视频/Aroll/Tim-Macbook Neo Talking节选.mp4"
	defaultIssue71BGM   = "/Users/yoryon/影视飓风剪辑课/课程1-2 第二节课/第二节课-练习素材包/音频/Evo.wav"
)

func TestIssue71RealTalkingHeadRenderAcceptance(t *testing.T) {
	if os.Getenv("RUSHES_REAL_RENDER_EVAL") != "1" {
		t.Skip("设置 RUSHES_REAL_RENDER_EVAL=1 才运行真实口播渲染验收")
	}
	aroll := envOrDefault("RUSHES_REAL_RENDER_AROLL", defaultIssue71ARoll)
	bgm := envOrDefault("RUSHES_REAL_RENDER_BGM", defaultIssue71BGM)
	for _, path := range []string{aroll, bgm} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("真实渲染素材不可读 %s: %v", path, err)
		}
	}
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	for _, asset := range []struct {
		id, path, kind string
	}{{"issue71_aroll", aroll, "video"}, {"issue71_bgm", bgm, "audio"}} {
		info, statErr := os.Stat(asset.path)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if _, err := database.Write().ExecContext(t.Context(), `
			INSERT INTO assets(asset_id,storage_mode,reference_path,kind,source,filename,hash,mtime,size,ingest_status,usable)
			VALUES(?,'reference',?,?, 'local_path',?,?,?,?, 'ready',1)`,
			asset.id, asset.path, asset.kind, filepath.Base(asset.path), asset.id,
			info.ModTime().UnixNano(), info.Size(),
		); err != nil {
			t.Fatal(err)
		}
	}
	document, err := timeline.ComposeInitial("issue71_real_render", 1, []timeline.Selection{{
		AssetID: "issue71_aroll", AssetKind: "video", SourceStartFrame: 0, SourceEndFrame: 1200,
		Role: "a_roll", HasAudio: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	document.Tracks[0].Clips[0].FadeInFrames = 12
	document.Tracks[0].Clips[0].FadeOutFrames = 12
	document.Tracks[4].Clips = []timeline.Clip{{
		TimelineClipID: "issue71_bgm_clip", TrackID: "bgm", AssetID: "issue71_bgm", AssetKind: "audio",
		Role: "bgm", TimelineStartFrame: 0, TimelineEndFrame: 1200,
		SourceStartFrame: 0, SourceEndFrame: 1200, PlaybackRate: 1, GainDB: 6,
	}}
	document.Tracks[4].Ducking = &timeline.TrackDucking{
		Enabled: true, DuckDB: -12, TriggerTracks: []string{"original_audio"},
	}
	document.Tracks[5].Clips = []timeline.Clip{{
		TimelineClipID: "issue71_subtitle", TrackID: "subtitles", Text: "指纹解锁位于键盘右上角",
		TimelineStartFrame: 30, TimelineEndFrame: 270, SubtitleStyle: "bold_bottom",
	}}
	profile := RenderProfile{Name: "issue71-real", Width: 640, Height: 360, CRF: 24}
	rendered, err := RenderTimeline(t.Context(), database, document, profile, nil)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := TimelineInspectionIntent(t.Context(), database, document)
	if err != nil {
		t.Fatal(err)
	}
	expected.Width, expected.Height = profile.Width, profile.Height
	expected.FPS, expected.DurationSec = 30, 40
	inspection, err := InspectVideo(t.Context(), rendered.Object.Path, expected, []string{"decode", "black", "silence", "loudness"})
	if err != nil || inspection.Degraded || len(inspection.Issues) != 0 {
		t.Fatalf("inspection=%#v err=%v", inspection, err)
	}
	probe, err := ProbeFile(t.Context(), rendered.Object.Path)
	if err != nil || !probe.HasAudio {
		t.Fatalf("真实口播成片必须包含非静音音轨: probe=%#v err=%v", probe, err)
	}

	// 同一素材、编码参数的无效果对照，排除源画面与源音频本身造成的误判。
	document.Tracks[0].Clips[0].FadeInFrames = 0
	document.Tracks[0].Clips[0].FadeOutFrames = 0
	document.Tracks[4].Ducking.Enabled = false
	document.Tracks[5].Clips = nil
	control, err := RenderTimeline(t.Context(), database, document, profile, nil)
	if err != nil {
		t.Fatal(err)
	}
	fadedFrame := acceptanceGrayFrame(t, rendered.Object.Path, 0.10)
	controlFrame := acceptanceGrayFrame(t, control.Object.Path, 0.10)
	if acceptanceMeanLuma(fadedFrame) >= acceptanceMeanLuma(controlFrame)-5 {
		t.Fatalf("淡入未形成可测亮度变化: faded=%.2f control=%.2f", acceptanceMeanLuma(fadedFrame), acceptanceMeanLuma(controlFrame))
	}
	fadedOutFrame := acceptanceGrayFrame(t, rendered.Object.Path, 39.90)
	controlEndFrame := acceptanceGrayFrame(t, control.Object.Path, 39.90)
	if acceptanceMeanLuma(fadedOutFrame) >= acceptanceMeanLuma(controlEndFrame)-5 {
		t.Fatalf("淡出未形成可测亮度变化: faded=%.2f control=%.2f", acceptanceMeanLuma(fadedOutFrame), acceptanceMeanLuma(controlEndFrame))
	}
	subtitleFrame := acceptanceGrayFrame(t, rendered.Object.Path, 5)
	plainFrame := acceptanceGrayFrame(t, control.Object.Path, 5)
	if difference := acceptanceBottomHalfDifference(subtitleFrame, plainFrame); difference < 0.5 {
		t.Fatalf("字幕区间未形成可测像素变化: mean_abs_difference=%.3f", difference)
	}
	bgmPilot := acceptanceToneAsset(t, database, "issue71_bgm_pilot", 12000, 0, 40)
	triggerPilot := acceptanceToneAsset(t, database, "issue71_trigger_pilot", 14000, 34, 38)
	document.Tracks[4].Clips = append(document.Tracks[4].Clips, timeline.Clip{
		TimelineClipID: "issue71_bgm_pilot_clip", TrackID: "bgm", AssetID: bgmPilot, AssetKind: "audio",
		TimelineStartFrame: 0, TimelineEndFrame: 1200, SourceStartFrame: 0, SourceEndFrame: 1200,
	})
	document.Tracks[3].Clips = []timeline.Clip{{
		TimelineClipID: "issue71_trigger_pilot_clip", TrackID: "voiceover", AssetID: triggerPilot, AssetKind: "audio",
		TimelineStartFrame: 0, TimelineEndFrame: 1200, SourceStartFrame: 0, SourceEndFrame: 1200,
	}}
	document.Tracks[4].Ducking.TriggerTracks = []string{"voiceover"}
	document.Tracks[4].Ducking.Enabled = true
	signalDucked, err := RenderTimeline(t.Context(), database, document, profile, nil)
	if err != nil {
		t.Fatal(err)
	}
	document.Tracks[4].Ducking.Enabled = false
	signalControl, err := RenderTimeline(t.Context(), database, document, profile, nil)
	if err != nil {
		t.Fatal(err)
	}
	duckedBGMSpeech := acceptanceBandMeanVolume(t, signalDucked.Object.Path, 36, 1, 12000)
	controlBGMSpeech := acceptanceBandMeanVolume(t, signalControl.Object.Path, 36, 1, 12000)
	if reduction := duckedBGMSpeech - controlBGMSpeech; math.Abs(reduction+12) > 1.5 {
		t.Fatalf("BGM ducking 不符合 duck_db=-12: ducked=%.2f control=%.2f reduction=%.2f", duckedBGMSpeech, controlBGMSpeech, reduction)
	}
	duckedBGMGap := acceptanceBandMeanVolume(t, signalDucked.Object.Path, 32, 0.6, 12000)
	controlBGMGap := acceptanceBandMeanVolume(t, signalControl.Object.Path, 32, 0.6, 12000)
	if math.Abs(duckedBGMGap-controlBGMGap) > 1.5 {
		t.Fatalf("静音触发区间 BGM 未恢复: ducked=%.2f control=%.2f", duckedBGMGap, controlBGMGap)
	}
	duckedTrigger := acceptanceBandMeanVolume(t, signalDucked.Object.Path, 36, 1, 14000)
	controlTrigger := acceptanceBandMeanVolume(t, signalControl.Object.Path, 36, 1, 14000)
	if math.Abs(duckedTrigger-controlTrigger) > 1.0 {
		t.Fatalf("ducking 不应改变触发轨: ducked=%.2f control=%.2f", duckedTrigger, controlTrigger)
	}
	if output := strings.TrimSpace(os.Getenv("RUSHES_REAL_RENDER_OUTPUT")); output != "" {
		if err := copyAcceptanceArtifact(rendered.Object.Path, output); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf(
		"REAL_RENDER_RESULT width=%d height=%d duration_sec=%.2f bytes=%d duck_db=-12 subtitle_style=bold_bottom fade_frames=12",
		rendered.Width, rendered.Height, rendered.DurationSec, rendered.Object.Size,
	)
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func copyAcceptanceArtifact(source, destination string) error {
	directory := filepath.Dir(destination)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = input.Close() }()
	output, err := os.CreateTemp(directory, "."+filepath.Base(destination)+"-*")
	if err != nil {
		return err
	}
	temporary := output.Name()
	defer func() { _ = os.Remove(temporary) }()
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	return os.Rename(temporary, destination)
}

const acceptanceFrameWidth = 160
const acceptanceFrameHeight = 90

func acceptanceGrayFrame(t *testing.T, path string, timestamp float64) []byte {
	t.Helper()
	result, err := RunCommand(t.Context(), "ffmpeg", "-hide_banner", "-loglevel", "error", "-ss", strconv.FormatFloat(timestamp, 'f', 3, 64),
		"-i", path, "-frames:v", "1", "-vf", "scale=160:90,format=gray", "-f", "rawvideo", "-")
	if err != nil || len(result.Stdout) != acceptanceFrameWidth*acceptanceFrameHeight {
		t.Fatalf("抽取验收帧失败: err=%v bytes=%d", err, len(result.Stdout))
	}
	return result.Stdout
}

func acceptanceMeanLuma(frame []byte) float64 {
	total := 0
	for _, value := range frame {
		total += int(value)
	}
	return float64(total) / float64(len(frame))
}

func acceptanceBottomHalfDifference(left, right []byte) float64 {
	start := acceptanceFrameWidth * acceptanceFrameHeight / 2
	total := 0
	for index := start; index < len(left); index++ {
		difference := int(left[index]) - int(right[index])
		if difference < 0 {
			difference = -difference
		}
		total += difference
	}
	return float64(total) / float64(len(left)-start)
}

func acceptanceBandMeanVolume(t *testing.T, path string, start, duration float64, frequency int) float64 {
	t.Helper()
	return acceptanceFilterMeanVolume(t, []string{path}, start, duration,
		fmt.Sprintf("bandpass=f=%d:w=30,volumedetect", frequency))
}

func acceptanceToneAsset(t *testing.T, database *storage.DB, assetID string, frequency int, start, end float64) string {
	t.Helper()
	path := filepath.Join(database.Paths.Temporary, assetID+".wav")
	if _, err := RunCommand(t.Context(), "ffmpeg", "-y", "-f", "lavfi", "-i",
		fmt.Sprintf(`aevalsrc=if(between(t\,%s\,%s)\,0.03*sin(2*PI*%d*t)\,0):s=48000:d=40`,
			formatSeconds(start), formatSeconds(end), frequency), "-c:a", "pcm_s16le", path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = database.Write().ExecContext(t.Context(), `
		INSERT INTO assets(asset_id,storage_mode,reference_path,kind,source,filename,hash,mtime,size,ingest_status,usable)
		VALUES(?,'reference',?,'audio','local_path',?,?,?,?, 'ready',1)`,
		assetID, path, filepath.Base(path), assetID, info.ModTime().UnixNano(), info.Size()); err != nil {
		t.Fatal(err)
	}
	return assetID
}

func acceptanceFilterMeanVolume(t *testing.T, paths []string, start, duration float64, filter string) float64 {
	t.Helper()
	args := []string{"-hide_banner", "-nostats"}
	for _, path := range paths {
		args = append(args, "-ss", formatSeconds(start), "-t", formatSeconds(duration), "-i", path)
	}
	args = append(args, "-filter_complex", filter, "-f", "null", "-")
	result, err := RunCommand(t.Context(), "ffmpeg", args...)
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`mean_volume: (-?[0-9.]+) dB`).FindStringSubmatch(string(result.Stderr))
	if len(match) != 2 {
		t.Fatalf("无法解析验收区间音量: %s", result.Stderr)
	}
	value, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
