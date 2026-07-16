package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
)

type RenderProfile struct {
	Name       string
	Width      int
	Height     int
	CRF        int
	AutoOrient bool
}

var (
	PreviewProfile = RenderProfile{Name: "preview", Width: 540, Height: 960, CRF: 24, AutoOrient: true}
	FinalProfile   = RenderProfile{Name: "final", Width: 1080, Height: 1920, CRF: 18, AutoOrient: true}
)

func ProfileForOrientation(profile RenderProfile, orientation string) (RenderProfile, error) {
	switch strings.TrimSpace(strings.ToLower(orientation)) {
	case "", "auto":
		return profile, nil
	case "portrait":
		profile.Width, profile.Height = min(profile.Width, profile.Height), max(profile.Width, profile.Height)
		profile.AutoOrient = false
		return profile, nil
	case "landscape":
		profile.Width, profile.Height = max(profile.Width, profile.Height), min(profile.Width, profile.Height)
		profile.AutoOrient = false
		return profile, nil
	default:
		return RenderProfile{}, errors.New("orientation 必须是 auto、portrait 或 landscape")
	}
}

type RenderResult struct {
	Object      ObjectRef
	Width       int
	Height      int
	FPS         float64
	DurationSec float64
}

func RenderTimeline(
	ctx context.Context,
	database *storage.DB,
	document timeline.Document,
	profile RenderProfile,
	onProgress func(Progress),
) (RenderResult, error) {
	primary := timelineTrack(document, "visual_base")
	if primary == nil || len(primary.Clips) == 0 {
		return RenderResult{}, errors.New("时间线没有主视觉 clip")
	}
	if document.FPS <= 0 {
		return RenderResult{}, errors.New("时间线 fps 无效")
	}
	profile = orientProfileToPrimary(ctx, database, primary.Clips, profile)
	args := []string{"-y"}
	filters := make([]string, 0, len(primary.Clips)+16)
	labels := strings.Builder{}
	primaryInputs := make([]preparedPrimaryInput, 0, len(primary.Clips))
	inputIndex := 0
	for index, clip := range primary.Clips {
		source, kind, err := ResolveAssetSource(ctx, database, clip.AssetID)
		if err != nil {
			return RenderResult{}, fmt.Errorf("解析 clip %s: %w", clip.TimelineClipID, err)
		}
		sourceStart := float64(clip.SourceStartFrame) / float64(document.FPS)
		sourceDuration := float64(clip.SourceEndFrame-clip.SourceStartFrame) / float64(document.FPS)
		timelineDuration := float64(clip.TimelineEndFrame-clip.TimelineStartFrame) / float64(document.FPS)
		switch kind {
		case "image":
			args = append(args, "-loop", "1", "-t", formatSeconds(timelineDuration), "-i", source)
		case "video":
			args = append(args, "-ss", formatSeconds(sourceStart), "-t", formatSeconds(max(sourceDuration, 0.04)), "-i", source)
		default:
			return RenderResult{}, fmt.Errorf("主视觉 clip %s 不是视频或图片", clip.TimelineClipID)
		}
		videoFilter := primaryVideoFilter(inputIndex, clip, document, profile, index, sourceDuration, timelineDuration)
		filters = append(filters, videoFilter)
		_, _ = fmt.Fprintf(&labels, "[v%d]", index)
		probe := Probe{}
		if kind == "video" {
			probe, _ = ProbeFile(ctx, source)
		}
		primaryInputs = append(primaryInputs, preparedPrimaryInput{
			clip: clip, inputIndex: inputIndex, source: source, kind: kind, probe: probe,
		})
		inputIndex++
	}
	filters = append(filters, fmt.Sprintf("%sconcat=n=%d:v=1:a=0[basev]", labels.String(), len(primary.Clips)))
	videoLabel := "basev"

	videoLabel, args, filters, inputIndex, err := appendVisualOverlays(
		ctx, database, document, profile, videoLabel, args, filters, inputIndex,
	)
	if err != nil {
		return RenderResult{}, err
	}
	videoLabel, subtitlePath, fontDirectory, err := appendSubtitles(ctx, database, document, profile, videoLabel, &filters)
	if err != nil {
		return RenderResult{}, err
	}
	if subtitlePath != "" {
		defer func() { _ = os.Remove(subtitlePath) }()
	}
	if fontDirectory != "" {
		defer func() { _ = os.RemoveAll(fontDirectory) }()
	}

	audioLabel, nextArgs, nextFilters, _, err := appendAudioMix(
		ctx, database, document, primaryInputs, args, filters, inputIndex,
	)
	if err != nil {
		return RenderResult{}, err
	}
	args = nextArgs
	filters = nextFilters
	output, err := os.CreateTemp(database.Paths.Temporary, "render-*.mp4")
	if err != nil {
		return RenderResult{}, err
	}
	outputPath := output.Name()
	_ = output.Close()
	defer func() { _ = os.Remove(outputPath) }()
	args = append(args, "-filter_complex", strings.Join(filters, ";"), "-map", "["+videoLabel+"]")
	if audioLabel == "" {
		args = append(args, "-an")
	} else {
		args = append(args, "-map", "["+audioLabel+"]", "-c:a", "aac", "-b:a", "160k")
	}
	args = append(args,
		"-c:v", "libx264", "-preset", "veryfast", "-crf", strconv.Itoa(profile.CRF),
		"-pix_fmt", "yuv420p", "-movflags", "+faststart", outputPath,
	)
	if err := RunFFmpegProgress(ctx, "ffmpeg", args, onProgress); err != nil {
		return RenderResult{}, err
	}
	object, err := NewObjectStore(database.Paths).PutFile(ctx, outputPath)
	if err != nil {
		return RenderResult{}, err
	}
	return RenderResult{
		Object: object, Width: profile.Width, Height: profile.Height,
		FPS: float64(document.FPS), DurationSec: float64(document.DurationFrames) / float64(document.FPS),
	}, nil
}

