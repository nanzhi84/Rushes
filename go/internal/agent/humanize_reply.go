package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
)

type replyShotLabel struct {
	name  string
	start int
	end   int
}

// humanizeFinalReplyReferences is the deterministic guard behind the prompt
// rule: internal stable IDs remain model/tool coordinates but are never the only
// name shown to the user.
func (service *Service) humanizeFinalReplyReferences(
	ctx context.Context,
	draftID, content string,
) string {
	assets, err := storage.ListDraftAssets(ctx, service.database.Read(), draftID)
	if err != nil {
		return content
	}
	replacements := map[string]string{}
	filenameByAsset := make(map[string]string, len(assets))
	for _, asset := range assets {
		filenameByAsset[asset.ID] = asset.Filename
		replacements[asset.ID] = "《" + asset.Filename + "》"
	}
	indexed, err := storage.ListReadyIndexedShotsForDraft(ctx, service.database.Read(), draftID)
	if err != nil {
		return content
	}
	shotsByAsset := map[string][]replyShotLabel{}
	for _, item := range indexed {
		name := strings.TrimSpace(item.Shot.SemanticName)
		if name == "" {
			continue
		}
		replacements[item.Shot.ShotID] = "「" + name + "」"
		shotsByAsset[item.AssetID] = append(shotsByAsset[item.AssetID], replyShotLabel{
			name: name, start: item.Shot.SourceStartFrame, end: item.Shot.SourceEndFrame,
		})
	}
	document, timelineErr := timeline.Latest(ctx, service.database, draftID)
	if timelineErr == nil {
		fps := document.FPS
		if fps <= 0 {
			fps = timeline.DefaultFPS
		}
		for _, track := range document.Tracks {
			for _, clip := range track.Clips {
				name := bestReplyShotName(
					shotsByAsset[clip.AssetID], clip.SourceStartFrame, clip.SourceEndFrame,
				)
				if name == "" {
					name = filenameByAsset[clip.AssetID]
				}
				if name == "" {
					continue
				}
				label := fmt.Sprintf(
					"「%s」%.2f–%.2f 秒", name,
					float64(clip.TimelineStartFrame)/float64(fps),
					float64(clip.TimelineEndFrame)/float64(fps),
				)
				replacements[clip.TimelineClipID] = fmt.Sprintf(
					"[%s](#timeline-clip=%s)", label, clip.TimelineClipID,
				)
			}
		}
	}
	keys := make([]string, 0, len(replacements))
	for key := range replacements {
		if key != "" && strings.Contains(content, key) {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(left, right int) bool { return len(keys[left]) > len(keys[right]) })
	for _, key := range keys {
		content = strings.ReplaceAll(content, key, replacements[key])
	}
	return content
}

func bestReplyShotName(shots []replyShotLabel, startFrame, endFrame int) string {
	bestName, bestOverlap := "", 0
	for _, shot := range shots {
		overlap := min(endFrame, shot.end) - max(startFrame, shot.start)
		if overlap > bestOverlap {
			bestName, bestOverlap = shot.name, overlap
		}
	}
	return bestName
}
