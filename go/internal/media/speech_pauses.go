package media

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
)

type SpeechPauseOptions struct {
	ThresholdDB       float64
	MinPauseFrames    int
	KeepEdgeFrames    int
	MaxPauses         int
	IncludeBoundaries bool
}

type SpeechPause struct {
	SourceStartFrame int
	SourceEndFrame   int
	DeleteStartFrame int
	DeleteEndFrame   int
}

type SpeechPauseAnalysis struct {
	DurationFrames int
	Pauses         []SpeechPause
	Truncated      bool
	AnalysisMethod string
}

var (
	silenceStartPattern = regexp.MustCompile(`silence_start:\s*([0-9]+(?:\.[0-9]+)?)`)
	silenceEndPattern   = regexp.MustCompile(`silence_end:\s*([0-9]+(?:\.[0-9]+)?)`)
)

func AnalyzeSpeechPauses(
	ctx context.Context,
	source string,
	fps int,
	options SpeechPauseOptions,
) (SpeechPauseAnalysis, error) {
	if fps <= 0 {
		return SpeechPauseAnalysis{}, errors.New("气口分析 fps 必须为正数")
	}
	probe, err := ProbeFile(ctx, source)
	if err != nil {
		return SpeechPauseAnalysis{}, err
	}
	if !probe.HasAudio {
		return SpeechPauseAnalysis{}, errors.New("素材没有可分析的音轨")
	}
	options = normalizeSpeechPauseOptions(options, fps)
	minDuration := float64(options.MinPauseFrames) / float64(fps)
	filter := fmt.Sprintf("silencedetect=noise=%sdB:d=%s", formatSeconds(options.ThresholdDB), formatSeconds(minDuration))
	result, err := RunCommand(
		ctx,
		"ffmpeg",
		"-hide_banner",
		"-nostats",
		"-i", source,
		"-map", "0:a:0",
		"-af", filter,
		"-f", "null",
		"-",
	)
	if err != nil {
		return SpeechPauseAnalysis{}, err
	}
	durationFrames := max(0, int(math.Round(probe.DurationSec*float64(fps))))
	ranges := parseSilenceRanges(result.Stderr, probe.DurationSec)
	pauses := make([]SpeechPause, 0, len(ranges))
	for _, silence := range ranges {
		start := max(0, int(math.Round(silence[0]*float64(fps))))
		end := min(durationFrames, int(math.Round(silence[1]*float64(fps))))
		if end-start < options.MinPauseFrames {
			continue
		}
		if !options.IncludeBoundaries && (start == 0 || end >= durationFrames) {
			continue
		}
		deleteStart := min(end, start+options.KeepEdgeFrames)
		deleteEnd := max(deleteStart, end-options.KeepEdgeFrames)
		if deleteEnd <= deleteStart {
			continue
		}
		pauses = append(pauses, SpeechPause{
			SourceStartFrame: start,
			SourceEndFrame:   end,
			DeleteStartFrame: deleteStart,
			DeleteEndFrame:   deleteEnd,
		})
	}
	sort.Slice(pauses, func(left, right int) bool {
		return pauses[left].SourceStartFrame < pauses[right].SourceStartFrame
	})
	truncated := len(pauses) > options.MaxPauses
	if truncated {
		pauses = pauses[:options.MaxPauses]
	}
	return SpeechPauseAnalysis{
		DurationFrames: durationFrames,
		Pauses:         pauses,
		Truncated:      truncated,
		AnalysisMethod: "ffmpeg-silencedetect-rms",
	}, nil
}

func normalizeSpeechPauseOptions(options SpeechPauseOptions, fps int) SpeechPauseOptions {
	if options.ThresholdDB == 0 {
		options.ThresholdDB = -35
	}
	options.ThresholdDB = math.Max(-80, math.Min(-10, options.ThresholdDB))
	if options.MinPauseFrames <= 0 {
		options.MinPauseFrames = max(4, int(math.Round(float64(fps)*0.18)))
	}
	if options.KeepEdgeFrames < 0 {
		options.KeepEdgeFrames = 0
	}
	if options.KeepEdgeFrames == 0 {
		options.KeepEdgeFrames = max(1, int(math.Round(float64(fps)*0.06)))
	}
	if options.MaxPauses <= 0 {
		options.MaxPauses = 200
	}
	options.MaxPauses = min(options.MaxPauses, 1000)
	return options
}

func parseSilenceRanges(output []byte, durationSec float64) [][2]float64 {
	starts := silenceStartPattern.FindAllSubmatch(output, -1)
	ends := silenceEndPattern.FindAllSubmatch(output, -1)
	values := make([]struct {
		second float64
		start  bool
	}, 0, len(starts)+len(ends))
	for _, match := range starts {
		second, err := strconv.ParseFloat(string(match[1]), 64)
		if err == nil {
			values = append(values, struct {
				second float64
				start  bool
			}{second: second, start: true})
		}
	}
	for _, match := range ends {
		second, err := strconv.ParseFloat(string(match[1]), 64)
		if err == nil {
			values = append(values, struct {
				second float64
				start  bool
			}{second: second})
		}
	}
	sort.SliceStable(values, func(left, right int) bool {
		if values[left].second == values[right].second {
			return values[left].start
		}
		return values[left].second < values[right].second
	})
	ranges := make([][2]float64, 0, len(starts))
	current := -1.0
	for _, value := range values {
		if value.start {
			current = value.second
			continue
		}
		if current >= 0 && value.second > current {
			ranges = append(ranges, [2]float64{current, value.second})
			current = -1
		}
	}
	if current >= 0 && durationSec > current {
		ranges = append(ranges, [2]float64{current, durationSec})
	}
	return ranges
}
