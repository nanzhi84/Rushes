package agentexec

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
)

const (
	// minTalkingHeadRetainedIslandFrames 是"保留孤岛"的时长下限：低于 2 秒的连续
	// 保留台词被视为语义孤岛。孤岛防护与口播质检共用这一个阈值。
	MinTalkingHeadRetainedIslandFrames = 2 * timeline.DefaultFPS
	// minTalkingHeadResidualBreathFrames 是口播质检列出残留气口的时长下限（0.3 秒）。
	MinTalkingHeadResidualBreathFrames = timeline.DefaultFPS * 3 / 10
	// MinTalkingHeadBrollQualityFrames 是口播质检提示过短 B-roll 的客观时长下限（1.5 秒）。
	// 原子 insert 允许模型显式决定片段长度，本项负责让过短结果在 timeline.check 中可观察。
	MinTalkingHeadBrollQualityFrames = timeline.DefaultFPS * 3 / 2
)

type TalkingHeadAssetTranscript struct {
	Utterances []SpeechUtterance
	Pauses     []SpeechPause
	Present    bool
}

type talkingHeadRetainedRun struct {
	assetID       string
	timelineStart int
	timelineEnd   int
	sourceStart   int
	sourceEnd     int
}

func TalkingHeadResidualBreaths(
	baseClips []timeline.Clip,
	transcripts map[string]TalkingHeadAssetTranscript,
	fps int,
) []map[string]any {
	assetIDs := make([]string, 0, len(transcripts))
	for assetID, transcript := range transcripts {
		if transcript.Present {
			assetIDs = append(assetIDs, assetID)
		}
	}
	sort.Strings(assetIDs)
	result := []map[string]any{}
	for _, assetID := range assetIDs {
		transcript := transcripts[assetID]
		clips := []timeline.Clip{}
		for _, clip := range baseClips {
			if clip.AssetID == assetID {
				clips = append(clips, clip)
			}
		}
		seen := map[[2]int]struct{}{}
		for _, pause := range transcript.Pauses {
			key := [2]int{pause.StartFrame, pause.EndFrame}
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			for _, clip := range clips {
				start, end, ok := MapSourceRangeToTimelineClip(clip, pause.StartFrame, pause.EndFrame)
				if !ok || end-start < MinTalkingHeadResidualBreathFrames {
					continue
				}
				previous, next := talkingHeadBreathContext(transcript.Utterances, pause.StartFrame, pause.EndFrame)
				result = append(result, map[string]any{
					"a_roll_asset_id":        assetID,
					"timeline_start_frame":   start,
					"timeline_end_frame":     end,
					"timeline_start_seconds": FrameSeconds(start, fps),
					"duration_frames":        end - start,
					"duration_seconds":       FrameSeconds(end-start, fps),
					"previous_text":          previous,
					"next_text":              next,
				})
			}
		}
	}
	sort.SliceStable(result, func(left, right int) bool {
		return result[left]["timeline_start_frame"].(int) < result[right]["timeline_start_frame"].(int)
	})
	return result
}

func TalkingHeadRetainedIslands(
	baseClips []timeline.Clip,
	transcripts map[string]TalkingHeadAssetTranscript,
	fps int,
) ([]map[string]any, []talkingHeadRetainedRun) {
	runs := []talkingHeadRetainedRun{}
	var current *talkingHeadRetainedRun
	flush := func() {
		if current != nil {
			runs = append(runs, *current)
			current = nil
		}
	}
	for _, clip := range baseClips {
		transcript, ok := transcripts[clip.AssetID]
		if !ok || !transcript.Present {
			flush()
			continue
		}
		if current != nil && current.assetID == clip.AssetID &&
			current.timelineEnd == clip.TimelineStartFrame &&
			current.sourceEnd == clip.SourceStartFrame {
			current.timelineEnd = clip.TimelineEndFrame
			current.sourceEnd = clip.SourceEndFrame
			continue
		}
		flush()
		run := talkingHeadRetainedRun{
			assetID:       clip.AssetID,
			timelineStart: clip.TimelineStartFrame, timelineEnd: clip.TimelineEndFrame,
			sourceStart: clip.SourceStartFrame, sourceEnd: clip.SourceEndFrame,
		}
		current = &run
	}
	flush()
	islands := []map[string]any{}
	for _, run := range runs {
		duration := run.timelineEnd - run.timelineStart
		if duration <= 0 || duration >= MinTalkingHeadRetainedIslandFrames {
			continue
		}
		islands = append(islands, map[string]any{
			"a_roll_asset_id":      run.assetID,
			"timeline_start_frame": run.timelineStart,
			"timeline_end_frame":   run.timelineEnd,
			"duration_frames":      duration,
			"duration_seconds":     FrameSeconds(duration, fps),
			"text": TalkingHeadTranscriptText(
				transcripts[run.assetID].Utterances, run.sourceStart, run.sourceEnd,
			),
		})
	}
	return islands, runs
}

func TalkingHeadUncoveredSeams(
	runs []talkingHeadRetainedRun,
	overlays []timeline.Clip,
	transcripts map[string]TalkingHeadAssetTranscript,
	fps int,
) []map[string]any {
	result := []map[string]any{}
	for index := 0; index+1 < len(runs); index++ {
		left, right := runs[index], runs[index+1]
		if left.timelineEnd != right.timelineStart {
			continue
		}
		seam := left.timelineEnd
		covered := false
		for _, overlay := range overlays {
			if overlay.TimelineStartFrame < seam && seam < overlay.TimelineEndFrame {
				covered = true
				break
			}
		}
		if covered {
			continue
		}
		previous, _ := talkingHeadBreathContext(transcripts[left.assetID].Utterances, left.sourceEnd, left.sourceEnd)
		_, next := talkingHeadBreathContext(transcripts[right.assetID].Utterances, right.sourceStart, right.sourceStart)
		result = append(result, map[string]any{
			"timeline_frame":   seam,
			"timeline_seconds": FrameSeconds(seam, fps),
			"previous_text":    previous,
			"next_text":        next,
		})
	}
	return result
}

