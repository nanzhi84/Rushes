package agentexec

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/nanzhi84/Rushes/go/internal/media"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

const AudioBeatPhaseNote = "强拍来自频谱通量瞬态；每 4 拍网格以强拍贴合度推断 4/4 小节相位，仍可由剪辑者微调；拍点、强拍和 downbeat 只是音频结构证据，不能自动等同于高潮或好剪辑。"

const AudioWaveformUsageNote = "waveform.sample_frames 与 samples 一一对应；前者是按 timeline_fps 标尺表示的素材内 RMS 窗口起始帧，后者是该点 0-100 原始响度。本结果返回本次请求的完整压缩波形；WorldState 只常驻最多 24 点摘要。"

func (exec *Executor) toolAnalyzeAudioBeats(
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
	assets, err := storage.ListDraftAssets(ctx, exec.database.Read(), draftID)
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
	source, _, err := media.ResolveAssetSource(ctx, exec.database, selected.ID)
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
		PhaseNote:         AudioBeatPhaseNote,
		WaveformUsageNote: AudioWaveformUsageNote,
		Waveform:          waveformToolValue(waveform),
	}, nil
}

func (exec *Executor) toolAnalyzeSpeechPauses(
	ctx context.Context,
	draftID string,
	input rushestools.SpeechPauseAnalysisInput,
) (rushestools.SpeechPauseAnalysisResult, error) {
	assetID := strings.TrimSpace(input.AssetID)
	var timelineClip *timeline.Clip
	if input.TimelineClipID != "" {
		current, err := timeline.Latest(ctx, exec.database, draftID)
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
	assets, err := storage.ListDraftAssets(ctx, exec.database.Read(), draftID)
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
	source, _, err := media.ResolveAssetSource(ctx, exec.database, selected.ID)
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

func (exec *Executor) toolRecutToBeats(
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
		if _, latestErr := timeline.Latest(ctx, exec.database, draftID); errors.Is(latestErr, storage.ErrNotFound) {
			return exec.toolBuildBeatMix(ctx, draftID, input)
		}
		return exec.toolRecutCurrentClipsToBeats(ctx, draftID, input)
	}
	return exec.toolBuildBeatMix(ctx, draftID, input)
}

func (exec *Executor) toolRecutCurrentClipsToBeats(
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

	assets, err := storage.ListDraftAssets(ctx, exec.database.Read(), draftID)
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
	bgmSource, _, err := media.ResolveAssetSource(ctx, exec.database, bgmAsset.ID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	grid, err := media.AnalyzeBeatGrid(ctx, bgmSource, current.FPS, 4096)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	bgmDuration, _ := NumericValue(bgmAsset.Probe["duration_sec"])
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
		sfxDuration, _ := NumericValue(sfxAsset.Probe["duration_sec"])
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
		sfxClipID = RandomID("sfx_beat")
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

	typedOperations := make([]rushestools.TimelineOp, len(operations))
	for index := range operations {
		typedOperations[index] = rushestools.TimelineOp(operations[index])
	}
	result, err := exec.toolApplyPatches(ctx, draftID, rushestools.TimelinePatchBatchInput{Ops: typedOperations})
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

func SourceRangeContains(ranges []BeatMixSourceRange, startFrame, endFrame int) bool {
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

func ChooseBeatMixCuts(everyFour, everyBeat []int, targetFrames, maxClips int) []int {
	if targetFrames <= 0 || maxClips <= 0 {
		return nil
	}
	candidates := BeatCandidatesWithin(everyFour, targetFrames)
	if len(candidates) == 0 {
		candidates = BeatCandidatesWithin(everyBeat, targetFrames)
	}
	return distributeBeatMixCuts(candidates, targetFrames, maxClips)
}

func ChooseAllBeatMixCuts(everyFour, everyBeat []int, targetFrames, clipCount int) []int {
	candidates := BeatCandidatesWithin(everyFour, targetFrames)
	// 四拍网格不足以为每个素材提供一个切点时，回退到完整拍点网格。
	// 只有显式 use_all_video_assets 才提高密度，避免默认规划为了短素材过度切碎。
	if len(candidates)+1 < clipCount {
		candidates = BeatCandidatesWithin(everyBeat, targetFrames)
	}
	return distributeBeatMixCuts(candidates, targetFrames, clipCount)
}

// chooseCapacityAwareBeatMixCuts keeps one automatic segment per requested
// video while moving the cuts onto real beat markers that each source can
// actually cover. Equal-duration cuts can reject an otherwise feasible mix
// when the final short source is a few frames below the average segment size.
// Prefer the sparser four-beat grid, then fall back to the full beat grid. If
// neither grid has a capacity-feasible assignment, preserve the prior planner
// result so the existing source-selection failure remains specific and useful.
func ChooseCapacityAwareBeatMixCuts(
	everyFour, everyBeat []int,
	targetFrames int,
	capacities []int,
) []int {
	if targetFrames <= 0 || len(capacities) == 0 {
		return nil
	}
	for _, grid := range [][]int{everyFour, everyBeat} {
		candidates := BeatCandidatesWithin(grid, targetFrames)
		if cuts, ok := DistributeCapacityAwareBeatMixCuts(candidates, targetFrames, capacities); ok {
			return cuts
		}
	}
	return ChooseAllBeatMixCuts(everyFour, everyBeat, targetFrames, len(capacities))
}

func DistributeCapacityAwareBeatMixCuts(
	candidates []int,
	targetFrames int,
	capacities []int,
) ([]int, bool) {
	if targetFrames <= 0 || len(capacities) == 0 {
		return nil, false
	}
	totalCapacity := 0
	for _, capacity := range capacities {
		if capacity <= 0 {
			return nil, false
		}
		totalCapacity += capacity
	}
	if totalCapacity < targetFrames || len(capacities) > len(candidates)+1 {
		return nil, false
	}
	if len(capacities) == 1 {
		if capacities[0] < targetFrames {
			return nil, false
		}
		return []int{targetFrames}, true
	}

	type state struct {
		segment int
		cursor  int
	}
	failed := map[state]struct{}{}
	var solve func(segment, cursor int) ([]int, bool)
	solve = func(segment, cursor int) ([]int, bool) {
		if segment == len(capacities)-1 {
			if targetFrames-cursor > 0 && targetFrames-cursor <= capacities[segment] {
				return []int{targetFrames}, true
			}
			return nil, false
		}
		key := state{segment: segment, cursor: cursor}
		if _, known := failed[key]; known {
			return nil, false
		}
		remainingCapacity := 0
		for _, capacity := range capacities[segment+1:] {
			remainingCapacity += capacity
		}
		minimumCut := max(cursor+1, targetFrames-remainingCapacity)
		maximumCut := min(cursor+capacities[segment], targetFrames-1)
		idealCut := int(math.Round(float64(targetFrames*(segment+1)) / float64(len(capacities))))
		options := make([]int, 0)
		for _, candidate := range candidates {
			if candidate >= minimumCut && candidate <= maximumCut {
				options = append(options, candidate)
			}
		}
		sort.SliceStable(options, func(left, right int) bool {
			leftDistance := AbsInt(options[left] - idealCut)
			rightDistance := AbsInt(options[right] - idealCut)
			if leftDistance == rightDistance {
				return options[left] < options[right]
			}
			return leftDistance < rightDistance
		})
		for _, candidate := range options {
			if suffix, ok := solve(segment+1, candidate); ok {
				return append([]int{candidate}, suffix...), true
			}
		}
		failed[key] = struct{}{}
		return nil, false
	}
	return solve(0, 0)
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
			if AbsInt(candidates[index]-ideal) < AbsInt(candidates[selectedIndex]-ideal) {
				selectedIndex = index
			}
		}
		cuts = append(cuts, candidates[selectedIndex])
		previousIndex = selectedIndex
	}
	return append(cuts, targetFrames)
}

func BeatCandidatesWithin(frames []int, targetFrames int) []int {
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

func AbsInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
