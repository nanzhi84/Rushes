package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"

	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/media"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func (service *Service) toolInspectSpeech(
	ctx context.Context,
	draftID string,
	input rushestools.SpeechInspectInput,
) (rushestools.SpeechInspectResult, error) {
	includeWords := input.IncludeWords || strings.TrimSpace(input.Query) != "" ||
		input.SourceStartFrame != nil || input.SourceEndFrame != nil
	asset, timelineClip, err := service.resolveSpeechAsset(ctx, draftID, input.AssetID, input.TimelineClipID)
	if err != nil {
		return rushestools.SpeechInspectResult{}, err
	}
	transcript, cacheHit, err := service.loadOrBuildSpeechTranscript(
		ctx, draftID, asset, strings.TrimSpace(input.Language), input.ForceRefresh, includeWords,
	)
	if err != nil {
		return rushestools.SpeechInspectResult{}, err
	}
	utterances, err := agentexec.DecodeSpeechUtterances(transcript.Utterances)
	if err != nil {
		return rushestools.SpeechInspectResult{}, err
	}
	pauses, err := agentexec.DecodeSpeechPauses(transcript.VADSegments)
	if err != nil {
		return rushestools.SpeechInspectResult{}, err
	}
	pauses = agentexec.ClampSpeechPausesToWordBoundaries(asset.ID, pauses, utterances)

	queryTokens := agentexec.SemanticTokens(input.Query)
	selected := make([]agentexec.SpeechUtterance, 0, len(utterances))
	for _, utterance := range utterances {
		if timelineClip != nil && !agentexec.SourceRangesOverlap(
			utterance.StartFrame, utterance.EndFrame,
			timelineClip.SourceStartFrame, timelineClip.SourceEndFrame,
		) {
			continue
		}
		if input.SourceStartFrame != nil && utterance.EndFrame <= *input.SourceStartFrame ||
			input.SourceEndFrame != nil && utterance.StartFrame >= *input.SourceEndFrame {
			continue
		}
		if len(queryTokens) > 0 && agentexec.SemanticMatchScore(queryTokens, strings.ToLower(utterance.Text)) == 0 {
			continue
		}
		selected = append(selected, utterance)
	}
	limit := input.MaxUtterances
	if limit <= 0 {
		limit = 80
	}
	limit = min(limit, 240)
	truncated := len(selected) > limit
	if truncated {
		selected = selected[:limit]
	}
	wordTotal := 0
	for _, utterance := range utterances {
		wordTotal += len(utterance.Words)
	}
	wordLimit := input.MaxWords
	if wordLimit <= 0 {
		wordLimit = 400
	}
	wordLimit = min(wordLimit, 2000)
	wordCount := 0
	wordsTruncated := false
	evidence := make([]rushestools.SpeechUtteranceEvidence, 0, len(selected))
	for _, utterance := range selected {
		sourceStart, sourceEnd, clamped := utterance.StartFrame, utterance.EndFrame, false
		if timelineClip != nil {
			sourceStart, sourceEnd, clamped = agentexec.ClampSpeechRangeToClip(*timelineClip, sourceStart, sourceEnd)
		}
		item := rushestools.SpeechUtteranceEvidence{
			UtteranceID: utterance.ID, SourceStartFrame: sourceStart,
			SourceEndFrame: sourceEnd, Text: utterance.Text,
			Language: utterance.Language, Emotion: utterance.Emotion, Clamped: clamped,
		}
		if timelineClip != nil {
			if start, end, ok := agentexec.MapSourceRangeToTimelineClip(*timelineClip, utterance.StartFrame, utterance.EndFrame); ok {
				item.TimelineStartFrame, item.TimelineEndFrame = &start, &end
			}
		}
		if includeWords {
			for _, word := range utterance.Words {
				if wordCount >= wordLimit {
					wordsTruncated = true
					break
				}
				wordStart, wordEnd, wordClamped := word.StartFrame, word.EndFrame, false
				if timelineClip != nil {
					wordStart, wordEnd, wordClamped = agentexec.ClampSpeechRangeToClip(*timelineClip, wordStart, wordEnd)
					if wordEnd <= wordStart {
						continue
					}
				}
				wordItem := rushestools.SpeechWordEvidence{
					WordID: word.ID, SourceStartFrame: wordStart,
					SourceEndFrame: wordEnd, Text: word.Text, Punctuation: word.Punctuation,
					Clamped: wordClamped,
				}
				if timelineClip != nil {
					if start, end, ok := agentexec.MapSourceRangeToTimelineClip(*timelineClip, word.StartFrame, word.EndFrame); ok {
						wordItem.TimelineStartFrame, wordItem.TimelineEndFrame = &start, &end
					}
				}
				item.Words = append(item.Words, wordItem)
				wordCount++
			}
		}
		evidence = append(evidence, item)
	}

	includePauses := input.IncludePauses == nil || *input.IncludePauses
	pauseEvidence := []rushestools.SpeechPauseEvidence{}
	if includePauses {
		for _, pause := range pauses {
			if timelineClip != nil && !agentexec.SourceRangesOverlap(
				pause.DeleteStart, pause.DeleteEnd,
				timelineClip.SourceStartFrame, timelineClip.SourceEndFrame,
			) {
				continue
			}
			if input.SourceStartFrame != nil && pause.DeleteEnd <= *input.SourceStartFrame ||
				input.SourceEndFrame != nil && pause.DeleteStart >= *input.SourceEndFrame {
				continue
			}
			deleteStart, deleteEnd, clamped := pause.DeleteStart, pause.DeleteEnd, false
			if timelineClip != nil {
				deleteStart, deleteEnd, clamped = agentexec.ClampSpeechRangeToClip(*timelineClip, deleteStart, deleteEnd)
			}
			item := rushestools.SpeechPauseEvidence{
				PauseID: pause.ID, SourceStartFrame: pause.StartFrame, SourceEndFrame: pause.EndFrame,
				DeleteStartFrame: deleteStart, DeleteEndFrame: deleteEnd,
				DurationFrames:       pause.EndFrame - pause.StartFrame,
				DeleteDurationFrames: deleteEnd - deleteStart,
				DetectionMethod:      pause.Method, Clamped: clamped,
			}
			agentexec.PopulateSpeechPauseContext(&item, utterances)
			if timelineClip != nil {
				if start, end, ok := agentexec.MapSourceRangeToTimelineClip(*timelineClip, deleteStart, deleteEnd); ok {
					item.TimelineStartFrame, item.TimelineEndFrame = &start, &end
				}
			}
			pauseEvidence = append(pauseEvidence, item)
		}
	}
	pauseEvidence, pauseTotal, pausesTruncated := agentexec.RankSpeechPauseEvidence(pauseEvidence, input.MaxPauses)
	includeSimilar := input.IncludeSimilar == nil || *input.IncludeSimilar
	similar := []rushestools.SpeechSimilarityEvidence{}
	repetitions := []rushestools.SpeechRepetitionEvidence{}
	repetitionTotal, repetitionsTruncated := 0, false
	if includeSimilar {
		similar = agentexec.SimilarSpeechPairs(utterances, agentexec.MaxSimilarPairs)
		repetitions = agentexec.IntraUtteranceSpeechRepetitions(asset.ID, selected, int(^uint(0)>>1))
		repetitionTotal = len(repetitions)
		if repetitionsTruncated = len(repetitions) > agentexec.MaxSimilarPairs; repetitionsTruncated {
			repetitions = repetitions[:agentexec.MaxSimilarPairs]
		}
	}
	allShortFragments := agentexec.ShortLeadingSpeechFragments(asset.ID, utterances, pauses, int(^uint(0)>>1))
	selectedUtteranceIDs := make(map[string]struct{}, len(selected))
	for _, utterance := range selected {
		selectedUtteranceIDs[utterance.ID] = struct{}{}
	}
	shortFragments := make([]rushestools.SpeechFragmentEvidence, 0, len(allShortFragments))
	for _, fragment := range allShortFragments {
		if _, selected := selectedUtteranceIDs[fragment.UtteranceID]; selected {
			shortFragments = append(shortFragments, fragment)
		}
	}
	shortFragmentTotal := len(shortFragments)
	shortFragmentsTruncated := shortFragmentTotal > agentexec.MaxSimilarPairs
	if shortFragmentsTruncated {
		shortFragments = shortFragments[:agentexec.MaxSimilarPairs]
	}
	usageNote := "utterance_id、word_id、pause_id 与帧坐标是客观证据；传入 timeline_clip_id 时 clamped=true 表示该证据已按当前 clip 裁剪，utterance/word 文本与 pause 声学边界保持完整，只有帧坐标、删除范围与词列表取落在 clip 内的子集；similarity、intra_utterance_repetitions 与 short_speech_fragments 只是单句、连续台词块、句内重复或停顿前短语音岛的证据，不代表必须删除。" +
		"intra_utterance_repetitions 会优先列出全部相邻同词证据，并自带 repetition_id 与前后两段精确 word_id（其中数字拆词和叠词也可能是正常表达）；模型应结合 context_text 自主逐项判断并一次性通过 repetition_decisions 提交 remove_earlier/remove_later/preserve；" +
		"pauses 默认按可安全删除时长从长到短排列，previous_context、next_context 与 joined_context 用于判断气口是否影响表达；口播删剪时应对可见的显著候选一次性通过 pause_decisions 提交 remove/preserve，工具不会按时长替模型判断；" +
		"不属于现成 repetition/fragment 证据的句内卡壳或半句重说，才需要设置 include_words=true 并把连续 word_id 范围传给 timeline.edit_talking_head；" +
		"short_speech_fragments 应一次性通过 short_fragment_decisions 提交 remove/preserve 决定；其中 earlier_take_before_repeated_phrase_restart 暴露共同短语、分叉尾部与停顿共同组成的完整较早一遍说法，避免只删尾部后留下半句；模型也可继续按 query 或源帧范围检索。"
	if includeWords && wordTotal == 0 {
		usageNote += "当前持久化索引没有词级时间戳（例如来源仅为 SRT）；不能猜 word_id，可使用逐句证据或配置带词时间戳的 ASR 后重新转写。"
	}
	return rushestools.SpeechInspectResult{
		TranscriptID: transcript.ID, AssetID: asset.ID, TimelineClipID: input.TimelineClipID,
		TimelineFPS: timeline.DefaultFPS, ProviderID: transcript.ProviderID, CacheHit: cacheHit,
		Utterances: evidence, UtteranceTotal: len(utterances), WordTotal: wordTotal,
		WordsTruncated: wordsTruncated, Pauses: pauseEvidence,
		PauseTotal: pauseTotal, PausesTruncated: pausesTruncated,
		SimilarPairs: similar, Repetitions: repetitions,
		RepetitionTotal: repetitionTotal, RepetitionsTruncated: repetitionsTruncated,
		ShortFragments: shortFragments, ShortFragmentTotal: shortFragmentTotal,
		ShortFragmentsTruncated: shortFragmentsTruncated,
		Truncated:               truncated,
		UsageNote:               usageNote,
	}, nil
}

