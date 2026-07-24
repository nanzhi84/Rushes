package agentexec

import (
	"context"
	"errors"
	"fmt"
	"math"
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
		usage += "需要删除多个候选时，按 timeline_start_frame 从大到小分别调用 timeline.delete(kind=delete_range)，每次只删除一个连续范围，避免前序波纹删除改变后续坐标。"
	}
	return rushestools.SpeechPauseAnalysisResult{
		AssetID: selected.ID, TimelineClipID: input.TimelineClipID,
		TimelineFPS: timeline.DefaultFPS, DurationFrames: analysis.DurationFrames,
		Pauses: pauses, AnalysisMethod: analysis.AnalysisMethod, Truncated: analysis.Truncated,
		UsageNote: usage,
	}, nil
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

func AbsInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
