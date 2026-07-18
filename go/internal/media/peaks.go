package media

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"
)

const (
	// PeaksSchemaVersion identifies the on-disk peaks JSON layout so the
	// frontend can evolve rendering without breaking older artifacts.
	PeaksSchemaVersion = 1
	// PeaksSampleRateHz is how many min/max pairs are produced per second of
	// audio. ~100 pairs/sec keeps the JSON small while giving the timeline
	// enough resolution at any practical zoom.
	PeaksSampleRateHz = 100
	// peaksInternalRate is the ffmpeg resample rate the window size is computed
	// against.
	peaksInternalRate = 48000
	// maxPeaksPairs caps the array so a pathologically long asset can't blow up
	// memory / payload (~2.7h of audio at PeaksSampleRateHz).
	maxPeaksPairs = 1_000_000
)

// WaveformPeaks is a precomputed, self-describing audio waveform: one [min,max]
// amplitude pair per 1/SampleRateHz window, each value normalized to [-1,1].
// The frontend renders these directly instead of downloading and decoding the
// whole audio track in the browser.
type WaveformPeaks struct {
	Version      int          `json:"version"`
	SampleRateHz int          `json:"sample_rate_hz"`
	DurationSec  float64      `json:"duration_sec"`
	Peaks        [][2]float64 `json:"peaks"`
}

// AnalyzeWaveformPeaks extracts min/max amplitude pairs from the first audio
// stream of source using ffmpeg astats over fixed-size windows.
func AnalyzeWaveformPeaks(ctx context.Context, source string, durationSec float64) (WaveformPeaks, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return WaveformPeaks{}, errors.New("未安装 ffmpeg，无法生成波形峰值")
	}
	window := peaksInternalRate / PeaksSampleRateHz
	if window < 1 {
		window = 1
	}
	// aformat=flt 让 astats 的 Min/Max_level 归一化到 [-1,1]（否则是原始采样单位）；
	// asetnsamples 把音频切成固定窗口，astats reset 后每窗口报一次 Overall 统计。
	filter := fmt.Sprintf(
		"aresample=%d,aformat=sample_fmts=flt,asetnsamples=n=%d:p=0,"+
			"astats=metadata=1:reset=1,ametadata=print:file=-",
		peaksInternalRate,
		window,
	)
	result, err := RunCommand(
		ctx,
		"ffmpeg",
		"-hide_banner", "-nostats", "-loglevel", "error",
		"-i", source,
		"-map", "0:a:0", "-vn", "-sn", "-dn", "-ac", "1",
		"-af", filter,
		"-f", "null", "-",
	)
	if err != nil {
		return WaveformPeaks{}, err
	}
	pairs := parseWaveformPeaks(result.Stdout, maxPeaksPairs)
	if len(pairs) == 0 {
		return WaveformPeaks{}, errors.New("ffmpeg 未返回可用的波形峰值")
	}
	return WaveformPeaks{
		Version:      PeaksSchemaVersion,
		SampleRateHz: PeaksSampleRateHz,
		DurationSec:  durationSec,
		Peaks:        pairs,
	}, nil
}

// parseWaveformPeaks reads ametadata=print output, taking Overall.Min_level and
// Overall.Max_level from each astats window (windows are delimited by a
// "frame:" header line).
func parseWaveformPeaks(output []byte, maxPairs int) [][2]float64 {
	const (
		framePrefix = "frame:"
		minKey      = "lavfi.astats.Overall.Min_level="
		maxKey      = "lavfi.astats.Overall.Max_level="
	)
	pairs := make([][2]float64, 0, 1024)
	var curMin, curMax float64
	var inFrame bool
	flush := func() {
		if inFrame {
			pairs = append(pairs, [2]float64{clampPeak(curMin), clampPeak(curMax)})
		}
	}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), framePrefix) {
			flush()
			if len(pairs) >= maxPairs {
				return pairs
			}
			inFrame, curMin, curMax = true, 0, 0
			continue
		}
		if idx := strings.Index(line, minKey); idx >= 0 {
			curMin = parseLevel(line[idx+len(minKey):])
			continue
		}
		if idx := strings.Index(line, maxKey); idx >= 0 {
			curMax = parseLevel(line[idx+len(maxKey):])
		}
	}
	flush()
	return pairs
}

func parseLevel(raw string) float64 {
	raw = strings.TrimSpace(raw)
	if strings.EqualFold(raw, "-inf") || strings.EqualFold(raw, "inf") ||
		strings.EqualFold(raw, "+inf") || strings.EqualFold(raw, "nan") {
		return 0
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0
	}
	return value
}

func clampPeak(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return math.Max(-1, math.Min(1, value))
}
