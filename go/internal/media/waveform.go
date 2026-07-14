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
	DefaultWaveformPoints = 96
	MaxWaveformPoints     = 256
	WaveformEncoding      = "rms_db_-60_0_to_0_100"
	waveformSampleRate    = 48000
	waveformFloorDB       = -60.0
	waveformCeilingDB     = 0.0
)

// WaveformEnvelope is a deliberately small, time-ordered audio observation for
// model context. Samples are fixed-scale measurements rather than semantic
// labels: 0 means <= -60 dB RMS and 100 means 0 dB RMS. The consumer decides
// what the dynamics mean for the requested edit.
type WaveformEnvelope struct {
	SampleIntervalFrames int     `json:"sample_interval_frames"`
	SampleFrames         []int   `json:"sample_frames"`
	Samples              []int   `json:"samples"`
	Encoding             string  `json:"encoding"`
	FloorDB              float64 `json:"floor_db"`
	CeilingDB            float64 `json:"ceiling_db"`
}

func AnalyzeWaveformEnvelope(
	ctx context.Context,
	source string,
	fps int,
	durationFrames int,
	maxPoints int,
) (WaveformEnvelope, error) {
	if fps <= 0 || durationFrames <= 0 {
		return WaveformEnvelope{}, errors.New("压缩波形需要正数 fps 和 duration_frames")
	}
	if maxPoints <= 0 {
		maxPoints = DefaultWaveformPoints
	}
	if maxPoints > MaxWaveformPoints {
		return WaveformEnvelope{}, fmt.Errorf("压缩波形最多 %d 个采样点", MaxWaveformPoints)
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return WaveformEnvelope{}, errors.New("未安装 ffmpeg，无法生成压缩波形")
	}

	intervalFrames := max(1, int(math.Ceil(float64(durationFrames)/float64(maxPoints))))
	windowSamples := max(1, int(math.Round(
		float64(intervalFrames)*waveformSampleRate/float64(fps),
	)))
	filter := fmt.Sprintf(
		"aresample=%d,asetnsamples=n=%d:p=0,astats=metadata=1:reset=1,"+
			"ametadata=print:key=lavfi.astats.Overall.RMS_level:file=-",
		waveformSampleRate,
		windowSamples,
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
		return WaveformEnvelope{}, err
	}
	samples := parseWaveformRMSLevels(result.Stdout)
	if len(samples) == 0 {
		return WaveformEnvelope{}, errors.New("ffmpeg 未返回可用的 RMS 波形采样")
	}
	if len(samples) > maxPoints {
		samples = samples[:maxPoints]
	}
	sampleFrames := make([]int, len(samples))
	for index := range samples {
		// 每个值描述从该帧开始、长度为 SampleIntervalFrames 的 RMS 窗口。
		// 显式返回坐标，避免模型自行做数组下标到时间线帧的换算。
		sampleFrames[index] = index * intervalFrames
	}
	return WaveformEnvelope{
		SampleIntervalFrames: intervalFrames,
		SampleFrames:         sampleFrames,
		Samples:              samples,
		Encoding:             WaveformEncoding,
		FloorDB:              waveformFloorDB,
		CeilingDB:            waveformCeilingDB,
	}, nil
}

func parseWaveformRMSLevels(output []byte) []int {
	const key = "lavfi.astats.Overall.RMS_level="
	samples := make([]int, 0, DefaultWaveformPoints)
	for _, line := range strings.Split(string(output), "\n") {
		index := strings.Index(line, key)
		if index < 0 {
			continue
		}
		raw := strings.TrimSpace(line[index+len(key):])
		value := waveformFloorDB
		if !strings.EqualFold(raw, "-inf") && !strings.EqualFold(raw, "nan") {
			parsed, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				continue
			}
			value = parsed
		}
		samples = append(samples, normalizeWaveformDB(value))
	}
	return samples
}

func normalizeWaveformDB(value float64) int {
	if math.IsNaN(value) || math.IsInf(value, -1) {
		value = waveformFloorDB
	} else if math.IsInf(value, 1) {
		value = waveformCeilingDB
	}
	value = math.Max(waveformFloorDB, math.Min(waveformCeilingDB, value))
	return int(math.Round((value - waveformFloorDB) / (waveformCeilingDB - waveformFloorDB) * 100))
}
