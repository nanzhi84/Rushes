package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/media"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
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
		document, err := timeline.Latest(ctx, database, draftID)
		if err != nil {
			return nil, err
		}
		if err := report(ctx, job, 0.05); err != nil {
			return nil, err
		}
		profile := media.PreviewProfile
		if final {
			profile = media.FinalProfile
		}
		rendered, err := media.RenderTimeline(ctx, database, document, profile, func(progress media.Progress) {
			fraction := 0.1
			if renderedDuration := float64(document.DurationFrames) / float64(document.FPS); renderedDuration > 0 {
				fraction += min(progress.OutTime.Seconds()/renderedDuration, 1) * 0.8
			}
			_ = report(ctx, job, fraction)
		})
		if err != nil {
			return nil, err
		}
		artifactID := fmt.Sprintf("preview_%d", time.Now().UnixNano())
		eventType := "PreviewRendered"
		if final {
			artifactID = fmt.Sprintf("export_%d", time.Now().UnixNano())
			eventType = "ExportCompleted"
		}
		payload := map[string]any{
			"artifact_id": artifactID, "timeline_version": document.Version,
			"object_hash": rendered.Object.Hash, "object_size": rendered.Object.Size,
			"quality":      map[string]any{"profile": profile.Name},
			"render_width": rendered.Width, "render_height": rendered.Height,
			"render_fps": rendered.FPS, "expected_duration_sec": rendered.DurationSec,
		}
		result, err := reducer.Apply(ctx, database, []contracts.Event{{
			Type: eventType, DraftID: draftID, Payload: payload,
		}}, reducer.Options{Actor: contracts.ActorJob})
		if err != nil || result.Status != reducer.StatusApplied {
			return nil, errors.Join(err, fmt.Errorf("render reducer status: %s", result.Status))
		}
		if err := report(ctx, job, 0.98); err != nil {
			return nil, err
		}
		return map[string]any{
			"artifact_id": artifactID, "timeline_version": document.Version,
			"object_hash": rendered.Object.Hash, "profile": profile.Name,
		}, nil
	}
}