func primaryVideoFilter(
	inputIndex int,
	clip timeline.Clip,
	document timeline.Document,
	profile RenderProfile,
	outputIndex int,
	sourceDuration, timelineDuration float64,
) string {
	parts := []string{
		fmt.Sprintf("[%d:v]scale=%d:%d:force_original_aspect_ratio=decrease", inputIndex, profile.Width, profile.Height),
		fmt.Sprintf("pad=%d:%d:(ow-iw)/2:(oh-ih)/2:black", profile.Width, profile.Height),
		"setsar=1",
		fmt.Sprintf("fps=%d", document.FPS),
		"trim=duration=" + formatSeconds(max(sourceDuration, timelineDuration)),
		"setpts=(PTS-STARTPTS)/" + formatSeconds(effectivePlaybackRate(clip.PlaybackRate)),
		"trim=duration=" + formatSeconds(timelineDuration),
	}
	parts = append(parts, videoFadeFilters(clip, document.FPS, timelineDuration, false)...)
	return strings.Join(parts, ",") + fmt.Sprintf("[v%d]", outputIndex)
}

func videoFadeFilters(clip timeline.Clip, fps int, durationSec float64, alpha bool) []string {
	if fps <= 0 || durationSec <= 0 {
		return nil
	}
	alphaOption := ""
	if alpha {
		alphaOption = ":alpha=1"
	}
	filters := []string{}
	if clip.FadeInFrames > 0 {
		filters = append(filters, fmt.Sprintf("fade=t=in:st=0:d=%s%s", formatSeconds(float64(clip.FadeInFrames)/float64(fps)), alphaOption))
	}
	if clip.FadeOutFrames > 0 {
		fadeDuration := float64(clip.FadeOutFrames) / float64(fps)
		filters = append(filters, fmt.Sprintf(
			"fade=t=out:st=%s:d=%s%s",
			formatSeconds(max(0, durationSec-fadeDuration)), formatSeconds(fadeDuration), alphaOption,
		))
	}
	return filters
}

func orientProfileToPrimary(
	ctx context.Context,
	database *storage.DB,
	clips []timeline.Clip,
	profile RenderProfile,
) RenderProfile {
	if !profile.AutoOrient || profile.Width == profile.Height {
		return profile
	}
	landscapeWeight := 0
	portraitWeight := 0
	probes := map[string]Probe{}
	for _, clip := range clips {
		probe, exists := probes[clip.AssetID]
		if !exists {
			source, kind, err := ResolveAssetSource(ctx, database, clip.AssetID)
			if err != nil || kind != "video" && kind != "image" {
				continue
			}
			probe, err = ProbeFile(ctx, source)
			if err != nil {
				continue
			}
			probes[clip.AssetID] = probe
		}
		displayWidth, displayHeight := probe.displayDimensions()
		if displayWidth == nil || displayHeight == nil || *displayWidth == *displayHeight {
			continue
		}
		weight := max(1, clip.TimelineEndFrame-clip.TimelineStartFrame)
		if *displayWidth > *displayHeight {
			landscapeWeight += weight
		} else {
			portraitWeight += weight
		}
	}
	if landscapeWeight > portraitWeight && profile.Width < profile.Height ||
		portraitWeight > landscapeWeight && profile.Width > profile.Height {
		profile.Width, profile.Height = profile.Height, profile.Width
	}
	return profile
}

type preparedPrimaryInput struct {
	clip       timeline.Clip
	inputIndex int
	source     string
	kind       string
	probe      Probe
}

func appendVisualOverlays(
	ctx context.Context,
	database *storage.DB,
	document timeline.Document,
	profile RenderProfile,
	videoLabel string,
	args, filters []string,
	inputIndex int,
) (string, []string, []string, int, error) {
	overlayTracks := renderableTracks(document, "visual", false)
	overlayNumber := 0
	for _, track := range overlayTracks {
		if track.TrackID == "visual_base" {
			continue
		}
		for _, clip := range track.Clips {
			source, kind, err := ResolveAssetSource(ctx, database, clip.AssetID)
			if err != nil {
				return "", args, filters, inputIndex, fmt.Errorf("解析叠加 clip %s: %w", clip.TimelineClipID, err)
			}
			sourceStart := float64(clip.SourceStartFrame) / float64(document.FPS)
			sourceDuration := float64(clip.SourceEndFrame-clip.SourceStartFrame) / float64(document.FPS)
			timelineDuration := float64(clip.TimelineEndFrame-clip.TimelineStartFrame) / float64(document.FPS)
			switch kind {
			case "image":
				args = append(args, "-loop", "1", "-t", formatSeconds(timelineDuration), "-i", source)
			case "video":
				args = append(args, "-ss", formatSeconds(sourceStart), "-t", formatSeconds(max(sourceDuration, 0.04)), "-i", source)
			default:
				return "", args, filters, inputIndex, fmt.Errorf("叠加 clip %s 不是视频或图片", clip.TimelineClipID)
			}
			inputLabel := fmt.Sprintf("overlay%d", overlayNumber)
			filters = append(filters, overlayVideoFilter(
				inputIndex, clip, document, profile, inputLabel, sourceDuration, timelineDuration,
			))
			outputLabel := fmt.Sprintf("video_overlayed_%d", overlayNumber)
			filters = append(filters, fmt.Sprintf(
				"[%s][%s]overlay=eof_action=pass:shortest=0:enable='between(t,%s,%s)'[%s]",
				videoLabel, inputLabel,
				formatSeconds(float64(clip.TimelineStartFrame)/float64(document.FPS)),
				formatSeconds(float64(clip.TimelineEndFrame)/float64(document.FPS)), outputLabel,
			))
			videoLabel = outputLabel
			inputIndex++
			overlayNumber++
		}
	}
	return videoLabel, args, filters, inputIndex, nil
}

func overlayVideoFilter(
	inputIndex int,
	clip timeline.Clip,
	document timeline.Document,
	profile RenderProfile,
	outputLabel string,
	sourceDuration, timelineDuration float64,
) string {
	parts := []string{
		fmt.Sprintf("[%d:v]scale=%d:%d:force_original_aspect_ratio=decrease", inputIndex, profile.Width, profile.Height),
		"format=rgba",
		fmt.Sprintf("pad=%d:%d:(ow-iw)/2:(oh-ih)/2:color=black@0", profile.Width, profile.Height),
		"setsar=1",
		fmt.Sprintf("fps=%d", document.FPS),
		"trim=duration=" + formatSeconds(max(sourceDuration, timelineDuration)),
		"setpts=(PTS-STARTPTS)/" + formatSeconds(effectivePlaybackRate(clip.PlaybackRate)),
		"trim=duration=" + formatSeconds(timelineDuration),
	}
	parts = append(parts, videoFadeFilters(clip, document.FPS, timelineDuration, true)...)
	parts = append(parts, "setpts=PTS+"+formatSeconds(float64(clip.TimelineStartFrame)/float64(document.FPS))+"/TB")
	return strings.Join(parts, ",") + "[" + outputLabel + "]"
}

