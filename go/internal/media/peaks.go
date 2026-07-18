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

	peaksFramePrefix = "frame:"
	peaksMinKey      = "lavfi.astats.Overall.Min_level="
	peaksMaxKey      = "lavfi.astats.Overall.Max_level="
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
// stream of source using ffmpeg astats over fixed-size windows. Output is
// streamed line by line and capped at maxPeaksPairs — a multi-hour asset can't
// buffer hundreds of MB of astats text nor blow past the pair budget.
func AnalyzeWaveformPeaks(ctx context.Context, source string, durationSec float64) (WaveformPeaks, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return WaveformPeaks{}, errors.New("未安装 ffmpeg，无法生成波形峰值")
	}
	window := peaksInternalRate / PeaksSampleRateHz
	if window < 1 {
		window = 1
	}
	// aformat=flt 让 astats 的 Min/Max_level 归一化到 [-1,1]（否则是原始采样单位）；
	// asetnsamples 切固定窗口，astats reset 后每窗口报一次统计；measure 收窄到只算
	// Overall 的 Min/Max_level，把每窗口输出从 ~14 行压到 3 行，砍掉绝大部分文本量。
	filter := fmt.Sprintf(
		"aresample=%d,aformat=sample_fmts=flt,asetnsamples=n=%d:p=0,"+
			"astats=metadata=1:reset=1:measure_perchannel=none:measure_overall=Min_level+Max_level,"+
			"ametadata=print:file=-",
		peaksInternalRate,
		window,
	)
	args := []string{
		"-hide_banner", "-nostats", "-loglevel", "error",
		"-i", source,
		"-map", "0:a:0", "-vn", "-sn", "-dn", "-ac", "1",
		"-af", filter,
		"-f", "null", "-",
	}
	accumulator := &peaksAccumulator{maxPairs: maxPeaksPairs}
	if err := RunFFmpegLines(ctx, "ffmpeg", args, accumulator.addLine); err != nil {
		return WaveformPeaks{}, err
	}
	accumulator.flush()
	if len(accumulator.pairs) == 0 {
		return WaveformPeaks{}, errors.New("ffmpeg 未返回可用的波形峰值")
	}
	return WaveformPeaks{
		Version:      PeaksSchemaVersion,
		SampleRateHz: PeaksSampleRateHz,
		DurationSec:  durationSec,
		Peaks:        accumulator.pairs,
	}, nil
}

// peaksAccumulator parses ametadata=print output one line at a time, taking
// Overall.Min_level/Max_level per astats window (windows delimited by a "frame:"
// header). It caps at maxPairs and signals the streamer to stop the moment the
// cap is reached, so memory stays O(maxPairs) regardless of asset length.
type peaksAccumulator struct {
	pairs    [][2]float64
	maxPairs int
	curMin   float64
	curMax   float64
	inFrame  bool
}

// addLine returns false when enough pairs are collected (stop the ffmpeg stream).
func (a *peaksAccumulator) addLine(line string) bool {
	if strings.HasPrefix(strings.TrimSpace(line), peaksFramePrefix) {
		a.flush()
		if len(a.pairs) >= a.maxPairs {
			return false
		}
		a.inFrame, a.curMin, a.curMax = true, 0, 0
		return true
	}
	if idx := strings.Index(line, peaksMinKey); idx >= 0 {
		a.curMin = parseLevel(line[idx+len(peaksMinKey):])
	} else if idx := strings.Index(line, peaksMaxKey); idx >= 0 {
		a.curMax = parseLevel(line[idx+len(peaksMaxKey):])
	}
	return true
}

func (a *peaksAccumulator) flush() {
	if a.inFrame {
		a.pairs = append(a.pairs, [2]float64{clampPeak(a.curMin), clampPeak(a.curMax)})
		a.inFrame = false
	}
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
