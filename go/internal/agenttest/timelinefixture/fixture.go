package timelinefixture

import (
	"errors"
	"fmt"

	"github.com/nanzhi84/Rushes/go/internal/timeline"
)

// Selection describes one primary visual clip in a test-only timeline.
type Selection struct {
	AssetID          string
	AssetKind        string
	SourceStartFrame int
	SourceEndFrame   int
	Role             string
	HasAudio         bool
}

// Compose builds a deterministic multi-clip document for tests outside the
// timeline package. Timeline's own tests keep a local helper to avoid a cycle.
func Compose(draftID string, version int, selections []Selection) (timeline.Document, error) {
	if draftID == "" || version < 1 || len(selections) == 0 {
		return timeline.Document{}, errors.New("timeline fixture 参数无效")
	}
	document := timeline.Empty(draftID, version)
	primary := &document.Tracks[0]
	originalAudio := &document.Tracks[2]
	cursor := 0
	for index, selection := range selections {
		if selection.AssetID == "" ||
			selection.SourceStartFrame < 0 ||
			selection.SourceEndFrame <= selection.SourceStartFrame {
			return timeline.Document{}, fmt.Errorf("clip %d 源范围无效", index)
		}
		if selection.AssetKind != "" &&
			selection.AssetKind != "video" &&
			selection.AssetKind != "image" {
			return timeline.Document{}, fmt.Errorf(
				"clip %d 素材类型 %s 不能放入主视觉轨，仅支持 video/image",
				index,
				selection.AssetKind,
			)
		}
		duration := selection.SourceEndFrame - selection.SourceStartFrame
		clipID := fmt.Sprintf("clip_v%d_%03d", version, index+1)
		parentBlockID := fmt.Sprintf("block_%03d", index+1)
		primary.Clips = append(primary.Clips, timeline.Clip{
			TimelineClipID:     clipID,
			TrackID:            primary.TrackID,
			AssetID:            selection.AssetID,
			AssetKind:          selection.AssetKind,
			Role:               selection.Role,
			TimelineStartFrame: cursor,
			TimelineEndFrame:   cursor + duration,
			SourceStartFrame:   selection.SourceStartFrame,
			SourceEndFrame:     selection.SourceEndFrame,
			PlaybackRate:       1,
			ParentBlockID:      parentBlockID,
			Linked:             selection.HasAudio,
		})
		if selection.HasAudio {
			originalAudio.Clips = append(originalAudio.Clips, timeline.Clip{
				TimelineClipID:     clipID + "_audio",
				TrackID:            originalAudio.TrackID,
				AssetID:            selection.AssetID,
				AssetKind:          selection.AssetKind,
				Role:               "original_audio",
				TimelineStartFrame: cursor,
				TimelineEndFrame:   cursor + duration,
				SourceStartFrame:   selection.SourceStartFrame,
				SourceEndFrame:     selection.SourceEndFrame,
				PlaybackRate:       1,
				ParentBlockID:      parentBlockID,
				Linked:             true,
			})
		}
		cursor += duration
	}
	document.DurationFrames = cursor
	return document, nil
}
