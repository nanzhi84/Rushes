package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/media"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
	"github.com/nanzhi84/Rushes/go/internal/understanding"
)

func (service *Service) ExecuteTool(ctx context.Context, name string, input any) (any, error) {
	draftID, err := rushestools.DraftID(ctx)
	if err != nil {
		return nil, err
	}
	switch name {
	case "asset.import_local_file":
		return nil, errors.New("本地导入仅由已确认的 REST 文件选择流程执行")
	case "asset.list_assets":
		return service.toolListAssets(ctx, draftID, input.(rushestools.AssetListInput))
	case "understand.materials":
		return service.toolUnderstand(ctx, draftID, input.(rushestools.UnderstandInput))
	case "media.search_shots":
		return service.toolSearchShots(ctx, draftID, input.(rushestools.ShotSearchInput))
	case "audio.analyze_beats":
		return service.toolAnalyzeAudioBeats(ctx, draftID, input.(rushestools.AudioBeatAnalysisInput))
	case "audio.analyze_speech_pauses":
		return service.toolAnalyzeSpeechPauses(ctx, draftID, input.(rushestools.SpeechPauseAnalysisInput))
	case "speech.inspect":
		return service.toolInspectSpeech(ctx, draftID, input.(rushestools.SpeechInspectInput))
	case "interaction.ask_user":
		return service.toolAskUser(ctx, draftID, input.(rushestools.AskUserInput), nil)
	case "interaction.confirm_action":
		confirmation := input.(rushestools.ConfirmActionInput)
		return service.toolAskUser(ctx, draftID, rushestools.AskUserInput{
			Question: confirmation.Question,
			Options: []rushestools.DecisionOptionInput{
				{OptionID: "confirm", Label: "确认"}, {OptionID: "cancel", Label: "取消"},
			},
		}, map[string]any{"tool_name": confirmation.ToolName, "arguments": confirmation.Arguments})
	case "decision.answer":
		return service.toolDecisionAnswer(ctx, draftID, input.(rushestools.DecisionAnswerInput))
	case "timeline.compose_initial":
		return service.toolComposeInitial(ctx, draftID, input.(rushestools.ComposeInitialInput))
	case "timeline.apply_patch":
		return service.toolApplyPatch(ctx, draftID, input.(rushestools.TimelinePatchInput))
	case "timeline.apply_patches":
		return service.toolApplyPatches(ctx, draftID, input.(rushestools.TimelinePatchBatchInput))
	case "timeline.recut_to_beats":
		return service.toolRecutToBeats(ctx, draftID, input.(rushestools.TimelineBeatRecutInput))
	case "timeline.edit_talking_head":
		return service.toolEditTalkingHead(ctx, draftID, input.(rushestools.TalkingHeadEditInput))
	case "timeline.validate":
		return service.toolValidateTimeline(ctx, draftID)
	case "timeline.inspect":
		return service.toolInspectTimeline(ctx, draftID, input.(rushestools.TimelineInspectInput))
	case "render.preview":
		return service.toolEnqueueRender(ctx, draftID, "render_preview")
	case "render.final_mp4":
		return service.toolEnqueueRender(ctx, draftID, "render_final")
	case "render.status":
		return service.toolRenderStatus(ctx, draftID)
	case "render.inspect_preview":
		return service.toolInspectPreview(ctx, draftID, input.(rushestools.RenderInspectInput))
	default:
		return nil, fmt.Errorf("工具未注册执行器: %s", name)
	}
}

func (service *Service) toolAnalyzeAudioBeats(
	ctx context.Context,
	draftID string,
	input rushestools.AudioBeatAnalysisInput,
) (rushestools.AudioBeatAnalysisResult, error) {
	if input.AssetID == "" {
		return rushestools.AudioBeatAnalysisResult{}, errors.New("audio.analyze_beats 缺少 asset_id")
	}
	if input.WaveformPoints != 0 &&
		(input.WaveformPoints < 16 || input.WaveformPoints > media.MaxWaveformPoints) {
		return rushestools.AudioBeatAnalysisResult{}, fmt.Errorf(
			"waveform_points 必须在 [16,%d] 范围内",
			media.MaxWaveformPoints,
		)
	}
	assets, err := storage.ListDraftAssets(ctx, service.database.Read(), draftID)
	if err != nil {
		return rushestools.AudioBeatAnalysisResult{}, err
	}
	var selected *storage.Asset
	for index := range assets {
		if assets[index].ID == input.AssetID {
			selected = &assets[index]
			break
		}
	}
	if selected == nil {
		return rushestools.AudioBeatAnalysisResult{}, errors.New("音频素材不属于当前草稿")
	}
	if (selected.Kind != "audio" && selected.Kind != "video") || !selected.Usable {
		return rushestools.AudioBeatAnalysisResult{}, errors.New("节拍分析只支持当前草稿中可用的音频或带音轨视频素材")
	}
	source, _, err := media.ResolveAssetSource(ctx, service.database, selected.ID)
	if err != nil {
		return rushestools.AudioBeatAnalysisResult{}, err
	}
	probe, err := media.ProbeFile(ctx, source)
	if err != nil {
		return rushestools.AudioBeatAnalysisResult{}, err
	}
	if !probe.HasAudio {
		return rushestools.AudioBeatAnalysisResult{}, errors.New("素材没有可分析的音轨")
	}
	grid, err := media.AnalyzeBeatGrid(ctx, source, timeline.DefaultFPS, input.MaxBeats)
	if err != nil {
		return rushestools.AudioBeatAnalysisResult{}, err
	}
	durationFrames := int(math.Round(probe.DurationSec * timeline.DefaultFPS))
	waveform, err := media.AnalyzeWaveformEnvelope(
		ctx,
		source,
		timeline.DefaultFPS,
		durationFrames,
		input.WaveformPoints,
	)
	if err != nil {
		return rushestools.AudioBeatAnalysisResult{}, err
	}
	return rushestools.AudioBeatAnalysisResult{
		AssetID: selected.ID, BPM: grid.BPM, TimelineFPS: timeline.DefaultFPS,
		DurationFrames: durationFrames,
		BeatFrames:     grid.BeatFrames, StrongBeatFrames: grid.StrongBeatFrames,
		DownbeatFrames: grid.DownbeatFrames, EveryTwoBeatFrames: grid.EveryTwoBeatFrames,
		EveryFourBeatFrames: grid.EveryFourBeatFrames, AnalysisMethod: grid.AnalysisMethod,
		BarPhase: grid.BarPhase, Truncated: grid.Truncated,
		PhaseNote: "强拍来自频谱通量瞬态；每 4 拍网格以强拍贴合度推断 4/4 小节相位，仍可由剪辑者微调。",
		Waveform:  waveformToolValue(waveform),
	}, nil
}

