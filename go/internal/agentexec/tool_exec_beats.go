package agentexec

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/media"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
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
	return exec.ensureBeatAnalysis(ctx, draftID, input)
}

// EnsureBeatAnalysis is the Harness entry point used when a BGM role becomes
// concrete. It persists a complete grid and reuses it across drafts by content
// hash; the model never needs to call or copy the old analysis tool.
func (exec *Executor) EnsureBeatAnalysis(
	ctx context.Context,
	draftID, assetID string,
) (rushestools.AudioBeatAnalysisResult, error) {
	return exec.ensureBeatAnalysis(ctx, draftID, rushestools.AudioBeatAnalysisInput{
		AssetID: assetID, MaxBeats: 2000, WaveformPoints: media.DefaultWaveformPoints,
	})
}

func (exec *Executor) ensureBeatAnalysis(
	ctx context.Context,
	draftID string,
	input rushestools.AudioBeatAnalysisInput,
) (result rushestools.AudioBeatAnalysisResult, returnedErr error) {
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
	maxBeats := input.MaxBeats
	if maxBeats <= 0 {
		maxBeats = 512
	}
	maxBeats = min(maxBeats, 2000)
	waveformPoints := input.WaveformPoints
	if waveformPoints <= 0 {
		waveformPoints = media.DefaultWaveformPoints
	}
	identity, err := newAssetAnalysisIdentity(
		selected.Hash, BeatAnalysisType, beatAnalyzerVersion,
		map[string]any{
			"max_beats": maxBeats, "timeline_fps": timeline.DefaultFPS,
			"waveform_points": waveformPoints,
		},
		assetAnalysisOutputSchema,
	)
	if err != nil {
		return rushestools.AudioBeatAnalysisResult{}, err
	}
	startedAt := time.Now()
	cacheHit := false
	defer func() { logAssetAnalysis(identity, cacheHit, startedAt, returnedErr) }()
	if cached, cacheErr := exec.cachedAssetAnalysis(ctx, identity); cacheErr == nil {
		result, returnedErr = decodeAssetAnalysisResult[rushestools.AudioBeatAnalysisResult](cached)
		if returnedErr == nil {
			result.AssetID, result.AnalysisID, result.CacheHit = selected.ID, identity.ID, true
			cacheHit = true
		}
		return result, returnedErr
	} else if !errors.Is(cacheErr, storage.ErrNotFound) {
		return rushestools.AudioBeatAnalysisResult{}, cacheErr
	}
	returnedErr = exec.withAnalysisSingleflight(identity, func() error {
		if cached, cacheErr := exec.cachedAssetAnalysis(ctx, identity); cacheErr == nil {
			result, returnedErr = decodeAssetAnalysisResult[rushestools.AudioBeatAnalysisResult](cached)
			if returnedErr == nil {
				result.AssetID, result.AnalysisID, result.CacheHit = selected.ID, identity.ID, true
				cacheHit = true
			}
			return returnedErr
		} else if !errors.Is(cacheErr, storage.ErrNotFound) {
			return cacheErr
		}
		built, buildErr := exec.buildBeatAnalysis(ctx, *selected, maxBeats, waveformPoints)
		if buildErr != nil {
			return buildErr
		}
		built.AnalysisID = identity.ID
		built.CacheHit = false
		encoded, encodeErr := assetAnalysisResultMap(built)
		if encodeErr != nil {
			return encodeErr
		}
		if persistErr := exec.persistAssetAnalyses(ctx, []reducer.AssetAnalysisRow{{
			ID: identity.ID, AssetContentHash: identity.AssetContentHash,
			AnalysisType: identity.AnalysisType, AnalyzerVersion: identity.AnalyzerVersion,
			NormalizedOptionsJSON: identity.NormalizedOptionsJSON,
			OutputSchemaVersion:   identity.OutputSchemaVersion, Result: encoded,
		}}); persistErr != nil {
			return persistErr
		}
		result = built
		return nil
	})
	return result, returnedErr
}

