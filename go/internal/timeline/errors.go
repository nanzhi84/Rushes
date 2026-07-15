package timeline

import "fmt"

type SemanticErrorKind string

const (
	SemanticClipNotFound SemanticErrorKind = "clip_not_found"
	SemanticFrameRange   SemanticErrorKind = "frame_out_of_range"
	SemanticTrackLocked  SemanticErrorKind = "track_locked"
)

// SemanticError carries the current-document facts needed for one-round model repair.
// It is intentionally independent from the tools package so timeline remains the lower layer.
type SemanticError struct {
	Kind               SemanticErrorKind
	ClipID             string
	TrackID            string
	ProvidedFrame      int
	TimelineStartFrame int
	TimelineEndFrame   int
	SourceStartFrame   int
	SourceEndFrame     int
	Message            string
}

func trackLockedError(trackID string) error {
	return &SemanticError{Kind: SemanticTrackLocked, TrackID: trackID}
}

func clipFrameRangeError(clip Clip, providedFrame int, message string) error {
	return &SemanticError{
		Kind: SemanticFrameRange, ClipID: clip.TimelineClipID, ProvidedFrame: providedFrame,
		TimelineStartFrame: clip.TimelineStartFrame, TimelineEndFrame: clip.TimelineEndFrame,
		SourceStartFrame: clip.SourceStartFrame, SourceEndFrame: clip.SourceEndFrame,
		Message: message,
	}
}

func (err *SemanticError) Error() string {
	if err == nil {
		return ""
	}
	if err.Message != "" {
		return err.Message
	}
	switch err.Kind {
	case SemanticClipNotFound:
		return fmt.Sprintf("clip 不存在: %s", err.ClipID)
	case SemanticTrackLocked:
		return fmt.Sprintf("轨道 %s 已锁定", err.TrackID)
	case SemanticFrameRange:
		return fmt.Sprintf(
			"帧 %d 必须位于片段 %s 的时间线范围 (%d,%d) 内",
			err.ProvidedFrame, err.ClipID, err.TimelineStartFrame, err.TimelineEndFrame,
		)
	default:
		return "时间线语义约束失败"
	}
}
