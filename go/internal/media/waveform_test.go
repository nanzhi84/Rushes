package media

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestAnalyzeWaveformEnvelopeReturnsBoundedTimeOrderedSamples(t *testing.T) {
	fakeBin := t.TempDir()
	fakeFFmpeg := filepath.Join(fakeBin, "ffmpeg")
	output := `frame:0 pts_time:0
lavfi.astats.Overall.RMS_level=-60
frame:1 pts_time:1
lavfi.astats.Overall.RMS_level=-30
frame:2 pts_time:2
lavfi.astats.Overall.RMS_level=-12
frame:3 pts_time:3
lavfi.astats.Overall.RMS_level=-inf`
	if err := os.WriteFile(
		fakeFFmpeg,
		[]byte("#!/bin/sh\nprintf '%s\\n' '"+output+"'\n"),
		0o755,
	); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin)

	waveform, err := AnalyzeWaveformEnvelope(t.Context(), "ignored.wav", 30, 120, 4)
	if err != nil {
		t.Fatal(err)
	}
	if waveform.SampleIntervalFrames != 30 || waveform.Encoding != WaveformEncoding ||
		waveform.FloorDB != -60 || waveform.CeilingDB != 0 ||
		!reflect.DeepEqual(waveform.SampleFrames, []int{0, 30, 60, 90}) ||
		!reflect.DeepEqual(waveform.Samples, []int{0, 50, 80, 0}) {
		t.Fatalf("waveform=%#v", waveform)
	}
}

func TestWaveformEnvelopeValidatesCoordinatesAndPointLimit(t *testing.T) {
	if _, err := AnalyzeWaveformEnvelope(t.Context(), "ignored.wav", 0, 120, 4); err == nil {
		t.Fatal("fps=0 应该失败")
	}
	if _, err := AnalyzeWaveformEnvelope(
		t.Context(), "ignored.wav", 30, 120, MaxWaveformPoints+1,
	); err == nil {
		t.Fatal("超出采样点上限应该失败")
	}
}
