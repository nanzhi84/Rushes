package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
)

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

type shortWriter struct{}

func (shortWriter) Write(data []byte) (int, error) { return max(0, len(data)-1), nil }

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestObjectStoreDeduplicatesContent(t *testing.T) {
	t.Parallel()
	paths, err := storage.NewPaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := NewObjectStore(paths)
	first, err := store.Put(t.Context(), bytes.NewReader([]byte("same")))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Put(t.Context(), bytes.NewReader([]byte("same")))
	if err != nil {
		t.Fatal(err)
	}
	if first.Hash != second.Hash || first.Path != second.Path || first.Size != 4 {
		t.Fatalf("first=%#v second=%#v", first, second)
	}
	if data, err := os.ReadFile(first.Path); err != nil || string(data) != "same" {
		t.Fatalf("data=%q err=%v", data, err)
	}
}

func TestRenderTimelineAndInspectSnapshot(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 未安装")
	}
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	source := filepath.Join(database.Paths.Temporary, "render-source.mp4")
	if _, err := RunCommand(t.Context(), "ffmpeg", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=30:duration=1",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", source); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(source)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO assets(asset_id,storage_mode,reference_path,kind,source,filename,hash,mtime,size,ingest_status,usable)
		VALUES('render_asset','reference',?,'video','local_path','render-source.mp4','hash',?,?,'ready',1)`,
		source, info.ModTime().UnixNano(), info.Size()); err != nil {
		t.Fatal(err)
	}
	document, err := timeline.ComposeInitial("draft", 1, []timeline.Selection{{
		AssetID: "render_asset", AssetKind: "video", HasAudio: true,
		SourceStartFrame: 0, SourceEndFrame: 30, Role: "a_roll",
	}})
	if err != nil {
		t.Fatal(err)
	}
	document.Tracks[1].Clips = []timeline.Clip{{
		TimelineClipID: "overlay_render", TrackID: "visual_overlay", AssetID: "render_asset", AssetKind: "video",
		TimelineStartFrame: 10, TimelineEndFrame: 25, SourceStartFrame: 0, SourceEndFrame: 15, PlaybackRate: 1,
	}}
	document.Tracks[5].Clips = []timeline.Clip{{
		TimelineClipID: "subtitle_render", TrackID: "subtitles", Text: "真实多轨预览",
		TimelineStartFrame: 5, TimelineEndFrame: 25,
	}}
	var progress int
	rendered, err := RenderTimeline(t.Context(), database, document, PreviewProfile, func(Progress) { progress++ })
	if err != nil || rendered.Object.Size == 0 || progress == 0 {
		t.Fatalf("rendered=%#v progress=%d err=%v", rendered, progress, err)
	}
	probe, err := ProbeFile(t.Context(), rendered.Object.Path)
	if err != nil || !probe.HasAudio {
		t.Fatalf("rendered probe=%#v err=%v", probe, err)
	}
	inspection, err := InspectVideo(t.Context(), rendered.Object.Path, ExpectedVideo{
		Width: rendered.Width, Height: rendered.Height, FPS: rendered.FPS, DurationSec: rendered.DurationSec,
	}, []string{"streams", "decode"})
	if err != nil || inspection.Degraded || len(inspection.Issues) != 0 {
		t.Fatalf("inspection=%#v err=%v now=%s", inspection, err, now)
	}
	mismatch, err := InspectVideo(t.Context(), rendered.Object.Path, ExpectedVideo{Width: 999, Height: 999}, []string{"black"})
	if err != nil || mismatch.Degraded || len(mismatch.Issues) != 1 || mismatch.Issues[0].Check != "streams" {
		t.Fatalf("mismatch=%#v err=%v", mismatch, err)
	}
}

func TestInspectVideoSignalsReturnsFrameAnchorsWithoutCleanFalsePositives(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 未安装")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe 未安装")
	}
	directory := t.TempDir()
	broken := filepath.Join(directory, "broken.mp4")
	_, err := RunCommand(t.Context(), "ffmpeg", "-y",
		"-f", "lavfi", "-i", "color=black:s=64x64:d=0.5:r=30",
		"-f", "lavfi", "-i", "color=red:s=64x64:d=0.5:r=30",
		"-f", "lavfi", "-i", "anullsrc=r=48000:cl=stereo:d=1",
		"-filter_complex", "[0:v][1:v]concat=n=2:v=1:a=0[v]", "-map", "[v]", "-map", "2:a",
		"-c:v", "libx264", "-c:a", "aac", broken,
	)
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := InspectVideo(t.Context(), broken, ExpectedVideo{Width: 64, Height: 64, FPS: 30, DurationSec: 1}, nil)
	if err != nil || inspection.Degraded {
		t.Fatalf("inspection=%#v err=%v", inspection, err)
	}
	byCheck := map[string]InspectionIssue{}
	for _, issue := range inspection.Issues {
		byCheck[issue.Check] = issue
	}
	for _, check := range []string{"black", "freeze", "silence", "loudness"} {
		issue, ok := byCheck[check]
		if !ok || len(issue.Frames) != 2 || issue.Frames[1] <= issue.Frames[0] {
			t.Fatalf("%s issue=%#v all=%#v", check, issue, inspection.Issues)
		}
	}
	if frames := byCheck["black"].Frames; frames[0] != 0 || frames[1] != 15 {
		t.Fatalf("black frames=%v", frames)
	}

	clean := filepath.Join(directory, "clean.mp4")
	_, err = RunCommand(t.Context(), "ffmpeg", "-y",
		"-f", "lavfi", "-i", "testsrc2=s=64x64:d=1:r=30",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		"-map", "0:v", "-map", "1:a", "-c:v", "libx264", "-c:a", "aac", clean,
	)
	if err != nil {
		t.Fatal(err)
	}
	cleanInspection, err := InspectVideo(t.Context(), clean, ExpectedVideo{Width: 64, Height: 64, FPS: 30, DurationSec: 1}, nil)
	if err != nil || cleanInspection.Degraded || len(cleanInspection.Issues) != 0 {
		t.Fatalf("clean inspection=%#v err=%v", cleanInspection, err)
	}
}

func TestInspectVideoDecodeIssueDoesNotExposeCommandError(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 未安装")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe 未安装")
	}
	directory := t.TempDir()
	videoPath := filepath.Join(directory, "sensitive-object-hash.mp4")
	if _, err := RunCommand(t.Context(), "ffmpeg", "-y", "-f", "lavfi", "-i", "testsrc2=s=64x64:d=1:r=30", videoPath); err != nil {
		t.Fatal(err)
	}
	fakeBin := filepath.Join(directory, "bin")
	if err := os.Mkdir(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeFFmpeg := filepath.Join(fakeBin, "ffmpeg")
	if err := os.WriteFile(fakeFFmpeg, []byte("#!/bin/sh\necho 'decoder leaked "+videoPath+"' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	inspection, err := InspectVideo(t.Context(), videoPath, ExpectedVideo{}, []string{"decode"})
	if err != nil || len(inspection.Issues) != 1 {
		t.Fatalf("inspection=%#v err=%v", inspection, err)
	}
	issue := inspection.Issues[0]
	if issue.ErrorCode != "preview_decode_failed" || issue.Message != "预览视频无法完整解码。" {
		t.Fatalf("decode issue=%#v", issue)
	}
	encoded, err := json.Marshal(inspection)
	if err != nil {
		t.Fatal(err)
	}
	for _, sensitive := range []string{videoPath, "sensitive-object-hash", "decoder leaked", "ffmpeg"} {
		if strings.Contains(string(encoded), sensitive) {
			t.Fatalf("inspection leaked %q: %s", sensitive, encoded)
		}
	}
}

func TestInspectVideoReportsMissingAudioOnlyWhenTimelineExpectedAudio(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 未安装")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe 未安装")
	}
	path := filepath.Join(t.TempDir(), "video-only.mp4")
	if _, err := RunCommand(t.Context(), "ffmpeg", "-y", "-f", "lavfi", "-i", "testsrc2=s=64x64:d=1:r=30", "-an", "-c:v", "libx264", path); err != nil {
		t.Fatal(err)
	}
	inspection, err := InspectVideo(t.Context(), path, ExpectedVideo{Width: 64, Height: 64, FPS: 30, DurationSec: 1}, nil)
	if err != nil || len(inspection.Issues) != 0 {
		t.Fatalf("合法无音轨预览不应降级: inspection=%#v err=%v", inspection, err)
	}
	inspection, err = InspectVideo(t.Context(), path, ExpectedVideo{
		Width: 64, Height: 64, FPS: 30, DurationSec: 1, ExpectAudio: true,
	}, []string{"silence"})
	if err != nil || len(inspection.Issues) != 1 || inspection.Issues[0].Check != "audio_stream" || inspection.Issues[0].Severity != "error" {
		t.Fatalf("audio inspection=%#v err=%v", inspection, err)
	}
	inspection, err = InspectVideo(t.Context(), path, ExpectedVideo{Width: 64, Height: 64, FPS: 30, DurationSec: 1}, []string{"decode", "black"})
	if err != nil {
		t.Fatal(err)
	}
	for _, issue := range inspection.Issues {
		if issue.Check == "audio_stream" {
			t.Fatalf("未请求音频检查时不应报告音轨缺失: %#v", inspection)
		}
	}
}

func TestTimelineInspectionIntentTracksFadesImagesAndRenderableAudio(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 未安装")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe 未安装")
	}
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	image := filepath.Join(database.Paths.Temporary, "still.png")
	video := filepath.Join(database.Paths.Temporary, "with-audio.mp4")
	if _, err := RunCommand(t.Context(), "ffmpeg", "-y", "-f", "lavfi", "-i", "color=blue:s=64x64", "-frames:v", "1", image); err != nil {
		t.Fatal(err)
	}
	if _, err := RunCommand(t.Context(), "ffmpeg", "-y",
		"-f", "lavfi", "-i", "testsrc2=s=64x64:d=1:r=30",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		"-c:v", "libx264", "-c:a", "aac", "-shortest", video); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO assets(asset_id,storage_mode,reference_path,kind,source,filename,hash,size,ingest_status,usable)
		VALUES('still','reference',?,'image','local_path','still.png','still',1,'ready',1),
		      ('talk','reference',?,'video','local_path','talk.mp4','talk',1,'ready',1)`, image, video); err != nil {
		t.Fatal(err)
	}
	document := timeline.Empty("intent", 1)
	document.DurationFrames = 60
	document.Tracks[0].Clips = []timeline.Clip{
		{TimelineClipID: "fade", TrackID: "visual_base", AssetID: "talk", AssetKind: "video", TimelineStartFrame: 0, TimelineEndFrame: 30, FadeInFrames: 3, FadeOutFrames: 4},
		{TimelineClipID: "still", TrackID: "visual_base", AssetID: "still", AssetKind: "image", TimelineStartFrame: 30, TimelineEndFrame: 60},
	}
	intent, err := TimelineInspectionIntent(t.Context(), database, document)
	if err != nil {
		t.Fatal(err)
	}
	if !intent.ExpectAudio || !reflect.DeepEqual(intent.BlackFrames, []FrameInterval{{Start: 0, End: 3}, {Start: 26, End: 30}}) ||
		!reflect.DeepEqual(intent.FreezeFrames, []FrameInterval{{Start: 30, End: 60}}) {
		t.Fatalf("intent=%#v", intent)
	}
}

