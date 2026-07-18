package agent

import (
	"context"
	"fmt"
	"math"
	"sort"

	"github.com/nanzhi84/Rushes/go/internal/media"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
)

func (service *Service) enrichTimelineOperations(
	ctx context.Context,
	draftID string,
	operations []map[string]any,
) ([]map[string]any, error) {
	assets, err := storage.ListDraftAssets(ctx, service.database.Read(), draftID)
	if err != nil {
		return nil, err
	}
	assetByID := make(map[string]storage.Asset, len(assets))
	audioAssetIDs := make([]string, 0, len(assets))
	for _, asset := range assets {
		assetByID[asset.ID] = asset
		hasAudio, _ := asset.Probe["has_audio"].(bool)
		if asset.Kind == "video" && hasAudio {
			audioAssetIDs = append(audioAssetIDs, asset.ID)
		}
	}
	sort.Strings(audioAssetIDs)

	result := make([]map[string]any, 0, len(operations))
	for _, original := range operations {
		operation := make(map[string]any, len(original)+2)
		for key, value := range original {
			operation[key] = value
		}
		switch stringValue(operation["kind"]) {
		case "insert_clip":
			asset, exists := assetByID[stringValue(operation["asset_id"])]
			if !exists {
				break
			}
			if stringValue(operation["asset_kind"]) == "" {
				operation["asset_kind"] = asset.Kind
			}
			if valueOr(stringValue(operation["track_id"]), "visual_base") != "visual_base" {
				break
			}
			if _, explicit := operation["include_original_audio"]; !explicit {
				hasAudio, _ := asset.Probe["has_audio"].(bool)
				operation["include_original_audio"] = asset.Kind == "video" && hasAudio
			}
		case "sync_original_audio":
			operation["audio_asset_ids"] = append([]string(nil), audioAssetIDs...)
		}
		result = append(result, operation)
	}
	return result, nil
}

func (service *Service) attachMissingBGMBeatGrids(
	ctx context.Context,
	draftID string,
	document *timeline.Document,
) (int, []string) {
	assets, err := storage.ListDraftAssets(ctx, service.database.Read(), draftID)
	if err != nil {
		return 0, []string{err.Error()}
	}
	assetByID := make(map[string]storage.Asset, len(assets))
	for _, asset := range assets {
		assetByID[asset.ID] = asset
	}
	gridByAsset := map[string]media.BeatGrid{}
	waveformByAsset := map[string]*media.WaveformEnvelope{}
	attached := 0
	warnings := []string{}
	for trackIndex := range document.Tracks {
		if document.Tracks[trackIndex].TrackID != "bgm" {
			continue
		}
		for clipIndex := range document.Tracks[trackIndex].Clips {
			clip := &document.Tracks[trackIndex].Clips[clipIndex]
			if clip.AssetID == "" || hasBeatGrid(clip.Effects) {
				continue
			}
			grid, cached := gridByAsset[clip.AssetID]
			waveform := waveformByAsset[clip.AssetID]
			if !cached {
				asset, exists := assetByID[clip.AssetID]
				if !exists || asset.Kind != "audio" || !asset.Usable {
					warnings = append(warnings, fmt.Sprintf("BGM %s 不是可分析的音频素材", clip.AssetID))
					continue
				}
				source, _, resolveErr := media.ResolveAssetSource(ctx, service.database, asset.ID)
				if resolveErr != nil {
					warnings = append(warnings, fmt.Sprintf("BGM %s 节拍源不可用: %v", clip.AssetID, resolveErr))
					continue
				}
				grid, err = media.AnalyzeBeatGrid(ctx, source, document.FPS, 4096)
				if err != nil {
					warnings = append(warnings, fmt.Sprintf("BGM %s 节拍分析失败: %v", clip.AssetID, err))
					continue
				}
				gridByAsset[clip.AssetID] = grid
				durationSec, _ := numericValue(asset.Probe["duration_sec"])
				waveform = optionalWaveformEnvelope(
					ctx,
					source,
					document.FPS,
					int(math.Round(durationSec*float64(document.FPS))),
				)
				waveformByAsset[clip.AssetID] = waveform
			}
			clip.Effects = append(clip.Effects, beatGridEffect(grid, waveform))
			attached++
		}
	}
	return attached, warnings
}
