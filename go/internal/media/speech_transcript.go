package media

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

type SubtitleCue struct {
	StartFrame int
	EndFrame   int
	Text       string
}

var srtTimePattern = regexp.MustCompile(
	`^\s*([0-9]{1,2}):([0-9]{2}):([0-9]{2})[,.]([0-9]{3})\s*-->\s*` +
		`([0-9]{1,2}):([0-9]{2}):([0-9]{2})[,.]([0-9]{3})`,
)

func FindSidecarSRT(source string) string {
	extension := filepath.Ext(source)
	base := strings.TrimSuffix(source, extension)
	for _, candidate := range []string{base + ".srt", base + ".SRT"} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
}

func ParseSRT(path string, fps int) ([]SubtitleCue, error) {
	if fps <= 0 {
		return nil, errors.New("解析 SRT 的 fps 必须为正数")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	lines := []string{}
	for scanner.Scan() {
		lines = append(lines, strings.TrimSuffix(scanner.Text(), "\r"))
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	cues := []SubtitleCue{}
	for index := 0; index < len(lines); index++ {
		match := srtTimePattern.FindStringSubmatch(lines[index])
		if len(match) == 0 {
			continue
		}
		startMS, parseStartErr := srtTimestampMilliseconds(match[1:5])
		endMS, parseEndErr := srtTimestampMilliseconds(match[5:9])
		if parseStartErr != nil || parseEndErr != nil || endMS <= startMS {
			return nil, fmt.Errorf("SRT 第 %d 行时间范围无效", index+1)
		}
		textLines := []string{}
		for index++; index < len(lines) && strings.TrimSpace(lines[index]) != ""; index++ {
			textLines = append(textLines, strings.TrimSpace(lines[index]))
		}
		text := strings.TrimSpace(strings.Join(textLines, " "))
		if text == "" {
			continue
		}
		startFrame := millisecondsToFrame(startMS, fps)
		endFrame := max(startFrame+1, millisecondsToFrame(endMS, fps))
		cues = append(cues, SubtitleCue{StartFrame: startFrame, EndFrame: endFrame, Text: text})
	}
	if len(cues) == 0 {
		return nil, errors.New("SRT 没有可用字幕条目")
	}
	return cues, nil
}

func ExtractAudioSegmentMP3(
	ctx context.Context,
	temporaryDirectory, source string,
	startFrame, endFrame, fps int,
) (string, error) {
	if fps <= 0 || startFrame < 0 || endFrame <= startFrame {
		return "", errors.New("ASR 音频裁切范围无效")
	}
	file, err := os.CreateTemp(temporaryDirectory, "rushes-asr-*.mp3")
	if err != nil {
		return "", err
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	startSec := float64(startFrame) / float64(fps)
	durationSec := float64(endFrame-startFrame) / float64(fps)
	_, err = RunCommand(
		ctx, "ffmpeg", "-y", "-hide_banner", "-loglevel", "error",
		"-ss", formatSeconds(startSec), "-t", formatSeconds(durationSec), "-i", source,
		"-vn", "-ac", "1", "-ar", "16000", "-b:a", "64k", path,
	)
	if err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

func srtTimestampMilliseconds(parts []string) (int, error) {
	if len(parts) != 4 {
		return 0, errors.New("SRT 时间字段数量无效")
	}
	values := make([]int, 4)
	for index, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil {
			return 0, err
		}
		values[index] = value
	}
	return ((values[0]*60+values[1])*60+values[2])*1000 + values[3], nil
}

func millisecondsToFrame(milliseconds, fps int) int {
	return (milliseconds*fps + 500) / 1000
}