func (service *Service) toolAnalyzeSpeechPauses(
	ctx context.Context,
	draftID string,
	input rushestools.SpeechPauseAnalysisInput,
) (rushestools.SpeechPauseAnalysisResult, error) {
	assetID := strings.TrimSpace(input.AssetID)
	var timelineClip *timeline.Clip
	if input.TimelineClipID != "" {
		current, err := timeline.Latest(ctx, service.database, draftID)
		if err != nil {
			return rushestools.SpeechPauseAnalysisResult{}, err
		}
		for trackIndex := range current.Tracks {
			for clipIndex := range current.Tracks[trackIndex].Clips {
				candidate := &current.Tracks[trackIndex].Clips[clipIndex]
				if candidate.TimelineClipID == input.TimelineClipID {
					timelineClip = candidate
					break
				}
			}
			if timelineClip != nil {
				break
			}
		}
		if timelineClip == nil || timelineClip.AssetID == "" {
			return rushestools.SpeechPauseAnalysisResult{}, errors.New("timeline_clip_id 不存在或不是素材片段")
		}
		if assetID != "" && assetID != timelineClip.AssetID {
			return rushestools.SpeechPauseAnalysisResult{}, errors.New("asset_id 与 timeline_clip_id 指向的素材不一致")
		}
		assetID = timelineClip.AssetID
	}
	if assetID == "" {
		return rushestools.SpeechPauseAnalysisResult{}, errors.New("audio.analyze_speech_pauses 至少需要 asset_id 或 timeline_clip_id")
	}
	assets, err := storage.ListDraftAssets(ctx, service.database.Read(), draftID)
	if err != nil {
		return rushestools.SpeechPauseAnalysisResult{}, err
	}
	var selected *storage.Asset
	for index := range assets {
		if assets[index].ID == assetID {
			selected = &assets[index]
			break
		}
	}
	if selected == nil || !selected.Usable || selected.Kind != "audio" && selected.Kind != "video" {
		return rushestools.SpeechPauseAnalysisResult{}, errors.New("气口分析只支持当前草稿中可用的音频或视频素材")
	}
	source, _, err := media.ResolveAssetSource(ctx, service.database, selected.ID)
	if err != nil {
		return rushestools.SpeechPauseAnalysisResult{}, err
	}
	analysis, err := media.AnalyzeSpeechPauses(ctx, source, timeline.DefaultFPS, media.SpeechPauseOptions{
		ThresholdDB: input.ThresholdDB, MinPauseFrames: input.MinPauseFrames,
		KeepEdgeFrames: input.KeepEdgeFrames, MaxPauses: input.MaxPauses,
		IncludeBoundaries: input.IncludeBoundaries,
	})
	if err != nil {
		return rushestools.SpeechPauseAnalysisResult{}, err
	}
	pauses := make([]rushestools.SpeechPauseCandidate, 0, len(analysis.Pauses))
	for _, pause := range analysis.Pauses {
		candidate := rushestools.SpeechPauseCandidate{
			SourceStartFrame: pause.SourceStartFrame, SourceEndFrame: pause.SourceEndFrame,
			DeleteStartFrame: pause.DeleteStartFrame, DeleteEndFrame: pause.DeleteEndFrame,
		}
		if timelineClip != nil {
			sourceStart := max(pause.DeleteStartFrame, timelineClip.SourceStartFrame)
			sourceEnd := min(pause.DeleteEndFrame, timelineClip.SourceEndFrame)
			if sourceEnd <= sourceStart {
				continue
			}
			rate := timelineClip.PlaybackRate
			if rate <= 0 {
				rate = 1
			}
			timelineStart := timelineClip.TimelineStartFrame + int(math.Round(float64(sourceStart-timelineClip.SourceStartFrame)/rate))
			timelineEnd := timelineClip.TimelineStartFrame + int(math.Round(float64(sourceEnd-timelineClip.SourceStartFrame)/rate))
			timelineStart = max(timelineClip.TimelineStartFrame, timelineStart)
			timelineEnd = min(timelineClip.TimelineEndFrame, timelineEnd)
			if timelineEnd <= timelineStart {
				continue
			}
			candidate.TimelineStartFrame = &timelineStart
			candidate.TimelineEndFrame = &timelineEnd
		}
		pauses = append(pauses, candidate)
	}
	usage := "这些范围是静音能量候选；确认语义后再剪。"
	if timelineClip != nil {
		usage += "如需批量 delete_range，必须按 timeline_start_frame 从大到小提交，避免前序波纹删除改变后续坐标。"
	}
	return rushestools.SpeechPauseAnalysisResult{
		AssetID: selected.ID, TimelineClipID: input.TimelineClipID,
		TimelineFPS: timeline.DefaultFPS, DurationFrames: analysis.DurationFrames,
		Pauses: pauses, AnalysisMethod: analysis.AnalysisMethod, Truncated: analysis.Truncated,
		UsageNote: usage,
	}, nil
}

func (service *Service) toolRecutToBeats(
	ctx context.Context,
	draftID string,
	input rushestools.TimelineBeatRecutInput,
) (rushestools.ToolResult, error) {
	// 只有只给现有 BGM clip + cut_frames 的窄调用才按当前片段局部裁短。
	// 一旦给出 BGM 素材、目标时长或“覆盖整首”，即使模型同时提供了
	// cut_frames，也必须从完整源素材池重建。否则已经裁短的当前 clip
	// 无法扩回更长节拍区间，模型会逐段修改 cut_frames 仍必然失败。
	if len(input.CutFrames) > 0 && input.BGMAssetID == "" &&
		input.TargetDurationFrames == 0 && !input.CoverEntireBGM && len(input.VideoAssetIDs) == 0 {
		if _, latestErr := timeline.Latest(ctx, service.database, draftID); errors.Is(latestErr, storage.ErrNotFound) {
			return service.toolBuildBeatMix(ctx, draftID, input)
		}
		return service.toolRecutCurrentClipsToBeats(ctx, draftID, input)
	}
	return service.toolBuildBeatMix(ctx, draftID, input)
}