func appendAudioMix(
	ctx context.Context,
	database *storage.DB,
	document timeline.Document,
	primaryInputs []preparedPrimaryInput,
	args, filters []string,
	inputIndex int,
) (string, []string, []string, int, error) {
	audioTracks := renderableTracks(document, "audio", true)
	ducking := (*timeline.TrackDucking)(nil)
	if bgm := timelineTrack(document, "bgm"); bgm != nil {
		ducking = bgm.Ducking
	}
	duckingTriggers := map[string]bool{}
	if ducking != nil && ducking.Enabled {
		for _, trackID := range ducking.TriggerTracks {
			duckingTriggers[trackID] = true
		}
	}
	trackLabels := []audioTrackLabel{}
	audioNumber := 0
	for _, track := range audioTracks {
		clipLabels := []string{}
		duckingKeyLabels := []string{}
		if track.TrackID == "original_audio" && len(track.Clips) == 0 {
			for _, input := range primaryInputs {
				if input.kind != "video" || !input.probe.HasAudio {
					continue
				}
				label := fmt.Sprintf("audio%d", audioNumber)
				filters = append(filters, audioFilter(
					input.inputIndex, input.clip, track.GainDB, document, label,
				))
				label, keyLabel, splitFilter := splitDuckingKey(label, duckingTriggers[track.TrackID])
				if splitFilter != "" {
					filters = append(filters, splitFilter)
					duckingKeyLabels = append(duckingKeyLabels, keyLabel)
				}
				clipLabels = append(clipLabels, label)
				audioNumber++
			}
		} else {
			for _, clip := range track.Clips {
				source, kind, err := ResolveAssetSource(ctx, database, clip.AssetID)
				if err != nil {
					return "", args, filters, inputIndex, fmt.Errorf("解析音频 clip %s: %w", clip.TimelineClipID, err)
				}
				if kind == "image" {
					continue
				}
				probe, probeErr := ProbeFile(ctx, source)
				if probeErr != nil {
					return "", args, filters, inputIndex, probeErr
				}
				if !probe.HasAudio {
					continue
				}
				sourceStart := float64(clip.SourceStartFrame) / float64(document.FPS)
				sourceDuration := float64(clip.SourceEndFrame-clip.SourceStartFrame) / float64(document.FPS)
				args = append(args,
					"-ss", formatSeconds(sourceStart), "-t", formatSeconds(max(sourceDuration, 0.04)), "-i", source,
				)
				label := fmt.Sprintf("audio%d", audioNumber)
				filters = append(filters, audioFilter(inputIndex, clip, track.GainDB, document, label))
				label, keyLabel, splitFilter := splitDuckingKey(label, duckingTriggers[track.TrackID])
				if splitFilter != "" {
					filters = append(filters, splitFilter)
					duckingKeyLabels = append(duckingKeyLabels, keyLabel)
				}
				clipLabels = append(clipLabels, label)
				inputIndex++
				audioNumber++
			}
		}
		if len(clipLabels) > 0 {
			label := "track_" + track.TrackID
			filters = append(filters, mixAudioFilter(clipLabels, label, document))
			trackLabels = append(trackLabels, audioTrackLabel{
				TrackID: track.TrackID, Label: label, DuckingKeyLabels: duckingKeyLabels,
			})
		}
	}
	if len(trackLabels) == 0 {
		return "", args, filters, inputIndex, nil
	}
	durationSec := float64(document.DurationFrames) / float64(document.FPS)
	finalLabels, duckingFilters := buildDuckingFilterGraph(trackLabels, ducking, durationSec)
	filters = append(filters, duckingFilters...)
	outputLabel := "mixed_audio"
	filters = append(filters, mixAudioFilter(finalLabels, outputLabel, document))
	return outputLabel, args, filters, inputIndex, nil
}

type audioTrackLabel struct {
	TrackID          string
	Label            string
	DuckingKeyLabels []string
}

func splitDuckingKey(label string, enabled bool) (string, string, string) {
	if !enabled {
		return label, "", ""
	}
	mixLabel := label + "_mix"
	keyLabel := label + "_duck_key"
	return mixLabel, keyLabel, fmt.Sprintf("[%s]asplit=2[%s][%s]", label, mixLabel, keyLabel)
}

func mixAudioFilter(labels []string, outputLabel string, document timeline.Document) string {
	duration := formatSeconds(float64(document.DurationFrames) / float64(document.FPS))
	inputs := make([]string, 0, len(labels))
	for _, label := range labels {
		inputs = append(inputs, "["+label+"]")
	}
	if len(inputs) == 1 {
		return fmt.Sprintf("%sanull,apad=whole_dur=%s,atrim=duration=%s[%s]", inputs[0], duration, duration, outputLabel)
	}
	return fmt.Sprintf(
		"%samix=inputs=%d:duration=longest:dropout_transition=0:normalize=0,"+
			"apad=whole_dur=%s,atrim=duration=%s[%s]",
		strings.Join(inputs, ""), len(inputs), duration, duration, outputLabel,
	)
}