func (service *Service) resolveSpeechAsset(
	ctx context.Context,
	draftID, requestedAssetID, timelineClipID string,
) (storage.Asset, *timeline.Clip, error) {
	assetID := strings.TrimSpace(requestedAssetID)
	var selectedClip *timeline.Clip
	if strings.TrimSpace(timelineClipID) != "" {
		document, err := timeline.Latest(ctx, service.database, draftID)
		if err != nil {
			return storage.Asset{}, nil, err
		}
		for trackIndex := range document.Tracks {
			for clipIndex := range document.Tracks[trackIndex].Clips {
				clip := document.Tracks[trackIndex].Clips[clipIndex]
				if clip.TimelineClipID == timelineClipID {
					selectedClip = &clip
					break
				}
			}
		}
		if selectedClip == nil || selectedClip.AssetID == "" {
			return storage.Asset{}, nil, errors.New("speech.inspect 的 timeline_clip_id 不存在或不是素材片段")
		}
		if assetID != "" && assetID != selectedClip.AssetID {
			return storage.Asset{}, nil, errors.New("speech.inspect 的 asset_id 与 timeline_clip_id 不一致")
		}
		assetID = selectedClip.AssetID
	}
	if assetID == "" {
		return storage.Asset{}, nil, errors.New("speech.inspect 至少需要 asset_id 或 timeline_clip_id")
	}
	assets, err := storage.ListDraftAssets(ctx, service.database.Read(), draftID)
	if err != nil {
		return storage.Asset{}, nil, err
	}
	for _, asset := range assets {
		if asset.ID != assetID {
			continue
		}
		if !asset.Usable || asset.Kind != "video" && asset.Kind != "audio" {
			return storage.Asset{}, nil, errors.New("speech.inspect 只支持当前草稿中可用的音频或视频素材")
		}
		return asset, selectedClip, nil
	}
	return storage.Asset{}, nil, errors.New("speech.inspect 的素材不属于当前草稿")
}