func TestRenderedDuckingLowersBGMUnderVoiceAndDisabledPreservesStaticMix(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 未安装")
	}
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	video := filepath.Join(database.Paths.Temporary, "duck-video.mp4")
	bgm := filepath.Join(database.Paths.Temporary, "duck-bgm.wav")
	commands := [][]string{
		{"-y", "-f", "lavfi", "-i", "testsrc2=s=160x90:r=30:d=4",
			"-f", "lavfi", "-i", `aevalsrc=if(between(t\,1\,2)\,0.002*sin(2*PI*200*t)\,0):s=48000:d=4`,
			"-map", "0:v", "-map", "1:a", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", video},
		{"-y", "-f", "lavfi", "-i", "sine=frequency=2000:duration=4,volume=0.2", "-c:a", "pcm_s16le", bgm},
	}
	voiceAssets := []struct {
		id        string
		amplitude float64
		path      string
	}{
		{id: "duck_voice_low", amplitude: 0.005, path: filepath.Join(database.Paths.Temporary, "duck-voice-low.wav")},
		{id: "duck_voice_mid", amplitude: 0.05, path: filepath.Join(database.Paths.Temporary, "duck-voice-mid.wav")},
		{id: "duck_voice_high", amplitude: 0.8, path: filepath.Join(database.Paths.Temporary, "duck-voice-high.wav")},
		{id: "duck_noise", amplitude: 0.002, path: filepath.Join(database.Paths.Temporary, "duck-noise.wav")},
	}
	for _, voice := range voiceAssets {
		commands = append(commands, []string{"-y", "-f", "lavfi", "-i", fmt.Sprintf(
			`aevalsrc=if(between(t\,1\,2)\,%s*sin(2*PI*200*t)\,0):s=48000:d=4`, formatSeconds(voice.amplitude),
		), "-c:a", "pcm_s16le", voice.path})
	}
	for _, args := range commands {
		if _, err := RunCommand(t.Context(), "ffmpeg", args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, asset := range []struct {
		id, path, kind string
	}{{"duck_video", video, "video"}, {"duck_bgm", bgm, "audio"},
		{voiceAssets[0].id, voiceAssets[0].path, "audio"},
		{voiceAssets[1].id, voiceAssets[1].path, "audio"},
		{voiceAssets[2].id, voiceAssets[2].path, "audio"},
		{voiceAssets[3].id, voiceAssets[3].path, "audio"}} {
		info, statErr := os.Stat(asset.path)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if _, err := database.Write().ExecContext(t.Context(), `
			INSERT INTO assets(asset_id,storage_mode,reference_path,kind,source,filename,hash,mtime,size,ingest_status,usable)
			VALUES(?,?,?,?,?,?,?,?,?,'ready',1)`,
			asset.id, "reference", asset.path, asset.kind, "local_path", filepath.Base(asset.path), asset.id,
			info.ModTime().UnixNano(), info.Size(),
		); err != nil {
			t.Fatal(err)
		}
	}
	document, err := timeline.ComposeInitial("duck_render", 1, []timeline.Selection{{
		AssetID: "duck_video", AssetKind: "video", SourceStartFrame: 0, SourceEndFrame: 120, Role: "a_roll",
	}})
	if err != nil {
		t.Fatal(err)
	}
	document.Tracks[3].Clips = []timeline.Clip{{
		TimelineClipID: "voice", TrackID: "voiceover", AssetID: voiceAssets[1].id, AssetKind: "audio",
		TimelineStartFrame: 30, TimelineEndFrame: 60, SourceStartFrame: 30, SourceEndFrame: 60,
	}}
	document.Tracks[4].Clips = []timeline.Clip{{
		TimelineClipID: "bgm", TrackID: "bgm", AssetID: "duck_bgm", AssetKind: "audio",
		TimelineStartFrame: 0, TimelineEndFrame: 120, SourceStartFrame: 0, SourceEndFrame: 120,
	}}
	profile := RenderProfile{Name: "fixture", Width: 160, Height: 90, CRF: 24}
	document.Tracks[4].Ducking = &timeline.TrackDucking{Enabled: false, DuckDB: -12, TriggerTracks: []string{"voiceover"}}
	staticMix, err := RenderTimeline(t.Context(), database, document, profile, nil)
	if err != nil {
		t.Fatal(err)
	}
	staticGap := measuredBandMeanVolume(t, staticMix.Object.Path, 0.3, 0.5, 2000)
	staticSpeech := measuredBandMeanVolume(t, staticMix.Object.Path, 1.3, 0.5, 2000)
	staticPostGap := measuredBandMeanVolume(t, staticMix.Object.Path, 2.7, 0.5, 2000)
	if abs(staticSpeech-staticGap) > 1.5 || abs(staticPostGap-staticGap) > 1.5 {
		t.Fatalf("disabled ducking changed BGM: gap=%.2f speech=%.2f post_gap=%.2f", staticGap, staticSpeech, staticPostGap)
	}
	document.Tracks[4].Ducking.Enabled = true
	cases := []struct {
		voiceID string
		duckDB  float64
	}{
		{voiceAssets[0].id, -12},
		{voiceAssets[1].id, -6},
		{voiceAssets[1].id, -12},
		{voiceAssets[1].id, -18},
		{voiceAssets[2].id, -12},
	}
	for _, item := range cases {
		document.Tracks[3].Clips[0].AssetID = item.voiceID
		document.Tracks[4].Ducking.DuckDB = item.duckDB
		duckedMix, renderErr := RenderTimeline(t.Context(), database, document, profile, nil)
		if renderErr != nil {
			t.Fatal(renderErr)
		}
		duckedGap := measuredBandMeanVolume(t, duckedMix.Object.Path, 0.3, 0.5, 2000)
		duckedSpeech := measuredBandMeanVolume(t, duckedMix.Object.Path, 1.3, 0.5, 2000)
		duckedPostGap := measuredBandMeanVolume(t, duckedMix.Object.Path, 2.7, 0.5, 2000)
		measuredDrop := duckedSpeech - duckedGap
		if math.Abs(measuredDrop-item.duckDB) > 1.5 || abs(duckedGap-staticGap) > 1.5 || abs(duckedPostGap-staticPostGap) > 1.5 {
			t.Fatalf("voice=%s duck_db %.0f produced %.2fdB: static_gap=%.2f gap=%.2f speech=%.2f post_gap=%.2f",
				item.voiceID, item.duckDB, measuredDrop, staticGap, duckedGap, duckedSpeech, duckedPostGap)
		}
	}
	document.Tracks[3].Clips[0].AssetID = voiceAssets[3].id
	document.Tracks[4].Ducking.DuckDB = -12
	noiseMix, err := RenderTimeline(t.Context(), database, document, profile, nil)
	if err != nil {
		t.Fatal(err)
	}
	noiseGap := measuredBandMeanVolume(t, noiseMix.Object.Path, 0.3, 0.5, 2000)
	noiseTrigger := measuredBandMeanVolume(t, noiseMix.Object.Path, 1.3, 0.5, 2000)
	noisePostGap := measuredBandMeanVolume(t, noiseMix.Object.Path, 2.7, 0.5, 2000)
	if abs(noiseTrigger-noiseGap) > 1.5 || abs(noiseGap-staticGap) > 1.5 || abs(noisePostGap-staticPostGap) > 1.5 {
		t.Fatalf("sub-threshold noise changed BGM: static_gap=%.2f gap=%.2f trigger=%.2f post_gap=%.2f",
			staticGap, noiseGap, noiseTrigger, noisePostGap)
	}
	document.Tracks[4].Ducking.TriggerTracks = []string{"voiceover", "original_audio"}
	doubleNoiseMix, err := RenderTimeline(t.Context(), database, document, profile, nil)
	if err != nil {
		t.Fatal(err)
	}
	doubleNoiseGap := measuredBandMeanVolume(t, doubleNoiseMix.Object.Path, 0.3, 0.5, 2000)
	doubleNoiseTrigger := measuredBandMeanVolume(t, doubleNoiseMix.Object.Path, 1.3, 0.5, 2000)
	doubleNoisePostGap := measuredBandMeanVolume(t, doubleNoiseMix.Object.Path, 2.7, 0.5, 2000)
	if abs(doubleNoiseTrigger-doubleNoiseGap) > 1.5 || abs(doubleNoiseGap-staticGap) > 1.5 || abs(doubleNoisePostGap-staticPostGap) > 1.5 {
		t.Fatalf("two sub-threshold trigger tracks changed BGM: static_gap=%.2f gap=%.2f trigger=%.2f post_gap=%.2f",
			staticGap, doubleNoiseGap, doubleNoiseTrigger, doubleNoisePostGap)
	}
	document.Tracks[4].Ducking.TriggerTracks = []string{"voiceover"}
	document.Tracks[3].Clips = []timeline.Clip{
		{TimelineClipID: "noise_1", TrackID: "voiceover", AssetID: voiceAssets[3].id, AssetKind: "audio", TimelineStartFrame: 30, TimelineEndFrame: 60, SourceStartFrame: 30, SourceEndFrame: 60},
		{TimelineClipID: "noise_2", TrackID: "voiceover", AssetID: voiceAssets[3].id, AssetKind: "audio", TimelineStartFrame: 30, TimelineEndFrame: 60, SourceStartFrame: 30, SourceEndFrame: 60},
		{TimelineClipID: "noise_3", TrackID: "voiceover", AssetID: voiceAssets[3].id, AssetKind: "audio", TimelineStartFrame: 30, TimelineEndFrame: 60, SourceStartFrame: 30, SourceEndFrame: 60},
	}
	overlappingNoiseMix, err := RenderTimeline(t.Context(), database, document, profile, nil)
	if err != nil {
		t.Fatal(err)
	}
	overlappingNoiseGap := measuredBandMeanVolume(t, overlappingNoiseMix.Object.Path, 0.3, 0.5, 2000)
	overlappingNoiseTrigger := measuredBandMeanVolume(t, overlappingNoiseMix.Object.Path, 1.3, 0.5, 2000)
	overlappingNoisePostGap := measuredBandMeanVolume(t, overlappingNoiseMix.Object.Path, 2.7, 0.5, 2000)
	if abs(overlappingNoiseTrigger-overlappingNoiseGap) > 1.5 || abs(overlappingNoiseGap-staticGap) > 1.5 || abs(overlappingNoisePostGap-staticPostGap) > 1.5 {
		t.Fatalf("overlapping sub-threshold clips changed BGM: static_gap=%.2f gap=%.2f trigger=%.2f post_gap=%.2f",
			staticGap, overlappingNoiseGap, overlappingNoiseTrigger, overlappingNoisePostGap)
	}
}

func TestRenderedVideoFadesApproachBlackAtBothEnds(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 未安装")
	}
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	source := filepath.Join(database.Paths.Temporary, "white.mp4")
	if _, err := RunCommand(t.Context(), "ffmpeg", "-y", "-f", "lavfi", "-i", "color=white:s=160x90:r=30:d=1", "-c:v", "libx264", "-pix_fmt", "yuv420p", source); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(source)
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO assets(asset_id,storage_mode,reference_path,kind,source,filename,hash,mtime,size,ingest_status,usable)
		VALUES('fade_video','reference',?,'video','local_path','white.mp4','fade',?,?,'ready',1)`,
		source, info.ModTime().UnixNano(), info.Size()); err != nil {
		t.Fatal(err)
	}
	document, err := timeline.ComposeInitial("fade_render", 1, []timeline.Selection{{
		AssetID: "fade_video", AssetKind: "video", SourceStartFrame: 0, SourceEndFrame: 30, Role: "a_roll",
	}})
	if err != nil {
		t.Fatal(err)
	}
	document.Tracks[0].Clips[0].FadeInFrames = 10
	document.Tracks[0].Clips[0].FadeOutFrames = 10
	rendered, err := RenderTimeline(t.Context(), database, document, RenderProfile{Name: "fixture", Width: 160, Height: 90, CRF: 20}, nil)
	if err != nil {
		t.Fatal(err)
	}
	first := measuredFrameLuma(t, rendered.Object.Path, 0)
	middle := measuredFrameLuma(t, rendered.Object.Path, 0.5)
	last := measuredFrameLuma(t, rendered.Object.Path, 0.96)
	if first > 30 || last > 50 || middle < 200 {
		t.Fatalf("luma first=%d middle=%d last=%d", first, middle, last)
	}
}

func TestRenderedOverlayFadesRevealUnderlyingVideo(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 未安装")
	}
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	for _, fixture := range []struct {
		id, color string
	}{{"overlay_base", "white"}, {"overlay_top", "black"}} {
		path := filepath.Join(database.Paths.Temporary, fixture.id+".mp4")
		if _, err := RunCommand(t.Context(), "ffmpeg", "-y", "-f", "lavfi", "-i",
			"color="+fixture.color+":s=160x90:r=30:d=1", "-c:v", "libx264", "-pix_fmt", "yuv420p", path); err != nil {
			t.Fatal(err)
		}
		info, _ := os.Stat(path)
		if _, err := database.Write().ExecContext(t.Context(), `
			INSERT INTO assets(asset_id,storage_mode,reference_path,kind,source,filename,hash,mtime,size,ingest_status,usable)
			VALUES(?,'reference',?,'video','local_path',?,?,?,?,'ready',1)`,
			fixture.id, path, fixture.id+".mp4", fixture.id, info.ModTime().UnixNano(), info.Size()); err != nil {
			t.Fatal(err)
		}
	}
	document, err := timeline.ComposeInitial("overlay_fade_render", 1, []timeline.Selection{{
		AssetID: "overlay_base", AssetKind: "video", SourceStartFrame: 0, SourceEndFrame: 30, Role: "a_roll",
	}})
	if err != nil {
		t.Fatal(err)
	}
	document.Tracks[1].Clips = []timeline.Clip{{
		TimelineClipID: "overlay_fade", TrackID: "visual_overlay", AssetID: "overlay_top", AssetKind: "video",
		TimelineStartFrame: 0, TimelineEndFrame: 30, SourceStartFrame: 0, SourceEndFrame: 30,
		PlaybackRate: 1, FadeInFrames: 10, FadeOutFrames: 10,
	}}
	rendered, err := RenderTimeline(t.Context(), database, document, RenderProfile{Name: "fixture", Width: 160, Height: 90, CRF: 20}, nil)
	if err != nil {
		t.Fatal(err)
	}
	first := measuredFrameLuma(t, rendered.Object.Path, 0)
	middle := measuredFrameLuma(t, rendered.Object.Path, 0.5)
	last := measuredFrameLuma(t, rendered.Object.Path, 0.96)
	if first < 200 || last < 180 || middle > 40 {
		t.Fatalf("overlay must reveal base: first=%d middle=%d last=%d", first, middle, last)
	}
}

func measuredBandMeanVolume(t *testing.T, path string, start, duration float64, frequency int) float64 {
	t.Helper()
	result, err := RunCommand(t.Context(), "ffmpeg", "-hide_banner", "-nostats", "-ss", formatSeconds(start), "-t", formatSeconds(duration),
		"-i", path, "-vn", "-af", fmt.Sprintf("bandpass=f=%d:w=100,volumedetect", frequency), "-f", "null", "-")
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`mean_volume: (-?[0-9.]+) dB`).FindStringSubmatch(string(result.Stderr))
	if len(match) != 2 {
		t.Fatalf("missing mean_volume: %s", result.Stderr)
	}
	value, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func measuredFrameLuma(t *testing.T, path string, timestamp float64) int {
	t.Helper()
	result, err := RunCommand(t.Context(), "ffmpeg", "-hide_banner", "-loglevel", "error", "-i", path,
		"-ss", formatSeconds(timestamp), "-frames:v", "1", "-vf", "scale=1:1,format=gray", "-f", "rawvideo", "-")
	if err != nil || len(result.Stdout) == 0 {
		t.Fatalf("frame luma err=%v bytes=%d", err, len(result.Stdout))
	}
	return int(result.Stdout[0])
}

func TestParseInspectionSignalsClosesIntervalsAtMediaEnd(t *testing.T) {
	stderr := strings.Join([]string{
		"black_start:0 black_end:0.25 black_duration:0.25",
		"lavfi.freezedetect.freeze_start: 0.5",
		"silence_start: 0.75",
		"  Integrated loudness:",
		"    I:         -30.0 LUFS",
	}, "\n")
	issues := parseInspectionSignals(stderr, 2, 30, nil)
	if len(issues) != 4 || issues[1].Frames[0] != 15 || issues[1].Frames[1] != 60 ||
		issues[2].Frames[0] != 23 || issues[2].Frames[1] != 60 || issues[3].Check != "loudness" {
		t.Fatalf("issues=%#v", issues)
	}
}

func TestParseInspectionSignalsRejectsUnusableLoudnessSummary(t *testing.T) {
	for _, test := range []struct {
		name   string
		stderr string
		want   string
	}{
		{name: "negative infinity", stderr: "Integrated loudness:\n I: -inf LUFS", want: "完全静音"},
		{name: "missing summary", stderr: "ebur128 analysis started", want: "未能解析"},
	} {
		t.Run(test.name, func(t *testing.T) {
			issues := parseInspectionSignals(test.stderr, 2, 30, []string{"loudness"})
			if len(issues) != 1 || issues[0].Check != "loudness" || !strings.Contains(issues[0].Message, test.want) {
				t.Fatalf("issues=%#v", issues)
			}
		})
	}
}

func TestFilterExpectedSignalIssuesKeepsOnlyUnexpectedRemainder(t *testing.T) {
	issues := []InspectionIssue{
		intervalInspectionIssue("black", "检测到黑帧区间", 0, 0.2, 30),
		intervalInspectionIssue("freeze", "检测到静帧区间", 1, 2, 30),
	}
	filtered := filterExpectedSignalIssues(issues, ExpectedVideo{
		BlackFrames:  []FrameInterval{{Start: 0, End: 6}},
		FreezeFrames: []FrameInterval{{Start: 30, End: 45}},
	}, 30)
	if len(filtered) != 1 || filtered[0].Check != "freeze" || !reflect.DeepEqual(filtered[0].Frames, []int{45, 60}) {
		t.Fatalf("filtered=%#v", filtered)
	}
}

func TestParseProbe(t *testing.T) {
	t.Parallel()
	probe, err := parseProbe([]byte(`{
		"format":{"duration":"2.5"},
		"streams":[
			{"codec_type":"video","avg_frame_rate":"30000/1001","width":1920,"height":1080},
			{"codec_type":"audio"}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if probe.DurationSec != 2.5 || probe.FPS == nil || probe.Width == nil || !probe.HasAudio {
		t.Fatalf("probe=%#v", probe)
	}
}

func TestParseProbeDisplayRotationSwapsDisplayDimensions(t *testing.T) {
	for _, test := range []struct {
		name       string
		meta       string
		wantWidth  int
		wantHeight int
	}{
		{"side_data_90", `"side_data_list":[{"rotation":90}]`, 1080, 1920},
		{"side_data_270", `"side_data_list":[{"rotation":-90}]`, 1080, 1920},
		{"tag_90", `"tags":{"rotate":"90"}`, 1080, 1920},
		{"zero", `"side_data_list":[{"rotation":0}]`, 1920, 1080},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := fmt.Sprintf(`{"streams":[{"codec_type":"video","width":1920,"height":1080,%s}]}`, test.meta)
			probe, err := parseProbe([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			width, height := probe.displayDimensions()
			if width == nil || height == nil || *width != test.wantWidth || *height != test.wantHeight {
				t.Fatalf("probe=%#v display=%v×%v", probe, width, height)
			}
		})
	}
}

func TestRenderTimelineAutoOrientationUsesDisplayRotation(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 未安装")
	}
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	base := filepath.Join(database.Paths.Temporary, "coded-landscape.mp4")
	if _, err := RunCommand(t.Context(), "ffmpeg", "-y", "-f", "lavfi", "-i",
		"color=blue:s=160x90:r=30:d=0.4", "-c:v", "libx264", "-pix_fmt", "yuv420p", base); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		rotation string
		portrait bool
	}{
		{"zero", "", false},
		{"ninety", "90", true},
		{"two_seventy", "-90", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := base
			if test.rotation != "" {
				source = filepath.Join(database.Paths.Temporary, test.name+".mp4")
				if _, err := RunCommand(t.Context(), "ffmpeg", "-y",
					"-display_rotation:v:0", test.rotation, "-i", base, "-c", "copy", source); err != nil {
					t.Fatal(err)
				}
			}
			info, err := os.Stat(source)
			if err != nil {
				t.Fatal(err)
			}
			assetID := "rotation_" + test.name
			if _, err := database.Write().ExecContext(t.Context(), `
				INSERT INTO assets(asset_id,storage_mode,reference_path,kind,source,filename,hash,mtime,size,ingest_status,usable)
				VALUES(?,'reference',?,'video','local_path',?,?,?,?,'ready',1)`,
				assetID, source, filepath.Base(source), assetID, info.ModTime().UnixNano(), info.Size()); err != nil {
				t.Fatal(err)
			}
			document, err := timeline.ComposeInitial("rotation_render", 1, []timeline.Selection{{
				AssetID: assetID, AssetKind: "video", SourceStartFrame: 0, SourceEndFrame: 12, Role: "a_roll",
			}})
			if err != nil {
				t.Fatal(err)
			}
			rendered, err := RenderTimeline(t.Context(), database, document, RenderProfile{
				Name: "rotation", Width: 160, Height: 90, CRF: 24, AutoOrient: true,
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			wantWidth, wantHeight := 160, 90
			if test.portrait {
				wantWidth, wantHeight = 90, 160
			}
			probe, err := ProbeFile(t.Context(), rendered.Object.Path)
			if err != nil || rendered.Width != wantWidth || rendered.Height != wantHeight ||
				probe.Width == nil || probe.Height == nil || *probe.Width != wantWidth || *probe.Height != wantHeight {
				t.Fatalf("rendered=%#v probe=%#v err=%v want=%dx%d", rendered, probe, err, wantWidth, wantHeight)
			}
		})
	}
}

func TestFFmpegProbeThumbnailAndProxy(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 未安装")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe 未安装")
	}
	paths, err := storage.NewPaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Initialize(); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(paths.Temporary, "source.mp4")
	_, err = RunCommand(context.Background(), "ffmpeg", "-y", "-f", "lavfi", "-i", "color=c=blue:s=320x240:d=1", "-f", "lavfi", "-i", "sine=frequency=440:duration=1", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", source)
	if err != nil {
		t.Fatal(err)
	}
	probe, err := ProbeFile(t.Context(), source)
	if err != nil || probe.DurationSec <= 0 || !probe.HasAudio {
		t.Fatalf("probe=%#v err=%v", probe, err)
	}
	store := NewObjectStore(paths)
	thumbnail, err := GenerateThumbnail(t.Context(), store, source, "video")
	if err != nil || thumbnail == nil || thumbnail.Size == 0 {
		t.Fatalf("thumbnail=%#v err=%v", thumbnail, err)
	}
	var progressCalls int
	proxy, err := GenerateProxy(t.Context(), store, source, "video", func(Progress) { progressCalls++ })
	if err != nil || proxy == nil || proxy.Size == 0 || progressCalls == 0 {
		t.Fatalf("proxy=%#v progress=%d err=%v", proxy, progressCalls, err)
	}
}

func TestObjectStoreProcessAndProbeErrorBranches(t *testing.T) {
	t.Parallel()
	paths, err := storage.NewPaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := NewObjectStore(paths)
	if _, err := store.PutFile(t.Context(), filepath.Join(paths.Root, "missing")); err == nil {
		t.Fatal("missing file should fail")
	}
	if _, err := store.Put(t.Context(), failingReader{}); err == nil {
		t.Fatal("reader failure should propagate")
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := store.Put(cancelled, bytes.NewReader([]byte("x"))); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel err=%v", err)
	}
	if _, err := copyWithContext(t.Context(), shortWriter{}, bytes.NewReader([]byte("abc"))); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write err=%v", err)
	}
	if _, err := copyWithContext(t.Context(), failingWriter{}, bytes.NewReader([]byte("abc"))); err == nil {
		t.Fatal("writer failure should propagate")
	}

	commandErr := &CommandError{Name: "tool", Err: errors.New("boom")}
	if !strings.Contains(commandErr.Error(), "tool 执行失败") || !errors.Is(commandErr, commandErr.Err) {
		t.Fatalf("command err=%v", commandErr)
	}
	commandErr.Stderr = "details"
	if !strings.Contains(commandErr.Error(), "details") {
		t.Fatalf("stderr missing: %v", commandErr)
	}
	if _, err := RunCommand(t.Context(), "rushes-command-does-not-exist"); err == nil {
		t.Fatal("missing command should fail")
	}
	if got := stderrSummary(strings.Repeat("line\n", 12)); strings.Count(got, "\n") > 7 {
		t.Fatalf("stderr summary=%q", got)
	}
	if err := RunFFmpegProgress(t.Context(), "rushes-ffmpeg-does-not-exist", nil, nil); err == nil {
		t.Fatal("missing ffmpeg should fail")
	}

	if _, err := parseProbe([]byte("{")); err == nil {
		t.Fatal("invalid probe JSON should fail")
	}
	probe, err := parseProbe([]byte(`{"format":{"duration":"0"},"streams":[{"codec_type":"video","duration":"2","avg_frame_rate":"25","width":0,"height":0},{"codec_type":"audio","duration":"3"}]}`))
	if err != nil || probe.DurationSec != 2 || probe.FPS == nil || !probe.HasAudio {
		t.Fatalf("probe=%#v err=%v", probe, err)
	}
	negative, err := parseProbe([]byte(`{"format":{"duration":"-1"}}`))
	if err != nil || negative.DurationSec != 0 {
		t.Fatalf("negative=%#v err=%v", negative, err)
	}
	for _, item := range []struct {
		raw   string
		valid bool
	}{
		{"25", true}, {"30000/1001", true}, {"", false}, {"0/0", false}, {"a/1", false}, {"1/0", false},
	} {
		_, err := parseRate(item.raw)
		if (err == nil) != item.valid {
			t.Fatalf("rate=%q valid=%v err=%v", item.raw, item.valid, err)
		}
	}
}

