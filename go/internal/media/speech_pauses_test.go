package media

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSilenceRanges(t *testing.T) {
	output := strings.Join([]string{
		"[silencedetect @ 0x1] silence_start: 0",
		"[silencedetect @ 0x1] silence_end: 0.42 | silence_duration: 0.42",
		"[silencedetect @ 0x1] silence_start: 1.25",
		"[silencedetect @ 0x1] silence_end: 1.80 | silence_duration: 0.55",
		"[silencedetect @ 0x1] silence_start: 2.6",
	}, "\n")

	ranges := parseSilenceRanges([]byte(output), 3)
	if len(ranges) != 3 {
		t.Fatalf("ranges=%v", ranges)
	}
	if ranges[0] != [2]float64{0, 0.42} || ranges[1] != [2]float64{1.25, 1.8} ||
		ranges[2] != [2]float64{2.6, 3} {
		t.Fatalf("unexpected ranges=%v", ranges)
	}
}

func TestNormalizeSpeechPauseOptions(t *testing.T) {
	options := normalizeSpeechPauseOptions(SpeechPauseOptions{ThresholdDB: -100, MaxPauses: 5000}, 30)
	if options.ThresholdDB != -80 || options.MinPauseFrames != 5 ||
		options.KeepEdgeFrames != 2 || options.MaxPauses != 1000 {
		t.Fatalf("options=%+v", options)
	}
}

func TestAnalyzeSpeechPausesIncludesBoundariesAndTruncates(t *testing.T) {
	if _, err := AnalyzeSpeechPauses(t.Context(), "unused", 0, SpeechPauseOptions{}); err == nil {
		t.Fatal("fps=0 should fail")
	}
	fakeBin := t.TempDir()
	ffprobe := filepath.Join(fakeBin, "ffprobe")
	ffmpeg := filepath.Join(fakeBin, "ffmpeg")
	probePayload := `{"format":{"duration":"3"},"streams":[{"codec_type":"audio","duration":"3"}]}`
	if err := os.WriteFile(ffprobe, []byte("#!/bin/sh\nprintf '%s' '"+probePayload+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	logs := strings.Join([]string{
		"silence_start: 0",
		"silence_end: 0.4 | silence_duration: 0.4",
		"silence_start: 1.0",
		"silence_end: 1.6 | silence_duration: 0.6",
	}, "\n")
	if err := os.WriteFile(ffmpeg, []byte("#!/bin/sh\nprintf '%s' '"+logs+"' >&2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	analysis, err := AnalyzeSpeechPauses(t.Context(), "unused", 30, SpeechPauseOptions{
		ThresholdDB: -30, MinPauseFrames: 6, KeepEdgeFrames: 1,
		MaxPauses: 1, IncludeBoundaries: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !analysis.Truncated || analysis.DurationFrames != 90 || len(analysis.Pauses) != 1 ||
		analysis.Pauses[0].DeleteStartFrame != 1 || analysis.Pauses[0].DeleteEndFrame != 11 {
		t.Fatalf("analysis=%#v", analysis)
	}
	if boundaryFiltered, err := AnalyzeSpeechPauses(t.Context(), "unused", 30, SpeechPauseOptions{
		MinPauseFrames: 6, KeepEdgeFrames: 1,
	}); err != nil || len(boundaryFiltered.Pauses) != 1 {
		t.Fatalf("boundaryFiltered=%#v err=%v", boundaryFiltered, err)
	}
	if tooShort, err := AnalyzeSpeechPauses(t.Context(), "unused", 30, SpeechPauseOptions{
		MinPauseFrames: 100, KeepEdgeFrames: 1, IncludeBoundaries: true,
	}); err != nil || len(tooShort.Pauses) != 0 {
		t.Fatalf("tooShort=%#v err=%v", tooShort, err)
	}
	if noSafeCut, err := AnalyzeSpeechPauses(t.Context(), "unused", 30, SpeechPauseOptions{
		MinPauseFrames: 6, KeepEdgeFrames: 20, IncludeBoundaries: true,
	}); err != nil || len(noSafeCut.Pauses) != 0 {
		t.Fatalf("noSafeCut=%#v err=%v", noSafeCut, err)
	}

	noAudioPayload := `{"format":{"duration":"3"},"streams":[{"codec_type":"video","duration":"3"}]}`
	if err := os.WriteFile(ffprobe, []byte("#!/bin/sh\nprintf '%s' '"+noAudioPayload+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := AnalyzeSpeechPauses(t.Context(), "unused", 30, SpeechPauseOptions{}); err == nil {
		t.Fatal("video without audio should fail")
	}
	if err := os.WriteFile(ffprobe, []byte("#!/bin/sh\nprintf 'not-json'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := AnalyzeSpeechPauses(t.Context(), "unused", 30, SpeechPauseOptions{}); err == nil {
		t.Fatal("invalid probe payload should fail")
	}
	if err := os.WriteFile(ffprobe, []byte("#!/bin/sh\nprintf '%s' '"+probePayload+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ffmpeg, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := AnalyzeSpeechPauses(t.Context(), "unused", 30, SpeechPauseOptions{}); err == nil {
		t.Fatal("ffmpeg failure should propagate")
	}
}
