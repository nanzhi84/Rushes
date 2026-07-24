package agent

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/media"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

const defaultBeatMixMaterialRoot = "/Users/yoryon/影视飓风剪辑课/课程1-3.4.5混剪实操（1080P版）/混剪练习素材包【1080p压缩版】"

func TestBeatMixRealMaterialAcceptance(t *testing.T) {
	if os.Getenv("RUSHES_BEAT_MIX_EVAL") != "1" {
		t.Skip("设置 RUSHES_BEAT_MIX_EVAL=1 才运行真实卡点素材验收")
	}
	root := strings.TrimSpace(os.Getenv("RUSHES_BEAT_MIX_MATERIAL_ROOT"))
	if root == "" {
		root = defaultBeatMixMaterialRoot
	}
	assets := []struct {
		id, relative, kind string
	}{
		{"beat_bgm", "音频/BGM/IGNIS by 秋予.wav", "audio"},
		{"beat_video_1", "视频/混剪素材-5.mov", "video"},
		{"beat_video_2", "视频/混剪素材-12.mov", "video"},
		{"beat_video_3", "视频/混剪素材-17.mov", "video"},
	}
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_beat_mix_real")
	now := time.Now().UTC()
	videoDurations := map[string]int{}
	for index, asset := range assets {
		path := filepath.Join(root, asset.relative)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("真实卡点素材不可读 %s: %v", path, err)
		}
		probe, err := media.ProbeFile(t.Context(), path)
		if err != nil {
			t.Fatal(err)
		}
		if asset.kind == "video" {
			videoDurations[asset.id] = int(probe.DurationSec * 30)
		}
		probeJSON, err := json.Marshal(probe)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := database.Write().ExecContext(t.Context(), `
			INSERT INTO assets(
				asset_id,storage_mode,reference_path,kind,source,filename,hash,mtime,size,
				probe_json,ingest_status,understanding_status,usable
			) VALUES(?,'reference',?,?, 'local_path',?,?,?,?,?,'ready','none',1);
			INSERT INTO draft_asset_links(draft_id,asset_id,rel_dir,linked_at)
			VALUES('draft_beat_mix_real',?,?,?);`,
			asset.id, path, asset.kind, filepath.Base(path), asset.id, info.ModTime().UnixNano(), info.Size(), string(probeJSON),
			asset.id, filepath.Dir(asset.relative), now.Add(time.Duration(index)*time.Millisecond).Format(time.RFC3339Nano),
		); err != nil {
			t.Fatal(err)
		}
	}
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ctx := rushestools.WithDraftID(t.Context(), "draft_beat_mix_real")
	beatOutput, err := service.ExecuteTool(ctx, "audio.analyze_beats", rushestools.AudioBeatAnalysisInput{
		AssetID: "beat_bgm", MaxBeats: 512, WaveformPoints: 96,
	})
	if err != nil {
		t.Fatal(err)
	}
	beats := beatOutput.(rushestools.AudioBeatAnalysisResult)
	if beats.BPM <= 0 || beats.DurationFrames < 1400 || len(beats.BeatFrames) < 20 || len(beats.Waveform.Samples) == 0 {
		t.Fatalf("beats=%#v", beats)
	}
	videoIDs := []string{"beat_video_1", "beat_video_2", "beat_video_3"}
	cuts, ok := chooseThreeAtomicBeatSegments(beats.BeatFrames, beats.DurationFrames, videoIDs, videoDurations)
	if !ok {
		t.Fatalf("三个真实视频不足以按真实拍点覆盖 BGM: target=%d durations=%v", beats.DurationFrames, videoDurations)
	}
	trace := []string{"audio.analyze_beats"}
	start := 0
	for index, end := range cuts {
		raw, executeErr := service.ExecuteTool(ctx, "timeline.insert", rushestools.TimelineInsertInput{
			"kind": "insert_clip", "asset_id": videoIDs[index], "role": "b_roll",
			"source_start_frame": 0, "source_end_frame": end - start,
		})
		if executeErr != nil || raw.(rushestools.ToolResult).Status != string(rushestools.StatusSucceeded) {
			t.Fatalf("visual insert %d=%#v err=%v", index, raw, executeErr)
		}
		trace = append(trace, "timeline.insert")
		start = end
	}
	bgmRaw, err := service.ExecuteTool(ctx, "timeline.insert", rushestools.TimelineInsertInput{
		"kind": "insert_clip", "track_id": "bgm", "asset_id": "beat_bgm", "role": "bgm",
		"timeline_start_frame": 0, "source_start_frame": 0, "source_end_frame": beats.DurationFrames,
		"metadata": map[string]any{"beat_grid": map[string]any{
			"bpm": beats.BPM, "beat_frames": beats.BeatFrames,
			"strong_beat_frames": beats.StrongBeatFrames,
			"downbeat_frames":    beats.DownbeatFrames,
			"bar_phase":          beats.BarPhase,
			"analysis_method":    beats.AnalysisMethod,
		}},
	})
	if err != nil || bgmRaw.(rushestools.ToolResult).Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("bgm insert=%#v err=%v", bgmRaw, err)
	}
	trace = append(trace, "timeline.insert")
	checkRaw, err := service.ExecuteTool(ctx, "timeline.check", rushestools.TimelineCheckInput{})
	if err != nil || checkRaw.(rushestools.ToolResult).Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("timeline.check=%#v err=%v", checkRaw, err)
	}
	trace = append(trace, "timeline.check")
	alignment, _ := checkRaw.(rushestools.ToolResult).Data["beat_alignment"].(map[string]any)
	if alignment["all_cuts_on_beat_grid"] != true {
		t.Fatalf("beat alignment=%#v", alignment)
	}
	document, err := timeline.Latest(t.Context(), database, "draft_beat_mix_real")
	if err != nil {
		t.Fatal(err)
	}
	if !timeline.Validate(document).Valid || document.DurationFrames != beats.DurationFrames || len(document.Tracks[0].Clips) != 3 {
		t.Fatalf("timeline=%#v", document)
	}
	beatSet := map[int]struct{}{}
	for _, frame := range beats.BeatFrames {
		beatSet[frame] = struct{}{}
	}
	used := map[string]struct{}{}
	for index, clip := range document.Tracks[0].Clips {
		used[clip.AssetID] = struct{}{}
		if index < len(document.Tracks[0].Clips)-1 {
			if _, ok := beatSet[clip.TimelineEndFrame]; !ok {
				t.Fatalf("切点 %d 不在真实 beat grid", clip.TimelineEndFrame)
			}
		}
	}
	if len(used) != 3 {
		t.Fatalf("未使用全部视频素材: %v", used)
	}
	// 该验收只保留 BGM，避免视频原声让“渲染器漏掉 BGM”误通过音频检查。
	for index := range document.Tracks {
		if document.Tracks[index].TrackID == "original_audio" {
			document.Tracks[index].Muted = true
		}
	}
	profile := media.RenderProfile{Name: "issue71-beat-real", Width: 320, Height: 180, CRF: 28}
	rendered, err := media.RenderTimeline(t.Context(), database, document, profile, nil)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := media.TimelineInspectionIntent(t.Context(), database, document)
	if err != nil {
		t.Fatal(err)
	}
	expected.Width, expected.Height = profile.Width, profile.Height
	expected.FPS = float64(document.FPS)
	expected.DurationSec = float64(document.DurationFrames) / float64(document.FPS)
	inspection, err := media.InspectVideo(t.Context(), rendered.Object.Path, expected, []string{"decode", "loudness"})
	if err != nil || inspection.Degraded || len(inspection.Issues) != 0 {
		t.Fatalf("inspection=%#v err=%v", inspection, err)
	}
	probe, err := media.ProbeFile(t.Context(), rendered.Object.Path)
	if err != nil || !probe.HasAudio {
		t.Fatalf("卡点成片必须包含非静音 BGM 音轨: probe=%#v err=%v", probe, err)
	}
	if output := strings.TrimSpace(os.Getenv("RUSHES_BEAT_MIX_EVAL_OUTPUT")); output != "" {
		if err := copyBeatMixArtifact(rendered.Object.Path, output); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf(
		"BEAT_MIX_ACCEPTANCE bpm=%.2f beats=%d duration_frames=%d cuts=%v trace=%v rendered=%dx%d bytes=%d",
		beats.BPM, len(beats.BeatFrames), beats.DurationFrames, cuts[:len(cuts)-1], trace,
		rendered.Width, rendered.Height, rendered.Object.Size,
	)
}

func chooseThreeAtomicBeatSegments(
	beatFrames []int,
	target int,
	videoIDs []string,
	durations map[string]int,
) ([]int, bool) {
	if len(videoIDs) != 3 || target <= 0 {
		return nil, false
	}
	bestCuts := []int(nil)
	bestDistance := int(^uint(0) >> 1)
	for _, first := range beatFrames {
		if first <= 0 || first >= target || first > durations[videoIDs[0]] {
			continue
		}
		for _, second := range beatFrames {
			if second <= first || second >= target ||
				second-first > durations[videoIDs[1]] ||
				target-second > durations[videoIDs[2]] {
				continue
			}
			distance := absBeatFrameDistance(first-target/3) +
				absBeatFrameDistance(second-target*2/3)
			if distance < bestDistance {
				bestDistance = distance
				bestCuts = []int{first, second, target}
			}
		}
	}
	return bestCuts, len(bestCuts) == 3
}

func absBeatFrameDistance(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func copyBeatMixArtifact(source, destination string) error {
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
