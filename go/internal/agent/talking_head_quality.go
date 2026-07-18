package agent

import (
	"context"
	"errors"
	"sort"

	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
)

// speechQualityReport 是纯读函数：只读取当前时间线文档与已持久化的转写，量化含
// a_roll 的口播成片里"还剩什么没剪干净"——残留气口、过短保留孤岛、未被 overlay
// 遮盖的硬接缝、过短 B-roll。它只陈述证据、不做裁决，供模型自主收敛、供用户验收。
func (service *Service) speechQualityReport(
	ctx context.Context,
	document timeline.Document,
) (map[string]any, error) {
	baseClips := []timeline.Clip{}
	overlayClips := []timeline.Clip{}
	for _, track := range document.Tracks {
		switch track.TrackID {
		case "visual_base":
			baseClips = append(baseClips, track.Clips...)
		case "visual_overlay":
			overlayClips = append(overlayClips, track.Clips...)
		}
	}
	sort.SliceStable(baseClips, func(left, right int) bool {
		return baseClips[left].TimelineStartFrame < baseClips[right].TimelineStartFrame
	})

	transcripts := map[string]agentexec.TalkingHeadAssetTranscript{}
	aRollPresent := false
	for _, clip := range baseClips {
		if clip.AssetKind != "video" || clip.AssetID == "" {
			continue
		}
		if _, done := transcripts[clip.AssetID]; done {
			continue
		}
		transcript, err := storage.LatestTranscript(ctx, service.database.Read(), clip.AssetID)
		if errors.Is(err, storage.ErrNotFound) {
			transcripts[clip.AssetID] = agentexec.TalkingHeadAssetTranscript{}
			continue
		}
		if err != nil {
			return nil, err
		}
		utterances, err := agentexec.DecodeSpeechUtterances(transcript.Utterances)
		if err != nil {
			return nil, err
		}
		pauses, err := agentexec.DecodeSpeechPauses(transcript.VADSegments)
		if err != nil {
			return nil, err
		}
		transcripts[clip.AssetID] = agentexec.TalkingHeadAssetTranscript{
			Utterances: utterances, Pauses: pauses, Present: true,
		}
		aRollPresent = true
	}
	if !aRollPresent {
		return map[string]any{"a_roll_present": false}, nil
	}
	fps := document.FPS
	if fps <= 0 {
		fps = timeline.DefaultFPS
	}
	breaths := agentexec.TalkingHeadResidualBreaths(baseClips, transcripts, fps)
	islands, runs := agentexec.TalkingHeadRetainedIslands(baseClips, transcripts, fps)
	seams := agentexec.TalkingHeadUncoveredSeams(runs, overlayClips, transcripts, fps)
	shortBroll := agentexec.TalkingHeadShortBrollClips(overlayClips, fps)
	return map[string]any{
		"a_roll_present":              true,
		"residual_breaths":            breaths,
		"residual_breath_count":       len(breaths),
		"short_retained_islands":      islands,
		"short_retained_island_count": len(islands),
		"uncovered_a_roll_seams":      seams,
		"uncovered_a_roll_seam_count": len(seams),
		"short_b_roll_clips":          shortBroll,
		"short_b_roll_clip_count":     len(shortBroll),
		"thresholds": map[string]any{
			"residual_breath_frames": agentexec.MinTalkingHeadResidualBreathFrames,
			"retained_island_frames": agentexec.MinTalkingHeadRetainedIslandFrames,
			"short_b_roll_frames":    agentexec.MinTalkingHeadBrollQualityFrames,
		},
	}, nil
}
