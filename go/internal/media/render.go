package media

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"

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
		videoFilter := fmt.Sprintf(
			"[%d:v]scale=%d:%d:force_original_aspect_ratio=decrease,"+
				"pad=%d:%d:(ow-iw)/2:(oh-ih)/2:black,setsar=1,fps=%d,"+
				"trim=duration=%s,setpts=(PTS-STARTPTS)/%s,trim=duration=%s[v%d]",
			inputIndex, profile.Width, profile.Height, profile.Width, profile.Height,
			document.FPS, formatSeconds(max(sourceDuration, timelineDuration)),
			formatSeconds(effectivePlaybackRate(clip.PlaybackRate)), formatSeconds(timelineDuration), index,
		)
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
	videoLabel, subtitlePath, err := appendSubtitles(database, document, videoLabel, &filters)
	if err != nil {
		return RenderResult{}, err
	}
	if subtitlePath != "" {
		defer func() { _ = os.Remove(subtitlePath) }()
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
		if probe.Width == nil || probe.Height == nil || *probe.Width == *probe.Height {
			continue
		}
		weight := max(1, clip.TimelineEndFrame-clip.TimelineStartFrame)
		if *probe.Width > *probe.Height {
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
			filters = append(filters, fmt.Sprintf(
				"[%d:v]scale=%d:%d:force_original_aspect_ratio=decrease,"+
					"pad=%d:%d:(ow-iw)/2:(oh-ih)/2:color=black@0,setsar=1,fps=%d,"+
					"trim=duration=%s,setpts=(PTS-STARTPTS)/%s+%s/TB[%s]",
				inputIndex, profile.Width, profile.Height, profile.Width, profile.Height,
				document.FPS, formatSeconds(max(sourceDuration, timelineDuration)),
				formatSeconds(effectivePlaybackRate(clip.PlaybackRate)),
				formatSeconds(float64(clip.TimelineStartFrame)/float64(document.FPS)), inputLabel,
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

func appendAudioMix(
	ctx context.Context,
	database *storage.DB,
	document timeline.Document,
	primaryInputs []preparedPrimaryInput,
	args, filters []string,
	inputIndex int,
) (string, []string, []string, int, error) {
	audioTracks := renderableTracks(document, "audio", true)
	audioLabels := []string{}
	audioNumber := 0
	for _, track := range audioTracks {
		if track.TrackID == "original_audio" && len(track.Clips) == 0 {
			for _, input := range primaryInputs {
				if input.kind != "video" || !input.probe.HasAudio {
					continue
				}
				label := fmt.Sprintf("audio%d", audioNumber)
				filters = append(filters, audioFilter(
					input.inputIndex, input.clip, track.GainDB, document, label,
				))
				audioLabels = append(audioLabels, "["+label+"]")
				audioNumber++
			}
			continue
		}
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
			audioLabels = append(audioLabels, "["+label+"]")
			inputIndex++
			audioNumber++
		}
	}
	if len(audioLabels) == 0 {
		return "", args, filters, inputIndex, nil
	}
	outputLabel := "mixed_audio"
	duration := formatSeconds(float64(document.DurationFrames) / float64(document.FPS))
	if len(audioLabels) == 1 {
		filters = append(filters, fmt.Sprintf(
			"%sanull,apad=whole_dur=%s,atrim=duration=%s[%s]",
			audioLabels[0], duration, duration, outputLabel,
		))
	} else {
		filters = append(filters, fmt.Sprintf(
			"%samix=inputs=%d:duration=longest:dropout_transition=0:normalize=0,"+
				"apad=whole_dur=%s,atrim=duration=%s[%s]",
			strings.Join(audioLabels, ""), len(audioLabels), duration, duration, outputLabel,
		))
	}
	return outputLabel, args, filters, inputIndex, nil
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
	database *storage.DB,
	document timeline.Document,
	videoLabel string,
	filters *[]string,
) (string, string, error) {
	track := timelineTrack(document, "subtitles")
	if track == nil || track.Muted || len(track.Clips) == 0 {
		return videoLabel, "", nil
	}
	clips := append([]timeline.Clip(nil), track.Clips...)
	sort.SliceStable(clips, func(i, j int) bool {
		return clips[i].TimelineStartFrame < clips[j].TimelineStartFrame
	})
	file, err := os.CreateTemp(database.Paths.Temporary, "rushes-subtitles-*.srt")
	if err != nil {
		return "", "", err
	}
	path := file.Name()
	for index, clip := range clips {
		text := strings.TrimSpace(strings.ReplaceAll(clip.Text, "\r", ""))
		if text == "" {
			continue
		}
		if _, err := fmt.Fprintf(file, "%d\n%s --> %s\n%s\n\n",
			index+1,
			formatSRTTime(float64(clip.TimelineStartFrame)/float64(document.FPS)),
			formatSRTTime(float64(clip.TimelineEndFrame)/float64(document.FPS)), text,
		); err != nil {
			_ = file.Close()
			_ = os.Remove(path)
			return "", "", err
		}
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", "", err
	}
	outputLabel := "subtitled_video"
	*filters = append(*filters, fmt.Sprintf(
		"[%s]subtitles=filename='%s':force_style='Alignment=2,FontSize=18,MarginV=42,Outline=2,Shadow=0'[%s]",
		videoLabel, escapeFilterPath(path), outputLabel,
	))
	return outputLabel, path, nil
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

func formatSRTTime(seconds float64) string {
	if seconds < 0 {
		seconds = 0
	}
	totalMilliseconds := int(math.Round(seconds * 1000))
	hours := totalMilliseconds / 3_600_000
	minutes := totalMilliseconds / 60_000 % 60
	wholeSeconds := totalMilliseconds / 1000 % 60
	milliseconds := totalMilliseconds % 1000
	return fmt.Sprintf("%02d:%02d:%02d,%03d", hours, minutes, wholeSeconds, milliseconds)
}

func escapeFilterPath(path string) string {
	replacer := strings.NewReplacer("\\", "\\\\", ":", "\\:", "'", "\\'")
	return replacer.Replace(path)
}

type InspectionIssue struct {
	Check    string `json:"check"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

type Inspection struct {
	Summary  string            `json:"summary"`
	Degraded bool              `json:"degraded"`
	Issues   []InspectionIssue `json:"issues"`
}

type ExpectedVideo struct {
	Width       int
	Height      int
	FPS         float64
	DurationSec float64
}

func InspectVideo(ctx context.Context, path string, expected ExpectedVideo, checks []string) (Inspection, error) {
	probe, err := ProbeFile(ctx, path)
	if err != nil {
		return Inspection{}, err
	}
	inspection := Inspection{Summary: "预览流与渲染快照一致，可正常解码。", Issues: []InspectionIssue{}}
	if probe.Width == nil || probe.Height == nil {
		inspection.Issues = append(inspection.Issues, InspectionIssue{"streams", "error", "缺少视频流"})
	} else if expected.Width > 0 && (*probe.Width != expected.Width || *probe.Height != expected.Height) {
		inspection.Issues = append(inspection.Issues, InspectionIssue{
			"streams", "error", fmt.Sprintf("分辨率 %dx%d 与快照 %dx%d 不一致", *probe.Width, *probe.Height, expected.Width, expected.Height),
		})
	}
	if expected.DurationSec > 0 && abs(probe.DurationSec-expected.DurationSec) > 0.25 {
		inspection.Issues = append(inspection.Issues, InspectionIssue{
			"streams", "warning", fmt.Sprintf("时长 %.3fs 与快照 %.3fs 不一致", probe.DurationSec, expected.DurationSec),
		})
	}
	if containsCheck(checks, "decode") || len(checks) == 0 {
		if _, err := RunCommand(ctx, "ffmpeg", "-v", "error", "-i", path, "-f", "null", "-"); err != nil {
			inspection.Issues = append(inspection.Issues, InspectionIssue{"decode", "error", err.Error()})
		}
	}
	for _, check := range []string{"black", "freeze", "silence", "loudness"} {
		if containsCheck(checks, check) {
			inspection.Degraded = true
			inspection.Issues = append(inspection.Issues, InspectionIssue{
				check, "info", "精简版仅完成流/解码/快照检查，此项保留为建议性降级。",
			})
		}
	}
	if len(inspection.Issues) > 0 {
		inspection.Summary = fmt.Sprintf("成片检查完成：发现 %d 项提示。", len(inspection.Issues))
	}
	return inspection, nil
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
