package worker

import (
	"context"
	"encoding/json"
	"errors"
	"math"

	"github.com/nanzhi84/Rushes/go/internal/media"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
)

func RegisterRender(registry *Registry, database *storage.DB) error {
	if err := registry.Register("render_preview", renderHandler(database, false)); err != nil {
		return err
	}
	return registry.Register("render_final", renderHandler(database, true))
}

func renderHandler(database *storage.DB, final bool) Handler {
	return func(ctx context.Context, job Job, report ProgressReporter) (map[string]any, error) {
		draftID := value(job.DraftID)
		if draftID == "" {
			return nil, errors.New("render job 缺少 draft_id")
		}
		timelineVersion, err := renderTimelineVersion(job.Payload["timeline_version"])
		if err != nil {
			return nil, err
		}
		document, err := timeline.Get(ctx, database, draftID, timelineVersion)
		if err != nil {
			return nil, err
		}
		if err := report(ctx, job, Progress(0.05)); err != nil {
			return nil, err
		}
		profile := media.PreviewProfile
		if final {
			profile = media.FinalProfile
		}
		orientation, _ := job.Payload["orientation"].(string)
		profile, err = media.ProfileForOrientation(profile, orientation)
		if err != nil {
			return nil, err
		}
		rendered, err := media.RenderTimeline(ctx, database, document, profile, func(progress media.Progress) {
			fraction := 0.1
			if renderedDuration := float64(document.DurationFrames) / float64(document.FPS); renderedDuration > 0 {
				fraction += min(progress.OutTime.Seconds()/renderedDuration, 1) * 0.8
			}
			_ = report(ctx, job, Progress(fraction))
		})
		if err != nil {
			return nil, err
		}
		artifactID := renderArtifactID(job)
		payload := map[string]any{
			"artifact_id": artifactID, "timeline_version": document.Version,
			"object_hash": rendered.Object.Hash, "object_size": rendered.Object.Size,
			"quality":      map[string]any{"profile": profile.Name, "orientation": orientation},
			"render_width": rendered.Width, "render_height": rendered.Height,
			"render_fps": rendered.FPS, "expected_duration_sec": rendered.DurationSec,
			"profile": profile.Name, "orientation": orientation,
		}
		if err := report(ctx, job, Progress(0.98)); err != nil {
			return nil, err
		}
		return payload, nil
	}
}

func renderTimelineVersion(raw any) (int, error) {
	var version int64
	switch value := raw.(type) {
	case int:
		version = int64(value)
	case int64:
		version = value
	case float64:
		if math.Trunc(value) != value || value > math.MaxInt64 || value < math.MinInt64 {
			return 0, errors.New("render job 的 timeline_version 必须是正整数")
		}
		version = int64(value)
	case json.Number:
		parsed, err := value.Int64()
		if err != nil {
			return 0, errors.New("render job 的 timeline_version 必须是正整数")
		}
		version = parsed
	default:
		return 0, errors.New("render job 缺少有效的 timeline_version")
	}
	if version <= 0 || int64(int(version)) != version {
		return 0, errors.New("render job 的 timeline_version 必须是正整数")
	}
	return int(version), nil
}
