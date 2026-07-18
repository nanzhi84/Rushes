package media

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeFakePeaksFFmpeg(t *testing.T, stdout string) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(
		script,
		[]byte("#!/bin/sh\nprintf '%s\\n' '"+stdout+"'\n"),
		0o755,
	); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func TestAnalyzeWaveformPeaksParsesClampedOverallMinMaxPairs(t *testing.T) {
	// 每窗口取 Overall.Min/Max_level（忽略 .1. per-channel）；-inf/nan→0，越界值 clamp 到 [-1,1]。
	stdout := `frame:0    pts:0       pts_time:0
lavfi.astats.1.Min_level=-0.9
lavfi.astats.Overall.Min_level=-0.5
lavfi.astats.Overall.Max_level=0.5
frame:1    pts:480     pts_time:0.01
lavfi.astats.Overall.Min_level=-inf
lavfi.astats.Overall.Max_level=nan
frame:2    pts:960     pts_time:0.02
lavfi.astats.Overall.Min_level=-2.0
lavfi.astats.Overall.Max_level=1.5`
	writeFakePeaksFFmpeg(t, stdout)

	peaks, err := AnalyzeWaveformPeaks(t.Context(), "ignored.wav", 3.5)
	if err != nil {
		t.Fatal(err)
	}
	if peaks.Version != PeaksSchemaVersion || peaks.SampleRateHz != PeaksSampleRateHz ||
		peaks.DurationSec != 3.5 {
		t.Fatalf("meta 不符: %#v", peaks)
	}
	want := [][2]float64{{-0.5, 0.5}, {0, 0}, {-1, 1}}
	if !reflect.DeepEqual(peaks.Peaks, want) {
		t.Fatalf("peaks=%v want=%v", peaks.Peaks, want)
	}
}

func TestAnalyzeWaveformPeaksErrorsWhenNoWindows(t *testing.T) {
	writeFakePeaksFFmpeg(t, "")
	if _, err := AnalyzeWaveformPeaks(t.Context(), "ignored.wav", 1); err == nil {
		t.Fatal("空输出应报错")
	}
}

func TestAnalyzeWaveformPeaksErrorsWithoutFFmpeg(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := AnalyzeWaveformPeaks(t.Context(), "ignored.wav", 1); err == nil {
		t.Fatal("缺 ffmpeg 应报错")
	}
}