func TestMediaKindShortcutsResolveAndRenderValidation(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 未安装")
	}
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := NewObjectStore(database.Paths)
	if thumbnail, err := GenerateThumbnail(t.Context(), store, "unused", "audio"); err != nil || thumbnail != nil {
		t.Fatalf("audio thumbnail=%#v err=%v", thumbnail, err)
	}
	for _, kind := range []string{"image", "font"} {
		if proxy, err := GenerateProxy(t.Context(), store, "unused", kind, nil); err != nil || proxy != nil {
			t.Fatalf("%s proxy=%#v err=%v", kind, proxy, err)
		}
	}
	if _, _, err := ResolveAssetSource(t.Context(), database, "missing"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("missing source err=%v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO assets(asset_id,storage_mode,reference_path,kind,source,filename,hash,size,ingest_status,usable)
		VALUES('invalid','reference','/missing','video','local','x','x',1,'ready',0),
		      ('missing_ref','reference','/missing','video','local','x','y',1,'ready',1),
		      ('no_source','reference',NULL,'video','local','x','z',1,'ready',1)`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ResolveAssetSource(t.Context(), database, "invalid"); err == nil {
		t.Fatal("invalid asset should fail")
	}
	if _, _, err := ResolveAssetSource(t.Context(), database, "missing_ref"); err == nil {
		t.Fatal("missing reference should fail")
	}
	if _, _, err := ResolveAssetSource(t.Context(), database, "no_source"); err == nil {
		t.Fatal("source-less asset should fail")
	}
	object, err := store.Put(t.Context(), bytes.NewReader([]byte("copied")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO objects(hash,rel_path,size,created_at) VALUES(?,?,?,?);
		INSERT INTO assets(asset_id,storage_mode,object_hash,kind,source,filename,hash,size,ingest_status,usable)
		VALUES('copy','copy',?,'video','local','copy.mp4',?,?,'ready',1)`, object.Hash, object.Path, object.Size, now,
		object.Hash, object.Hash, object.Size); err != nil {
		t.Fatal(err)
	}
	if path, kind, err := ResolveAssetSource(t.Context(), database, "copy"); err != nil || path != object.Path || kind != "video" {
		t.Fatalf("copy path=%q kind=%q err=%v", path, kind, err)
	}

	if _, err := RenderTimeline(t.Context(), database, timeline.Empty("d", 1), PreviewProfile, nil); err == nil {
		t.Fatal("empty timeline should fail")
	}
	badFPS := timeline.Empty("d", 1)
	badFPS.FPS = 0
	badFPS.Tracks[0].Clips = []timeline.Clip{{TimelineClipID: "c", AssetID: "copy", TimelineEndFrame: 1}}
	if _, err := RenderTimeline(t.Context(), database, badFPS, PreviewProfile, nil); err == nil {
		t.Fatal("bad fps should fail")
	}
	missingAsset, _ := timeline.ComposeInitial("d", 1, []timeline.Selection{{AssetID: "missing", SourceEndFrame: 30}})
	if _, err := RenderTimeline(t.Context(), database, missingAsset, PreviewProfile, nil); err == nil {
		t.Fatal("missing clip asset should fail")
	}
	if timelineTrack(missingAsset, "missing") != nil || formatSeconds(1.25) != "1.250000" ||
		!containsCheck([]string{"decode"}, "decode") || containsCheck(nil, "decode") || abs(-2) != 2 {
		t.Fatal("render helper mismatch")
	}
}

