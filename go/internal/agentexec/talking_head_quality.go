package agentexec

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

const (
	// minTalkingHeadRetainedIslandFrames 是"保留孤岛"的时长下限：低于 2 秒的连续
	// 保留台词被视为语义孤岛。孤岛防护与口播质检共用这一个阈值。
	minTalkingHeadRetainedIslandFrames = 2 * timeline.DefaultFPS
	// minTalkingHeadResidualBreathFrames 是口播质检列出残留气口的时长下限（0.3 秒）。
	minTalkingHeadResidualBreathFrames = timeline.DefaultFPS * 3 / 10
	// minTalkingHeadBrollQualityFrames 是口播质检提示过短 B-roll 的时长下限（1.5 秒），
	// 比放置时的硬失败下限 minTalkingHeadBrollDurationFrames（0.5 秒）更宽，只作复检提示。
	minTalkingHeadBrollQualityFrames = timeline.DefaultFPS * 3 / 2
)

type talkingHeadAssetTranscript struct {
	utterances []speechUtterance
	pauses     []speechPause
	present    bool
}

type talkingHeadRetainedRun struct {
	assetID       string
	timelineStart int
	timelineEnd   int
	sourceStart   int
	sourceEnd     int
}

func talkingHeadResidualBreaths(
	baseClips []timeline.Clip,
	transcripts map[string]talkingHeadAssetTranscript,
	fps int,
) []map[string]any {
	assetIDs := make([]string, 0, len(transcripts))
	for assetID, transcript := range transcripts {
		if transcript.present {
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
		for _, pause := range transcript.pauses {
			key := [2]int{pause.StartFrame, pause.EndFrame}
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			for _, clip := range clips {
				start, end, ok := mapSourceRangeToTimelineClip(clip, pause.StartFrame, pause.EndFrame)
				if !ok || end-start < minTalkingHeadResidualBreathFrames {
					continue
				}
				previous, next := talkingHeadBreathContext(transcript.utterances, pause.StartFrame, pause.EndFrame)
				result = append(result, map[string]any{
					"a_roll_asset_id":        assetID,
					"timeline_start_frame":   start,
					"timeline_end_frame":     end,
					"timeline_start_seconds": frameSeconds(start, fps),
					"duration_frames":        end - start,
					"duration_seconds":       frameSeconds(end-start, fps),
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

func talkingHeadRetainedIslands(
	baseClips []timeline.Clip,
	transcripts map[string]talkingHeadAssetTranscript,
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
		if !ok || !transcript.present {
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
		if duration <= 0 || duration >= minTalkingHeadRetainedIslandFrames {
			continue
		}
		islands = append(islands, map[string]any{
			"a_roll_asset_id":      run.assetID,
			"timeline_start_frame": run.timelineStart,
			"timeline_end_frame":   run.timelineEnd,
			"duration_frames":      duration,
			"duration_seconds":     frameSeconds(duration, fps),
			"text": talkingHeadTranscriptText(
				transcripts[run.assetID].utterances, run.sourceStart, run.sourceEnd, nil, nil,
			),
		})
	}
	return islands, runs
}

func talkingHeadUncoveredSeams(
	runs []talkingHeadRetainedRun,
	overlays []timeline.Clip,
	transcripts map[string]talkingHeadAssetTranscript,
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
		previous, _ := talkingHeadBreathContext(transcripts[left.assetID].utterances, left.sourceEnd, left.sourceEnd)
		_, next := talkingHeadBreathContext(transcripts[right.assetID].utterances, right.sourceStart, right.sourceStart)
		result = append(result, map[string]any{
			"timeline_frame":   seam,
			"timeline_seconds": frameSeconds(seam, fps),
			"previous_text":    previous,
			"next_text":        next,
		})
	}
	return result
}

func talkingHeadShortBrollClips(overlays []timeline.Clip, fps int) []map[string]any {
	result := []map[string]any{}
	for _, clip := range overlays {
		if clip.Role != "b_roll" {
			continue
		}
		duration := clip.TimelineEndFrame - clip.TimelineStartFrame
		if duration <= 0 || duration >= minTalkingHeadBrollQualityFrames {
			continue
		}
		filename := ""
		if clip.Metadata != nil {
			filename = interfaceString(clip.Metadata["b_roll_filename"])
		}
		result = append(result, map[string]any{
			"timeline_clip_id":     clip.TimelineClipID,
			"timeline_start_frame": clip.TimelineStartFrame,
			"timeline_end_frame":   clip.TimelineEndFrame,
			"duration_frames":      duration,
			"duration_seconds":     frameSeconds(duration, fps),
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
func talkingHeadBreathContext(utterances []speechUtterance, start, end int) (string, string) {
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

func talkingHeadQualitySummary(report map[string]any) string {
	if present, _ := report["a_roll_present"].(bool); !present {
		return ""
	}
	return fmt.Sprintf(
		" 口播质检：残留 %v 处≥0.3秒气口、%v 处<2秒保留孤岛、%v 处未遮盖硬接缝、%v 段<1.5秒 B-roll；均为客观证据供你自主收敛，可带理由保留但不得无视清单。",
		report["residual_breath_count"], report["short_retained_island_count"],
		report["uncovered_a_roll_seam_count"], report["short_b_roll_clip_count"],
	)
}

func frameSeconds(frames, fps int) float64 {
	if fps <= 0 {
		fps = timeline.DefaultFPS
	}
	return math.Round(float64(frames)/float64(fps)*100) / 100
}

// confirmedToolReplayKey 标记"用户在决策卡上确认后重放的工具调用"。只有此时
// edit_talking_head 才会把执行结果与用户批准的删除方案的偏差输出为 plan_drift。
type confirmedToolReplayKey struct{}

func withConfirmedToolReplay(ctx context.Context) context.Context {
	return context.WithValue(ctx, confirmedToolReplayKey{}, true)
}

func isConfirmedToolReplay(ctx context.Context) bool {
	value, _ := ctx.Value(confirmedToolReplayKey{}).(bool)
	return value
}

// talkingHeadPlanDrift 在"决策卡批准后的重放"里，把工具为避免制造孤岛而保守撤回
// 的气口列成偏差清单：这些是用户已批准删除、却因防护被保留下来的碎片。
func talkingHeadPlanDrift(
	ctx context.Context,
	autoPreserved []speechPause,
	utterances []speechUtterance,
) map[string]any {
	if !isConfirmedToolReplay(ctx) || len(autoPreserved) == 0 {
		return nil
	}
	items := make([]map[string]any, 0, len(autoPreserved))
	for _, pause := range autoPreserved {
		evidence := rushestools.SpeechPauseEvidence{
			SourceStartFrame: pause.StartFrame, SourceEndFrame: pause.EndFrame,
		}
		populateSpeechPauseContext(&evidence, utterances)
		items = append(items, map[string]any{
			"pause_id":               pause.ID,
			"delete_duration_frames": pause.DeleteEnd - pause.DeleteStart,
			"previous_text":          evidence.PreviousText,
			"next_text":              evidence.NextText,
		})
	}
	return map[string]any{
		"approved_plan":        true,
		"retained_pause_count": len(items),
		"retained_pauses":      items,
		"summary": fmt.Sprintf(
			"与你批准的删除方案相比，为避免制造不足 2 秒的保留孤岛，本次实际保留了 %d 处气口未删；请在回复中如实向用户说明这一偏差。",
			len(items),
		),
	}
}