func (service *Service) toolRecutCurrentClipsToBeats(
	ctx context.Context,
	draftID string,
	input rushestools.TimelineBeatRecutInput,
) (rushestools.ToolResult, error) {
	failed := func(message string, data map[string]any) (rushestools.ToolResult, error) {
		if data == nil {
			data = map[string]any{}
		}
		return rushestools.ToolResult{Status: "failed", Observation: message, Data: data}, nil
	}
	current, err := timeline.Latest(ctx, service.database, draftID)
	if errors.Is(err, storage.ErrNotFound) {
		current = timeline.Empty(draftID, 0)
	} else if err != nil {
		return rushestools.ToolResult{}, err
	}
	var primary *timeline.Track
	var bgmClip *timeline.Clip
	for trackIndex := range current.Tracks {
		track := &current.Tracks[trackIndex]
		switch track.TrackID {
		case "visual_base":
			primary = track
		case "bgm":
			for clipIndex := range track.Clips {
				if track.Clips[clipIndex].TimelineClipID == input.BGMTimelineClipID {
					bgmClip = &track.Clips[clipIndex]
					break
				}
			}
		}
	}
	if primary == nil || len(primary.Clips) == 0 {
		return failed("主视觉轨为空，无法按节拍重剪", nil)
	}
	if bgmClip == nil || bgmClip.AssetID == "" {
		return failed("指定的 BGM clip 不存在于 bgm 轨", map[string]any{
			"bgm_timeline_clip_id": input.BGMTimelineClipID,
		})
	}
	visuals := append([]timeline.Clip(nil), primary.Clips...)
	sort.SliceStable(visuals, func(i, j int) bool {
		return visuals[i].TimelineStartFrame < visuals[j].TimelineStartFrame
	})

	assets, err := storage.ListDraftAssets(ctx, service.database.Read(), draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	assetByID := make(map[string]storage.Asset, len(assets))
	for _, asset := range assets {
		assetByID[asset.ID] = asset
	}
	bgmAsset, exists := assetByID[bgmClip.AssetID]
	if !exists || bgmAsset.Kind != "audio" || !bgmAsset.Usable {
		return failed("BGM clip 未关联当前草稿中的可用音频素材", nil)
	}
	bgmSource, _, err := media.ResolveAssetSource(ctx, service.database, bgmAsset.ID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	grid, err := media.AnalyzeBeatGrid(ctx, bgmSource, current.FPS, 4096)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	bgmDuration, _ := numericValue(bgmAsset.Probe["duration_sec"])
	waveform := optionalWaveformEnvelope(
		ctx,
		bgmSource,
		current.FPS,
		int(math.Round(bgmDuration*float64(current.FPS))),
	)
	beatSet := make(map[int]struct{}, len(grid.BeatFrames))
	for _, frame := range grid.BeatFrames {
		beatSet[frame] = struct{}{}
	}
	cutFrames := append([]int(nil), input.CutFrames...)
	if len(cutFrames) == 0 {
		cursor := 0
		cutFrames = make([]int, 0, len(visuals))
		for index, clip := range visuals {
			rate := clip.PlaybackRate
			if rate <= 0 {
				rate = 1
			}
			maxDuration := max(1, int(math.Floor(float64(clip.SourceEndFrame-clip.SourceStartFrame)/rate)))
			maxCut := cursor + maxDuration
			selected := 0
			for _, candidate := range grid.EveryFourBeatFrames {
				if candidate > cursor && candidate <= maxCut {
					selected = candidate
					break
				}
			}
			if selected == 0 {
				for _, candidate := range grid.BeatFrames {
					if candidate > cursor && candidate <= maxCut {
						selected = candidate
						break
					}
				}
			}
			if selected == 0 {
				return failed("素材短于下一个可用节拍，无法自动卡点", map[string]any{
					"cut_index": index + 1, "timeline_clip_id": clip.TimelineClipID,
					"cursor_frame": cursor, "max_cut_frame": maxCut,
				})
			}
			cutFrames = append(cutFrames, selected)
			cursor = selected
		}
	}
	if len(cutFrames) != len(visuals) {
		return failed("cut_frames 数量必须等于主视频片段数", map[string]any{
			"expected_count": len(visuals), "provided_count": len(cutFrames),
		})
	}

	operations := []map[string]any{{"kind": "remove_track_clips", "track_id": "sfx"}}
	cursor := 0
	for index, clip := range visuals {
		cutFrame := cutFrames[index]
		if cutFrame <= cursor {
			return failed("cut_frames 必须严格递增", map[string]any{
				"cut_index": index + 1, "previous_frame": cursor, "cut_frame": cutFrame,
			})
		}
		if _, isBeat := beatSet[cutFrame]; !isBeat {
			return failed("cut_frames 中存在不属于真实节拍网格的帧", map[string]any{
				"cut_index": index + 1, "cut_frame": cutFrame,
			})
		}
		duration := cutFrame - cursor
		rate := clip.PlaybackRate
		if rate <= 0 {
			rate = 1
		}
		sourceDuration := max(1, int(math.Round(float64(duration)*rate)))
		sourceEnd := clip.SourceStartFrame + sourceDuration
		if sourceEnd > clip.SourceEndFrame {
			return failed("节拍区间长于对应素材当前可用源区间", map[string]any{
				"cut_index": index + 1, "timeline_clip_id": clip.TimelineClipID,
				"requested_duration_frames": duration,
				"max_duration_frames":       clip.TimelineEndFrame - clip.TimelineStartFrame,
			})
		}
		operations = append(operations, map[string]any{
			"kind": "trim_clip", "timeline_clip_id": clip.TimelineClipID,
			"source_start_frame": clip.SourceStartFrame, "source_end_frame": sourceEnd,
		})
		cursor = cutFrame
	}
	finalFrame := cutFrames[len(cutFrames)-1]
	if bgmClip.TimelineStartFrame != 0 {
		return failed("BGM 必须从第 0 帧开始才能铺满卡点成片", nil)
	}
	bgmRate := bgmClip.PlaybackRate
	if bgmRate <= 0 {
		bgmRate = 1
	}
	bgmSourceEnd := bgmClip.SourceStartFrame + max(1, int(math.Round(float64(finalFrame)*bgmRate)))
	bgmAvailableEnd := bgmClip.SourceEndFrame
	if probedEnd := int(math.Round(bgmDuration * float64(current.FPS))); probedEnd > 0 {
		bgmAvailableEnd = min(bgmAvailableEnd, probedEnd)
	}
	if bgmSourceEnd > bgmAvailableEnd {
		return failed("卡点成片超过 BGM 可用时长", map[string]any{
			"final_frame": finalFrame, "bgm_available_frames": bgmAvailableEnd - bgmClip.SourceStartFrame,
		})
	}
	operations = append(operations, map[string]any{
		"kind": "trim_clip", "timeline_clip_id": bgmClip.TimelineClipID,
		"source_start_frame": bgmClip.SourceStartFrame, "source_end_frame": bgmSourceEnd,
	})

	sfxClipID := ""
	sfxStartFrame := 0
	if input.SFX != nil {
		sfx := input.SFX
		if sfx.StartFrame == nil {
			return failed("SFX start_frame 必须显式提供", map[string]any{
				"recovery": "根据 audio.analyze_beats 返回的波形、拍点和创作意图自主选择合法整数帧",
			})
		}
		sfxStartFrame = *sfx.StartFrame
		if sfx.DurationFrames <= 0 || sfx.DurationFrames > current.FPS*3 ||
			sfxStartFrame < 0 || sfxStartFrame+sfx.DurationFrames > finalFrame {
			return failed("SFX 必须是位于成片范围内、不超过 3 秒的短点缀", map[string]any{
				"start_frame": sfxStartFrame, "duration_frames": sfx.DurationFrames,
				"final_frame": finalFrame,
			})
		}
		sfxAsset, found := assetByID[sfx.AssetID]
		if !found || sfxAsset.Kind != "audio" || !sfxAsset.Usable {
			return failed("SFX 素材必须是当前草稿中的可用音频", map[string]any{"asset_id": sfx.AssetID})
		}
		sfxDuration, _ := numericValue(sfxAsset.Probe["duration_sec"])
		if available := int(math.Round(sfxDuration * float64(current.FPS))); available > 0 && sfx.DurationFrames > available {
			return failed("SFX 请求时长超过素材时长", map[string]any{
				"requested_frames": sfx.DurationFrames, "available_frames": available,
			})
		}
		gain := -12.0
		if sfx.GainDB != nil {
			gain = *sfx.GainDB
		}
		if gain < -60 || gain > 12 {
			return failed("SFX gain_db 必须在 [-60,12] 范围内", nil)
		}
		sfxClipID = randomID("sfx_beat")
		operations = append(operations,
			map[string]any{
				"kind": "insert_clip", "track_id": "sfx", "timeline_clip_id": sfxClipID,
				"asset_id": sfx.AssetID, "asset_kind": "audio", "role": "sfx",
				"timeline_start_frame": sfxStartFrame,
				"source_start_frame":   0, "source_end_frame": sfx.DurationFrames,
			},
			map[string]any{
				"kind": "adjust_gain", "timeline_clip_id": sfxClipID, "gain_db": gain,
			},
		)
	}

	result, err := service.toolApplyPatches(ctx, draftID, rushestools.TimelinePatchBatchInput{Ops: operations})
	if err != nil || result.Status != "succeeded" {
		return result, err
	}
	result.Observation = fmt.Sprintf(
		"已按 %.2f BPM 节拍原子重剪 %d 个主视频；成片 %d 帧，BGM 已铺满，SFX 独立分轨。",
		grid.BPM, len(visuals), finalFrame,
	)
	result.Data["bpm"] = grid.BPM
	result.Data["cut_frames"] = cutFrames
	result.Data["duration_frames"] = finalFrame
	result.Data["sfx_timeline_clip_id"] = sfxClipID
	result.Data["sfx_start_frame"] = sfxStartFrame
	if waveform != nil {
		result.Data["waveform"] = waveformToolValue(*waveform)
	}
	return result, nil
}

// toolBuildBeatMix 是面向 Agent 的高层卡点工具。它从素材库中的完整源时长
// 重建主视觉，而不是继续消耗当前时间线上已经裁短的 source range。这样“重剪
// 到 48 秒并覆盖整首音乐”可以在一个原子提交里完成，不需要模型手工拼几十个
// compose/insert/trim 补丁。
func (service *Service) toolBuildBeatMix(
	ctx context.Context,
	draftID string,
	input rushestools.TimelineBeatRecutInput,
) (rushestools.ToolResult, error) {
	failed := func(message string, data map[string]any) (rushestools.ToolResult, error) {
		if data == nil {
			data = map[string]any{}
		}
		return rushestools.ToolResult{Status: "failed", Observation: message, Data: data}, nil
	}
	current, err := timeline.Latest(ctx, service.database, draftID)
	if errors.Is(err, storage.ErrNotFound) {
		current = timeline.Empty(draftID, 0)
	} else if err != nil {
		return rushestools.ToolResult{}, err
	}
	assets, err := storage.ListDraftAssets(ctx, service.database.Read(), draftID)
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
			duration, _ := numericValue(asset.Probe["duration_sec"])
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
	bgmDurationSec, _ := numericValue(bgmAsset.Probe["duration_sec"])
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

	bgmSource, _, err := media.ResolveAssetSource(ctx, service.database, bgmAsset.ID)
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
		analysisRanges []beatMixSourceRange
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
		durationSec, _ := numericValue(asset.Probe["duration_sec"])
		available := int(math.Round(durationSec * float64(current.FPS)))
		if available <= 0 {
			continue
		}
		analysisRanges := service.latestBeatMixSourceRanges(ctx, asset.ID, available)
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
		indexedShots, _, indexErr := service.draftShotIndex(ctx, draftID, nil)
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
				"recovery":         "重新调用 media.search_shots，并原样使用返回的 shot_id",
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
			if cutFrame != targetFrames && !containsFrame(grid.BeatFrames, cutFrame) {
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
			cutFrames = chooseAllBeatMixCuts(
				grid.EveryFourBeatFrames, grid.BeatFrames, targetFrames, len(input.ShotIDs),
			)
		} else if input.UseAllVideoAssets {
			cutFrames = chooseAllBeatMixCuts(
				grid.EveryFourBeatFrames, grid.BeatFrames, targetFrames, len(videoSources),
			)
		} else {
			cutFrames = chooseBeatMixCuts(
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
	usedSourceRanges := map[string][]beatMixSourceRange{}
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
			start, fits = chooseUnusedBeatMixSourceStart(
				videoSources[selectedIndex].availableFrame, duration,
				[]beatMixSourceRange{shot.rangeInfo}, usedSourceRanges[shot.candidate.AssetID], 0, true,
			)
			if !fits {
				return failed("所选镜头无法覆盖对应节拍片段，或其源区间已被重复使用", map[string]any{
					"shot_id": selectedShotID, "required_frames": duration,
					"shot_duration_frames": shot.candidate.DurationFrames,
					"recovery":             "用 media.search_shots 按该片段 min_duration_frames 重新检索，且不要重复传同一 shot_id",
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
					candidateStart, fits := chooseUnusedBeatMixSourceStart(
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
				"recovery":            "减少切点密度、补充素材，或用 media.search_shots 选择更长的镜头区间",
			})
		}
		selected := videoSources[selectedIndex]
		usedRange := beatMixSourceRange{StartFrame: start, EndFrame: start + duration}
		usedSourceRanges[selected.asset.ID] = append(usedSourceRanges[selected.asset.ID], usedRange)
		usedAssets[selected.asset.ID] = struct{}{}
		if sourceRangeContains(selected.analysisRanges, start, start+duration) {
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

	nextVersion, err := timeline.NextVersion(ctx, service.database, draftID)
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
	bgmClipID := randomID("bgm_beat")
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
		sfxDurationSec, _ := numericValue(sfxAsset.Probe["duration_sec"])
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
		sfxClipID = randomID("sfx_beat")
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
	result, err := service.persistTimeline(
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

type beatMixSourceRange struct {
	StartFrame int
	EndFrame   int
}

// latestBeatMixSourceRanges 读取质量最完整的理解摘要中的源帧证据。摘要缺失、损坏或
// 区间不合法时静默回退到完整素材，避免让理解服务的降级阻塞高层剪辑工具。
func (service *Service) latestBeatMixSourceRanges(
	ctx context.Context,
	assetID string,
	availableFrames int,
) []beatMixSourceRange {
	raw, err := storage.BestMaterialSummary(ctx, service.database.Read(), assetID)
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
	return beatMixRangesFromUnderstanding(summary.Segments, availableFrames)
}

func beatMixRangesFromUnderstanding(
	segments []understanding.Segment,
	availableFrames int,
) []beatMixSourceRange {
	ranges := make([]beatMixSourceRange, 0, len(segments)*2)
	var continuous *beatMixSourceRange
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
		ranges = append(ranges, beatMixSourceRange{StartFrame: start, EndFrame: end})
		// analysis_window 是同一长镜头内的理解采样边界。卡点片段可以跨越
		// 相邻窗口，但不能跨越 VLM 已确认的真实切镜或不可用区间。
		if continuous != nil && start <= continuous.EndFrame+1 && segment.BoundaryKind == "analysis_window" {
			continuous.EndFrame = max(continuous.EndFrame, end)
			continue
		}
		flushContinuous()
		continuous = &beatMixSourceRange{StartFrame: start, EndFrame: end}
	}
	flushContinuous()
	sort.SliceStable(ranges, func(i, j int) bool {
		if ranges[i].StartFrame == ranges[j].StartFrame {
			return ranges[i].EndFrame < ranges[j].EndFrame
		}
		return ranges[i].StartFrame < ranges[j].StartFrame
	})
	return ranges
}

func chooseUnusedBeatMixSourceStart(
	availableFrames int,
	durationFrames int,
	ranges []beatMixSourceRange,
	used []beatMixSourceRange,
	rangeOffset int,
	strictRanges bool,
) (int, bool) {
	if durationFrames <= 0 || availableFrames < durationFrames {
		return 0, false
	}
	candidates := make([]beatMixSourceRange, 0, max(1, len(ranges)))
	for _, sourceRange := range ranges {
		sourceRange.StartFrame = max(0, sourceRange.StartFrame)
		sourceRange.EndFrame = min(availableFrames, sourceRange.EndFrame)
		if sourceRange.EndFrame-sourceRange.StartFrame >= durationFrames {
			candidates = append(candidates, sourceRange)
		}
	}
	if len(candidates) == 0 && !strictRanges {
		candidates = append(candidates, beatMixSourceRange{StartFrame: 0, EndFrame: availableFrames})
	}
	if len(candidates) == 0 {
		return 0, false
	}
	sortedUsed := append([]beatMixSourceRange(nil), used...)
	sort.SliceStable(sortedUsed, func(i, j int) bool {
		return sortedUsed[i].StartFrame < sortedUsed[j].StartFrame
	})
	if rangeOffset < 0 {
		rangeOffset = -rangeOffset
	}
	for step := 0; step < len(candidates); step++ {
		sourceRange := candidates[(rangeOffset+step)%len(candidates)]
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
		return chooseUnusedBeatMixSourceStart(
			availableFrames, durationFrames,
			[]beatMixSourceRange{{StartFrame: 0, EndFrame: availableFrames}},
			used, rangeOffset, true,
		)
	}
	return 0, false
}

func sourceRangeContains(ranges []beatMixSourceRange, startFrame, endFrame int) bool {
	for _, sourceRange := range ranges {
		if startFrame >= sourceRange.StartFrame && endFrame <= sourceRange.EndFrame {
			return true
		}
	}
	return false
}

func beatGridEffect(grid media.BeatGrid, waveform *media.WaveformEnvelope) map[string]any {
	effect := map[string]any{
		"kind":               "beat_grid",
		"bpm":                grid.BPM,
		"beat_frames":        grid.BeatFrames,
		"strong_beat_frames": grid.StrongBeatFrames,
		"downbeat_frames":    grid.DownbeatFrames,
		"bar_phase":          grid.BarPhase,
		"analysis_method":    grid.AnalysisMethod,
	}
	if waveform != nil {
		effect["waveform"] = waveformToolValue(*waveform)
	}
	return effect
}

func optionalWaveformEnvelope(
	ctx context.Context,
	source string,
	fps int,
	durationFrames int,
) *media.WaveformEnvelope {
	if durationFrames <= 0 {
		return nil
	}
	waveform, err := media.AnalyzeWaveformEnvelope(
		ctx,
		source,
		fps,
		durationFrames,
		media.DefaultWaveformPoints,
	)
	if err != nil {
		return nil
	}
	return &waveform
}

func waveformToolValue(waveform media.WaveformEnvelope) rushestools.AudioWaveformEnvelope {
	return rushestools.AudioWaveformEnvelope{
		SampleIntervalFrames: waveform.SampleIntervalFrames,
		SampleFrames:         append([]int(nil), waveform.SampleFrames...),
		Samples:              append([]int(nil), waveform.Samples...),
		Encoding:             waveform.Encoding,
		FloorDB:              waveform.FloorDB,
		CeilingDB:            waveform.CeilingDB,
	}
}

func chooseBeatMixCuts(everyFour, everyBeat []int, targetFrames, maxClips int) []int {
	if targetFrames <= 0 || maxClips <= 0 {
		return nil
	}
	candidates := beatCandidatesWithin(everyFour, targetFrames)
	if len(candidates) == 0 {
		candidates = beatCandidatesWithin(everyBeat, targetFrames)
	}
	return distributeBeatMixCuts(candidates, targetFrames, maxClips)
}

func chooseAllBeatMixCuts(everyFour, everyBeat []int, targetFrames, clipCount int) []int {
	candidates := beatCandidatesWithin(everyFour, targetFrames)
	// 四拍网格不足以为每个素材提供一个切点时，回退到完整拍点网格。
	// 只有显式 use_all_video_assets 才提高密度，避免默认规划为了短素材过度切碎。
	if len(candidates)+1 < clipCount {
		candidates = beatCandidatesWithin(everyBeat, targetFrames)
	}
	return distributeBeatMixCuts(candidates, targetFrames, clipCount)
}

func distributeBeatMixCuts(candidates []int, targetFrames, maxClips int) []int {
	if targetFrames <= 0 || maxClips <= 0 {
		return nil
	}
	clipCount := min(maxClips, len(candidates)+1)
	if clipCount <= 1 {
		return []int{targetFrames}
	}
	cuts := make([]int, 0, clipCount)
	previousIndex := -1
	for segment := 1; segment < clipCount; segment++ {
		remainingCuts := clipCount - segment - 1
		minIndex := previousIndex + 1
		maxIndex := len(candidates) - remainingCuts - 1
		ideal := int(math.Round(float64(targetFrames*segment) / float64(clipCount)))
		selectedIndex := minIndex
		for index := minIndex + 1; index <= maxIndex; index++ {
			if absInt(candidates[index]-ideal) < absInt(candidates[selectedIndex]-ideal) {
				selectedIndex = index
			}
		}
		cuts = append(cuts, candidates[selectedIndex])
		previousIndex = selectedIndex
	}
	return append(cuts, targetFrames)
}

func beatCandidatesWithin(frames []int, targetFrames int) []int {
	result := make([]int, 0, len(frames))
	previous := -1
	for _, frame := range frames {
		if frame <= 0 || frame >= targetFrames || frame == previous {
			continue
		}
		result = append(result, frame)
		previous = frame
	}
	return result
}

func containsFrame(frames []int, target int) bool {
	index := sort.SearchInts(frames, target)
	return index < len(frames) && frames[index] == target
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func (service *Service) toolListAssets(
	ctx context.Context,
	draftID string,
	input rushestools.AssetListInput,
) (rushestools.AssetListResult, error) {
	assets, err := storage.ListDraftAssets(ctx, service.database.Read(), draftID)
	if err != nil {
		return rushestools.AssetListResult{}, err
	}
	result := rushestools.AssetListResult{DraftID: draftID, Assets: []rushestools.AssetManifest{}}
	for _, asset := range assets {
		if input.Kind != "" && asset.Kind != input.Kind || input.After != "" && asset.ID <= input.After {
			continue
		}
		if input.OnlyUsable != nil && *input.OnlyUsable != asset.Usable {
			continue
		}
		duration, _ := numericValue(asset.Probe["duration_sec"])
		suggestedRole := ""
		suggestedVisualRole := ""
		switch asset.Kind {
		case "audio":
			suggestedRole = understanding.ClassifyAudioRole(asset.Filename, duration)
		case "video":
			relDir := ""
			if asset.RelDir != nil {
				relDir = *asset.RelDir
			}
			understoodRole := ""
			if raw, summaryErr := storage.BestMaterialSummary(ctx, service.database.Read(), asset.ID); summaryErr == nil {
				encoded, _ := json.Marshal(raw)
				var summary understanding.Summary
				if json.Unmarshal(encoded, &summary) == nil {
					understoodRole = summary.SemanticRole
				}
			}
			suggestedVisualRole = understanding.SuggestVisualRole(asset.Filename, relDir, understoodRole)
		}
		relDir := ""
		if asset.RelDir != nil {
			relDir = *asset.RelDir
		}
		result.Assets = append(result.Assets, rushestools.AssetManifest{
			AssetID: asset.ID, Filename: asset.Filename, Kind: asset.Kind,
			RelDir: relDir, SuggestedRole: suggestedRole, SuggestedVisualRole: suggestedVisualRole,
			DurationFrames: int(math.Round(duration * timeline.DefaultFPS)), TimelineFPS: timeline.DefaultFPS,
			Usable: asset.Usable, IngestStatus: asset.IngestStatus,
			UnderstandingStatus: asset.UnderstandingStatus,
		})
	}
	result.Total = len(result.Assets)
	limit := input.Limit
	if limit <= 0 || limit > 200 {
		limit = min(200, len(result.Assets))
	}
	if len(result.Assets) > limit {
		result.Assets = result.Assets[:limit]
		result.NextAfter = result.Assets[len(result.Assets)-1].AssetID
	}
	return result, nil
}

func (service *Service) toolUnderstand(
	ctx context.Context,
	draftID string,
	input rushestools.UnderstandInput,
) (rushestools.UnderstandResult, error) {
	if len(input.AssetIDs) == 0 {
		return rushestools.UnderstandResult{}, errors.New("understand.materials 至少需要一个 asset_id")
	}
	linkedAssets, err := storage.ListDraftAssets(ctx, service.database.Read(), draftID)
	if err != nil {
		return rushestools.UnderstandResult{}, err
	}
	assetByID := make(map[string]storage.Asset, len(linkedAssets))
	for _, asset := range linkedAssets {
		assetByID[asset.ID] = asset
	}
	jobID := randomID("understand")
	summaries := make([]rushestools.MaterialUnderstandingSummary, 0, len(input.AssetIDs))
	cacheHitAssetIDs := make([]string, 0, len(input.AssetIDs))
	analyzedAssetIDs := make([]string, 0, len(input.AssetIDs))
	for index, assetID := range input.AssetIDs {
		if input.Focus == "e2e_cancel" && index > 0 {
			select {
			case <-ctx.Done():
				return rushestools.UnderstandResult{}, ctx.Err()
			case <-time.After(30 * time.Second):
			}
		}
		if err := ctx.Err(); err != nil {
			return rushestools.UnderstandResult{}, err
		}
		asset, exists := assetByID[assetID]
		if !exists {
			return rushestools.UnderstandResult{}, fmt.Errorf("素材 %s 不属于当前草稿", assetID)
		}
		options := understanding.NormalizeAnalyzeOptions(asset, understanding.AnalyzeOptions{
			Focus: input.Focus, Depth: input.Depth, MaxStepsPerAsset: input.MaxStepsPerAsset,
		})
		fingerprint := understanding.AnalysisFingerprint(asset, options)
		if !input.ForceRefresh {
			if _, cacheErr := storage.MaterialSummaryByFingerprint(
				ctx, service.database.Read(), assetID, fingerprint,
			); cacheErr == nil {
				effective, bestErr := storage.BestMaterialSummary(ctx, service.database.Read(), assetID)
				if bestErr != nil {
					return rushestools.UnderstandResult{}, bestErr
				}
				var cached understanding.Summary
				encoded, _ := json.Marshal(effective)
				if err := json.Unmarshal(encoded, &cached); err != nil {
					return rushestools.UnderstandResult{}, err
				}
				summaries = append(summaries, compactUnderstandingSummary(asset, cached, 12))
				cacheHitAssetIDs = append(cacheHitAssetIDs, assetID)
				continue
			} else if !errors.Is(cacheErr, storage.ErrNotFound) {
				return rushestools.UnderstandResult{}, cacheErr
			}
		}
		started, err := reducer.Apply(ctx, service.database, []contracts.Event{{
			Type:    "MaterialUnderstandingStarted",
			Payload: map[string]any{"asset_id": assetID, "job_id": jobID},
		}}, reducer.Options{Actor: contracts.ActorAgent})
		if err != nil || started.Status != reducer.StatusApplied {
			return rushestools.UnderstandResult{}, errors.Join(err, fmt.Errorf("start reducer status: %s", started.Status))
		}
		summary, analyzeErr := service.analyzer.AnalyzeWithOptions(
			ctx, service.database, asset, options, func(note string) {
				service.hub.Record(draftID, StreamEvent{
					"type": "subagent_progress", "tool": "understand.materials",
					"asset_id": assetID, "note": note, "completed": index, "total": len(input.AssetIDs),
				})
			},
		)
		if analyzeErr != nil {
			cancelled := errors.Is(analyzeErr, context.Canceled)
			_, _ = reducer.Apply(context.WithoutCancel(ctx), service.database, []contracts.Event{{
				Type: "MaterialUnderstandingFailed",
				Payload: map[string]any{
					"asset_id": assetID, "job_id": jobID, "cancelled": cancelled,
					"failure": map[string]any{"error_code": "understanding_failed", "message": analyzeErr.Error()},
				},
			}}, reducer.Options{Actor: contracts.ActorAgent})
			return rushestools.UnderstandResult{}, analyzeErr
		}
		var summaryMap map[string]any
		encoded, _ := json.Marshal(summary)
		_ = json.Unmarshal(encoded, &summaryMap)
		summaryID := randomID("summary")
		completed, err := reducer.Apply(ctx, service.database, []contracts.Event{{
			Type:    "MaterialUnderstandingCompleted",
			Payload: map[string]any{"asset_id": assetID, "job_id": jobID, "summary_id": summaryID},
		}}, reducer.Options{
			Actor: contracts.ActorAgent,
			ResultRows: reducer.ResultRows{MaterialSummaries: []reducer.MaterialSummaryRow{{
				ID: summaryID, AssetID: assetID, Version: 0,
				Focus: stringPointerValue(options.Focus), Status: "ready", Summary: summaryMap,
				Model: stringPointerValue(summary.Model), Fingerprint: stringPointerValue(fingerprint),
				PromptVersion: stringPointerValue(understanding.PromptVersion),
			}}},
		})
		if err != nil || completed.Status != reducer.StatusApplied {
			return rushestools.UnderstandResult{}, errors.Join(err, fmt.Errorf("complete reducer status: %s", completed.Status))
		}
		effective, bestErr := storage.BestMaterialSummary(ctx, service.database.Read(), assetID)
		if bestErr != nil {
			return rushestools.UnderstandResult{}, bestErr
		}
		var bestSummary understanding.Summary
		bestEncoded, _ := json.Marshal(effective)
		if err := json.Unmarshal(bestEncoded, &bestSummary); err != nil {
			return rushestools.UnderstandResult{}, err
		}
		summaries = append(summaries, compactUnderstandingSummary(asset, bestSummary, 12))
		analyzedAssetIDs = append(analyzedAssetIDs, assetID)
		service.hub.Record(draftID, StreamEvent{
			"type": "subagent_progress", "tool": "understand.materials",
			"asset_id": assetID, "note": "摘要已完成", "completed": index + 1, "total": len(input.AssetIDs),
		})
	}
	return rushestools.UnderstandResult{
		DraftID: draftID, JobID: jobID, AssetIDs: input.AssetIDs, Status: "completed",
		Summaries: summaries, CacheHitAssetIDs: cacheHitAssetIDs, AnalyzedAssetIDs: analyzedAssetIDs,
	}, nil
}

func compactUnderstandingSummary(
	asset storage.Asset,
	summary understanding.Summary,
	evidenceLimit int,
) rushestools.MaterialUnderstandingSummary {
	overall := truncateRunes(strings.TrimSpace(summary.Overall), 320)
	segments := sampleUnderstandingSegments(summary.Segments, evidenceLimit)
	evidence := make([]rushestools.MaterialEvidence, 0, len(segments))
	for _, segment := range segments {
		description := truncateRunes(strings.TrimSpace(segment.Description), 220)
		if description == overall {
			description = ""
		}
		transcript := ""
		if segment.Transcript != nil {
			transcript = truncateRunes(strings.TrimSpace(*segment.Transcript), 160)
		}
		startFrame := segment.SourceStartFrame
		endFrame := segment.SourceEndFrame
		if startFrame < 0 || endFrame <= startFrame {
			startFrame = max(0, int(math.Floor(segment.StartSec*timeline.DefaultFPS)))
			endFrame = max(startFrame, int(math.Ceil(segment.EndSec*timeline.DefaultFPS)))
			if segment.EndSec > segment.StartSec && endFrame == startFrame {
				endFrame++
			}
		}
		evidence = append(evidence, rushestools.MaterialEvidence{
			StartSec: segment.StartSec, EndSec: segment.EndSec,
			SourceStartFrame: startFrame, SourceEndFrame: endFrame,
			Description: description, Transcript: transcript,
			Tags:    append([]string(nil), segment.Tags[:min(6, len(segment.Tags))]...),
			Quality: segment.Quality, BoundaryKind: segment.BoundaryKind,
			BoundaryScore: segment.BoundaryScore, BoundaryVerified: segment.BoundaryVerified,
			Subjects:  append([]string(nil), segment.Subjects...),
			Actions:   append([]string(nil), segment.Actions...),
			Setting:   append([]string(nil), segment.Setting...),
			ShotScale: segment.ShotScale, Composition: segment.Composition,
			Lighting:  append([]string(nil), segment.Lighting...),
			Mood:      append([]string(nil), segment.Mood...),
			EditHints: append([]string(nil), segment.EditHints...),
		})
	}
	return rushestools.MaterialUnderstandingSummary{
		AssetID: asset.ID, Filename: asset.Filename, Kind: asset.Kind,
		TimelineFPS: timeline.DefaultFPS, SemanticRole: summary.SemanticRole,
		Overall: overall, Evidence: evidence,
		EvidenceTotal: len(summary.Segments), EvidenceTruncated: len(evidence) < len(summary.Segments),
		AnalysisMethod:    summary.AnalysisMethod,
		CandidateCutCount: summary.CandidateCuts, VerifiedCutCount: summary.VerifiedCuts,
		Degraded: append([]string(nil), summary.Degraded[:min(4, len(summary.Degraded))]...),
		UsageNote: "boundary_kind=analysis_window 只是长镜头理解采样窗口，不代表真实切镜；" +
			"只有 boundary_kind=visual_cut 且 boundary_verified=true 才能称为已验证切镜。",
	}
}

func sampleUnderstandingSegments(
	segments []understanding.Segment,
	limit int,
) []understanding.Segment {
	if limit <= 0 || len(segments) <= limit {
		return segments
	}
	if limit == 1 {
		return []understanding.Segment{segments[len(segments)/2]}
	}
	result := make([]understanding.Segment, 0, limit)
	for index := 0; index < limit; index++ {
		segmentIndex := int(math.Round(
			float64(index) * float64(len(segments)-1) / float64(limit-1),
		))
		result = append(result, segments[segmentIndex])
	}
	return result
}

func stringPointerValue(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func (service *Service) toolComposeInitial(
	ctx context.Context,
	draftID string,
	input rushestools.ComposeInitialInput,
) (rushestools.ToolResult, error) {
	version, err := timeline.NextVersion(ctx, service.database, draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	selections := make([]timeline.Selection, 0, len(input.Clips))
	for _, clip := range input.Clips {
		asset, assetErr := storage.GetAsset(ctx, service.database.Read(), clip.AssetID)
		if assetErr != nil {
			return rushestools.ToolResult{}, assetErr
		}
		hasAudio, _ := asset.Probe["has_audio"].(bool)
		selections = append(selections, timeline.Selection{
			AssetID: clip.AssetID, AssetKind: asset.Kind, HasAudio: hasAudio,
			SourceStartFrame: clip.SourceStartFrame, SourceEndFrame: clip.SourceEndFrame,
			Role: clip.Role,
		})
	}
	document, err := timeline.ComposeInitial(draftID, version, selections)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	return service.persistTimeline(ctx, draftID, document, "compose_initial", []map[string]any{{
		"kind": "compose_initial", "clip_count": len(input.Clips),
	}})
}

func (service *Service) toolApplyPatch(
	ctx context.Context,
	draftID string,
	input rushestools.TimelinePatchInput,
) (rushestools.ToolResult, error) {
	current, err := timeline.Latest(ctx, service.database, draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	operations, err := service.enrichTimelineOperations(ctx, draftID, []map[string]any{input.Op})
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	operation := operations[0]
	document, err := timeline.ApplyPatch(current, operation)
	if err != nil {
		if failure, ok := timelineOpFailureAt(err, operation, 0); ok {
			return failure, nil
		}
		return rushestools.ToolResult{}, err
	}
	attachedBeatGrids, beatWarnings := service.attachMissingBGMBeatGrids(ctx, draftID, &document)
	if report := timeline.Validate(document); !report.Valid {
		return rushestools.ToolResult{
			Status:      "failed",
			Observation: "补丁结果未通过时间线校验，当前时间线未更新",
			Data: map[string]any{
				"reason":                     "validation_failed",
				"current_timeline_unchanged": true,
				"recovery":                   "根据 validation_report 修正补丁；原声错位时使用 sync_original_audio 原子重建。",
				"validation_report": map[string]any{
					"valid": report.Valid, "checks": report.Checks, "issues": report.Issues,
				},
			},
		}, nil
	}
	next, err := timeline.NextVersion(ctx, service.database, draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	document.Version = next
	document.TimelineID = fmt.Sprintf("%s:v%d", draftID, next)
	result, err := service.persistTimeline(ctx, draftID, document, "apply_patch", []map[string]any{operation})
	appendBeatMetadataResult(&result, attachedBeatGrids, beatWarnings)
	return result, err
}

func (service *Service) toolApplyPatches(
	ctx context.Context,
	draftID string,
	input rushestools.TimelinePatchBatchInput,
) (rushestools.ToolResult, error) {
	if len(input.Ops) == 0 {
		return rushestools.ToolResult{}, errors.New("timeline.apply_patches 至少需要一个 op")
	}
	if len(input.Ops) > 100 {
		return rushestools.ToolResult{}, errors.New("timeline.apply_patches 单次最多 100 个 op")
	}
	current, err := timeline.Latest(ctx, service.database, draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	enrichedOperations, err := service.enrichTimelineOperations(ctx, draftID, input.Ops)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	plannedOperations, preservedAudio := prepareTimelineBatch(current, enrichedOperations)
	document := current
	for index, operation := range plannedOperations {
		document, err = timeline.ApplyPatch(document, operation)
		if err != nil {
			if failure, ok := timelineOpFailureAt(err, operation, index+1); ok {
				return failure, nil
			}
			message := fmt.Sprintf("第 %d 个时间线补丁失败: %v", index+1, err)
			return rushestools.ToolResult{
				Status: "failed", Observation: message,
				Data: map[string]any{
					"failed_op_index":            index + 1,
					"failed_op":                  operation,
					"reason":                     err.Error(),
					"current_timeline_unchanged": true,
					"recovery": "读取 failed_op 和 reason 后修正这一批；完整卡点重剪必须改用 timeline.recut_to_beats。" +
						"如需整批替换主视频，应把新 insert_clip 与旧 delete_clip 放在同一次调用中，工具会自动规划安全顺序并保护 BGM/SFX。",
				},
			}, nil
		}
	}
	if restoreErr := restoreIndependentAudioTracks(&document, preservedAudio); restoreErr != nil {
		return rushestools.ToolResult{
			Status:      "failed",
			Observation: "批量主视频编辑会破坏未被本批直接编辑的 BGM/SFX，当前时间线未更新",
			Data: map[string]any{
				"reason":                     restoreErr.Error(),
				"current_timeline_unchanged": true,
				"recovery": "把完整的新主视频 insert_clip 与旧主视频 delete_clip 放在同一次 timeline.apply_patches 调用中，" +
					"保证最终时长能容纳现有音轨；卡点混剪改用 timeline.recut_to_beats。",
			},
		}, nil
	}
	attachedBeatGrids, beatWarnings := service.attachMissingBGMBeatGrids(ctx, draftID, &document)
	if report := timeline.Validate(document); !report.Valid {
		return rushestools.ToolResult{
			Status:      "failed",
			Observation: "批量补丁结果未通过时间线校验，当前时间线未更新",
			Data: map[string]any{
				"failed_op_index":            len(plannedOperations),
				"reason":                     "validation_failed",
				"current_timeline_unchanged": true,
				"recovery":                   "根据 validation_report 修正整批参数后重试；卡点重剪不要降级为低层补丁。",
				"validation_report": map[string]any{
					"valid": report.Valid, "checks": report.Checks, "issues": report.Issues,
				},
			},
		}, nil
	}
	next, err := timeline.NextVersion(ctx, service.database, draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	document.Version = next
	document.TimelineID = fmt.Sprintf("%s:v%d", draftID, next)
	result, err := service.persistTimeline(ctx, draftID, document, "apply_patches", plannedOperations)
	appendBeatMetadataResult(&result, attachedBeatGrids, beatWarnings)
	return result, err
}

func appendBeatMetadataResult(
	result *rushestools.ToolResult,
	attached int,
	warnings []string,
) {
	if result == nil || attached == 0 && len(warnings) == 0 {
		return
	}
	if result.Data == nil {
		result.Data = map[string]any{}
	}
	result.Data["beat_grid_attached_count"] = attached
	if len(warnings) > 0 {
		result.Data["beat_grid_warnings"] = warnings
	}
}

func (service *Service) persistTimeline(
	ctx context.Context,
	draftID string,
	document timeline.Document,
	operation string,
	editOperationBatches ...[]map[string]any,
) (rushestools.ToolResult, error) {
	draft, err := storage.GetDraft(ctx, service.database.Read(), draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	documentMap, err := timeline.ToMap(document)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	report := timeline.Validate(document)
	validationType := "TimelineValidated"
	if !report.Valid {
		validationType = "TimelineValidationFailed"
	}
	reportMap := map[string]any{"valid": report.Valid, "checks": report.Checks, "issues": report.Issues}
	actor := contracts.ActorAgent
	origin := rushestools.TimelineMutationOrigin(ctx)
	if origin == "manual" {
		actor = contracts.ActorUser
	}
	if origin == "" {
		origin = "agent"
	}
	editOperations := []map[string]any{}
	if len(editOperationBatches) > 0 {
		editOperations = editOperationBatches[0]
	}
	patchID := operation + ":" + randomID("patch")
	result, err := reducer.Apply(ctx, service.database, []contracts.Event{
		{
			Type: "TimelineVersionCreated", DraftID: draftID,
			Payload: map[string]any{
				"timeline_id": document.TimelineID, "timeline_version": document.Version,
				"patch_id": patchID, "document_json": documentMap,
				"edit_origin": origin, "edit_operations": editOperations,
			},
		},
		{
			Type: validationType, DraftID: draftID,
			Payload: map[string]any{"timeline_version": document.Version, "validation_report": reportMap},
		},
	}, reducer.Options{Actor: actor, BaseVersion: &draft.StateVersion})
	if err != nil || result.Status != reducer.StatusApplied {
		return rushestools.ToolResult{}, errors.Join(err, fmt.Errorf("timeline reducer status: %s", result.Status))
	}
	status := "succeeded"
	if !report.Valid {
		status = "validation_failed"
	}
	return rushestools.ToolResult{
		Status: status, Observation: timeline.Inspect(document),
		Data: map[string]any{
			"validation_report": reportMap,
			"beat_alignment":    beatAlignmentData(document),
		},
	}, nil
}

func (service *Service) toolValidateTimeline(ctx context.Context, draftID string) (rushestools.ToolResult, error) {
	document, err := timeline.Latest(ctx, service.database, draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	draft, err := storage.GetDraft(ctx, service.database.Read(), draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	report := timeline.Validate(document)
	beatAlignment := beatAlignmentData(document)
	eventType := "TimelineValidated"
	if !report.Valid {
		eventType = "TimelineValidationFailed"
	}
	result, err := reducer.Apply(ctx, service.database, []contracts.Event{{
		Type: eventType, DraftID: draftID,
		Payload: map[string]any{
			"timeline_version":  document.Version,
			"validation_report": map[string]any{"valid": report.Valid, "checks": report.Checks, "issues": report.Issues},
		},
	}}, reducer.Options{Actor: contracts.ActorAgent, BaseVersion: &draft.StateVersion})
	if err != nil || result.Status != reducer.StatusApplied {
		return rushestools.ToolResult{}, errors.Join(err, fmt.Errorf("validation reducer status: %s", result.Status))
	}
	observation := timeline.Inspect(document)
	if report.Valid {
		if present, _ := beatAlignment["beat_grid_present"].(bool); !present {
			observation += " 结构校验通过，但 BGM 缺少节拍元数据，当前结果不能证明画面切点已卡点。"
		} else {
			observation += fmt.Sprintf(
				" 节拍诊断：%v/%v 个画面切点落在真实拍点。",
				beatAlignment["on_beat_cut_count"], beatAlignment["cut_count"],
			)
		}
	}
	return rushestools.ToolResult{
		Status:      map[bool]string{true: "succeeded", false: "validation_failed"}[report.Valid],
		Observation: observation,
		Data: map[string]any{
			"validation_report": report,
			"beat_alignment":    beatAlignment,
		},
	}, nil
}

func (service *Service) toolInspectTimeline(
	ctx context.Context,
	draftID string,
	_ rushestools.TimelineInspectInput,
) (rushestools.ToolResult, error) {
	document, err := timeline.Latest(ctx, service.database, draftID)
	if errors.Is(err, storage.ErrNotFound) {
		return rushestools.ToolResult{
			Status:      "succeeded",
			Observation: "当前草稿尚无时间线；请先选择素材并创建初版时间线。",
			Data: map[string]any{
				"timeline_exists": false,
				"fps":             timeline.DefaultFPS,
				"duration_frames": 0,
				"tracks":          []map[string]any{},
			},
		}, nil
	}
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	tracks := make([]map[string]any, 0, len(document.Tracks))
	for _, track := range document.Tracks {
		clips := make([]map[string]any, 0, len(track.Clips))
		for _, clip := range track.Clips {
			clipData := map[string]any{
				"timeline_clip_id":     clip.TimelineClipID,
				"asset_id":             clip.AssetID,
				"asset_kind":           clip.AssetKind,
				"role":                 clip.Role,
				"timeline_start_frame": clip.TimelineStartFrame,
				"timeline_end_frame":   clip.TimelineEndFrame,
				"source_start_frame":   clip.SourceStartFrame,
				"source_end_frame":     clip.SourceEndFrame,
				"text":                 clip.Text,
			}
			if len(clip.Effects) > 0 {
				clipData["effects"] = clip.Effects
			}
			if len(clip.Metadata) > 0 {
				clipData["metadata"] = clip.Metadata
			}
			clips = append(clips, clipData)
		}
		tracks = append(tracks, map[string]any{
			"track_id": track.TrackID, "track_type": track.TrackType,
			"muted": track.Muted, "locked": track.Locked, "clips": clips,
		})
	}
	return rushestools.ToolResult{
		Status: "succeeded", Observation: timeline.Inspect(document),
		Data: map[string]any{
			"timeline_exists": true,
			"fps":             document.FPS, "duration_frames": document.DurationFrames, "tracks": tracks,
			"audio_layout":   audioLayoutData(document),
			"beat_alignment": beatAlignmentData(document),
		},
	}, nil
}

func audioLayoutData(document timeline.Document) map[string]any {
	bgmClips := []timeline.Clip{}
	sfxClips := []timeline.Clip{}
	for _, track := range document.Tracks {
		switch track.TrackID {
		case "bgm":
			bgmClips = append(bgmClips, track.Clips...)
		case "sfx":
			sfxClips = append(sfxClips, track.Clips...)
		}
	}
	bgmEnd := 0
	bgmRanges := make([]map[string]int, 0, len(bgmClips))
	for _, clip := range bgmClips {
		bgmEnd = max(bgmEnd, clip.TimelineEndFrame)
		bgmRanges = append(bgmRanges, map[string]int{
			"start_frame": clip.TimelineStartFrame, "end_frame": clip.TimelineEndFrame,
		})
	}
	sfxRanges := make([]map[string]any, 0, len(sfxClips))
	sfxWithoutBGM := []string{}
	for _, sfx := range sfxClips {
		overlapsBGM := false
		for _, bgm := range bgmClips {
			if sfx.TimelineStartFrame < bgm.TimelineEndFrame && bgm.TimelineStartFrame < sfx.TimelineEndFrame {
				overlapsBGM = true
				break
			}
		}
		sfxRanges = append(sfxRanges, map[string]any{
			"timeline_clip_id": sfx.TimelineClipID,
			"start_frame":      sfx.TimelineStartFrame, "end_frame": sfx.TimelineEndFrame,
			"overlaps_bgm": overlapsBGM,
		})
		if len(bgmClips) > 0 && !overlapsBGM {
			sfxWithoutBGM = append(sfxWithoutBGM, sfx.TimelineClipID)
		}
	}
	warnings := []string{}
	if len(bgmClips) > 0 && bgmEnd < document.DurationFrames {
		warnings = append(warnings, fmt.Sprintf(
			"BGM 在 %d 帧结束，时间线到 %d 帧，尾部没有音乐覆盖",
			bgmEnd, document.DurationFrames,
		))
	}
	if len(sfxWithoutBGM) > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"SFX %s 没有与 BGM 重叠，无法作为音乐点缀",
			strings.Join(sfxWithoutBGM, ", "),
		))
	}
	return map[string]any{
		"bgm_ranges": bgmRanges, "sfx_ranges": sfxRanges,
		"bgm_coverage_end_frame": bgmEnd, "sfx_without_bgm": sfxWithoutBGM,
		"warnings": warnings,
	}
}

func (service *Service) toolAskUser(
	ctx context.Context,
	draftID string,
	input rushestools.AskUserInput,
	pending map[string]any,
) (rushestools.ToolResult, error) {
	draft, err := storage.GetDraft(ctx, service.database.Read(), draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	decisionID := randomID("decision")
	options := make([]map[string]any, 0, len(input.Options))
	for _, option := range input.Options {
		options = append(options, map[string]any{
			"option_id": option.OptionID, "label": option.Label, "description": option.Description,
		})
	}
	blocking := true
	if input.Blocking != nil {
		blocking = *input.Blocking
	}
	allowFreeText := true
	if input.AllowFreeText != nil {
		allowFreeText = *input.AllowFreeText
	}
	var pendingPayload any
	var pendingStatus any
	if len(pending) > 0 {
		pendingPayload = pending
		pendingStatus = "pending"
	}
	result, err := reducer.Apply(ctx, service.database, []contracts.Event{{
		Type: "DecisionCreated", DraftID: draftID,
		Payload: map[string]any{
			"decision_id": decisionID, "scope_type": "draft", "type": "generic",
			"question": input.Question, "options": options, "blocking": blocking,
			"allow_free_text": allowFreeText, "pending_tool_call": pendingPayload,
			"pending_tool_call_status": pendingStatus,
		},
	}}, reducer.Options{Actor: contracts.ActorAgent, BaseVersion: &draft.StateVersion})
	if err != nil || result.Status != reducer.StatusApplied {
		return rushestools.ToolResult{}, errors.Join(err, fmt.Errorf("reducer status: %s", result.Status))
	}
	return rushestools.ToolResult{
		Status: "waiting", Observation: "等待用户回答", Data: map[string]any{"decision_id": decisionID},
	}, nil
}

func (service *Service) toolDecisionAnswer(
	ctx context.Context,
	draftID string,
	input rushestools.DecisionAnswerInput,
) (rushestools.ToolResult, error) {
	draft, err := storage.GetDraft(ctx, service.database.Read(), draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	result, err := reducer.Apply(ctx, service.database, []contracts.Event{{
		Type: "DecisionAnswered", DraftID: draftID,
		Payload: map[string]any{
			"decision_id": input.DecisionID, "scope_type": "draft",
			"answer": map[string]any{
				"option_id": input.OptionID, "free_text": input.FreeText,
				"payload": input.Payload, "answered_via": "agent",
			},
		},
	}}, reducer.Options{Actor: contracts.ActorUser, BaseVersion: &draft.StateVersion})
	if err != nil || result.Status != reducer.StatusApplied {
		return rushestools.ToolResult{}, errors.Join(err, fmt.Errorf("reducer status: %s", result.Status))
	}
	return rushestools.ToolResult{Status: "succeeded", Observation: "决策已回答"}, nil
}

func (service *Service) toolEnqueueRender(
	ctx context.Context,
	draftID, kind string,
) (rushestools.ToolResult, error) {
	draft, err := storage.GetDraft(ctx, service.database.Read(), draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	if draft.TimelineCurrentVersion == nil || !draft.TimelineValidated {
		return rushestools.ToolResult{}, errors.New("当前时间线尚未验证")
	}
	baseIdempotencyKey := fmt.Sprintf("%s:%s:%d", kind, draftID, *draft.TimelineCurrentVersion)
	idempotencyKey := baseIdempotencyKey
	retryOfJobID := ""
	if existing, found, err := service.findRenderJob(ctx, kind, baseIdempotencyKey, true); err != nil {
		return rushestools.ToolResult{}, err
	} else if found {
		if existing.Status != "failed" && existing.Status != "cancelled" {
			return renderJobResult(kind, existing.ID, existing.Status), nil
		}
		retryOfJobID = existing.ID
		idempotencyKey = fmt.Sprintf("%s:retry:%s", baseIdempotencyKey, existing.ID)
	}
	jobID := randomID("job")
	jobPayload := map[string]any{"timeline_version": *draft.TimelineCurrentVersion}
	if retryOfJobID != "" {
		jobPayload["retry_of_job_id"] = retryOfJobID
	}
	result, err := reducer.Apply(ctx, service.database, []contracts.Event{{
		Type: "JobEnqueued", DraftID: draftID,
		Payload: map[string]any{
			"job_id": jobID, "kind": kind, "requested_by_draft_id": draftID,
			"idempotency_key": idempotencyKey,
			"job_payload":     jobPayload,
			"next_run_at":     time.Now().UTC().Format(time.RFC3339Nano), "priority": 30,
		},
	}}, reducer.Options{Actor: contracts.ActorAgent})
	if err != nil || result.Status != reducer.StatusApplied {
		if existing, found, lookupErr := service.findRenderJob(ctx, kind, idempotencyKey, false); lookupErr != nil {
			return rushestools.ToolResult{}, errors.Join(err, lookupErr)
		} else if found {
			return renderJobResult(kind, existing.ID, existing.Status), nil
		}
		return rushestools.ToolResult{}, errors.Join(err, fmt.Errorf("reducer status: %s", result.Status))
	}
	return renderJobResult(kind, jobID, "pending"), nil
}

type renderJobRef struct {
	ID     string
	Status string
}

func (service *Service) findRenderJob(
	ctx context.Context,
	kind, idempotencyKey string,
	includeRetries bool,
) (renderJobRef, bool, error) {
	query := "SELECT job_id, status FROM jobs WHERE kind=? AND idempotency_key=? LIMIT 1"
	arguments := []any{kind, idempotencyKey}
	if includeRetries {
		retryPrefix := idempotencyKey + ":retry:"
		query = `SELECT job_id, status FROM jobs
			WHERE kind=? AND (idempotency_key=? OR substr(idempotency_key, 1, length(?))=?)
			ORDER BY rowid DESC LIMIT 1`
		arguments = []any{kind, idempotencyKey, retryPrefix, retryPrefix}
	}
	var job renderJobRef
	err := service.database.Read().QueryRowContext(ctx, query, arguments...).Scan(&job.ID, &job.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return renderJobRef{}, false, nil
	}
	if err != nil {
		return renderJobRef{}, false, err
	}
	return job, true, nil
}

func renderJobResult(kind, jobID, jobStatus string) rushestools.ToolResult {
	status := jobStatus
	observation := kind + " 任务已存在"
	switch jobStatus {
	case "pending", "running":
		status = "queued"
		observation = kind + " 任务已排队"
	case "succeeded":
		observation = kind + " 任务已完成"
	}
	return rushestools.ToolResult{
		Status: status, Observation: observation,
		Data: map[string]any{"job_id": jobID, "job_status": jobStatus},
	}
}

func (service *Service) toolRenderStatus(ctx context.Context, draftID string) (rushestools.ToolResult, error) {
	draft, err := storage.GetDraft(ctx, service.database.Read(), draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	return rushestools.ToolResult{
		Status: "succeeded", Observation: "已读取渲染状态",
		Data: map[string]any{
			"preview_id": draft.PreviewCurrentID, "export_id": draft.ExportCurrentID,
			"running_jobs": draft.RunningJobs,
		},
	}, nil
}

func (service *Service) toolInspectPreview(
	ctx context.Context,
	draftID string,
	input rushestools.RenderInspectInput,
) (rushestools.PreviewInspectionResult, error) {
	var hash string
	var width, height sql.NullInt64
	var fps, duration sql.NullFloat64
	err := service.database.Read().QueryRowContext(ctx, `
		SELECT object_hash,render_width,render_height,render_fps,expected_duration_sec
		FROM previews WHERE preview_id=? AND draft_id=?`, input.PreviewID, draftID).Scan(
		&hash, &width, &height, &fps, &duration,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return rushestools.PreviewInspectionResult{}, storage.ErrNotFound
	}
	if err != nil {
		return rushestools.PreviewInspectionResult{}, err
	}
	path, err := service.database.Paths.ObjectPath(hash)
	if err != nil {
		return rushestools.PreviewInspectionResult{}, err
	}
	inspection, err := media.InspectVideo(ctx, path, media.ExpectedVideo{
		Width: int(width.Int64), Height: int(height.Int64),
		FPS: fps.Float64, DurationSec: duration.Float64,
	}, input.Checks)
	if err != nil {
		return rushestools.PreviewInspectionResult{}, err
	}
	issues := make([]map[string]interface{}, 0, len(inspection.Issues))
	for _, issue := range inspection.Issues {
		issues = append(issues, map[string]interface{}{
			"check": issue.Check, "severity": issue.Severity, "message": issue.Message,
		})
	}
	return rushestools.PreviewInspectionResult{
		Summary: inspection.Summary, Degraded: inspection.Degraded, Issues: issues,
	}, nil
}

func numericValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	default:
		return 0, false
	}
}
