package media

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
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
	// Method 标注检出来源：rms_silence（能量静音）或 rms_breath（频谱呼吸），合并段用 + 连接。
	Method string
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
			Method:           "rms_silence",
		})
	}
	// Q4: 频谱呼吸检测补一遍，把 silencedetect 漏掉的吸气/咂嘴并入气口集合（best-effort，
	// 失败则退回纯 silencedetect，不阻断分析）。
	analysisMethod := "ffmpeg-silencedetect-rms"
	if breathRanges, breathErr := detectBreathRanges(
		ctx, source, fps, options.ThresholdDB, durationFrames, options.MinPauseFrames, options.KeepEdgeFrames,
	); breathErr == nil {
		if !options.IncludeBoundaries {
			breathRanges = dropBoundaryRanges(breathRanges, durationFrames)
		}
		if len(breathRanges) > 0 {
			pauses = mergePausesWithBreath(pauses, breathRanges, options.KeepEdgeFrames)
			analysisMethod = "ffmpeg-silencedetect-rms+spectral-breath"
		}
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
		AnalysisMethod: analysisMethod,
	}, nil
}

// mergePausesWithBreath 把频谱呼吸段并入 silence 气口：起点排序后区间并集，重叠或相邻
// （≤mergeGap 帧）则扩展并合并 method；呼吸段整段可删（无 KeepEdge 收边）。
func mergePausesWithBreath(silencePauses []SpeechPause, breathRanges [][2]int, mergeGap int) []SpeechPause {
	segments := make([]SpeechPause, 0, len(silencePauses)+len(breathRanges))
	segments = append(segments, silencePauses...)
	for _, breath := range breathRanges {
		segments = append(segments, SpeechPause{
			SourceStartFrame: breath[0], SourceEndFrame: breath[1],
			DeleteStartFrame: breath[0], DeleteEndFrame: breath[1], Method: "rms_breath",
		})
	}
	sort.Slice(segments, func(left, right int) bool {
		return segments[left].SourceStartFrame < segments[right].SourceStartFrame
	})
	merged := make([]SpeechPause, 0, len(segments))
	for _, segment := range segments {
		last := len(merged) - 1
		if last >= 0 && segment.SourceStartFrame <= merged[last].SourceEndFrame+mergeGap {
			merged[last].SourceEndFrame = max(merged[last].SourceEndFrame, segment.SourceEndFrame)
			merged[last].DeleteStartFrame = min(merged[last].DeleteStartFrame, segment.DeleteStartFrame)
			merged[last].DeleteEndFrame = max(merged[last].DeleteEndFrame, segment.DeleteEndFrame)
			merged[last].Method = combineDetectionMethods(merged[last].Method, segment.Method)
			continue
		}
		merged = append(merged, segment)
	}
	return merged
}

// combineDetectionMethods 把两个检出来源合并成排序去重的规范形（与 agentexec 的
// joinSpeechDetectionMethods 同口径：跨包不复用，但保持一致），例如 rms_breath+rms_silence。
func combineDetectionMethods(left, right string) string {
	seen := map[string]struct{}{}
	for _, value := range strings.Split(left+"+"+right, "+") {
		if value = strings.TrimSpace(value); value != "" {
			seen[value] = struct{}{}
		}
	}
	ordered := make([]string, 0, len(seen))
	for value := range seen {
		ordered = append(ordered, value)
	}
	sort.Strings(ordered)
	return strings.Join(ordered, "+")
}