func buildDuckingFilterGraph(
	tracks []audioTrackLabel,
	ducking *timeline.TrackDucking,
	durationSec float64,
) ([]string, []string) {
	labels := make([]string, len(tracks))
	bgmIndex := -1
	trackIndex := map[string]int{}
	for index, track := range tracks {
		labels[index] = track.Label
		trackIndex[track.TrackID] = index
		if track.TrackID == "bgm" {
			bgmIndex = index
		}
	}
	if ducking == nil || !ducking.Enabled {
		return labels, nil
	}
	if bgmIndex < 0 {
		filters := []string{}
		for _, track := range tracks {
			for _, keyLabel := range track.DuckingKeyLabels {
				filters = append(filters, fmt.Sprintf("[%s]anullsink", keyLabel))
			}
		}
		return labels, filters
	}
	triggerIndexes := make([]int, 0, len(ducking.TriggerTracks))
	for _, trigger := range ducking.TriggerTracks {
		index, exists := trackIndex[trigger]
		if !exists {
			continue
		}
		triggerIndexes = append(triggerIndexes, index)
	}
	if len(triggerIndexes) == 0 {
		return labels, nil
	}
	filters := make([]string, 0, len(triggerIndexes)*2+2)
	keyLabels := []string{}
	for _, index := range triggerIndexes {
		rawKeyLabels := tracks[index].DuckingKeyLabels
		if len(rawKeyLabels) == 0 {
			mixLabel := tracks[index].Label + "_mix"
			rawKeyLabels = []string{tracks[index].Label + "_duck_key"}
			filters = append(filters, fmt.Sprintf(
				"[%s]asplit=2[%s][%s]", tracks[index].Label, mixLabel, rawKeyLabels[0],
			))
			labels[index] = mixLabel
		}
		for _, keyLabel := range rawKeyLabels {
			gatedKeyLabel := keyLabel + "_gated"
			filters = append(filters, fmt.Sprintf(
				"[%s]agate=threshold=0.003:ratio=9000:range=0:attack=15:release=250:knee=1:detection=rms,"+
					"apad=whole_dur=%s,atrim=duration=%s[%s]",
				keyLabel, formatSeconds(durationSec), formatSeconds(durationSec), gatedKeyLabel,
			))
			keyLabels = append(keyLabels, gatedKeyLabel)
		}
	}
	keyLabel := keyLabels[0]
	if len(keyLabels) > 1 {
		inputs := make([]string, 0, len(keyLabels))
		for _, label := range keyLabels {
			inputs = append(inputs, "["+label+"]")
		}
		keyLabel = "ducking_key"
		filters = append(filters, fmt.Sprintf(
			"%samix=inputs=%d:duration=longest:dropout_transition=0:normalize=0[%s]",
			strings.Join(inputs, ""), len(inputs), keyLabel,
		))
	}
	normalizedKeyLabel := keyLabel + "_normalized"
	filters = append(filters, fmt.Sprintf(
		"[%s]compand=attacks=0.015:decays=0.250:points=-90/-90|-50/0|0/0[%s]",
		keyLabel, normalizedKeyLabel,
	))
	duckAmount := math.Abs(ducking.DuckDB)
	// 每个 clip 先独立门控，避免多个底噪相加后误触发；compand 再把有效信号
	// 校准到稳定电平，并按约 26 dB 的可用压缩范围换算目标衰减。
	ratio := 1 / (1 - min(duckAmount, 18)/26)
	filters = append(filters, fmt.Sprintf(
		"[%s][%s]sidechaincompress=threshold=0.05:ratio=%s:attack=15:release=250:detection=rms:link=average[bgm_ducked]",
		tracks[bgmIndex].Label, normalizedKeyLabel, formatSeconds(ratio),
	))
	labels[bgmIndex] = "bgm_ducked"
	return labels, filters
}

func audioFilter(
	inputIndex int,
	clip timeline.Clip,
	trackGain float64,
	document timeline.Document,
	label string,
) string {
	parts := []string{
		fmt.Sprintf("[%d:a]atrim=duration=%s", inputIndex, formatSeconds(
			float64(clip.SourceEndFrame-clip.SourceStartFrame)/float64(document.FPS),
		)),
		"asetpts=PTS-STARTPTS",
	}
	parts = append(parts, atempoFilters(effectivePlaybackRate(clip.PlaybackRate))...)
	parts = append(parts,
		fmt.Sprintf("volume=%sdB", formatSeconds(clip.GainDB+trackGain)),
		fmt.Sprintf("atrim=duration=%s", formatSeconds(
			float64(clip.TimelineEndFrame-clip.TimelineStartFrame)/float64(document.FPS),
		)),
	)
	timelineDuration := float64(clip.TimelineEndFrame-clip.TimelineStartFrame) / float64(document.FPS)
	if clip.FadeInFrames > 0 {
		parts = append(parts, fmt.Sprintf(
			"afade=t=in:st=0:d=%s",
			formatSeconds(float64(clip.FadeInFrames)/float64(document.FPS)),
		))
	}
	if clip.FadeOutFrames > 0 {
		fadeDuration := float64(clip.FadeOutFrames) / float64(document.FPS)
		parts = append(parts, fmt.Sprintf(
			"afade=t=out:st=%s:d=%s",
			formatSeconds(max(0, timelineDuration-fadeDuration)), formatSeconds(fadeDuration),
		))
	}
	parts = append(parts, fmt.Sprintf("adelay=%d:all=1", int(math.Round(
		float64(clip.TimelineStartFrame)*1000/float64(document.FPS),
	))))
	return strings.Join(parts, ",") + "[" + label + "]"
}

func atempoFilters(rate float64) []string {
	if math.Abs(rate-1) < 0.000001 {
		return nil
	}
	filters := []string{}
	for rate > 2 {
		filters = append(filters, "atempo=2")
		rate /= 2
	}
	for rate < 0.5 {
		filters = append(filters, "atempo=0.5")
		rate /= 0.5
	}
	filters = append(filters, "atempo="+formatSeconds(rate))
	return filters
}

func appendSubtitles(
	ctx context.Context,
	database *storage.DB,
	document timeline.Document,
	profile RenderProfile,
	videoLabel string,
	filters *[]string,
) (string, string, string, error) {
	track := timelineTrack(document, "subtitles")
	if track == nil || track.Muted || len(track.Clips) == 0 {
		return videoLabel, "", "", nil
	}
	clips := append([]timeline.Clip(nil), track.Clips...)
	sort.SliceStable(clips, func(i, j int) bool {
		return clips[i].TimelineStartFrame < clips[j].TimelineStartFrame
	})
	file, err := os.CreateTemp(database.Paths.Temporary, "rushes-subtitles-*.ass")
	if err != nil {
		return "", "", "", err
	}
	path := file.Name()
	fontDirectory, fontName, err := subtitleFontDirectory(ctx, database, document.DraftID)
	if err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", "", "", err
	}
	if _, err := file.WriteString(subtitleASSHeader(profile, fontName)); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		_ = os.RemoveAll(fontDirectory)
		return "", "", "", err
	}
	for index, clip := range clips {
		text := strings.TrimSpace(strings.ReplaceAll(clip.Text, "\r", ""))
		if text == "" {
			continue
		}
		style := clip.SubtitleStyle
		if !containsSubtitleStyle(style) {
			style = "default"
		}
		if _, err := fmt.Fprintf(file, "Dialogue: %d,%s,%s,%s,,0,0,0,,%s\n",
			index,
			formatASSTime(float64(clip.TimelineStartFrame)/float64(document.FPS)),
			formatASSTime(float64(clip.TimelineEndFrame)/float64(document.FPS)), style,
			escapeASSText(text),
		); err != nil {
			_ = file.Close()
			_ = os.Remove(path)
			_ = os.RemoveAll(fontDirectory)
			return "", "", "", err
		}
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		_ = os.RemoveAll(fontDirectory)
		return "", "", "", err
	}
	outputLabel := "subtitled_video"
	filter := fmt.Sprintf("[%s]subtitles=filename='%s'", videoLabel, escapeFilterPath(path))
	if fontDirectory != "" {
		filter += ":fontsdir='" + escapeFilterPath(fontDirectory) + "'"
	}
	filter += fmt.Sprintf(":original_size=%dx%d[%s]", profile.Width, profile.Height, outputLabel)
	*filters = append(*filters, filter)
	return outputLabel, path, fontDirectory, nil
}

