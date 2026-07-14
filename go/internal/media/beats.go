package media

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

type BeatGrid struct {
	BPM                 float64
	BeatFrames          []int
	StrongBeatFrames    []int
	DownbeatFrames      []int
	EveryTwoBeatFrames  []int
	EveryFourBeatFrames []int
	BarPhase            int
	AnalysisMethod      string
	Truncated           bool
}

func AnalyzeBeatGrid(ctx context.Context, source string, fps, maxBeats int) (BeatGrid, error) {
	if fps <= 0 {
		return BeatGrid{}, errors.New("节拍分析 fps 必须为正数")
	}
	if maxBeats <= 0 {
		maxBeats = 512
	}
	if maxBeats > 2000 {
		maxBeats = 2000
	}
	if _, err := exec.LookPath("aubiotrack"); err != nil {
		return BeatGrid{}, errors.New("未安装 aubio；macOS 请先执行 brew install aubio")
	}
	result, err := RunCommand(ctx, "aubiotrack", "-i", source)
	if err != nil {
		return BeatGrid{}, err
	}
	seconds, err := parseBeatSeconds(result.Stdout)
	if err != nil {
		return BeatGrid{}, err
	}
	frames := beatFrames(seconds, fps)
	truncated := len(frames) > maxBeats
	if truncated {
		frames = frames[:maxBeats]
	}
	strongFrames := []int{}
	analysisMethod := "aubio-tempo"
	if onsetSeconds, onsetErr := analyzeStrongOnsets(ctx, source); onsetErr == nil {
		strongFrames = beatFrames(onsetSeconds, fps)
		if len(strongFrames) > maxBeats {
			strongFrames = strongFrames[:maxBeats]
			truncated = true
		}
		analysisMethod = "aubio-tempo+specflux-onset"
	} else if ctx.Err() != nil {
		return BeatGrid{}, ctx.Err()
	}
	barPhase := inferDownbeatPhase(frames, strongFrames, fps)
	downbeats := everyNthBeatFrom(frames, 4, barPhase)
	return BeatGrid{
		BPM:                 estimateBPM(seconds),
		BeatFrames:          frames,
		StrongBeatFrames:    strongFrames,
		DownbeatFrames:      downbeats,
		EveryTwoBeatFrames:  everyNthBeatFrom(frames, 2, barPhase%2),
		EveryFourBeatFrames: downbeats,
		BarPhase:            barPhase,
		AnalysisMethod:      analysisMethod,
		Truncated:           truncated,
	}, nil
}

func analyzeStrongOnsets(ctx context.Context, source string) ([]float64, error) {
	if _, err := exec.LookPath("aubioonset"); err != nil {
		return nil, err
	}
	result, err := RunCommand(ctx, "aubioonset", "-i", source, "-O", "specflux", "-t", "1.1")
	if err != nil {
		return nil, err
	}
	return parseOnsetSeconds(result.Stdout)
}

func parseBeatSeconds(output []byte) ([]float64, error) {
	seconds := parseMonotonicSeconds(output)
	if len(seconds) < 2 {
		return nil, fmt.Errorf("aubio 未返回足够的节拍点: %d", len(seconds))
	}
	return seconds, nil
}

func parseOnsetSeconds(output []byte) ([]float64, error) {
	seconds := parseMonotonicSeconds(output)
	if len(seconds) == 0 {
		return nil, errors.New("aubio 未返回强拍瞬态")
	}
	return seconds, nil
}

func parseMonotonicSeconds(output []byte) []float64 {
	seconds := make([]float64, 0, 128)
	for _, line := range strings.Split(string(output), "\n") {
		value, err := strconv.ParseFloat(strings.TrimSpace(line), 64)
		if err != nil || value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		if len(seconds) > 0 && value <= seconds[len(seconds)-1] {
			continue
		}
		seconds = append(seconds, value)
	}
	return seconds
}

func beatFrames(seconds []float64, fps int) []int {
	frames := make([]int, 0, len(seconds))
	for _, second := range seconds {
		frame := int(math.Round(second * float64(fps)))
		if len(frames) == 0 || frame > frames[len(frames)-1] {
			frames = append(frames, frame)
		}
	}
	return frames
}

func estimateBPM(seconds []float64) float64 {
	intervals := make([]float64, 0, len(seconds)-1)
	for index := 1; index < len(seconds); index++ {
		interval := seconds[index] - seconds[index-1]
		if interval >= 0.2 && interval <= 2 {
			intervals = append(intervals, interval)
		}
	}
	if len(intervals) == 0 {
		return 0
	}
	sort.Float64s(intervals)
	middle := len(intervals) / 2
	median := intervals[middle]
	if len(intervals)%2 == 0 {
		median = (intervals[middle-1] + intervals[middle]) / 2
	}
	return math.Round((60/median)*100) / 100
}

func everyNthBeat(frames []int, step int) []int {
	return everyNthBeatFrom(frames, step, 0)
}

func everyNthBeatFrom(frames []int, step, phase int) []int {
	if len(frames) == 0 || step <= 0 {
		return []int{}
	}
	phase %= step
	if phase < 0 {
		phase += step
	}
	if phase >= len(frames) {
		return []int{}
	}
	result := make([]int, 0, (len(frames)-phase+step-1)/step)
	for index := phase; index < len(frames); index += step {
		result = append(result, frames[index])
	}
	return result
}

// inferDownbeatPhase 不把节拍跟踪器返回的第一个点直接当成小节第一拍。
// 它用强瞬态与四种 4/4 相位的贴合度选择更可信的下拍相位；没有强瞬态时保持旧行为。
func inferDownbeatPhase(beats, strongOnsets []int, fps int) int {
	if len(beats) < 4 || len(strongOnsets) == 0 || fps <= 0 {
		return 0
	}
	tolerance := max(2, int(math.Round(float64(fps)*0.2)))
	bestPhase := 0
	bestScore := -1
	bestMatches := -1
	bestDistance := math.MaxInt
	for phase := 0; phase < 4; phase++ {
		score := 0
		matches := 0
		totalDistance := 0
		for _, onset := range strongOnsets {
			nearest := math.MaxInt
			for index := phase; index < len(beats); index += 4 {
				distance := absInt(beats[index] - onset)
				if distance < nearest {
					nearest = distance
				}
			}
			if nearest <= tolerance {
				matches++
				totalDistance += nearest
				score += tolerance - nearest + 1
			}
		}
		if score > bestScore ||
			(score == bestScore && matches > bestMatches) ||
			(score == bestScore && matches == bestMatches && totalDistance < bestDistance) {
			bestPhase = phase
			bestScore = score
			bestMatches = matches
			bestDistance = totalDistance
		}
	}
	return bestPhase
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
