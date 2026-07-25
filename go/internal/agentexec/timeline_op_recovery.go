package agentexec

import (
	"errors"

	"github.com/nanzhi84/Rushes/go/internal/timeline"
)

func TimelineOpFieldError(err error) (*timeline.OpFieldError, bool) {
	var fieldErr *timeline.OpFieldError
	if errors.As(err, &fieldErr) {
		return fieldErr, true
	}
	return nil, false
}

func timelineClipIDsByTrack(document timeline.Document) map[string][]string {
	result := make(map[string][]string, len(document.Tracks))
	for _, track := range document.Tracks {
		ids := make([]string, 0, len(track.Clips))
		for _, clip := range track.Clips {
			ids = append(ids, clip.TimelineClipID)
		}
		result[track.TrackID] = ids
	}
	return result
}