func (service *Service) loadOrBuildSpeechTranscript(
	ctx context.Context,
	draftID string,
	asset storage.Asset,
	language string,
	forceRefresh bool,
	requireWordSchema bool,
) (storage.Transcript, bool, error) {
	if !forceRefresh {
		if cached, err := storage.LatestTranscript(ctx, service.database.Read(), asset.ID); err == nil {
			if !requireWordSchema || agentexec.TranscriptHasWordSchema(cached.Utterances) ||
				cached.ProviderID == "sidecar-srt" || service.speechRecognizer == nil {
				return cached, true, nil
			}
		} else if !errors.Is(err, storage.ErrNotFound) {
			return storage.Transcript{}, false, err
		}
	}
	source, _, err := media.ResolveAssetSource(ctx, service.database, asset.ID)
	if err != nil {
		return storage.Transcript{}, false, err
	}
	probe, err := media.ProbeFile(ctx, source)
	if err != nil {
		return storage.Transcript{}, false, err
	}
	if !probe.HasAudio {
		return storage.Transcript{}, false, errors.New("speech.inspect 的素材没有可转写音轨")
	}
	durationFrames := max(1, int(math.Round(probe.DurationSec*timeline.DefaultFPS)))
	pauseAnalysis, err := media.AnalyzeSpeechPauses(ctx, source, timeline.DefaultFPS, media.SpeechPauseOptions{
		ThresholdDB: -35, MinPauseFrames: 5, KeepEdgeFrames: 2,
		MaxPauses: 1000, IncludeBoundaries: false,
	})
	if err != nil {
		return storage.Transcript{}, false, err
	}
	pauses := make([]agentexec.SpeechPause, 0, len(pauseAnalysis.Pauses))
	for _, pause := range pauseAnalysis.Pauses {
		pauses = append(pauses, agentexec.SpeechPause{
			ID:         agentexec.StableSpeechID("pause", asset.ID, pause.SourceStartFrame, pause.SourceEndFrame, ""),
			StartFrame: pause.SourceStartFrame, EndFrame: pause.SourceEndFrame,
			DeleteStart: pause.DeleteStartFrame, DeleteEnd: pause.DeleteEndFrame,
			Method: "rms_silence",
		})
	}

	providerID := ""
	utterances := []agentexec.SpeechUtterance{}
	if sidecar := media.FindSidecarSRT(source); sidecar != "" {
		cues, parseErr := media.ParseSRT(sidecar, timeline.DefaultFPS)
		if parseErr != nil {
			return storage.Transcript{}, false, parseErr
		}
		providerID = "sidecar-srt"
		for _, cue := range cues {
			utterances = append(utterances, agentexec.SpeechUtterance{
				ID:         agentexec.StableSpeechID("utt", asset.ID, cue.StartFrame, cue.EndFrame, cue.Text),
				StartFrame: cue.StartFrame, EndFrame: cue.EndFrame, Text: cue.Text, Language: language,
			})
		}
	} else {
		if service.speechRecognizer == nil {
			return storage.Transcript{}, false, errors.New(
				"素材没有同名 SRT，且当前环境未配置云端 ASR；请配置 RUSHES_DASHSCOPE_API_KEY 后重试",
			)
		}
		chunks := agentexec.BuildASRChunks(durationFrames, pauses, agentexec.MaxASRChunkFrames)
		for index, chunk := range chunks {
			service.hub.Record(draftID, StreamEvent{
				"type": TurnStreamSubagentProgress, "tool": "speech.inspect",
				"asset_id": asset.ID, "note": fmt.Sprintf("ASR 转写 %d/%d", index+1, len(chunks)),
				"completed": index, "total": len(chunks),
			})
			path, extractErr := media.ExtractAudioSegmentMP3(
				ctx, service.database.Paths.Temporary, source, chunk[0], chunk[1], timeline.DefaultFPS,
			)
			if extractErr != nil {
				return storage.Transcript{}, false, extractErr
			}
			recognized, recognizeErr := service.speechRecognizer.Recognize(ctx, contracts.SpeechRecognitionRequest{
				AudioPath: path, Language: language,
			})
			_ = os.Remove(path)
			if errors.Is(recognizeErr, contracts.ErrSpeechNoWords) {
				service.hub.Record(draftID, StreamEvent{
					"type": TurnStreamSubagentProgress, "tool": "speech.inspect",
					"asset_id":  asset.ID,
					"note":      fmt.Sprintf("ASR 转写 %d/%d：该分块无台词，已跳过", index+1, len(chunks)),
					"completed": index + 1, "total": len(chunks),
				})
				continue
			}
			if recognizeErr != nil {
				return storage.Transcript{}, false, fmt.Errorf(
					"ASR 转写分块 %d/%d 失败: %w", index+1, len(chunks), recognizeErr,
				)
			}
			alignmentID := "provider-timestamps"
			chunkUtterances := agentexec.AlignTimestampedRecognition(asset.ID, recognized, chunk[0], chunk[1])
			if len(chunkUtterances) == 0 {
				alignmentID = "local-frame-alignment"
				chunkUtterances = agentexec.AlignRecognizedClauses(
					asset.ID, recognized.Text, recognized.Language, recognized.Emotion, chunk[0], chunk[1],
				)
			}
			currentProviderID := recognized.ProviderID + "+" + alignmentID
			if providerID == "" {
				providerID = currentProviderID
			} else if providerID != currentProviderID {
				providerID = recognized.ProviderID + "+mixed-frame-alignment"
			}
			utterances = append(utterances, chunkUtterances...)
		}
	}
	if len(utterances) == 0 {
		return storage.Transcript{}, false, errors.New("speech.inspect 没有取得可用台词")
	}
	pauses = agentexec.ClampSpeechPausesToWordBoundaries(
		asset.ID,
		agentexec.MergeSpeechPauses(asset.ID, append(pauses, agentexec.DeriveASRWordGaps(asset.ID, utterances)...)),
		utterances,
	)
	utteranceMaps := agentexec.EncodeSpeechUtterances(utterances)
	pauseMaps := agentexec.EncodeSpeechPauses(pauses)
	fingerprint := agentexec.StableSpeechID("transcript", asset.Hash, 0, durationFrames, providerID)
	transcriptID := fingerprint
	if forceRefresh {
		transcriptID += "_" + randomID("run")
	}
	result, err := reducer.Apply(ctx, service.database, nil, reducer.Options{
		Actor: contracts.ActorAgent,
		ResultRows: reducer.ResultRows{Transcripts: []reducer.TranscriptRow{{
			ID: transcriptID, AssetID: asset.ID, ProviderID: providerID,
			RawPreserved: false, Utterances: utteranceMaps, VADSegments: pauseMaps,
		}}},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		return storage.Transcript{}, false, errors.Join(err, fmt.Errorf("transcript reducer status: %s", result.Status))
	}
	return storage.Transcript{
		ID: transcriptID, AssetID: asset.ID, ProviderID: providerID,
		Utterances: utteranceMaps, VADSegments: pauseMaps,
	}, false, nil
}
