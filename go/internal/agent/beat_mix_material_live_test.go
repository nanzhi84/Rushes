package agent

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_beat_mix_real")
	now := time.Now().UTC()
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
	recutOutput, err := service.ExecuteTool(ctx, "timeline.recut_to_beats", rushestools.TimelineBeatRecutInput{
		BGMAssetID: "beat_bgm", CoverEntireBGM: true,
		VideoAssetIDs: []string{"beat_video_1", "beat_video_2", "beat_video_3"}, UseAllVideoAssets: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	recut := recutOutput.(rushestools.ToolResult)
	if recut.Status != "succeeded" {
		t.Fatalf("recut=%#v", recut)
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
		"BEAT_MIX_ACCEPTANCE bpm=%.2f beats=%d duration_frames=%d cuts=%v rendered=%dx%d bytes=%d",
		beats.BPM, len(beats.BeatFrames), beats.DurationFrames, recut.Data["cut_frames"],
		rendered.Width, rendered.Height, rendered.Object.Size,
	)
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