func TalkingHeadShortBrollClips(overlays []timeline.Clip, fps int) []map[string]any {
	result := []map[string]any{}
	for _, clip := range overlays {
		if clip.Role != "b_roll" {
			continue
		}
		duration := clip.TimelineEndFrame - clip.TimelineStartFrame
		if duration <= 0 || duration >= MinTalkingHeadBrollQualityFrames {
			continue
		}
		filename := ""
		if clip.Metadata != nil {
			filename = InterfaceString(clip.Metadata["b_roll_filename"])
		}
		result = append(result, map[string]any{
			"timeline_clip_id":     clip.TimelineClipID,
			"timeline_start_frame": clip.TimelineStartFrame,
			"timeline_end_frame":   clip.TimelineEndFrame,
			"duration_frames":      duration,
			"duration_seconds":     FrameSeconds(duration, fps),
			"b_roll_filename":      filename,
		})
	}
	sort.SliceStable(result, func(left, right int) bool {
		return result[left]["timeline_start_frame"].(int) < result[right]["timeline_start_frame"].(int)
	})
	return result
}

// talkingHeadBreathContext 返回落在 [start, end) 前后最近整句的台词，用于给残留
// 气口和硬接缝提供人类可读的语境。
func talkingHeadBreathContext(utterances []SpeechUtterance, start, end int) (string, string) {
	previous, next := "", ""
	previousEnd, nextStart := -1, math.MaxInt
	for _, utterance := range utterances {
		if utterance.EndFrame <= start && utterance.EndFrame > previousEnd {
			previousEnd = utterance.EndFrame
			previous = utterance.Text
		}
		if utterance.StartFrame >= end && utterance.StartFrame < nextStart {
			nextStart = utterance.StartFrame
			next = utterance.Text
		}
	}
	return previous, next
}

func TalkingHeadQualitySummary(report map[string]any) string {
	if present, _ := report["a_roll_present"].(bool); !present {
		return ""
	}
	return fmt.Sprintf(
		" 口播质检：残留 %v 处≥0.3秒气口、%v 处<2秒保留孤岛、%v 处未遮盖硬接缝、%v 段<1.5秒 B-roll；均为客观证据供你自主收敛，可带理由保留但不得无视清单。",
		report["residual_breath_count"], report["short_retained_island_count"],
		report["uncovered_a_roll_seam_count"], report["short_b_roll_clip_count"],
	)
}

func FrameSeconds(frames, fps int) float64 {
	if fps <= 0 {
		fps = timeline.DefaultFPS
	}
	return math.Round(float64(frames)/float64(fps)*100) / 100
}

// confirmedToolReplayKey 标记“用户在决策卡上确认后重放的工具调用”，供统一确认拦截器
// 在重放一次已经批准的破坏性原子动作时识别确认凭证。
type confirmedToolReplayKey struct{}

func WithConfirmedToolReplay(ctx context.Context) context.Context {
	return context.WithValue(ctx, confirmedToolReplayKey{}, true)
}

// IsConfirmedToolReplay 报告本次调用是否为用户在决策卡上确认后的工具重放。
// G2 的破坏性确认拦截器据此在 ctx 持有确认凭证时放行（见 agent/interceptor.go）。
func IsConfirmedToolReplay(ctx context.Context) bool {
	value, _ := ctx.Value(confirmedToolReplayKey{}).(bool)
	return value
}

// SpeechQualityReport 汇总当前时间线上的客观口播质量证据，供 timeline.check 与
// render.start 的结构校验使用，不替模型做删除或镜头选择。
func (exec *Executor) SpeechQualityReport(
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

	transcripts := map[string]TalkingHeadAssetTranscript{}
	aRollPresent := false
	for _, clip := range baseClips {
		if clip.AssetKind != "video" || clip.AssetID == "" {
			continue
		}
		if _, done := transcripts[clip.AssetID]; done {
			continue
		}
		transcript, err := storage.LatestTranscript(ctx, exec.database.Read(), clip.AssetID)
		if errors.Is(err, storage.ErrNotFound) {
			transcripts[clip.AssetID] = TalkingHeadAssetTranscript{}
			continue
		}
		if err != nil {
			return nil, err
		}
		utterances, err := DecodeSpeechUtterances(transcript.Utterances)
		if err != nil {
			return nil, err
		}
		pauses, err := DecodeSpeechPauses(transcript.VADSegments)
		if err != nil {
			return nil, err
		}
		transcripts[clip.AssetID] = TalkingHeadAssetTranscript{
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
	breaths := TalkingHeadResidualBreaths(baseClips, transcripts, fps)
	islands, runs := TalkingHeadRetainedIslands(baseClips, transcripts, fps)
	seams := TalkingHeadUncoveredSeams(runs, overlayClips, transcripts, fps)
	shortBroll := TalkingHeadShortBrollClips(overlayClips, fps)
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
			"residual_breath_frames": MinTalkingHeadResidualBreathFrames,
			"retained_island_frames": MinTalkingHeadRetainedIslandFrames,
			"short_b_roll_frames":    MinTalkingHeadBrollQualityFrames,
		},
	}, nil
}
