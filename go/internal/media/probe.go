package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type Probe struct {
	DurationSec float64  `json:"duration_sec"`
	FPS         *float64 `json:"fps"`
	Width       *int     `json:"width"`
	Height      *int     `json:"height"`
	HasAudio    bool     `json:"has_audio"`
}

type ffprobePayload struct {
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
	Streams []struct {
		CodecType   string `json:"codec_type"`
		CodecName   string `json:"codec_name"`
		PixelFormat string `json:"pix_fmt"`
		Duration    string `json:"duration"`
		AverageRate string `json:"avg_frame_rate"`
		Width       int    `json:"width"`
		Height      int    `json:"height"`
	} `json:"streams"`
}

func ProbeFile(ctx context.Context, path string) (Probe, error) {
	result, err := RunCommand(ctx, "ffprobe", "-v", "error", "-print_format", "json", "-show_format", "-show_streams", path)
	if err != nil {
		return Probe{}, err
	}
	return parseProbe(result.Stdout)
}

func parseProbe(data []byte) (Probe, error) {
	var payload ffprobePayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return Probe{}, fmt.Errorf("ffprobe 返回无效 JSON: %w", err)
	}
	probe := Probe{}
	probe.DurationSec, _ = strconv.ParseFloat(payload.Format.Duration, 64)
	for _, stream := range payload.Streams {
		switch stream.CodecType {
		case "video":
			if stream.Width > 0 {
				probe.Width = intPointer(stream.Width)
			}
			if stream.Height > 0 {
				probe.Height = intPointer(stream.Height)
			}
			if rate, err := parseRate(stream.AverageRate); err == nil && rate > 0 {
				probe.FPS = floatPointer(rate)
			}
			if probe.DurationSec <= 0 {
				probe.DurationSec, _ = strconv.ParseFloat(stream.Duration, 64)
			}
		case "audio":
			probe.HasAudio = true
			if probe.DurationSec <= 0 {
				probe.DurationSec, _ = strconv.ParseFloat(stream.Duration, 64)
			}
		}
	}
	if probe.DurationSec < 0 {
		probe.DurationSec = 0
	}
	return probe, nil
}

func parseRate(raw string) (float64, error) {
	if raw == "" || raw == "0/0" || raw == "N/A" {
		return 0, errors.New("帧率不可用")
	}
	numerator, denominator, hasSlash := strings.Cut(raw, "/")
	if !hasSlash {
		return strconv.ParseFloat(raw, 64)
	}
	n, err := strconv.ParseFloat(numerator, 64)
	if err != nil {
		return 0, err
	}
	d, err := strconv.ParseFloat(denominator, 64)
	if err != nil || d == 0 {
		return 0, errors.New("帧率分母无效")
	}
	return n / d, nil
}

func intPointer(value int) *int { return &value }

func floatPointer(value float64) *float64 { return &value }
