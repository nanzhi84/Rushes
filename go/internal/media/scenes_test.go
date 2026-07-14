package media

import (
	"context"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseSceneCandidatesSortsFiltersAndDeduplicates(t *testing.T) {
	output := strings.Join([]string{
		"frame:20 pts:2500 pts_time:2.5",
		"lavfi.scd.score=11.250",
		"lavfi.scd.time=2.5",
		"frame:8 pts:1000 pts_time:1",
		"lavfi.scd.score=14",
		"lavfi.scd.time=1",
		"frame:9 pts:1001 pts_time:1.0000004",
		"lavfi.scd.score=18.5",
		"lavfi.scd.time=1.0000004",
		"frame:4 pts:500 pts_time:0.5",
		"lavfi.scd.score=9.999",
		"lavfi.scd.time=0.5",
		"[Parsed_scdet] lavfi.scd.score: 12.75, lavfi.scd.time: 3.25",
	}, "\n")

	candidates := parseSceneCandidates([]byte(output), 10)
	if len(candidates) != 3 {
		t.Fatalf("candidates=%#v", candidates)
	}
	if candidates[0].PTSTimeSeconds != 1 || candidates[0].Score != 18.5 {
		t.Fatalf("deduplicated candidate=%#v", candidates[0])
	}
	if candidates[1] != (SceneCandidate{PTSTimeSeconds: 2.5, Score: 11.25}) ||
		candidates[2] != (SceneCandidate{PTSTimeSeconds: 3.25, Score: 12.75}) {
		t.Fatalf("sorted candidates=%#v", candidates)
	}
}

func TestParseSceneCandidatesAllowsNoCandidates(t *testing.T) {
	for _, output := range [][]byte{
		nil,
		[]byte("unrelated ffmpeg output"),
		[]byte("frame:0 pts_time:0\nlavfi.scd.score=1"),
		[]byte("frame:0 pts_time:-1\nlavfi.scd.score=50"),
	} {
		candidates := parseSceneCandidates(output, 10)
		if candidates == nil || len(candidates) != 0 {
			t.Fatalf("output=%q candidates=%#v", output, candidates)
		}
	}
}

func TestNormalizeSceneDetectionOptions(t *testing.T) {
	threshold, width, err := normalizeSceneDetectionOptions(SceneDetectionOptions{})
	if err != nil || threshold != DefaultSceneThreshold || width != DefaultSceneDownscaleWidth {
		t.Fatalf("threshold=%v width=%d err=%v", threshold, width, err)
	}
	threshold, width, err = normalizeSceneDetectionOptions(SceneDetectionOptions{
		Threshold: 12.5, Timeout: time.Second, DownscaleWidth: 320,
	})
	if err != nil || threshold != 12.5 || width != 320 {
		t.Fatalf("threshold=%v width=%d err=%v", threshold, width, err)
	}
	_, width, err = normalizeSceneDetectionOptions(SceneDetectionOptions{DownscaleWidth: -1})
	if err != nil || width != 0 {
		t.Fatalf("disabled width=%d err=%v", width, err)
	}
	for _, options := range []SceneDetectionOptions{
		{Threshold: -1},
		{Threshold: 101},
		{Threshold: math.NaN()},
		{Threshold: math.Inf(1)},
		{Timeout: -time.Second},
	} {
		if _, _, err := normalizeSceneDetectionOptions(options); err == nil {
			t.Fatalf("options=%+v should fail", options)
		}
	}
}

func TestDetectSceneCandidatesUsesMetadataOnly(t *testing.T) {
	fakeBin := t.TempDir()
	ffmpeg := filepath.Join(fakeBin, "ffmpeg")
	argsFile := filepath.Join(t.TempDir(), "args")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$SCENE_ARGS_FILE"
printf '%s\n' 'frame:2 pts:200 pts_time:0.2' 'lavfi.scd.score=16.75' 'lavfi.scd.time=0.2'
`
	if err := os.WriteFile(ffmpeg, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SCENE_ARGS_FILE", argsFile)

	detection, err := DetectSceneCandidates(t.Context(), "source with spaces.mov", SceneDetectionOptions{Threshold: 12.5})
	if err != nil {
		t.Fatal(err)
	}
	if detection.Threshold != 12.5 || detection.DownscaleWidth != DefaultSceneDownscaleWidth ||
		detection.AnalysisMethod != sceneAnalysisMethod ||
		len(detection.Candidates) != 1 || detection.Candidates[0].PTSTimeSeconds != 0.2 ||
		detection.Candidates[0].Score != 16.75 {
		t.Fatalf("detection=%#v", detection)
	}
	arguments, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	argumentLines := strings.Split(strings.TrimSpace(string(arguments)), "\n")
	joined := strings.Join(argumentLines, " ")
	for _, expected := range []string{
		"-i source with spaces.mov",
		"-map 0:v:0",
		"scale=w='min(iw,640)':h=-2:flags=fast_bilinear,scdet=threshold=12.5",
		"scdet=threshold=12.5",
		"metadata=mode=select:key=lavfi.scd.time",
		"metadata=mode=print:file=-:direct=1",
		"-f null -",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("arguments missing %q: %q", expected, joined)
		}
	}
	for _, forbidden := range []string{"segment", "-c:v", "-y"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("metadata-only invocation contains %q: %q", forbidden, joined)
		}
	}
	if strings.Contains(joined, "fps=") || strings.Contains(joined, "framestep") {
		t.Fatalf("scene scan must retain every input frame: %q", joined)
	}

	disabled, err := DetectSceneCandidates(t.Context(), "source.mov", SceneDetectionOptions{DownscaleWidth: -1})
	if err != nil {
		t.Fatal(err)
	}
	if disabled.DownscaleWidth != 0 {
		t.Fatalf("disabled detection=%#v", disabled)
	}
	disabledArguments, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(disabledArguments), "scale=") {
		t.Fatalf("negative width should disable scaling: %q", disabledArguments)
	}
}

func TestDetectSceneCandidatesHonorsTimeoutAndCancellation(t *testing.T) {
	fakeBin := t.TempDir()
	ffmpeg := filepath.Join(fakeBin, "ffmpeg")
	if err := os.WriteFile(ffmpeg, []byte("#!/bin/sh\nexec sleep 10\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	started := time.Now()
	_, err := DetectSceneCandidates(t.Context(), "unused", SceneDetectionOptions{Timeout: 40 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout err=%v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = DetectSceneCandidates(ctx, "unused", SceneDetectionOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel err=%v", err)
	}
}

func TestDetectSceneCandidatesPropagatesFFmpegFailure(t *testing.T) {
	fakeBin := t.TempDir()
	ffmpeg := filepath.Join(fakeBin, "ffmpeg")
	if err := os.WriteFile(ffmpeg, []byte("#!/bin/sh\nprintf 'bad input' >&2\nexit 7\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := DetectSceneCandidates(t.Context(), "unused", SceneDetectionOptions{})
	var commandErr *CommandError
	if !errors.As(err, &commandErr) || !strings.Contains(commandErr.Stderr, "bad input") {
		t.Fatalf("err=%T %v", err, err)
	}
	if _, err := DetectSceneCandidates(t.Context(), "", SceneDetectionOptions{}); err == nil {
		t.Fatal("blank source should fail")
	}
}

func TestDetectSceneCandidatesFFmpegIntegration(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 未安装")
	}
	source := filepath.Join(t.TempDir(), "two-scenes.mkv")
	_, err := RunCommand(
		t.Context(),
		"ffmpeg",
		"-y",
		"-hide_banner",
		"-loglevel", "error",
		"-f", "lavfi", "-i", "color=c=red:s=160x90:r=10:d=1",
		"-f", "lavfi", "-i", "color=c=blue:s=160x90:r=10:d=1",
		"-filter_complex", "[0:v][1:v]concat=n=2:v=1:a=0",
		"-c:v", "ffv1",
		source,
	)
	if err != nil {
		t.Fatal(err)
	}

	detection, err := DetectSceneCandidates(t.Context(), source, SceneDetectionOptions{Threshold: 5, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if len(detection.Candidates) != 1 || math.Abs(detection.Candidates[0].PTSTimeSeconds-1) > 0.11 ||
		detection.Candidates[0].Score < 5 {
		t.Fatalf("detection=%#v", detection)
	}
}