func (exec *Executor) buildBeatAnalysis(
	ctx context.Context,
	selected storage.Asset,
	maxBeats, waveformPoints int,
) (rushestools.AudioBeatAnalysisResult, error) {
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
	grid, err := media.AnalyzeBeatGrid(ctx, source, timeline.DefaultFPS, maxBeats)
	if err != nil {
		return rushestools.AudioBeatAnalysisResult{}, err
	}
	durationFrames := int(math.Round(probe.DurationSec * timeline.DefaultFPS))
	waveform, err := media.AnalyzeWaveformEnvelope(
		ctx,
		source,
		timeline.DefaultFPS,
		durationFrames,
		waveformPoints,
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
	analysis, err := exec.ensureSpeechPauseAnalysis(ctx, *selected, input)
	if err != nil {
		return rushestools.SpeechPauseAnalysisResult{}, err
	}
	pauses := make([]rushestools.SpeechPauseCandidate, 0, len(analysis.Pauses))
	for _, pause := range analysis.Pauses {
		candidate := pause
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
		AnalysisID: analysis.AnalysisID,
		AssetID:    selected.ID, TimelineClipID: input.TimelineClipID,
		TimelineFPS: timeline.DefaultFPS, DurationFrames: analysis.DurationFrames,
		Pauses: pauses, AnalysisMethod: analysis.AnalysisMethod, Truncated: analysis.Truncated,
		UsageNote: usage, CacheHit: analysis.CacheHit,
	}, nil
}

func (exec *Executor) ensureSpeechPauseAnalysis(
	ctx context.Context,
	asset storage.Asset,
	input rushestools.SpeechPauseAnalysisInput,
) (result rushestools.SpeechPauseAnalysisResult, returnedErr error) {
	options := normalizePersistentSpeechPauseOptions(input)
	analyzerVersion := speechPauseAnalyzerVersion
	identityOptions := map[string]any{
		"include_boundaries": options.IncludeBoundaries,
		"keep_edge_frames":   options.KeepEdgeFrames,
		"max_pauses":         options.MaxPauses,
		"min_pause_frames":   options.MinPauseFrames,
		"threshold_db":       options.ThresholdDB,
		"timeline_fps":       timeline.DefaultFPS,
	}
	var transcript *storage.Transcript
	if cached, err := storage.LatestTranscript(ctx, exec.database.Read(), asset.ID); err == nil {
		transcript = &cached
		analyzerVersion = "transcript-vad-v1/" + cached.ProviderID
		identityOptions["transcript_id"] = cached.ID
		analyses, lookupErr := storage.LatestAssetAnalysesForContentHashes(
			ctx, exec.database.Read(), []string{asset.Hash}, TranscriptAnalysisType,
		)
		if lookupErr != nil {
			return rushestools.SpeechPauseAnalysisResult{}, lookupErr
		}
		if analysis, exists := analyses[asset.Hash]; exists {
			analyzerVersion = "transcript-vad-v1/" + analysis.AnalyzerVersion
			delete(identityOptions, "transcript_id")
			identityOptions["transcript_analysis_id"] = analysis.ID
		}
	} else if !errors.Is(err, storage.ErrNotFound) {
		return rushestools.SpeechPauseAnalysisResult{}, err
	}
	identity, err := newAssetAnalysisIdentity(
		asset.Hash, SpeechPauseAnalysisType, analyzerVersion,
		identityOptions, assetAnalysisOutputSchema,
	)
	if err != nil {
		return rushestools.SpeechPauseAnalysisResult{}, err
	}
	startedAt := time.Now()
	cacheHit := false
	defer func() { logAssetAnalysis(identity, cacheHit, startedAt, returnedErr) }()
	loadCached := func() (bool, error) {
		cached, cacheErr := exec.cachedAssetAnalysis(ctx, identity)
		if errors.Is(cacheErr, storage.ErrNotFound) {
			return false, nil
		}
		if cacheErr != nil {
			return false, cacheErr
		}
		decoded, decodeErr := decodeAssetAnalysisResult[rushestools.SpeechPauseAnalysisResult](cached)
		if decodeErr != nil {
			return false, decodeErr
		}
		decoded.AssetID, decoded.AnalysisID, decoded.CacheHit = asset.ID, identity.ID, true
		result, cacheHit = decoded, true
		return true, nil
	}
	if hit, cacheErr := loadCached(); hit || cacheErr != nil {
		return result, cacheErr
	}
	returnedErr = exec.withAnalysisSingleflight(identity, func() error {
		if hit, cacheErr := loadCached(); hit || cacheErr != nil {
			return cacheErr
		}
		var built rushestools.SpeechPauseAnalysisResult
		if transcript != nil {
			pauses, decodeErr := DecodeSpeechPauses(transcript.VADSegments)
			if decodeErr != nil {
				return decodeErr
			}
			durationFrames := assetDurationFrames(asset)
			candidates := make([]rushestools.SpeechPauseCandidate, 0, len(pauses))
			for _, pause := range pauses {
				if !options.IncludeBoundaries && (pause.StartFrame <= 0 ||
					durationFrames > 0 && pause.EndFrame >= durationFrames) {
					continue
				}
				candidates = append(candidates, rushestools.SpeechPauseCandidate{
					SourceStartFrame: pause.StartFrame, SourceEndFrame: pause.EndFrame,
					DeleteStartFrame: pause.DeleteStart, DeleteEndFrame: pause.DeleteEnd,
				})
			}
			truncated := len(candidates) > options.MaxPauses
			if truncated {
				candidates = candidates[:options.MaxPauses]
			}
			built = rushestools.SpeechPauseAnalysisResult{
				AssetID: asset.ID, TimelineFPS: timeline.DefaultFPS,
				DurationFrames: durationFrames, Pauses: candidates,
				AnalysisMethod: analyzerVersion, Truncated: truncated,
			}
		} else {
			source, _, resolveErr := media.ResolveAssetSource(ctx, exec.database, asset.ID)
			if resolveErr != nil {
				return resolveErr
			}
			analysis, analyzeErr := media.AnalyzeSpeechPauses(
				ctx, source, timeline.DefaultFPS, options,
			)
			if analyzeErr != nil {
				return analyzeErr
			}
			candidates := make([]rushestools.SpeechPauseCandidate, 0, len(analysis.Pauses))
			for _, pause := range analysis.Pauses {
				candidates = append(candidates, rushestools.SpeechPauseCandidate{
					SourceStartFrame: pause.SourceStartFrame,
					SourceEndFrame:   pause.SourceEndFrame,
					DeleteStartFrame: pause.DeleteStartFrame,
					DeleteEndFrame:   pause.DeleteEndFrame,
				})
			}
			built = rushestools.SpeechPauseAnalysisResult{
				AssetID: asset.ID, TimelineFPS: timeline.DefaultFPS,
				DurationFrames: analysis.DurationFrames, Pauses: candidates,
				AnalysisMethod: analysis.AnalysisMethod, Truncated: analysis.Truncated,
			}
		}
		built.AnalysisID = identity.ID
		built.CacheHit = false
		encoded, encodeErr := assetAnalysisResultMap(built)
		if encodeErr != nil {
			return encodeErr
		}
		if persistErr := exec.persistAssetAnalyses(ctx, []reducer.AssetAnalysisRow{{
			ID: identity.ID, AssetContentHash: identity.AssetContentHash,
			AnalysisType: identity.AnalysisType, AnalyzerVersion: identity.AnalyzerVersion,
			NormalizedOptionsJSON: identity.NormalizedOptionsJSON,
			OutputSchemaVersion:   identity.OutputSchemaVersion, Result: encoded,
		}}); persistErr != nil {
			return persistErr
		}
		result = built
		return nil
	})
	return result, returnedErr
}

func normalizePersistentSpeechPauseOptions(
	input rushestools.SpeechPauseAnalysisInput,
) media.SpeechPauseOptions {
	threshold := input.ThresholdDB
	if threshold == 0 {
		threshold = -35
	}
	threshold = math.Max(-80, math.Min(-10, threshold))
	minimum := input.MinPauseFrames
	if minimum <= 0 {
		minimum = max(4, int(math.Round(float64(timeline.DefaultFPS)*0.18)))
	}
	keep := input.KeepEdgeFrames
	if keep < 0 {
		keep = 0
	}
	if keep == 0 {
		keep = max(1, int(math.Round(float64(timeline.DefaultFPS)*0.06)))
	}
	maximum := input.MaxPauses
	if maximum <= 0 {
		maximum = 200
	}
	maximum = min(maximum, 1000)
	return media.SpeechPauseOptions{
		ThresholdDB: threshold, MinPauseFrames: minimum,
		KeepEdgeFrames: keep, MaxPauses: maximum,
		IncludeBoundaries: input.IncludeBoundaries,
	}
}

func assetDurationFrames(asset storage.Asset) int {
	duration, _ := NumericValue(asset.Probe["duration_sec"])
	return max(0, int(math.Round(duration*timeline.DefaultFPS)))
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
