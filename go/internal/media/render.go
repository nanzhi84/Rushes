package media

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
)

type RenderProfile struct {
	Name   string
	Width  int
	Height int
	CRF    int
}

var (
	PreviewProfile = RenderProfile{Name: "preview", Width: 360, Height: 640, CRF: 30}
	FinalProfile   = RenderProfile{Name: "final", Width: 720, Height: 1280, CRF: 20}
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
	args := []string{"-y"}
	filters := make([]string, 0, len(primary.Clips)+1)
	labels := strings.Builder{}
	for index, clip := range primary.Clips {
		source, kind, err := ResolveAssetSource(ctx, database, clip.AssetID)
		if err != nil {
			return RenderResult{}, fmt.Errorf("解析 clip %s: %w", clip.TimelineClipID, err)
		}
		sourceStart := float64(clip.SourceStart) / float64(document.FPS)
		timelineDuration := float64(clip.TimelineEnd-clip.TimelineStart) / float64(document.FPS)
		if kind == "image" {
			args = append(args, "-loop", "1", "-t", formatSeconds(timelineDuration), "-i", source)
		} else {
			args = append(args, "-ss", formatSeconds(sourceStart), "-t", formatSeconds(max(timelineDuration, 0.04)), "-i", source)
		}
		filter := fmt.Sprintf(
			"[%d:v]scale=%d:%d:force_original_aspect_ratio=decrease,"+
				"pad=%d:%d:(ow-iw)/2:(oh-ih)/2:black,setsar=1,fps=%d,"+
				"trim=duration=%s,setpts=PTS-STARTPTS[v%d]",
			index, profile.Width, profile.Height, profile.Width, profile.Height,
			document.FPS, formatSeconds(timelineDuration), index,
		)
		filters = append(filters, filter)
		_, _ = fmt.Fprintf(&labels, "[v%d]", index)
	}
	filters = append(filters, fmt.Sprintf("%sconcat=n=%d:v=1:a=0[outv]", labels.String(), len(primary.Clips)))
	output, err := os.CreateTemp(database.Paths.Temporary, "render-*.mp4")
	if err != nil {
		return RenderResult{}, err
	}
	outputPath := output.Name()
	_ = output.Close()
	defer func() { _ = os.Remove(outputPath) }()
	args = append(args,
		"-filter_complex", strings.Join(filters, ";"), "-map", "[outv]", "-an",
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
