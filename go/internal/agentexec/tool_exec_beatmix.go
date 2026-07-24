package agentexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/nanzhi84/Rushes/go/internal/media"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
	"github.com/nanzhi84/Rushes/go/internal/understanding"
)

// toolBuildBeatMix 是面向 Agent 的高层卡点工具。它从素材库中的完整源时长
// 重建主视觉，而不是继续消耗当前时间线上已经裁短的 source range。这样“重剪
// 到 48 秒并覆盖整首音乐”可以在一个原子提交里完成，不需要模型手工拼几十个
// compose/insert/trim 补丁。
func (exec *Executor) toolBuildBeatMix(
	ctx context.Context,
	draftID string,
	input rushestools.TimelineBeatRecutInput,
) (rushestools.ToolResult, error) {
	failed := func(message string, data map[string]any) (rushestools.ToolResult, error) {
		// 收口不变量：任何经 failed 返回的结构化失败都必带非空 recovery（#95 T5）。
		data = rushestools.EnsureFailureRecovery(data)
		return rushestools.ToolResult{Status: string(rushestools.StatusFailed), Observation: message, Data: data}, nil
	}
	current, err := timeline.Latest(ctx, exec.database, draftID)
	if errors.Is(err, storage.ErrNotFound) {
		current = timeline.Empty(draftID, 0)
	} else if err != nil {
		return rushestools.ToolResult{}, err
	}
	assets, err := storage.ListDraftAssets(ctx, exec.database.Read(), draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	assetByID := make(map[string]storage.Asset, len(assets))
	for _, asset := range assets {
		assetByID[asset.ID] = asset
	}

	bgmAssetID := strings.TrimSpace(input.BGMAssetID)
	if bgmAssetID == "" && input.BGMTimelineClipID != "" {
		for _, track := range current.Tracks {
			if track.TrackID != "bgm" {
				continue
			}
			for _, clip := range track.Clips {
				if clip.TimelineClipID == input.BGMTimelineClipID {
					bgmAssetID = clip.AssetID
					break
				}
			}
		}
	}
	if bgmAssetID == "" {
		candidates := make([]string, 0, 2)
		for _, asset := range assets {
			duration, _ := NumericValue(asset.Probe["duration_sec"])
			if asset.Kind == "audio" && asset.Usable &&
				understanding.ClassifyAudioRole(asset.Filename, duration) == "bgm" {
				candidates = append(candidates, asset.ID)
			}
		}
		if len(candidates) == 1 {
			bgmAssetID = candidates[0]
		} else {
			return failed("无法唯一确定 BGM，请传入 audio.analyze_beats 返回的 bgm_asset_id", map[string]any{
				"candidate_bgm_asset_ids": candidates,
				"recovery":                "先对目标音乐调用 audio.analyze_beats，再把其 asset_id 传给 timeline.recut_to_beats",
			})
		}
	}
	bgmAsset, exists := assetByID[bgmAssetID]
	if !exists || bgmAsset.Kind != "audio" || !bgmAsset.Usable {
		return failed("bgm_asset_id 必须是当前草稿中的可用音频素材", map[string]any{
			"bgm_asset_id": bgmAssetID,
		})
	}
	bgmDurationSec, _ := NumericValue(bgmAsset.Probe["duration_sec"])
	bgmAvailableFrames := int(math.Round(bgmDurationSec * float64(current.FPS)))
	if bgmAvailableFrames <= 0 {
		return failed("BGM 缺少可用时长，无法覆盖成片", map[string]any{"bgm_asset_id": bgmAssetID})
	}

	targetFrames := input.TargetDurationFrames
	if targetFrames == 0 {
		if input.CoverEntireBGM || input.BGMAssetID != "" {
			targetFrames = bgmAvailableFrames
		} else {
			targetFrames = min(current.DurationFrames, bgmAvailableFrames)
		}
	}
	if targetFrames <= 0 {
		return failed("target_duration_frames 必须为正数", nil)
	}
	if targetFrames > bgmAvailableFrames {
		return failed("目标成片超过 BGM 可用时长", map[string]any{
			"target_duration_frames": targetFrames,
			"bgm_available_frames":   bgmAvailableFrames,
			"recovery":               "将 target_duration_frames 调整到 BGM 可用范围，或设置 cover_entire_bgm=true",
		})
	}

	bgmSource, _, err := media.ResolveAssetSource(ctx, exec.database, bgmAsset.ID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	grid, err := media.AnalyzeBeatGrid(ctx, bgmSource, current.FPS, 4096)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	waveform := optionalWaveformEnvelope(
		ctx,
		bgmSource,
		current.FPS,
		bgmAvailableFrames,
	)

	type videoSource struct {
		asset          storage.Asset
		availableFrame int
		analysisRanges []BeatMixSourceRange
	}
	seenCurrentAssets := map[string]struct{}{}
	currentOrder := make([]string, 0)
	for _, track := range current.Tracks {
		if track.TrackID != "visual_base" {
			continue
		}
		clips := append([]timeline.Clip(nil), track.Clips...)
		sort.SliceStable(clips, func(i, j int) bool {
			return clips[i].TimelineStartFrame < clips[j].TimelineStartFrame
		})
		for _, clip := range clips {
			if _, seen := seenCurrentAssets[clip.AssetID]; seen {
				continue
			}
			seenCurrentAssets[clip.AssetID] = struct{}{}
			currentOrder = append(currentOrder, clip.AssetID)
		}
	}
	requestedIDs := append([]string(nil), input.VideoAssetIDs...)
	if len(requestedIDs) == 0 {
		requestedIDs = append(requestedIDs, currentOrder...)
		for _, asset := range assets {
			requestedIDs = append(requestedIDs, asset.ID)
		}
	}
	videoSources := make([]videoSource, 0, len(requestedIDs))
	seenVideo := map[string]struct{}{}
	invalidRequested := make([]string, 0)
	for _, assetID := range requestedIDs {
		if _, seen := seenVideo[assetID]; seen {
			continue
		}
		asset, found := assetByID[assetID]
		if !found || !asset.Usable || asset.Kind != "video" {
			if len(input.VideoAssetIDs) > 0 {
				invalidRequested = append(invalidRequested, assetID)
			}
			continue
		}
		durationSec, _ := NumericValue(asset.Probe["duration_sec"])
		available := int(math.Round(durationSec * float64(current.FPS)))
		if available <= 0 {
			continue
		}
		analysisRanges := exec.latestBeatMixSourceRanges(ctx, asset.ID, available)
		seenVideo[assetID] = struct{}{}
		videoSources = append(videoSources, videoSource{
			asset: asset, availableFrame: available,
			analysisRanges: analysisRanges,
		})
	}
	if len(invalidRequested) > 0 {
		return failed("video_asset_ids 包含不存在、不可用或非视觉素材", map[string]any{
			"invalid_asset_ids": invalidRequested,
		})
	}
	if len(videoSources) == 0 {
		return failed("当前草稿没有可用于卡点重剪的视频素材", nil)
	}
	shotByID := map[string]indexedShot{}
	if len(input.ShotIDs) > 0 {
		indexedShots, _, indexErr := exec.draftShotIndex(ctx, draftID, nil)
		if indexErr != nil {
			return rushestools.ToolResult{}, indexErr
		}
		for _, shot := range indexedShots {
			shotByID[shot.candidate.ShotID] = shot
		}
		invalidShotIDs := make([]string, 0)
		for _, shotID := range input.ShotIDs {
			shot, found := shotByID[shotID]
			_, assetAllowed := seenVideo[shot.candidate.AssetID]
			if !found || !assetAllowed {
				invalidShotIDs = append(invalidShotIDs, shotID)
			}
		}
		if len(invalidShotIDs) > 0 {
			return failed("shot_ids 包含不存在、已失效或不属于 video_asset_ids 的镜头", map[string]any{
				"invalid_shot_ids": invalidShotIDs,
				"recovery":         "重新调用 shot.search，并原样使用返回的 shot_id",
			})
		}
	}

	cutFrames := append([]int(nil), input.CutFrames...)
	if len(cutFrames) > 0 {
		if len(input.ShotIDs) > 0 && len(input.ShotIDs) != len(cutFrames) {
			return failed("shot_ids 必须与 cut_frames 一一对应", map[string]any{
				"shot_count": len(input.ShotIDs), "cut_count": len(cutFrames),
				"recovery": "按每个节拍片段所需时长重新检索镜头，或同时省略 shot_ids 与 cut_frames 让工具自动规划",
			})
		}
		previous := 0
		for index, cutFrame := range cutFrames {
			if cutFrame <= previous || cutFrame > targetFrames {
				return failed("cut_frames 必须严格递增且不超过目标时长", map[string]any{
					"cut_index": index + 1, "previous_frame": previous,
					"cut_frame": cutFrame, "target_duration_frames": targetFrames,
					"recovery": "修正累计结束帧，或省略 cut_frames 让工具自动规划",
				})
			}
			// 最后一帧允许是音乐/目标边界；其余切点必须来自真实节拍网格。
			if cutFrame != targetFrames && !ContainsFrame(grid.BeatFrames, cutFrame) {
				return failed("cut_frames 中存在不属于真实节拍网格的帧", map[string]any{
					"cut_index": index + 1, "cut_frame": cutFrame,
					"recovery": "使用 audio.analyze_beats 返回的 beat_frames，或省略 cut_frames",
				})
			}
			previous = cutFrame
		}
		if cutFrames[len(cutFrames)-1] != targetFrames {
			return failed("cut_frames 最后一项必须等于目标成片帧", map[string]any{
				"last_cut_frame":         cutFrames[len(cutFrames)-1],
				"target_duration_frames": targetFrames,
				"recovery":               "把最后一项设为 target_duration_frames，或省略 cut_frames",
			})
		}
		if input.UseAllVideoAssets && len(cutFrames) < len(videoSources) {
			return failed("要求使用全部视频素材时，cut_frames 数量不能少于可用视频素材数", map[string]any{
				"provided_cut_count": len(cutFrames),
				"video_asset_count":  len(videoSources),
				"recovery":           "增加切点或减少 video_asset_ids；额外切点可以从同一素材的其他不重叠镜头取得。",
			})
		}
	} else {
		if len(input.ShotIDs) > 0 {
			cutFrames = ChooseAllBeatMixCuts(
				grid.EveryFourBeatFrames, grid.BeatFrames, targetFrames, len(input.ShotIDs),
			)
		} else if input.UseAllVideoAssets {
			capacities := make([]int, 0, len(videoSources))
			for _, source := range videoSources {
				capacities = append(capacities, source.availableFrame)
			}
			cutFrames = ChooseCapacityAwareBeatMixCuts(
				grid.EveryFourBeatFrames, grid.BeatFrames, targetFrames, capacities,
			)
		} else {
			cutFrames = ChooseBeatMixCuts(
				grid.EveryFourBeatFrames, grid.BeatFrames, targetFrames, len(videoSources),
			)
		}
	}
	if len(cutFrames) == 0 {
		return failed("目标范围内没有可用节拍切点", map[string]any{
			"target_duration_frames": targetFrames,
		})
	}
	if len(input.ShotIDs) > 0 && len(cutFrames) != len(input.ShotIDs) {
		return failed("目标时长内的真实拍点不足，无法容纳全部已选镜头", map[string]any{
			"planned_cut_count": len(cutFrames), "shot_count": len(input.ShotIDs),
			"recovery": "减少 shot_ids，或提供来自 audio.analyze_beats 的显式 cut_frames",
		})
	}
	if input.UseAllVideoAssets && len(cutFrames) < len(videoSources) {
		return failed("目标时长内的真实拍点不足，无法让全部视频素材至少出现一次", map[string]any{
			"planned_cut_count": len(cutFrames),
			"video_asset_count": len(videoSources),
			"recovery":          "延长目标时长、减少视频素材，或取消 use_all_video_assets；不要降级为 compose_initial + 通用 patch。",
		})
	}
	selections := make([]timeline.Selection, 0, len(cutFrames))
	usedVideoIDs := make([]string, 0, len(cutFrames))
	usedAssets := map[string]struct{}{}
	usedSourceRanges := map[string][]BeatMixSourceRange{}
	sourceIndexByAsset := make(map[string]int, len(videoSources))
	for index, source := range videoSources {
		sourceIndexByAsset[source.asset.ID] = index
	}
	sourceRangeUsage := make([]map[string]any, 0, len(cutFrames))
	understandingRangesUsed := 0
	cursor := 0
	for segmentIndex, cutFrame := range cutFrames {
		duration := cutFrame - cursor
		selectedIndex, start := -1, 0
		selectedShotID := ""
		if len(input.ShotIDs) > 0 {
			selectedShotID = input.ShotIDs[segmentIndex]
			shot := shotByID[selectedShotID]
			selectedIndex = sourceIndexByAsset[shot.candidate.AssetID]
			var fits bool
			start, fits = ChooseUnusedBeatMixSourceStart(
				videoSources[selectedIndex].availableFrame, duration,
				[]BeatMixSourceRange{shot.rangeInfo}, usedSourceRanges[shot.candidate.AssetID], 0, true,
			)
			if !fits {
				return failed("所选镜头无法覆盖对应节拍片段，或其源区间已被重复使用", map[string]any{
					"shot_id": selectedShotID, "required_frames": duration,
					"shot_duration_frames": shot.candidate.DurationFrames,
					"recovery":             "用 shot.search 按该片段 min_duration_frames 重新检索，且不要重复传同一 shot_id",
				})
			}
		} else {
			// Prefer every requested asset once for visual diversity, then cycle back
			// through their remaining non-overlapping semantic ranges.
			for phase := 0; phase < 2 && selectedIndex < 0; phase++ {
				for step := 0; step < len(videoSources); step++ {
					index := (segmentIndex + step) % len(videoSources)
					source := videoSources[index]
					_, alreadyUsed := usedAssets[source.asset.ID]
					if phase == 0 && alreadyUsed || phase == 1 && !alreadyUsed {
						continue
					}
					candidateStart, fits := ChooseUnusedBeatMixSourceStart(
						source.availableFrame, duration, source.analysisRanges,
						usedSourceRanges[source.asset.ID], segmentIndex+index, false,
					)
					if fits {
						selectedIndex, start = index, candidateStart
						break
					}
				}
			}
		}
		if selectedIndex < 0 {
			return failed("没有足够长且未被使用的视频源区间覆盖某个节拍片段", map[string]any{
				"segment_start_frame": cursor,
				"segment_end_frame":   cutFrame,
				"required_frames":     duration,
				"recovery":            "减少切点密度、补充素材，或用 shot.search 选择更长的镜头区间",
			})
		}
		selected := videoSources[selectedIndex]
		usedRange := BeatMixSourceRange{StartFrame: start, EndFrame: start + duration}
		usedSourceRanges[selected.asset.ID] = append(usedSourceRanges[selected.asset.ID], usedRange)
		usedAssets[selected.asset.ID] = struct{}{}
		if SourceRangeContains(selected.analysisRanges, start, start+duration) {
			understandingRangesUsed++
		}
		selections = append(selections, timeline.Selection{
			AssetID: selected.asset.ID, AssetKind: selected.asset.Kind,
			SourceStartFrame: start, SourceEndFrame: start + duration,
			Role: "video", HasAudio: false,
		})
		usedVideoIDs = append(usedVideoIDs, selected.asset.ID)
		usage := map[string]any{
			"asset_id": selected.asset.ID, "source_start_frame": start,
			"source_end_frame": start + duration,
		}
		if selectedShotID != "" {
			usage["shot_id"] = selectedShotID
		}
		sourceRangeUsage = append(sourceRangeUsage, usage)
		cursor = cutFrame
	}
	unusedVideoIDs := make([]string, 0)
	for _, source := range videoSources {
		if _, used := usedAssets[source.asset.ID]; !used {
			unusedVideoIDs = append(unusedVideoIDs, source.asset.ID)
		}
	}
	if input.UseAllVideoAssets && len(unusedVideoIDs) > 0 {
		return failed("卡点规划没有覆盖全部请求视频素材，当前时间线未更新", map[string]any{
			"unused_video_asset_ids": unusedVideoIDs,
			"recovery":               "增加切点，让每个素材至少出现一次；额外切点允许复用同一素材的其他不重叠区间。",
		})
	}

	nextVersion, err := timeline.NextVersion(ctx, exec.database, draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	document, err := timeline.ComposeInitial(draftID, nextVersion, selections)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	var bgmTrack *timeline.Track
	var sfxTrack *timeline.Track
	for index := range document.Tracks {
		switch document.Tracks[index].TrackID {
		case "bgm":
			bgmTrack = &document.Tracks[index]
		case "sfx":
			sfxTrack = &document.Tracks[index]
		}
	}
	if bgmTrack == nil || sfxTrack == nil {
		return rushestools.ToolResult{}, errors.New("时间线缺少 bgm 或 sfx 轨")
	}
	bgmClipID := RandomID("bgm_beat")
	bgmTrack.Clips = append(bgmTrack.Clips, timeline.Clip{
		TimelineClipID: bgmClipID, TrackID: "bgm", AssetID: bgmAsset.ID,
		AssetKind: "audio", Role: "bgm", TimelineStartFrame: 0,
		TimelineEndFrame: targetFrames, SourceStartFrame: 0, SourceEndFrame: targetFrames,
		PlaybackRate: 1, Effects: []map[string]any{beatGridEffect(grid, waveform)},
	})

	sfxClipID := ""
	sfxStartFrame := 0
	if input.SFX != nil {
		sfx := input.SFX
		sfxAsset, found := assetByID[sfx.AssetID]
		if !found || sfxAsset.Kind != "audio" || !sfxAsset.Usable {
			return failed("SFX 素材必须是当前草稿中的可用音频", map[string]any{"asset_id": sfx.AssetID})
		}
		if sfx.DurationFrames <= 0 || sfx.DurationFrames > current.FPS*3 {
			return failed("SFX duration_frames 必须大于 0 且不超过 3 秒", nil)
		}
		sfxDurationSec, _ := NumericValue(sfxAsset.Probe["duration_sec"])
		available := int(math.Round(sfxDurationSec * float64(current.FPS)))
		if available > 0 && sfx.DurationFrames > available {
			return failed("SFX 请求时长超过素材时长", map[string]any{
				"requested_frames": sfx.DurationFrames, "available_frames": available,
			})
		}
		if sfx.StartFrame == nil {
			return failed("SFX start_frame 必须显式提供", map[string]any{
				"recovery": "根据 audio.analyze_beats 返回的波形、拍点和创作意图自主选择合法整数帧",
			})
		}
		sfxStartFrame = *sfx.StartFrame
		if sfxStartFrame < 0 || sfxStartFrame+sfx.DurationFrames > targetFrames {
			return failed("SFX start_frame 与 duration_frames 必须完整位于成片范围内", map[string]any{
				"start_frame": sfxStartFrame, "duration_frames": sfx.DurationFrames,
				"target_duration_frames": targetFrames,
			})
		}
		gain := -12.0
		if sfx.GainDB != nil {
			gain = *sfx.GainDB
		}
		if gain < -60 || gain > 12 {
			return failed("SFX gain_db 必须在 [-60,12] 范围内", nil)
		}
		sfxClipID = RandomID("sfx_beat")
		sfxTrack.Clips = append(sfxTrack.Clips, timeline.Clip{
			TimelineClipID: sfxClipID, TrackID: "sfx", AssetID: sfxAsset.ID,
			AssetKind: "audio", Role: "sfx", TimelineStartFrame: sfxStartFrame,
			TimelineEndFrame: sfxStartFrame + sfx.DurationFrames,
			SourceStartFrame: 0, SourceEndFrame: sfx.DurationFrames,
			PlaybackRate: 1, GainDB: gain,
		})
	}

	if report := timeline.Validate(document); !report.Valid {
		return failed("卡点重剪结果未通过时间线校验", map[string]any{
			"validation_report": report,
		})
	}
	semanticOperation := map[string]any{
		"kind": "recut_to_beats", "bgm_asset_id": bgmAsset.ID,
		"target_duration_frames": targetFrames, "video_asset_ids": usedVideoIDs,
		"cut_frames": cutFrames, "source_range_usage": sourceRangeUsage,
	}
	if len(input.ShotIDs) > 0 {
		semanticOperation["shot_ids"] = input.ShotIDs
	}
	if sfxClipID != "" {
		semanticOperation["sfx_asset_id"] = input.SFX.AssetID
		semanticOperation["sfx_start_frame"] = sfxStartFrame
	}
	result, err := exec.PersistTimeline(
		ctx, draftID, document, "recut_to_beats", []map[string]any{semanticOperation},
	)
	if err != nil || result.Status != "succeeded" {
		return result, err
	}
	result.Observation = fmt.Sprintf(
		"已按 %.2f BPM 原子重建卡点混剪：%d 个视频片段、%d 帧；BGM 覆盖 0-%d 帧，SFX 独立分轨。",
		grid.BPM, len(selections), targetFrames, targetFrames,
	)
	result.Data["bpm"] = grid.BPM
	result.Data["cut_frames"] = cutFrames
	result.Data["strong_beat_frames"] = grid.StrongBeatFrames
	result.Data["downbeat_frames"] = grid.DownbeatFrames
	result.Data["bar_phase"] = grid.BarPhase
	result.Data["duration_frames"] = targetFrames
	result.Data["video_asset_ids"] = usedVideoIDs
	result.Data["shot_ids"] = input.ShotIDs
	result.Data["source_range_usage"] = sourceRangeUsage
	result.Data["unused_video_asset_ids"] = unusedVideoIDs
	result.Data["used_all_video_assets"] = len(unusedVideoIDs) == 0
	result.Data["understanding_source_ranges_used"] = understandingRangesUsed
	result.Data["bgm_timeline_clip_id"] = bgmClipID
	result.Data["sfx_timeline_clip_id"] = sfxClipID
	result.Data["sfx_start_frame"] = sfxStartFrame
	if waveform != nil {
		result.Data["waveform"] = waveformToolValue(*waveform)
	}
	return result, nil
}

// latestBeatMixSourceRanges 读取质量最完整的理解摘要中的源帧证据。摘要缺失、损坏或
// 区间不合法时静默回退到完整素材，避免让理解服务的降级阻塞高层剪辑工具。
func (exec *Executor) latestBeatMixSourceRanges(
	ctx context.Context,
	assetID string,
	availableFrames int,
) []BeatMixSourceRange {
	raw, err := storage.BestMaterialSummary(ctx, exec.database.Read(), assetID)
	if err != nil {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var summary understanding.Summary
	if err := json.Unmarshal(encoded, &summary); err != nil {
		return nil
	}
	return BeatMixRangesFromUnderstanding(summary.Segments, availableFrames)
}

func BeatMixRangesFromUnderstanding(
	segments []understanding.Segment,
	availableFrames int,
) []BeatMixSourceRange {
	ranges := make([]BeatMixSourceRange, 0, len(segments)*2)
	var continuous *BeatMixSourceRange
	flushContinuous := func() {
		if continuous == nil {
			return
		}
		ranges = append(ranges, *continuous)
		continuous = nil
	}
	for _, segment := range segments {
		if segment.Quality == "unusable" {
			flushContinuous()
			continue
		}
		start := max(0, segment.SourceStartFrame)
		end := min(availableFrames, segment.SourceEndFrame)
		if end <= start {
			continue
		}
		penalty := understandingSegmentQualityPenalty(segment)
		ranges = append(ranges, BeatMixSourceRange{StartFrame: start, EndFrame: end, QualityPenalty: penalty})
		// analysis_window 是同一长镜头内的理解采样边界。卡点片段可以跨越
		// 相邻窗口，但不能跨越 VLM 已确认的真实切镜或不可用区间。
		if continuous != nil && start <= continuous.EndFrame+1 && segment.BoundaryKind == "analysis_window" {
			continuous.EndFrame = max(continuous.EndFrame, end)
			continuous.QualityPenalty = max(continuous.QualityPenalty, penalty)
			continue
		}
		flushContinuous()
		continuous = &BeatMixSourceRange{StartFrame: start, EndFrame: end, QualityPenalty: penalty}
	}
	flushContinuous()
	sort.SliceStable(ranges, func(i, j int) bool {
		if ranges[i].QualityPenalty != ranges[j].QualityPenalty {
			return ranges[i].QualityPenalty < ranges[j].QualityPenalty
		}
		if ranges[i].StartFrame == ranges[j].StartFrame {
			return ranges[i].EndFrame < ranges[j].EndFrame
		}
		return ranges[i].StartFrame < ranges[j].StartFrame
	})
	return ranges
}

func understandingSegmentQualityPenalty(segment understanding.Segment) float64 {
	penalty := 0.0
	if segment.OverexposedRatio != nil && *segment.OverexposedRatio > 0.10 {
		penalty += min(0.12, (*segment.OverexposedRatio-0.10)*0.15)
	}
	if segment.SharpnessScore != nil && *segment.SharpnessScore < 100 {
		penalty += min(0.10, (100-*segment.SharpnessScore)/1000)
	}
	return math.Round(penalty*10000) / 10000
}

func ChooseUnusedBeatMixSourceStart(
	availableFrames int,
	durationFrames int,
	ranges []BeatMixSourceRange,
	used []BeatMixSourceRange,
	rangeOffset int,
	strictRanges bool,
) (int, bool) {
	if durationFrames <= 0 || availableFrames < durationFrames {
		return 0, false
	}
	candidates := make([]BeatMixSourceRange, 0, max(1, len(ranges)))
	for _, sourceRange := range ranges {
		sourceRange.StartFrame = max(0, sourceRange.StartFrame)
		sourceRange.EndFrame = min(availableFrames, sourceRange.EndFrame)
		if sourceRange.EndFrame-sourceRange.StartFrame >= durationFrames {
			candidates = append(candidates, sourceRange)
		}
	}
	if len(candidates) == 0 && !strictRanges {
		candidates = append(candidates, BeatMixSourceRange{StartFrame: 0, EndFrame: availableFrames})
	}
	if len(candidates) == 0 {
		return 0, false
	}
	sort.SliceStable(candidates, func(left, right int) bool {
		if candidates[left].QualityPenalty != candidates[right].QualityPenalty {
			return candidates[left].QualityPenalty < candidates[right].QualityPenalty
		}
		return candidates[left].StartFrame < candidates[right].StartFrame
	})
	sortedUsed := append([]BeatMixSourceRange(nil), used...)
	sort.SliceStable(sortedUsed, func(i, j int) bool {
		return sortedUsed[i].StartFrame < sortedUsed[j].StartFrame
	})
	if rangeOffset < 0 {
		rangeOffset = -rangeOffset
	}
	preferredCount := 1
	for preferredCount < len(candidates) && candidates[preferredCount].QualityPenalty == candidates[0].QualityPenalty {
		preferredCount++
	}
	ordered := make([]BeatMixSourceRange, 0, len(candidates))
	for step := 0; step < preferredCount; step++ {
		ordered = append(ordered, candidates[(rangeOffset+step)%preferredCount])
	}
	ordered = append(ordered, candidates[preferredCount:]...)
	for _, sourceRange := range ordered {
		cursor := sourceRange.StartFrame
		for _, occupied := range sortedUsed {
			if occupied.EndFrame <= cursor || occupied.StartFrame >= sourceRange.EndFrame {
				continue
			}
			if occupied.StartFrame-cursor >= durationFrames {
				return cursor, true
			}
			cursor = max(cursor, occupied.EndFrame)
			if cursor+durationFrames > sourceRange.EndFrame {
				break
			}
		}
		if cursor+durationFrames <= sourceRange.EndFrame {
			return cursor, true
		}
	}
	if !strictRanges && (len(candidates) != 1 || candidates[0].StartFrame != 0 || candidates[0].EndFrame != availableFrames) {
		return ChooseUnusedBeatMixSourceStart(
			availableFrames, durationFrames,
			[]BeatMixSourceRange{{StartFrame: 0, EndFrame: availableFrames}},
			used, rangeOffset, true,
		)
	}
	return 0, false
}