func TestMediaAudioImageAndProcessBoundaryPaths(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 未安装")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe 未安装")
	}
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := NewObjectStore(database.Paths)
	missing := filepath.Join(database.Paths.Temporary, "missing.mp4")
	if _, err := GenerateThumbnail(t.Context(), store, missing, "video"); err == nil {
		t.Fatal("缺失视频的缩略图生成应失败")
	}
	if _, err := GenerateProxy(t.Context(), store, missing, "video", nil); err == nil {
		t.Fatal("缺失视频的代理生成应失败")
	}
	if _, err := ProbeFile(t.Context(), missing); err == nil {
		t.Fatal("缺失文件 probe 应失败")
	}
	if _, err := InspectVideo(t.Context(), missing, ExpectedVideo{}, nil); err == nil {
		t.Fatal("缺失文件 inspect 应失败")
	}

	audio := filepath.Join(database.Paths.Temporary, "tone.wav")
	if _, err := RunCommand(t.Context(), "ffmpeg", "-y", "-f", "lavfi", "-i", "sine=frequency=440:duration=1", "-c:a", "pcm_s16le", audio); err != nil {
		t.Fatal(err)
	}
	proxy, err := GenerateProxy(t.Context(), store, audio, "audio", nil)
	if err != nil || proxy == nil || proxy.Size == 0 {
		t.Fatalf("audio proxy=%#v err=%v", proxy, err)
	}
	inspection, err := InspectVideo(t.Context(), audio, ExpectedVideo{DurationSec: 9}, nil)
	if err != nil || len(inspection.Issues) < 2 || !strings.Contains(inspection.Summary, "提示") {
		t.Fatalf("audio inspection=%#v err=%v", inspection, err)
	}
	audioProbe, err := parseProbe([]byte(`{"format":{"duration":"0"},"streams":[{"codec_type":"audio","duration":"3"}]}`))
	if err != nil || audioProbe.DurationSec != 3 || !audioProbe.HasAudio {
		t.Fatalf("audio probe=%#v err=%v", audioProbe, err)
	}

	image := filepath.Join(database.Paths.Temporary, "still.png")
	if _, err := RunCommand(t.Context(), "ffmpeg", "-y", "-f", "lavfi", "-i", "color=c=green:s=64x64", "-frames:v", "1", image); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(image)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO assets(asset_id,storage_mode,reference_path,kind,source,filename,hash,mtime,size,ingest_status,usable)
		VALUES('image_render','reference',?,'image','local_path','still.png','image',?,?,'ready',1)`,
		image, info.ModTime().UnixNano(), info.Size()); err != nil {
		t.Fatal(err)
	}
	document, err := timeline.ComposeInitial("image_draft", 1, []timeline.Selection{{
		AssetID: "image_render", SourceStartFrame: 0, SourceEndFrame: 6, Role: "b_roll",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if rendered, err := RenderTimeline(t.Context(), database, document, RenderProfile{Name: "tiny", Width: 64, Height: 64, CRF: 35}, nil); err != nil || rendered.Object.Size == 0 {
		t.Fatalf("image render=%#v err=%v", rendered, err)
	}

	progressScript := filepath.Join(database.Paths.Temporary, "fake-ffmpeg")
	if err := os.WriteFile(progressScript, []byte("#!/bin/sh\nprintf 'not-a-pair\\nout_time_us=bad\\nprogress=continue\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	progressCalls := 0
	if err := RunFFmpegProgress(t.Context(), progressScript, nil, func(Progress) { progressCalls++ }); err != nil || progressCalls != 1 {
		t.Fatalf("fake progress calls=%d err=%v", progressCalls, err)
	}

	command := exec.CommandContext(t.Context(), "true")
	configureProcess(command)
	if err := command.Cancel(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("cancel before start err=%v", err)
	}
	finished := exec.CommandContext(t.Context(), "true")
	configureProcess(finished)
	if err := finished.Run(); err != nil {
		t.Fatal(err)
	}
	if err := finished.Cancel(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("cancel finished err=%v", err)
	}
	running := exec.CommandContext(t.Context(), "sh", "-c", "sleep 10")
	configureProcess(running)
	if err := running.Start(); err != nil {
		t.Fatal(err)
	}
	if err := running.Cancel(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("cancel running err=%v", err)
	}
	_ = running.Wait()
}