type subtitlePresetParameters struct {
	FontSize    int
	Alignment   int
	MarginV     int
	Outline     int
	Bold        int
	BorderStyle int
}

func subtitlePreset(name string, profile RenderProfile) subtitlePresetParameters {
	scale := float64(profile.Height) / 1080
	preset := subtitlePresetParameters{FontSize: 42, Alignment: 2, MarginV: 84, Outline: 3, BorderStyle: 1}
	switch name {
	case "large_center":
		preset = subtitlePresetParameters{FontSize: 62, Alignment: 5, MarginV: 0, Outline: 4, Bold: -1, BorderStyle: 1}
	case "top_bar":
		preset = subtitlePresetParameters{FontSize: 44, Alignment: 8, MarginV: 76, Outline: 1, Bold: -1, BorderStyle: 3}
	case "minimal":
		preset = subtitlePresetParameters{FontSize: 36, Alignment: 2, MarginV: 68, Outline: 1, BorderStyle: 1}
	case "bold_bottom":
		preset = subtitlePresetParameters{FontSize: 52, Alignment: 2, MarginV: 92, Outline: 5, Bold: -1, BorderStyle: 1}
	}
	preset.FontSize = max(1, int(math.Round(float64(preset.FontSize)*scale)))
	preset.MarginV = max(0, int(math.Round(float64(preset.MarginV)*scale)))
	preset.Outline = max(0, int(math.Round(float64(preset.Outline)*scale)))
	return preset
}

func subtitleASSHeader(profile RenderProfile, fontName string) string {
	fontName = sanitizeASSFontName(fontName)
	lines := []string{
		"[Script Info]",
		"ScriptType: v4.00+",
		fmt.Sprintf("PlayResX: %d", profile.Width),
		fmt.Sprintf("PlayResY: %d", profile.Height),
		"WrapStyle: 0",
		"ScaledBorderAndShadow: yes",
		"",
		"[V4+ Styles]",
		"Format: Name,Fontname,Fontsize,PrimaryColour,SecondaryColour,OutlineColour,BackColour,Bold,Italic,Underline,StrikeOut,ScaleX,ScaleY,Spacing,Angle,BorderStyle,Outline,Shadow,Alignment,MarginL,MarginR,MarginV,Encoding",
	}
	for _, name := range timeline.SubtitleStyleNames {
		preset := subtitlePreset(name, profile)
		lines = append(lines, fmt.Sprintf(
			"Style: %s,%s,%d,&H00FFFFFF,&H000000FF,&H00101010,&H90000000,%d,0,0,0,100,100,0,0,%d,%d,0,%d,40,40,%d,1",
			name, fontName, preset.FontSize, preset.Bold, preset.BorderStyle, preset.Outline, preset.Alignment, preset.MarginV,
		))
	}
	lines = append(lines, "", "[Events]", "Format: Layer,Start,End,Style,Name,MarginL,MarginR,MarginV,Effect,Text")
	return strings.Join(lines, "\n") + "\n"
}

