package media

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
	first, err := store.PutBytes(t.Context(), []byte("same"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.PutBytes(t.Context(), []byte("same"))
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
	if err != nil || !mismatch.Degraded || len(mismatch.Issues) < 2 {
		t.Fatalf("mismatch=%#v err=%v", mismatch, err)
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
	object, err := store.PutBytes(t.Context(), []byte("copied"))
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