// dropBoundaryRanges 丢弃触及素材首尾的呼吸段（start==0 或 end>=durationFrames），与静音
// 路径的 !IncludeBoundaries 守卫对称：clip 首尾的呼吸不标记为可删。
func dropBoundaryRanges(ranges [][2]int, durationFrames int) [][2]int {
	kept := make([][2]int, 0, len(ranges))
	for _, item := range ranges {
		if item[0] == 0 || item[1] >= durationFrames {
			continue
		}
		kept = append(kept, item)
	}
	return kept
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

// --- Q4: 频谱呼吸检测 ---
// silencedetect 只看 RMS 能量，漏掉「有能量但非语音」的吸气/咂嘴/唇音（接缝呼吸残渣的来源）。
// 呼吸是宽带噪声（谱平坦度高，元音≈0.003、呼吸≈0.75），且比语音安静。用窗口化
// astats(RMS)+aspectralstats(flatness) 补一遍，专抓安静且噪声样的段落。

const (
	breathFlatnessThreshold = 0.45 // 谱平坦度 ≥ 此值视为噪声/呼吸
	breathCeilingOffsetDB   = 22.0 // 呼吸能量上限 = 静音地板 + 此偏移；更响的按语音处理不删
	breathMaxWindows        = 200000
)

var (
	astatsRMSPattern        = regexp.MustCompile(`lavfi\.astats\.Overall\.RMS_level=(-?[0-9.]+|-?inf|nan)`)
	spectralFlatnessPattern = regexp.MustCompile(`lavfi\.aspectralstats\.1\.flatness=([0-9.eE+-]+)`)
	framePtsTimePattern     = regexp.MustCompile(`pts_time:([0-9.]+)`)
)

// detectBreathRanges 返回被 silencedetect 漏检的呼吸段 [startFrame,endFrame]。minFrames 为
// 呼吸段最短帧数，mergeGap 为相邻呼吸窗口的合并容差（帧）。
func detectBreathRanges(
	ctx context.Context,
	source string,
	fps int,
	silenceFloorDB float64,
	durationFrames, minFrames, mergeGap int,
) ([][2]int, error) {
	filter := "aformat=sample_fmts=flt:channel_layouts=mono," +
		"astats=metadata=1:reset=1:measure_overall=RMS_level," +
		"aspectralstats=measure=flatness,ametadata=print:file=-"
	result, err := RunCommand(
		ctx, "ffmpeg",
		"-hide_banner", "-nostats", "-loglevel", "error",
		"-i", source, "-map", "0:a:0",
		"-af", filter, "-f", "null", "-",
	)
	if err != nil {
		return nil, err
	}
	ceiling := silenceFloorDB + breathCeilingOffsetDB
	breathFrames := parseBreathFrames(result.Stdout, fps, silenceFloorDB, ceiling, durationFrames)
	return mergeFrameFlags(breathFrames, minFrames, mergeGap), nil
}

// parseBreathFrames 逐窗口读 RMS_level + flatness，标记「安静且噪声样」的窗口所覆盖的视频帧。
func parseBreathFrames(output []byte, fps int, floorDB, ceilingDB float64, durationFrames int) []bool {
	flags := make([]bool, max(0, durationFrames))
	blocks := framePtsTimePattern.FindAllSubmatchIndex(output, -1)
	if len(blocks) > breathMaxWindows {
		blocks = blocks[:breathMaxWindows]
	}
	for index, block := range blocks {
		startSec, err := strconv.ParseFloat(string(output[block[2]:block[3]]), 64)
		if err != nil {
			continue
		}
		segEnd := len(output)
		if index+1 < len(blocks) {
			segEnd = blocks[index+1][0]
		}
		segment := output[block[0]:segEnd]
		rms, okRMS := matchFloat(astatsRMSPattern, segment)
		flatness, okFlat := matchFloat(spectralFlatnessPattern, segment)
		if !okRMS || !okFlat {
			continue
		}
		if flatness < breathFlatnessThreshold || rms <= floorDB || rms > ceilingDB {
			continue
		}
		endSec := startSec + 1.0/float64(fps)
		if index+1 < len(blocks) {
			if next, err := strconv.ParseFloat(string(output[blocks[index+1][2]:blocks[index+1][3]]), 64); err == nil {
				endSec = next
			}
		}
		startFrame := max(0, int(math.Round(startSec*float64(fps))))
		endFrame := min(durationFrames, int(math.Round(endSec*float64(fps))))
		for frame := startFrame; frame < endFrame && frame < len(flags); frame++ {
			flags[frame] = true
		}
	}
	return flags
}

func matchFloat(pattern *regexp.Regexp, segment []byte) (float64, bool) {
	match := pattern.FindSubmatch(segment)
	if match == nil {
		return 0, false
	}
	raw := string(match[1])
	if raw == "-inf" || raw == "inf" || raw == "nan" {
		return 0, false
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}

// mergeFrameFlags 把逐帧布尔标记合并成 [start,end) 段，容忍 mergeGap 帧的空隙，丢弃 < minFrames 的段。
func mergeFrameFlags(flags []bool, minFrames, mergeGap int) [][2]int {
	ranges := make([][2]int, 0)
	start := -1
	gap := 0
	for frame := 0; frame < len(flags); frame++ {
		if flags[frame] {
			if start < 0 {
				start = frame
			}
			gap = 0
			continue
		}
		if start < 0 {
			continue
		}
		gap++
		if gap > mergeGap {
			end := frame - gap + 1
			if end-start >= minFrames {
				ranges = append(ranges, [2]int{start, end})
			}
			start = -1
			gap = 0
		}
	}
	if start >= 0 {
		end := len(flags)
		if end-start >= minFrames {
			ranges = append(ranges, [2]int{start, end})
		}
	}
	return ranges
}
