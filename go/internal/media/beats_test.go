package media

import (
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseBeatGridAndFrameConversions(t *testing.T) {
	seconds, err := parseBeatSeconds([]byte("junk\n0.500\n1.000\n1.500\n2.000\n"))
	if err != nil {
		t.Fatal(err)
	}
	frames := beatFrames(seconds, 30)
	if len(frames) != 4 || frames[0] != 15 || frames[3] != 60 {
		t.Fatalf("frames=%v", frames)
	}
	if bpm := estimateBPM(seconds); math.Abs(bpm-120) > 0.001 {
		t.Fatalf("bpm=%f", bpm)
	}
	if everyTwo := everyNthBeat(frames, 2); len(everyTwo) != 2 || everyTwo[1] != 45 {
		t.Fatalf("everyTwo=%v", everyTwo)
	}
	if everyFour := everyNthBeat(frames, 4); len(everyFour) != 1 || everyFour[0] != 15 {
		t.Fatalf("everyFour=%v", everyFour)
	}
}

func TestBeatParsingRejectsInsufficientAndDeduplicatesFrames(t *testing.T) {
	if _, err := parseBeatSeconds([]byte("0.5\n")); err == nil {
		t.Fatal("单个节拍点不应通过")
	}
	seconds, err := parseBeatSeconds([]byte("0.001\n0.010\n0.020\n"))
	if err != nil {
		t.Fatal(err)
	}
	if frames := beatFrames(seconds, 30); len(frames) != 2 || frames[0] != 0 || frames[1] != 1 {
		t.Fatalf("frames=%v", frames)
	}
	if bpm := estimateBPM([]float64{0, 3}); bpm != 0 {
		t.Fatalf("bpm=%f", bpm)
	}
	if values := everyNthBeat(nil, 0); len(values) != 0 {
		t.Fatalf("values=%v", values)
	}
	if _, err := parseOnsetSeconds([]byte("junk\n")); err == nil {
		t.Fatal("空瞬态不应通过")
	}
}

func TestInferDownbeatPhaseUsesStrongOnsetsInsteadOfFirstTrackedBeat(t *testing.T) {
	beats := []int{0, 15, 30, 45, 60, 75, 90, 105, 120, 135, 150, 165}
	strong := []int{16, 74, 136}
	phase := inferDownbeatPhase(beats, strong, 30)
	if phase != 1 {
		t.Fatalf("phase=%d", phase)
	}
	if downbeats := everyNthBeatFrom(beats, 4, phase); !reflect.DeepEqual(downbeats, []int{15, 75, 135}) {
		t.Fatalf("downbeats=%v", downbeats)
	}
	if everyTwo := everyNthBeatFrom(beats, 2, phase%2); !reflect.DeepEqual(everyTwo, []int{15, 45, 75, 105, 135, 165}) {
		t.Fatalf("everyTwo=%v", everyTwo)
	}
	if phase := inferDownbeatPhase(beats[:3], strong, 30); phase != 0 {
		t.Fatalf("short phase=%d", phase)
	}
	if values := everyNthBeatFrom(beats[:1], 4, 3); len(values) != 0 {
		t.Fatalf("out of range phase=%v", values)
	}
}

func TestAnalyzeBeatGridAddsStrongBeatsAndFallsBackWithoutOnsetTool(t *testing.T) {
	fakeBin := t.TempDir()
	writeExecutable := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(fakeBin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeExecutable("aubiotrack", "printf '0.000000\\n0.500000\\n1.000000\\n1.500000\\n2.000000\\n2.500000\\n3.000000\\n3.500000\\n'")
	writeExecutable("aubioonset", "printf '0.500000\\n2.500000\\n'")
	t.Setenv("PATH", fakeBin)

	grid, err := AnalyzeBeatGrid(t.Context(), "ignored.wav", 30, 32)
	if err != nil {
		t.Fatal(err)
	}
	if grid.BarPhase != 1 || grid.AnalysisMethod != "aubio-tempo+specflux-onset" ||
		!reflect.DeepEqual(grid.StrongBeatFrames, []int{15, 75}) ||
		!reflect.DeepEqual(grid.DownbeatFrames, []int{15, 75}) {
		t.Fatalf("grid=%#v", grid)
	}

	if err := os.Remove(filepath.Join(fakeBin, "aubioonset")); err != nil {
		t.Fatal(err)
	}
	grid, err = AnalyzeBeatGrid(t.Context(), "ignored.wav", 30, 3)
	if err != nil {
		t.Fatal(err)
	}
	if grid.BarPhase != 0 || grid.AnalysisMethod != "aubio-tempo" || len(grid.StrongBeatFrames) != 0 ||
		!grid.Truncated || !reflect.DeepEqual(grid.DownbeatFrames, []int{0}) {
		t.Fatalf("fallback grid=%#v", grid)
	}
}
