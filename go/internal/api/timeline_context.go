package api

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
)

type timelineShotLabel struct {
	assetID      string
	filename     string
	shotID       string
	semanticName string
	startFrame   int
	endFrame     int
}

type timelineLabelLookup struct {
	filenameByAsset map[string]string
	shotsByAsset    map[string][]timelineShotLabel
}

func loadTimelineLabelLookup(
	ctx context.Context,
	database *storage.DB,
	draftID string,
) (timelineLabelLookup, error) {
	assets, err := storage.ListDraftAssets(ctx, database.Read(), draftID)
	if err != nil {
		return timelineLabelLookup{}, err
	}
	lookup := timelineLabelLookup{
		filenameByAsset: make(map[string]string, len(assets)),
		shotsByAsset:    map[string][]timelineShotLabel{},
	}
	for _, asset := range assets {
		lookup.filenameByAsset[asset.ID] = asset.Filename
	}
	shots, err := storage.ListReadyIndexedShotsForDraft(ctx, database.Read(), draftID)
	if err != nil {
		return timelineLabelLookup{}, err
	}
	for _, item := range shots {
		lookup.shotsByAsset[item.AssetID] = append(lookup.shotsByAsset[item.AssetID], timelineShotLabel{
			assetID: item.AssetID, filename: item.Filename, shotID: item.Shot.ShotID,
			semanticName: strings.TrimSpace(item.Shot.SemanticName),
			startFrame:   item.Shot.SourceStartFrame, endFrame: item.Shot.SourceEndFrame,
		})
	}
	return lookup, nil
}

func (lookup timelineLabelLookup) bestShot(
	assetID string,
	startFrame, endFrame int,
) *timelineShotLabel {
	bestIndex, bestOverlap := -1, 0
	for index, shot := range lookup.shotsByAsset[assetID] {
		overlap := min(endFrame, shot.endFrame) - max(startFrame, shot.startFrame)
		if overlap > bestOverlap {
			bestIndex, bestOverlap = index, overlap
		}
	}
	if bestIndex < 0 {
		return nil
	}
	value := lookup.shotsByAsset[assetID][bestIndex]
	return &value
}

func annotateTimelineClipLabels(documentMap map[string]any, lookup timelineLabelLookup) {
	tracks, _ := documentMap["tracks"].([]any)
	for _, rawTrack := range tracks {
		track, _ := rawTrack.(map[string]any)
		clips, _ := track["clips"].([]any)
		for _, rawClip := range clips {
			clip, _ := rawClip.(map[string]any)
			assetID, _ := clip["asset_id"].(string)
			if assetID == "" {
				continue
			}
			if filename := lookup.filenameByAsset[assetID]; filename != "" {
				clip["asset_filename"] = filename
			}
			startFrame, startOK := intFromJSONNumber(clip["source_start_frame"])
			if _, present := clip["source_start_frame"]; !present {
				startFrame, startOK = 0, true
			}
			endFrame, endOK := intFromJSONNumber(clip["source_end_frame"])
			if !startOK || !endOK {
				continue
			}
			shot := lookup.bestShot(assetID, startFrame, endFrame)
			if shot == nil {
				continue
			}
			clip["shot_id"] = shot.shotID
			if shot.semanticName != "" {
				clip["semantic_name"] = shot.semanticName
			}
		}
	}
}

func intFromJSONNumber(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case float64:
		return int(typed), true
	default:
		return 0, false
	}
}

func resolveTimelineClipContext(
	ctx context.Context,
	database *storage.DB,
	draftID, timelineID string,
	timelineVersion int,
	clipID string,
) (map[string]any, error) {
	document, err := timeline.Latest(ctx, database, draftID)
	if err != nil {
		return nil, err
	}
	if document.TimelineID != timelineID || document.Version != timelineVersion {
		return nil, fmt.Errorf("引用的时间线版本已过期")
	}
	lookup, err := loadTimelineLabelLookup(ctx, database, draftID)
	if err != nil {
		return nil, err
	}
	for _, track := range document.Tracks {
		for _, clip := range track.Clips {
			if clip.TimelineClipID != clipID {
				continue
			}
			ref := map[string]any{
				"kind": "timeline_clip", "timeline_clip_id": clip.TimelineClipID,
				"timeline_id": document.TimelineID, "timeline_version": document.Version,
				"timeline_fps": document.FPS,
				"track_id":     track.TrackID, "timeline_start_frame": clip.TimelineStartFrame,
				"timeline_end_frame": clip.TimelineEndFrame,
				"asset_id":           clip.AssetID, "asset_filename": lookup.filenameByAsset[clip.AssetID],
				"source_start_frame": clip.SourceStartFrame, "source_end_frame": clip.SourceEndFrame,
			}
			if shot := lookup.bestShot(clip.AssetID, clip.SourceStartFrame, clip.SourceEndFrame); shot != nil {
				ref["shot_id"] = shot.shotID
				ref["semantic_name"] = shot.semanticName
			} else {
				ref["shot_id"] = ""
				ref["semantic_name"] = ""
			}
			return ref, nil
		}
	}
	return nil, errors.New("引用的时间线片段不存在")
}