func sanitizeASSFontName(value string) string {
	value = strings.Map(func(character rune) rune {
		if character == ',' || unicode.IsControl(character) {
			return ' '
		}
		return character
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		return "Arial"
	}
	return value
}

func subtitleFontDirectory(ctx context.Context, database *storage.DB, draftID string) (string, string, error) {
	assets, err := storage.ListDraftAssets(ctx, database.Read(), draftID)
	if err != nil {
		return "", "", err
	}
	fontSources := []string{}
	fontName := ""
	for _, asset := range assets {
		if asset.Kind != "font" || !asset.Usable {
			continue
		}
		source, _, resolveErr := ResolveAssetSource(ctx, database, asset.ID)
		if resolveErr != nil {
			continue
		}
		fontSources = append(fontSources, source)
		if fontName == "" {
			fontName = subtitleFontFamily(ctx, source)
		}
	}
	if len(fontSources) == 0 {
		return "", "", nil
	}
	directory, err := os.MkdirTemp(database.Paths.Temporary, "rushes-fonts-*")
	if err != nil {
		return "", "", err
	}
	for index, source := range fontSources {
		name := fmt.Sprintf("%03d-%s", index, filepath.Base(source))
		if err := linkOrCopyFile(source, filepath.Join(directory, name)); err != nil {
			_ = os.RemoveAll(directory)
			return "", "", err
		}
	}
	return directory, fontName, nil
}

func linkOrCopyFile(source, destination string) error {
	return linkOrCopyFileWith(source, destination, os.Symlink, os.Link)
}

func linkOrCopyFileWith(
	source, destination string,
	symlink func(string, string) error,
	hardlink func(string, string) error,
) error {
	if err := symlink(source, destination); err == nil {
		return nil
	}
	if err := hardlink(source, destination); err == nil {
		return nil
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = input.Close() }()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err = io.Copy(output, input); err != nil {
		_ = output.Close()
		_ = os.Remove(destination)
		return err
	}
	if err = output.Close(); err != nil {
		_ = os.Remove(destination)
		return err
	}
	return nil
}

func subtitleFontFamily(ctx context.Context, source string) string {
	fallback := strings.TrimSuffix(filepath.Base(source), filepath.Ext(source))
	if _, err := exec.LookPath("fc-scan"); err != nil {
		return fallback
	}
	result, err := RunCommand(ctx, "fc-scan", "--format=%{family[0]}", source)
	if err != nil {
		return fallback
	}
	if family := strings.TrimSpace(string(result.Stdout)); family != "" {
		return family
	}
	return fallback
}

func containsSubtitleStyle(value string) bool {
	for _, name := range timeline.SubtitleStyleNames {
		if value == name {
			return true
		}
	}
	return false
}

func formatASSTime(seconds float64) string {
	if seconds < 0 {
		seconds = 0
	}
	centiseconds := int(math.Round(seconds * 100))
	return fmt.Sprintf("%d:%02d:%02d.%02d", centiseconds/360000, centiseconds/6000%60, centiseconds/100%60, centiseconds%100)
}

func escapeASSText(value string) string {
	return strings.NewReplacer("\\", `\\`, "{", `\{`, "}", `\}`, "\n", `\N`).Replace(value)
}

func renderableTracks(document timeline.Document, family string, honorSolo bool) []timeline.Track {
	hasSolo := false
	if honorSolo {
		for _, track := range document.Tracks {
			if trackFamilyForRender(track) == family && track.Solo && !track.Muted {
				hasSolo = true
			}
		}
	}
	tracks := []timeline.Track{}
	for _, track := range document.Tracks {
		if trackFamilyForRender(track) != family || track.Muted || hasSolo && !track.Solo {
			continue
		}
		tracks = append(tracks, track)
	}
	return tracks
}

func trackFamilyForRender(track timeline.Track) string {
	switch track.TrackID {
	case "visual_base", "visual_overlay":
		return "visual"
	case "original_audio", "voiceover", "bgm", "sfx":
		return "audio"
	case "subtitles":
		return "text"
	default:
		return track.TrackType
	}
}

func effectivePlaybackRate(rate float64) float64 {
	if rate > 0 {
		return rate
	}
	return 1
}

func escapeFilterPath(path string) string {
	replacer := strings.NewReplacer("\\", "\\\\", ":", "\\:", "'", "\\'")
	return replacer.Replace(path)
}

type InspectionIssue struct {
	Check     string `json:"check"`
	Severity  string `json:"severity"`
	ErrorCode string `json:"error_code,omitempty"`
	Message   string `json:"message"`
	Frames    []int  `json:"frames,omitempty"`
}

type Inspection struct {
	Summary  string            `json:"summary"`
	Degraded bool              `json:"degraded"`
	Issues   []InspectionIssue `json:"issues"`
}

type ExpectedVideo struct {
	Width        int
	Height       int
	FPS          float64
	DurationSec  float64
	ExpectAudio  bool
	BlackFrames  []FrameInterval
	FreezeFrames []FrameInterval
}

type FrameInterval struct {
	Start int
	End   int
}

// TimelineInspectionIntent records signal ranges that are intentional in the
// exact timeline snapshot used to render a preview.
func TimelineInspectionIntent(
	ctx context.Context,
	database *storage.DB,
	document timeline.Document,
) (ExpectedVideo, error) {
	expected := ExpectedVideo{}
	primary := timelineTrack(document, "visual_base")
	if primary != nil {
		for _, clip := range primary.Clips {
			kind := clip.AssetKind
			if kind == "" {
				_, resolvedKind, err := ResolveAssetSource(ctx, database, clip.AssetID)
				if err != nil {
					return ExpectedVideo{}, fmt.Errorf("解析预览验收 clip %s: %w", clip.TimelineClipID, err)
				}
				kind = resolvedKind
			}
			if kind == "image" {
				expected.FreezeFrames = append(expected.FreezeFrames, FrameInterval{
					Start: clip.TimelineStartFrame, End: clip.TimelineEndFrame,
				})
			}
			if clip.FadeInFrames > 0 {
				expected.BlackFrames = append(expected.BlackFrames, FrameInterval{
					Start: clip.TimelineStartFrame,
					End:   min(clip.TimelineEndFrame, clip.TimelineStartFrame+clip.FadeInFrames),
				})
			}
			if clip.FadeOutFrames > 0 {
				expected.BlackFrames = append(expected.BlackFrames, FrameInterval{
					Start: max(clip.TimelineStartFrame, clip.TimelineEndFrame-clip.FadeOutFrames),
					End:   clip.TimelineEndFrame,
				})
			}
		}
	}
	expected.ExpectAudio = timelineExpectsRenderedAudio(ctx, database, document)
	return expected, nil
}

func timelineExpectsRenderedAudio(ctx context.Context, database *storage.DB, document timeline.Document) bool {
	for _, track := range renderableTracks(document, "audio", true) {
		clips := track.Clips
		if track.TrackID == "original_audio" && len(clips) == 0 {
			if primaryTrack := timelineTrack(document, "visual_base"); primaryTrack != nil {
				clips = primaryTrack.Clips
			}
		}
		for _, clip := range clips {
			source, kind, err := ResolveAssetSource(ctx, database, clip.AssetID)
			if err != nil || kind == "image" {
				continue
			}
			probe, err := ProbeFile(ctx, source)
			if err == nil && probe.HasAudio {
				return true
			}
		}
	}
	return false
}

func InspectVideo(ctx context.Context, path string, expected ExpectedVideo, checks []string) (Inspection, error) {
	probe, err := ProbeFile(ctx, path)
	if err != nil {
		return Inspection{}, err
	}
	inspection := Inspection{Summary: "预览流与渲染快照一致，可正常解码。", Issues: []InspectionIssue{}}
	if probe.Width == nil || probe.Height == nil {
		inspection.Issues = append(inspection.Issues, InspectionIssue{Check: "streams", Severity: "error", Message: "缺少视频流"})
	} else if expected.Width > 0 && (*probe.Width != expected.Width || *probe.Height != expected.Height) {
		inspection.Issues = append(inspection.Issues, InspectionIssue{
			Check: "streams", Severity: "error",
			Message: fmt.Sprintf("分辨率 %dx%d 与快照 %dx%d 不一致", *probe.Width, *probe.Height, expected.Width, expected.Height),
		})
	}
	if expected.DurationSec > 0 && abs(probe.DurationSec-expected.DurationSec) > 0.25 {
		inspection.Issues = append(inspection.Issues, InspectionIssue{
			Check: "streams", Severity: "warning",
			Message: fmt.Sprintf("时长 %.3fs 与快照 %.3fs 不一致", probe.DurationSec, expected.DurationSec),
		})
	}
	if containsCheck(checks, "decode") || len(checks) == 0 {
		if _, err := RunCommand(ctx, "ffmpeg", "-v", "error", "-i", path, "-f", "null", "-"); err != nil {
			slog.Warn("预览解码质检失败", "path", path, "error", err)
			inspection.Issues = append(inspection.Issues, InspectionIssue{
				Check: "decode", Severity: "error", ErrorCode: "preview_decode_failed",
				Message: "预览视频无法完整解码。",
			})
		}
	}
	if expected.ExpectAudio && !probe.HasAudio && (inspectionCheckEnabled(checks, "silence") || inspectionCheckEnabled(checks, "loudness")) {
		inspection.Issues = append(inspection.Issues, InspectionIssue{
			Check: "audio_stream", Severity: "error", Message: "缺少音频流，无法执行静音与响度检查",
		})
	}
	requestedSignalCheck := false
	for _, check := range []string{"black", "freeze", "silence", "loudness"} {
		requestedSignalCheck = requestedSignalCheck || inspectionCheckEnabled(checks, check)
	}
	if requestedSignalCheck {
		if _, lookupErr := exec.LookPath("ffmpeg"); lookupErr != nil {
			inspection.Degraded = true
			inspection.Issues = append(inspection.Issues, InspectionIssue{
				Check: "dependencies", Severity: "warning", Message: "本机缺少 ffmpeg，无法执行成片信号检查。",
			})
		} else {
			signalIssues, signalErr := inspectVideoSignals(ctx, path, probe, expected, checks)
			if signalErr != nil {
				return Inspection{}, signalErr
			}
			inspection.Issues = append(inspection.Issues, signalIssues...)
		}
	}
	FinalizeInspectionSummary(&inspection)
	return inspection, nil
}

// FinalizeInspectionSummary 在所有确定性检查与可选视觉检查合并后统一生成摘要。
func FinalizeInspectionSummary(inspection *Inspection) {
	switch {
	case inspection.Degraded && len(inspection.Issues) > 0:
		inspection.Summary = fmt.Sprintf("成片检查降级完成：发现 %d 项提示。", len(inspection.Issues))
	case len(inspection.Issues) > 0:
		inspection.Summary = fmt.Sprintf("成片检查完成：发现 %d 项提示。", len(inspection.Issues))
	case inspection.Degraded:
		inspection.Summary = "成片检查降级完成，部分检查未执行。"
	default:
		inspection.Summary = "预览流与渲染快照一致，可正常解码。"
	}
}

const (
	inspectionBlackDurationSec   = 0.10
	inspectionFreezeDurationSec  = 0.50
	inspectionSilenceDurationSec = 0.40
	inspectionMinLUFS            = -24.0
	inspectionMaxLUFS            = -8.0
)

var (
	blackIntervalPattern          = regexp.MustCompile(`black_start:([0-9.]+) black_end:([0-9.]+) black_duration:([0-9.]+)`)
	freezeStartPattern            = regexp.MustCompile(`freeze_start: ([0-9.]+)`)
	freezeEndPattern              = regexp.MustCompile(`freeze_end: ([0-9.]+)`)
	inspectionSilenceStartPattern = regexp.MustCompile(`silence_start: ([0-9.]+)`)
	inspectionSilenceEndPattern   = regexp.MustCompile(`silence_end: ([0-9.]+) \| silence_duration: ([0-9.]+)`)
	loudnessPattern               = regexp.MustCompile(`(?mi)^\s*I:\s*(-?(?:[0-9]+(?:\.[0-9]+)?|inf)) LUFS\s*$`)
)

func inspectVideoSignals(
	ctx context.Context,
	path string,
	probe Probe,
	expected ExpectedVideo,
	checks []string,
) ([]InspectionIssue, error) {
	fps := expected.FPS
	if fps <= 0 && probe.FPS != nil {
		fps = *probe.FPS
	}
	if fps <= 0 {
		fps = float64(timeline.DefaultFPS)
	}
	filters := make([]string, 0, 2)
	maps := make([]string, 0, 4)
	executedChecks := make([]string, 0, 4)
	if probe.Width != nil && (inspectionCheckEnabled(checks, "black") || inspectionCheckEnabled(checks, "freeze")) {
		videoFilters := make([]string, 0, 2)
		if inspectionCheckEnabled(checks, "black") {
			videoFilters = append(videoFilters, fmt.Sprintf("blackdetect=d=%.2f:pix_th=0.10", inspectionBlackDurationSec))
			executedChecks = append(executedChecks, "black")
		}
		if inspectionCheckEnabled(checks, "freeze") {
			videoFilters = append(videoFilters, fmt.Sprintf("freezedetect=n=-50dB:d=%.2f", inspectionFreezeDurationSec))
			executedChecks = append(executedChecks, "freeze")
		}
		filters = append(filters, "[0:v]"+strings.Join(videoFilters, ",")+"[inspectv]")
		maps = append(maps, "-map", "[inspectv]")
	}
	if probe.HasAudio && (inspectionCheckEnabled(checks, "silence") || inspectionCheckEnabled(checks, "loudness")) {
		audioFilters := make([]string, 0, 2)
		if inspectionCheckEnabled(checks, "silence") {
			audioFilters = append(audioFilters, fmt.Sprintf("silencedetect=n=-50dB:d=%.2f", inspectionSilenceDurationSec))
			executedChecks = append(executedChecks, "silence")
		}
		if inspectionCheckEnabled(checks, "loudness") {
			audioFilters = append(audioFilters, "ebur128=framelog=verbose")
			executedChecks = append(executedChecks, "loudness")
		}
		filters = append(filters, "[0:a]"+strings.Join(audioFilters, ",")+"[inspecta]")
		maps = append(maps, "-map", "[inspecta]")
	}
	if len(filters) == 0 {
		return nil, nil
	}
	args := []string{"-hide_banner", "-nostats", "-i", path, "-filter_complex", strings.Join(filters, ";")}
	args = append(args, maps...)
	args = append(args, "-f", "null", "-")
	result, err := RunCommand(ctx, "ffmpeg", args...)
	if err != nil {
		return nil, fmt.Errorf("执行成片信号检查: %w", err)
	}
	issues := parseInspectionSignals(string(result.Stderr), probe.DurationSec, fps, executedChecks)
	return filterExpectedSignalIssues(issues, expected, fps), nil
}

func filterExpectedSignalIssues(issues []InspectionIssue, expected ExpectedVideo, fps float64) []InspectionIssue {
	filtered := make([]InspectionIssue, 0, len(issues))
	for _, issue := range issues {
		intent := []FrameInterval(nil)
		switch issue.Check {
		case "black":
			intent = expected.BlackFrames
		case "freeze":
			intent = expected.FreezeFrames
		}
		if len(intent) == 0 || len(issue.Frames) != 2 {
			filtered = append(filtered, issue)
			continue
		}
		parts := []FrameInterval{{Start: issue.Frames[0], End: issue.Frames[1]}}
		for _, allowed := range intent {
			parts = subtractFrameInterval(parts, allowed)
		}
		label := "检测到黑帧区间"
		if issue.Check == "freeze" {
			label = "检测到静帧区间"
		}
		for _, part := range parts {
			if part.End <= part.Start {
				continue
			}
			filtered = append(filtered, intervalInspectionIssue(
				issue.Check, label, float64(part.Start)/fps, float64(part.End)/fps, fps,
			))
		}
	}
	return filtered
}

func subtractFrameInterval(parts []FrameInterval, allowed FrameInterval) []FrameInterval {
	result := make([]FrameInterval, 0, len(parts)+1)
	for _, part := range parts {
		if allowed.End <= part.Start || allowed.Start >= part.End {
			result = append(result, part)
			continue
		}
		if allowed.Start > part.Start {
			result = append(result, FrameInterval{Start: part.Start, End: min(part.End, allowed.Start)})
		}
		if allowed.End < part.End {
			result = append(result, FrameInterval{Start: max(part.Start, allowed.End), End: part.End})
		}
	}
	return result
}

func parseInspectionSignals(stderr string, durationSec, fps float64, checks []string) []InspectionIssue {
	issues := make([]InspectionIssue, 0)
	if inspectionCheckEnabled(checks, "black") {
		for _, match := range blackIntervalPattern.FindAllStringSubmatch(stderr, -1) {
			start, _ := strconv.ParseFloat(match[1], 64)
			end, _ := strconv.ParseFloat(match[2], 64)
			issues = append(issues, intervalInspectionIssue("black", "检测到黑帧区间", start, end, fps))
		}
	}
	if inspectionCheckEnabled(checks, "freeze") {
		for _, interval := range pairedInspectionIntervals(stderr, freezeStartPattern, freezeEndPattern, durationSec) {
			issues = append(issues, intervalInspectionIssue("freeze", "检测到静帧区间", interval[0], interval[1], fps))
		}
	}
	if inspectionCheckEnabled(checks, "silence") {
		for _, interval := range pairedInspectionIntervals(stderr, inspectionSilenceStartPattern, inspectionSilenceEndPattern, durationSec) {
			issues = append(issues, intervalInspectionIssue("silence", "检测到静音区间", interval[0], interval[1], fps))
		}
	}
	if inspectionCheckEnabled(checks, "loudness") {
		matches := loudnessPattern.FindAllStringSubmatch(stderr, -1)
		if len(matches) == 0 {
			issues = append(issues, InspectionIssue{
				Check: "loudness", Severity: "warning",
				Message: "未能解析整片综合响度，响度检查未通过。",
				Frames:  []int{0, secondsToInspectionFrame(durationSec, fps)},
			})
		} else {
			rawIntegrated := matches[len(matches)-1][1]
			integrated, parseErr := strconv.ParseFloat(rawIntegrated, 64)
			if parseErr != nil {
				issues = append(issues, InspectionIssue{
					Check: "loudness", Severity: "warning",
					Message: "未能解析整片综合响度，响度检查未通过。",
					Frames:  []int{0, secondsToInspectionFrame(durationSec, fps)},
				})
			} else if math.IsInf(integrated, -1) {
				issues = append(issues, InspectionIssue{
					Check: "loudness", Severity: "warning",
					Message: "整片综合响度为 -inf LUFS，音轨可能完全静音。",
					Frames:  []int{0, secondsToInspectionFrame(durationSec, fps)},
				})
			} else if integrated < inspectionMinLUFS || integrated > inspectionMaxLUFS {
				issues = append(issues, InspectionIssue{
					Check: "loudness", Severity: "warning",
					Message: fmt.Sprintf("整片综合响度 %.1f LUFS，建议控制在 %.0f 至 %.0f LUFS。", integrated, inspectionMinLUFS, inspectionMaxLUFS),
					Frames:  []int{0, secondsToInspectionFrame(durationSec, fps)},
				})
			}
		}
	}
	return issues
}

func pairedInspectionIntervals(stderr string, startPattern, endPattern *regexp.Regexp, durationSec float64) [][2]float64 {
	starts := startPattern.FindAllStringSubmatchIndex(stderr, -1)
	ends := endPattern.FindAllStringSubmatchIndex(stderr, -1)
	intervals := make([][2]float64, 0, len(starts))
	endIndex := 0
	for _, startMatch := range starts {
		start, _ := strconv.ParseFloat(stderr[startMatch[2]:startMatch[3]], 64)
		for endIndex < len(ends) && ends[endIndex][0] < startMatch[0] {
			endIndex++
		}
		end := durationSec
		if endIndex < len(ends) {
			end, _ = strconv.ParseFloat(stderr[ends[endIndex][2]:ends[endIndex][3]], 64)
			endIndex++
		}
		if end > start {
			intervals = append(intervals, [2]float64{start, end})
		}
	}
	return intervals
}

func intervalInspectionIssue(check, label string, start, end, fps float64) InspectionIssue {
	return InspectionIssue{
		Check: check, Severity: "warning",
		Message: fmt.Sprintf("%s %.3fs–%.3fs。", label, start, end),
		Frames:  []int{secondsToInspectionFrame(start, fps), secondsToInspectionFrame(end, fps)},
	}
}

func secondsToInspectionFrame(seconds, fps float64) int {
	return max(0, int(math.Round(seconds*fps)))
}

func inspectionCheckEnabled(checks []string, expected string) bool {
	return len(checks) == 0 || containsCheck(checks, expected)
}

func timelineTrack(document timeline.Document, trackID string) *timeline.Track {
	for index := range document.Tracks {
		if document.Tracks[index].TrackID == trackID {
			return &document.Tracks[index]
		}
	}
	return nil
}

func formatSeconds(value float64) string { return strconv.FormatFloat(value, 'f', 6, 64) }

func containsCheck(checks []string, expected string) bool {
	for _, check := range checks {
		if check == expected {
			return true
		}
	}
	return false
}

func abs(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}
