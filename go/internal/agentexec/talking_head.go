package agentexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
	"github.com/nanzhi84/Rushes/go/internal/understanding"
)

type talkingHeadRange struct {
	Start int
	End   int
}

const (
	maxTalkingHeadUnvoicedBridgeFrames = 12
	minTalkingHeadPauseCandidateFrames = 12
	minTalkingHeadPauseResidualFrames  = 5
	minTalkingHeadBrollDurationFrames  = timeline.DefaultFPS / 2
)

func attachTalkingHeadUnreviewedEvidence(
	result *rushestools.ToolResult,
	pauses []rushestools.SpeechPauseEvidence,
	repetitions []rushestools.SpeechRepetitionEvidence,
	fragments []rushestools.SpeechFragmentEvidence,
) {
	if result.Data == nil {
		result.Data = map[string]any{}
	}
	result.Data["unreviewed_pause_candidates"] = pauses
	result.Data["unreviewed_repetition_candidates"] = repetitions
	result.Data["unreviewed_short_fragment_candidates"] = fragments
	if len(pauses) == 0 && len(repetitions) == 0 && len(fragments) == 0 {
		return
	}
	result.Observation += fmt.Sprintf(
		" 另返回 %d 个气口、%d 个句内重复和 %d 个短语音候选供模型按需继续审阅；这些内容候选不影响本次合法修改成功。",
		len(pauses), len(repetitions), len(fragments),
	)
}

func expandTalkingHeadPauseDecisions(
	input rushestools.TalkingHeadEditInput,
	pauseByID map[string]speechPause,
) (rushestools.TalkingHeadEditInput, map[string]struct{}, []map[string]any) {
	input.RemovePauseIDs = append([]string(nil), input.RemovePauseIDs...)
	directRemovals := make(map[string]struct{}, len(input.RemovePauseIDs))
	for _, id := range input.RemovePauseIDs {
		directRemovals[strings.TrimSpace(id)] = struct{}{}
	}
	seen := map[string]struct{}{}
	invalid := []map[string]any{}
	for index, decision := range input.PauseDecisions {
		id := strings.TrimSpace(decision.PauseID)
		action := strings.ToLower(strings.TrimSpace(decision.Action))
		_, exists := pauseByID[id]
		_, duplicate := seen[id]
		_, directlyRemoved := directRemovals[id]
		conflict := action == "preserve" && directlyRemoved
		if id == "" || !exists || duplicate || conflict ||
			(action != "remove" && action != "preserve") {
			invalid = append(invalid, map[string]any{
				"index": index, "pause_id": id, "action": action,
				"unknown": !exists, "duplicate": duplicate, "conflicts_with_remove_pause_ids": conflict,
			})
			continue
		}
		seen[id] = struct{}{}
		if action == "remove" && !directlyRemoved {
			input.RemovePauseIDs = append(input.RemovePauseIDs, id)
			directRemovals[id] = struct{}{}
		}
	}
	for id := range directRemovals {
		if _, exists := pauseByID[id]; exists {
			seen[id] = struct{}{}
		}
	}
	return input, seen, invalid
}

func expandTalkingHeadRepetitionDecisions(
	input rushestools.TalkingHeadEditInput,
	repetitionByID map[string]rushestools.SpeechRepetitionEvidence,
) (rushestools.TalkingHeadEditInput, map[string]struct{}, []map[string]any) {
	input.RemoveWordRanges = append([]rushestools.TalkingHeadWordRange(nil), input.RemoveWordRanges...)
	seen := map[string]struct{}{}
	invalid := []map[string]any{}
	for index, decision := range input.RepetitionDecisions {
		id := strings.TrimSpace(decision.RepetitionID)
		action := strings.ToLower(strings.TrimSpace(decision.Action))
		repetition, exists := repetitionByID[id]
		_, duplicate := seen[id]
		if id == "" || !exists || duplicate ||
			(action != "remove_earlier" && action != "remove_later" && action != "preserve") {
			invalid = append(invalid, map[string]any{
				"index": index, "repetition_id": id, "action": action,
				"unknown": !exists, "duplicate": duplicate,
			})
			continue
		}
		seen[id] = struct{}{}
		if action == "preserve" {
			continue
		}
		rangeValue := rushestools.TalkingHeadWordRange{
			StartWordID: repetition.EarlierStartWordID,
			EndWordID:   repetition.EarlierEndWordID,
		}
		if action == "remove_later" {
			rangeValue = rushestools.TalkingHeadWordRange{
				StartWordID: repetition.LaterStartWordID,
				EndWordID:   repetition.LaterEndWordID,
			}
		}
		alreadyPresent := false
		for _, existing := range input.RemoveWordRanges {
			if existing.StartWordID == rangeValue.StartWordID && existing.EndWordID == rangeValue.EndWordID {
				alreadyPresent = true
				break
			}
		}
		if !alreadyPresent {
			input.RemoveWordRanges = append(input.RemoveWordRanges, rangeValue)
		}
	}
	return input, seen, invalid
}

type talkingHeadFragmentExpansion struct {
	Input            rushestools.TalkingHeadEditInput
	PreservedIDs     []string
	PreservedReasons map[string]string
	Invalid          []map[string]any
}

func expandTalkingHeadFragmentDecisions(
	input rushestools.TalkingHeadEditInput,
	fragmentByID map[string]rushestools.SpeechFragmentEvidence,
) talkingHeadFragmentExpansion {
	input.RemoveWordRanges = append([]rushestools.TalkingHeadWordRange(nil), input.RemoveWordRanges...)
	preservedIDs := []string{}
	preservedReasons := map[string]string{}
	seen := map[string]struct{}{}
	invalid := []map[string]any{}
	for index, decision := range input.ShortFragmentDecisions {
		id := strings.TrimSpace(decision.FragmentID)
		action := strings.ToLower(strings.TrimSpace(decision.Action))
		fragment, exists := fragmentByID[id]
		_, duplicate := seen[id]
		if id == "" || !exists || duplicate || (action != "remove" && action != "preserve") {
			invalid = append(invalid, map[string]any{
				"index": index, "fragment_id": id, "action": action,
				"unknown": !exists, "duplicate": duplicate,
			})
			continue
		}
		seen[id] = struct{}{}
		if action == "remove" {
			rangeValue := rushestools.TalkingHeadWordRange{
				StartWordID: fragment.StartWordID, EndWordID: fragment.EndWordID,
			}
			alreadyPresent := false
			for _, existing := range input.RemoveWordRanges {
				if existing.StartWordID == rangeValue.StartWordID && existing.EndWordID == rangeValue.EndWordID {
					alreadyPresent = true
					break
				}
			}
			if !alreadyPresent {
				input.RemoveWordRanges = append(input.RemoveWordRanges, rangeValue)
			}
			continue
		}
		preservedIDs = append(preservedIDs, id)
		if reason := strings.TrimSpace(decision.Reason); reason != "" {
			preservedReasons[id] = reason
		}
	}
	return talkingHeadFragmentExpansion{
		Input: input, PreservedIDs: preservedIDs, PreservedReasons: preservedReasons, Invalid: invalid,
	}
}

func validRestartFragmentPreserveReason(
	fragment rushestools.SpeechFragmentEvidence,
	reason string,
) bool {
	normalizedReason := normalizeSpeechText(reason)
	normalizedFragment := normalizeSpeechText(fragment.Text)
	normalizedAnchor := normalizeSpeechText(fragment.RestartAnchorText)
	return len([]rune(normalizedReason)) >= 20 && normalizedFragment != "" && normalizedAnchor != "" &&
		strings.Contains(normalizedReason, normalizedFragment) &&
		strings.Contains(normalizedReason, normalizedAnchor)
}

func talkingHeadRangeCoveredBy(target talkingHeadRange, ranges []talkingHeadRange) bool {
	for _, value := range mergeTalkingHeadRanges(ranges) {
		if value.Start <= target.Start && value.End >= target.End {
			return true
		}
	}
	return false
}

func resolveTalkingHeadPauseRanges(
	pauses []speechPause,
	semanticDeletions []talkingHeadRange,
	minimumResidualFrames int,
) (effective []speechPause, residualRanges []talkingHeadRange, redundant []speechPause) {
	for _, pause := range pauses {
		target := talkingHeadRange{Start: pause.DeleteStart, End: pause.DeleteEnd}
		residuals := subtractTalkingHeadRanges(target, semanticDeletions)
		kept := residuals[:0]
		for _, residual := range residuals {
			if residual.End-residual.Start >= minimumResidualFrames {
				kept = append(kept, residual)
			}
		}
		if len(kept) == 0 {
			redundant = append(redundant, pause)
			continue
		}
		effective = append(effective, pause)
		residualRanges = append(residualRanges, kept...)
	}
	return effective, mergeTalkingHeadRanges(residualRanges), redundant
}

// protectTalkingHeadOrphanFragments conservatively retracts only pause deletions
// when they are the mechanical reason retained speech would become a sub-2s
// island. Semantic removals remain model decisions and are never changed here.
func protectTalkingHeadOrphanFragments(
	semanticDeletions []talkingHeadRange,
	selectedPauses []speechPause,
	detectedPauses []speechPause,
	retainedSpeech []talkingHeadRange,
	utterances []speechUtterance,
	removedUtterances map[string]struct{},
	removedWords map[string]struct{},
	clip timeline.Clip,
	misspeakEvidence []talkingHeadEvidenceRange,
) (
	effectivePauses []speechPause,
	effectivePauseRanges []talkingHeadRange,
	sourceDeleteRanges []talkingHeadRange,
	autoPreservedPauses []speechPause,
	orphanFragments []map[string]any,
) {
	remaining := append([]speechPause(nil), selectedPauses...)
	autoPreservedIDs := map[string]struct{}{}
	finalizeAutoPreserved := func() []speechPause {
		result := make([]speechPause, 0, len(autoPreservedIDs))
		for _, pause := range selectedPauses {
			if _, preserved := autoPreservedIDs[pause.ID]; preserved {
				result = append(result, pause)
			}
		}
		return result
	}

	for attempt := 0; attempt <= len(selectedPauses); attempt++ {
		effectivePauses, effectivePauseRanges, _ = resolveTalkingHeadPauseRanges(
			remaining, semanticDeletions, minTalkingHeadPauseResidualFrames,
		)
		sourceDeleteRanges = append([]talkingHeadRange(nil), semanticDeletions...)
		sourceDeleteRanges = append(sourceDeleteRanges, effectivePauseRanges...)
		sourceDeleteRanges = mergeTalkingHeadRanges(sourceDeleteRanges)

		bridgePauses := make([]speechPause, 0, len(detectedPauses))
		for _, pause := range detectedPauses {
			if _, preserved := autoPreservedIDs[pause.ID]; !preserved {
				bridgePauses = append(bridgePauses, pause)
			}
		}
		sourceDeleteRanges = bridgeTalkingHeadRanges(
			sourceDeleteRanges, retainedSpeech, bridgePauses, maxTalkingHeadUnvoicedBridgeFrames,
		)
		sourceDeleteRanges = absorbTalkingHeadEdgeSlivers(
			sourceDeleteRanges, retainedSpeech,
			clip.SourceStartFrame, clip.SourceEndFrame, maxTalkingHeadUnvoicedBridgeFrames,
		)
		orphanFragments = talkingHeadOrphanSpeechFragments(
			sourceDeleteRanges, retainedSpeech, utterances,
			removedUtterances, removedWords, effectivePauses,
			clip.SourceStartFrame, clip.SourceEndFrame, minTalkingHeadRetainedIslandFrames,
			misspeakEvidence,
		)
		if len(orphanFragments) == 0 {
			return effectivePauses, effectivePauseRanges, sourceDeleteRanges,
				finalizeAutoPreserved(), nil
		}

		effectiveByID := make(map[string]speechPause, len(effectivePauses))
		for _, pause := range effectivePauses {
			effectiveByID[pause.ID] = pause
		}
		preserveNow := map[string]struct{}{}
		for _, fragment := range orphanFragments {
			adjacentPauseIDs, _ := fragment["adjacent_pause_ids"].([]string)
			bestFound := false
			bestCost := 0
			bestPause := speechPause{}
			for _, pauseID := range adjacentPauseIDs {
				pause, exists := effectiveByID[pauseID]
				if !exists {
					continue
				}
				cost := talkingHeadPauseResidualFrames(pause, semanticDeletions)
				if !bestFound || cost < bestCost || (cost == bestCost && pause.ID < bestPause.ID) {
					bestFound = true
					bestCost = cost
					bestPause = pause
				}
			}
			if bestFound {
				preserveNow[bestPause.ID] = struct{}{}
			}
		}
		if len(preserveNow) == 0 {
			return effectivePauses, effectivePauseRanges, sourceDeleteRanges,
				finalizeAutoPreserved(), orphanFragments
		}

		next := make([]speechPause, 0, len(remaining)-len(preserveNow))
		for _, pause := range remaining {
			if _, preserve := preserveNow[pause.ID]; preserve {
				autoPreservedIDs[pause.ID] = struct{}{}
				continue
			}
			next = append(next, pause)
		}
		if len(next) == len(remaining) {
			return effectivePauses, effectivePauseRanges, sourceDeleteRanges,
				finalizeAutoPreserved(), orphanFragments
		}
		remaining = next
	}

	return effectivePauses, effectivePauseRanges, sourceDeleteRanges,
		finalizeAutoPreserved(), orphanFragments
}

func talkingHeadPauseResidualFrames(pause speechPause, semanticDeletions []talkingHeadRange) int {
	frames := 0
	for _, residual := range subtractTalkingHeadRanges(
		talkingHeadRange{Start: pause.DeleteStart, End: pause.DeleteEnd}, semanticDeletions,
	) {
		if duration := residual.End - residual.Start; duration >= minTalkingHeadPauseResidualFrames {
			frames += duration
		}
	}
	return frames
}

func subtractTalkingHeadRanges(
	target talkingHeadRange,
	exclusions []talkingHeadRange,
) []talkingHeadRange {
	if target.End <= target.Start {
		return nil
	}
	cursor := target.Start
	result := []talkingHeadRange{}
	for _, exclusion := range mergeTalkingHeadRanges(exclusions) {
		if exclusion.End <= cursor {
			continue
		}
		if exclusion.Start >= target.End {
			break
		}
		if exclusion.Start > cursor {
			result = append(result, talkingHeadRange{
				Start: cursor,
				End:   min(exclusion.Start, target.End),
			})
		}
		cursor = max(cursor, exclusion.End)
		if cursor >= target.End {
			break
		}
	}
	if cursor < target.End {
		result = append(result, talkingHeadRange{Start: cursor, End: target.End})
	}
	return result
}

func speechPauseIDs(pauses []speechPause) []string {
	ids := make([]string, 0, len(pauses))
	for _, pause := range pauses {
		ids = append(ids, pause.ID)
	}
	return ids
}

func talkingHeadRetainedPauseCandidates(
	pauses []speechPause,
	semanticDeletions []talkingHeadRange,
	clip timeline.Clip,
	utterances []speechUtterance,
	minimumDeleteFrames int,
	limit int,
) []rushestools.SpeechPauseEvidence {
	if limit <= 0 {
		return nil
	}
	candidates := []rushestools.SpeechPauseEvidence{}
	for _, pause := range pauses {
		target := talkingHeadRange{Start: pause.DeleteStart, End: pause.DeleteEnd}
		if target.End-target.Start < minimumDeleteFrames ||
			target.Start < clip.SourceStartFrame || target.End > clip.SourceEndFrame ||
			talkingHeadRangeOverlapsAny(target, semanticDeletions) {
			continue
		}
		item := rushestools.SpeechPauseEvidence{
			PauseID: pause.ID, SourceStartFrame: pause.StartFrame, SourceEndFrame: pause.EndFrame,
			DeleteStartFrame: pause.DeleteStart, DeleteEndFrame: pause.DeleteEnd,
			DurationFrames:       pause.EndFrame - pause.StartFrame,
			DeleteDurationFrames: pause.DeleteEnd - pause.DeleteStart,
			DetectionMethod:      pause.Method,
		}
		populateSpeechPauseContext(&item, utterances)
		candidates = append(candidates, item)
	}
	sort.SliceStable(candidates, func(left, right int) bool {
		if candidates[left].DeleteDurationFrames == candidates[right].DeleteDurationFrames {
			return candidates[left].DeleteStartFrame < candidates[right].DeleteStartFrame
		}
		return candidates[left].DeleteDurationFrames > candidates[right].DeleteDurationFrames
	})
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	return candidates
}

func unresolvedTalkingHeadPauseDecisions(
	pauses []speechPause,
	semanticDeletions []talkingHeadRange,
	clip timeline.Clip,
	utterances []speechUtterance,
	decided map[string]struct{},
	minimumDeleteFrames int,
	limit int,
) []rushestools.SpeechPauseEvidence {
	candidates := talkingHeadRetainedPauseCandidates(
		pauses, semanticDeletions, clip, utterances, minimumDeleteFrames, limit,
	)
	result := make([]rushestools.SpeechPauseEvidence, 0, len(candidates))
	for _, candidate := range candidates {
		if _, exists := decided[candidate.PauseID]; exists {
			continue
		}
		result = append(result, candidate)
	}
	return result
}

func talkingHeadPrimaryClip(document timeline.Document, id string) (timeline.Clip, bool) {
	for _, track := range document.Tracks {
		if track.TrackID != "visual_base" {
			continue
		}
		for _, clip := range track.Clips {
			if clip.TimelineClipID == id && clip.AssetID != "" && clip.AssetKind == "video" {
				return clip, true
			}
		}
	}
	return timeline.Clip{}, false
}

// selectTalkingHeadUtterances 用交集解析选择待删句：证据与 clip 源区间交集非空即
// 合法，删除范围裁剪到交集；仅当 ID 未知或交集为空（完全落在 clip 之外）才判非法。
func selectTalkingHeadUtterances(
	ids []string, values map[string]speechUtterance, clip timeline.Clip,
) ([]talkingHeadRange, []string) {
	selected := []talkingHeadRange{}
	invalid := []string{}
	seen := map[string]struct{}{}
	for _, id := range ids {
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		value, exists := values[id]
		if !exists {
			invalid = append(invalid, id)
			continue
		}
		start := max(value.StartFrame, clip.SourceStartFrame)
		end := min(value.EndFrame, clip.SourceEndFrame)
		if end <= start {
			invalid = append(invalid, id)
			continue
		}
		selected = append(selected, talkingHeadRange{Start: start, End: end})
	}
	return selected, invalid
}

func selectTalkingHeadWordRanges(
	requested []rushestools.TalkingHeadWordRange,
	words []speechWord,
	clip timeline.Clip,
) ([]talkingHeadRange, []string, []map[string]any) {
	wordIndex := make(map[string]int, len(words))
	for index, word := range words {
		wordIndex[word.ID] = index
	}
	ranges := []talkingHeadRange{}
	removedIDs := []string{}
	invalid := []map[string]any{}
	seenWords := map[string]struct{}{}
	for index, requestedRange := range requested {
		startID := requestedRange.StartWordID
		endID := requestedRange.EndWordID
		if endID == "" {
			endID = startID
		}
		startIndex, startOK := wordIndex[startID]
		endIndex, endOK := wordIndex[endID]
		if !startOK || !endOK || startIndex > endIndex {
			invalid = append(invalid, map[string]any{
				"index": index, "start_word_id": startID, "end_word_id": endID,
			})
			continue
		}
		// 交集解析：删除范围裁剪到 clip 已裁剪源区间；仅当范围完全落在 clip 之外
		// 才判非法。词 ID 也只保留落在交集内、确实会被删除的那部分。
		start := max(words[startIndex].StartFrame, clip.SourceStartFrame)
		end := min(words[endIndex].EndFrame, clip.SourceEndFrame)
		if end <= start {
			invalid = append(invalid, map[string]any{
				"index": index, "start_word_id": startID, "end_word_id": endID,
			})
			continue
		}
		ranges = append(ranges, talkingHeadRange{Start: start, End: end})
		for cursor := startIndex; cursor <= endIndex; cursor++ {
			word := words[cursor]
			if word.EndFrame <= start || word.StartFrame >= end {
				continue
			}
			if _, duplicate := seenWords[word.ID]; duplicate {
				continue
			}
			seenWords[word.ID] = struct{}{}
			removedIDs = append(removedIDs, word.ID)
		}
	}
	return mergeTalkingHeadRanges(ranges), removedIDs, invalid
}

// selectTalkingHeadPauses 用交集解析选择待删气口：删除区间与 clip 源区间交集非空即
// 合法，并把该气口的删除区间裁剪到交集；仅当 ID 未知或交集为空才判非法。
func selectTalkingHeadPauses(
	ids []string, values map[string]speechPause, clip timeline.Clip,
) ([]speechPause, []string) {
	selected := []speechPause{}
	invalid := []string{}
	seen := map[string]struct{}{}
	for _, id := range ids {
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		value, exists := values[id]
		if !exists {
			invalid = append(invalid, id)
			continue
		}
		start := max(value.DeleteStart, clip.SourceStartFrame)
		end := min(value.DeleteEnd, clip.SourceEndFrame)
		if end <= start {
			invalid = append(invalid, id)
			continue
		}
		value.DeleteStart, value.DeleteEnd = start, end
		selected = append(selected, value)
	}
	return selected, invalid
}

// talkingHeadEvidenceClipHints 为每条非法证据查询它在当前时间线上实际所属的 clip，
// 让模型不必猜「该证据现在归哪个 clip」。未知 ID 或素材已不在时间线上的项没有提示。
func talkingHeadEvidenceClipHints(
	document timeline.Document,
	assetID string,
	invalidUtterances []string,
	utteranceByID map[string]speechUtterance,
	invalidWordRanges []map[string]any,
	words []speechWord,
	invalidPauses []string,
	pauseByID map[string]speechPause,
) []map[string]any {
	wordByID := make(map[string]speechWord, len(words))
	for _, word := range words {
		wordByID[word.ID] = word
	}
	hints := []map[string]any{}
	appendHint := func(kind, id string, start, end int) {
		if clipID, ok := talkingHeadSourceRangeClip(document, assetID, start, end); ok {
			hints = append(hints, map[string]any{
				"evidence_kind": kind, "evidence_id": id, "current_timeline_clip_id": clipID,
			})
		}
	}
	for _, id := range invalidUtterances {
		if value, exists := utteranceByID[id]; exists {
			appendHint("utterance", id, value.StartFrame, value.EndFrame)
		}
	}
	for _, item := range invalidWordRanges {
		startID, _ := item["start_word_id"].(string)
		endID, _ := item["end_word_id"].(string)
		start, startExists := wordByID[startID]
		end, endExists := wordByID[endID]
		if startExists && endExists && start.StartFrame < end.EndFrame {
			appendHint("word_range", startID+".."+endID, start.StartFrame, end.EndFrame)
		}
	}
	for _, id := range invalidPauses {
		if value, exists := pauseByID[id]; exists {
			appendHint("pause", id, value.DeleteStart, value.DeleteEnd)
		}
	}
	return hints
}

// talkingHeadSourceRangeClip 在主视频轨上找到与给定源区间重叠最多的同素材 clip，
// 作为「该证据当前位于哪个 clip」的建议。
func talkingHeadSourceRangeClip(
	document timeline.Document, assetID string, start, end int,
) (string, bool) {
	bestID := ""
	bestOverlap := 0
	for _, track := range document.Tracks {
		if track.TrackID != "visual_base" {
			continue
		}
		for _, clip := range track.Clips {
			if clip.AssetID != assetID {
				continue
			}
			overlap := min(end, clip.SourceEndFrame) - max(start, clip.SourceStartFrame)
			if overlap > bestOverlap {
				bestOverlap = overlap
				bestID = clip.TimelineClipID
			}
		}
	}
	return bestID, bestID != ""
}

func talkingHeadAssignmentSourceRange(
	assignment rushestools.TalkingHeadBrollAssignment,
	utterances map[string]speechUtterance,
	words []speechWord,
	removedUtterances map[string]struct{},
	removedWords map[string]struct{},
	clip timeline.Clip,
) (talkingHeadRange, error) {
	if assignment.ShotID == "" {
		return talkingHeadRange{}, errors.New("缺少 shot_id")
	}
	wordMode := assignment.StartWordID != "" || assignment.EndWordID != ""
	utteranceMode := assignment.StartUtteranceID != "" || assignment.EndUtteranceID != ""
	if wordMode == utteranceMode {
		return talkingHeadRange{}, errors.New("必须在 utterance 语义范围与 word 语义范围之间二选一")
	}
	anchorText := strings.TrimSpace(assignment.AnchorText)
	if wordMode && anchorText != "" {
		return talkingHeadRange{}, errors.New("anchor_text 只能与 utterance 语义范围一起使用，不能再传 word_id")
	}
	if utteranceMode {
		if assignment.StartUtteranceID == "" {
			return talkingHeadRange{}, errors.New("缺少 start_utterance_id")
		}
		start, startOK := utterances[assignment.StartUtteranceID]
		end := start
		endOK := startOK
		if assignment.EndUtteranceID != "" {
			end, endOK = utterances[assignment.EndUtteranceID]
		}
		_, startRemoved := removedUtterances[assignment.StartUtteranceID]
		_, endRemoved := removedUtterances[assignment.EndUtteranceID]
		if !startOK || !endOK || startRemoved || endRemoved || start.StartFrame > end.StartFrame {
			return talkingHeadRange{}, errors.New("utterance_id 未知、已删除或逆序")
		}
		if start.StartFrame < clip.SourceStartFrame || end.EndFrame > clip.SourceEndFrame {
			return talkingHeadRange{}, errors.New("utterance 语义范围不完整地落在指定 A-roll clip 内")
		}
		if anchorText != "" {
			return talkingHeadAnchorTextSourceRange(
				anchorText, start.StartFrame, end.EndFrame, utterances, words,
				removedUtterances, removedWords,
			)
		}
		return talkingHeadRange{Start: start.StartFrame, End: end.EndFrame}, nil
	}
	if assignment.StartWordID == "" {
		return talkingHeadRange{}, errors.New("缺少 start_word_id")
	}
	endWordID := assignment.EndWordID
	if endWordID == "" {
		endWordID = assignment.StartWordID
	}
	wordIndex := make(map[string]int, len(words))
	for index, word := range words {
		wordIndex[word.ID] = index
	}
	startIndex, startOK := wordIndex[assignment.StartWordID]
	endIndex, endOK := wordIndex[endWordID]
	if !startOK || !endOK || startIndex > endIndex {
		return talkingHeadRange{}, errors.New("word_id 未知或逆序")
	}
	for index := startIndex; index <= endIndex; index++ {
		if _, removed := removedWords[words[index].ID]; removed {
			return talkingHeadRange{}, errors.New("word_id 已在本次删除范围内")
		}
		for utteranceID := range removedUtterances {
			utterance, exists := utterances[utteranceID]
			if exists && words[index].StartFrame < utterance.EndFrame &&
				utterance.StartFrame < words[index].EndFrame {
				return talkingHeadRange{}, errors.New("word_id 属于本次已删除 utterance")
			}
		}
	}
	start, end := words[startIndex].StartFrame, words[endIndex].EndFrame
	if start < clip.SourceStartFrame || end > clip.SourceEndFrame {
		return talkingHeadRange{}, errors.New("word 语义范围不完整地落在指定 A-roll clip 内")
	}
	return talkingHeadRange{Start: start, End: end}, nil
}

func talkingHeadAnchorTextSourceRange(
	anchorText string,
	windowStart int,
	windowEnd int,
	utterances map[string]speechUtterance,
	words []speechWord,
	removedUtterances map[string]struct{},
	removedWords map[string]struct{},
) (talkingHeadRange, error) {
	target := []rune(normalizeSpeechText(anchorText))
	if len(target) < 2 {
		return talkingHeadRange{}, errors.New("anchor_text 过短，至少需要 2 个可检索字符")
	}
	windowWords := make([]speechWord, 0)
	blocked := make([]bool, 0)
	for _, word := range words {
		if word.EndFrame <= windowStart || word.StartFrame >= windowEnd {
			continue
		}
		isBlocked := false
		if _, removed := removedWords[word.ID]; removed {
			isBlocked = true
		}
		for utteranceID := range removedUtterances {
			utterance, exists := utterances[utteranceID]
			if exists && word.StartFrame < utterance.EndFrame && utterance.StartFrame < word.EndFrame {
				isBlocked = true
				break
			}
		}
		windowWords = append(windowWords, word)
		blocked = append(blocked, isBlocked)
	}
	characters := make([]rune, 0)
	owners := make([]int, 0)
	for index, word := range windowWords {
		for _, character := range normalizeSpeechText(word.Text + word.Punctuation) {
			characters = append(characters, character)
			owners = append(owners, index)
		}
	}
	if len(characters) < len(target) {
		return talkingHeadRange{}, errors.New("anchor_text 不存在于指定 utterance 范围的词级转写中")
	}
	type wordSpan struct{ Start, End int }
	matches := map[wordSpan]struct{}{}
	blockedMatch := false
	for offset := 0; offset+len(target) <= len(characters); offset++ {
		matched := true
		for index := range target {
			if characters[offset+index] != target[index] {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		span := wordSpan{Start: owners[offset], End: owners[offset+len(target)-1]}
		for index := span.Start; index <= span.End; index++ {
			if blocked[index] {
				blockedMatch = true
				matched = false
				break
			}
		}
		if matched {
			matches[span] = struct{}{}
		}
	}
	if len(matches) == 0 {
		if blockedMatch {
			return talkingHeadRange{}, errors.New("anchor_text 包含本次将删除的词或 utterance；请改用删除后仍连续保留的原文，或改传保留的 word_id")
		}
		return talkingHeadRange{}, errors.New("anchor_text 不存在、已删除，或没有连续落在指定 utterance 范围内")
	}
	if len(matches) > 1 {
		return talkingHeadRange{}, errors.New("anchor_text 在指定 utterance 范围内不唯一；请缩小 utterance 范围或改用 word_id")
	}
	for match := range matches {
		return talkingHeadRange{
			Start: windowWords[match.Start].StartFrame,
			End:   windowWords[match.End].EndFrame,
		}, nil
	}
	return talkingHeadRange{}, errors.New("anchor_text 无法解析")
}

func mergeTalkingHeadRanges(values []talkingHeadRange) []talkingHeadRange {
	if len(values) == 0 {
		return nil
	}
	sort.Slice(values, func(left, right int) bool {
		if values[left].Start != values[right].Start {
			return values[left].Start < values[right].Start
		}
		return values[left].End < values[right].End
	})
	result := []talkingHeadRange{values[0]}
	for _, value := range values[1:] {
		last := &result[len(result)-1]
		if value.Start <= last.End {
			last.End = max(last.End, value.End)
			continue
		}
		result = append(result, value)
	}
	return result
}

func talkingHeadRetainedSpeechRanges(
	utterances []speechUtterance,
	removedUtterances map[string]struct{},
	removedWords map[string]struct{},
	clip timeline.Clip,
) []talkingHeadRange {
	result := []talkingHeadRange{}
	for _, utterance := range utterances {
		if _, removed := removedUtterances[utterance.ID]; removed {
			continue
		}
		if utterance.EndFrame <= clip.SourceStartFrame || utterance.StartFrame >= clip.SourceEndFrame {
			continue
		}
		if len(utterance.Words) == 0 {
			result = append(result, talkingHeadRange{
				Start: max(utterance.StartFrame, clip.SourceStartFrame),
				End:   min(utterance.EndFrame, clip.SourceEndFrame),
			})
			continue
		}
		for _, word := range utterance.Words {
			if _, removed := removedWords[word.ID]; removed {
				continue
			}
			if word.EndFrame <= clip.SourceStartFrame || word.StartFrame >= clip.SourceEndFrame {
				continue
			}
			result = append(result, talkingHeadRange{
				Start: max(word.StartFrame, clip.SourceStartFrame),
				End:   min(word.EndFrame, clip.SourceEndFrame),
			})
		}
	}
	return mergeTalkingHeadRanges(result)
}

func bridgeTalkingHeadRanges(
	deletions []talkingHeadRange,
	retainedSpeech []talkingHeadRange,
	detectedPauses []speechPause,
	maxGap int,
) []talkingHeadRange {
	deletions = mergeTalkingHeadRanges(deletions)
	if len(deletions) < 2 {
		return deletions
	}
	result := []talkingHeadRange{deletions[0]}
	for _, deletion := range deletions[1:] {
		last := &result[len(result)-1]
		gap := talkingHeadRange{Start: last.End, End: deletion.Start}
		unvoiced := !talkingHeadRangeOverlapsAny(gap, retainedSpeech)
		microGap := maxGap > 0 && gap.End-gap.Start <= maxGap
		detectedSilence := talkingHeadPauseCoverage(gap, detectedPauses) >= 0.8
		if gap.End >= gap.Start && unvoiced && (microGap || detectedSilence) {
			last.End = max(last.End, deletion.End)
			continue
		}
		result = append(result, deletion)
	}
	return result
}

func talkingHeadPauseCoverage(target talkingHeadRange, pauses []speechPause) float64 {
	if target.End <= target.Start {
		return 0
	}
	covered := []talkingHeadRange{}
	for _, pause := range pauses {
		start := max(target.Start, pause.StartFrame)
		end := min(target.End, pause.EndFrame)
		if end > start {
			covered = append(covered, talkingHeadRange{Start: start, End: end})
		}
	}
	covered = mergeTalkingHeadRanges(covered)
	frames := 0
	for _, value := range covered {
		frames += value.End - value.Start
	}
	return float64(frames) / float64(target.End-target.Start)
}

func absorbTalkingHeadEdgeSlivers(
	deletions []talkingHeadRange,
	retainedSpeech []talkingHeadRange,
	clipStart, clipEnd, maxGap int,
) []talkingHeadRange {
	deletions = mergeTalkingHeadRanges(deletions)
	if len(deletions) == 0 || maxGap <= 0 {
		return deletions
	}
	leading := talkingHeadRange{Start: clipStart, End: deletions[0].Start}
	if leading.End >= leading.Start && leading.End-leading.Start <= maxGap &&
		!talkingHeadRangeOverlapsAny(leading, retainedSpeech) {
		deletions[0].Start = clipStart
	}
	last := &deletions[len(deletions)-1]
	trailing := talkingHeadRange{Start: last.End, End: clipEnd}
	if trailing.End >= trailing.Start && trailing.End-trailing.Start <= maxGap &&
		!talkingHeadRangeOverlapsAny(trailing, retainedSpeech) {
		last.End = clipEnd
	}
	return deletions
}

func talkingHeadOrphanSpeechFragments(
	deletions []talkingHeadRange,
	retainedSpeech []talkingHeadRange,
	utterances []speechUtterance,
	removedUtterances map[string]struct{},
	removedWords map[string]struct{},
	removedPauses []speechPause,
	clipStart, clipEnd, minimumFrames int,
	misspeakEvidence []talkingHeadEvidenceRange,
) []map[string]any {
	if minimumFrames <= 0 || clipEnd <= clipStart {
		return nil
	}
	deletions = mergeTalkingHeadRanges(deletions)
	type retainedFragment struct {
		rangeValue  talkingHeadRange
		leftDelete  *talkingHeadRange
		rightDelete *talkingHeadRange
	}
	fragments := []retainedFragment{}
	cursor := clipStart
	var previousDeletion *talkingHeadRange
	for index := range deletions {
		deletion := deletions[index]
		deletion.Start = max(deletion.Start, clipStart)
		deletion.End = min(deletion.End, clipEnd)
		if deletion.End <= clipStart || deletion.Start >= clipEnd || deletion.End <= deletion.Start {
			continue
		}
		if deletion.Start > cursor {
			copyOfDeletion := deletion
			fragments = append(fragments, retainedFragment{
				rangeValue: talkingHeadRange{Start: cursor, End: deletion.Start},
				leftDelete: previousDeletion, rightDelete: &copyOfDeletion,
			})
		}
		cursor = max(cursor, deletion.End)
		copyOfDeletion := deletion
		previousDeletion = &copyOfDeletion
	}
	if cursor < clipEnd {
		fragments = append(fragments, retainedFragment{
			rangeValue: talkingHeadRange{Start: cursor, End: clipEnd},
			leftDelete: previousDeletion,
		})
	}
	result := []map[string]any{}
	for _, fragment := range fragments {
		if !talkingHeadRangeOverlapsAny(fragment.rangeValue, retainedSpeech) {
			continue
		}
		// 只有被删除区间从两侧夹住的保留片段才算"孤岛"；开头或结尾的短片段有一侧连着
		// 素材边界、在时间线上与相邻内容相接，不视为孤立碎片。
		if fragment.leftDelete == nil || fragment.rightDelete == nil {
			continue
		}
		duration := fragment.rangeValue.End - fragment.rangeValue.Start
		misspeakIDs := talkingHeadIslandMisspeakMatches(fragment.rangeValue, misspeakEvidence)
		tooShort := duration < minimumFrames
		if len(misspeakIDs) == 0 && !tooShort {
			continue
		}
		reason := "too_short"
		if len(misspeakIDs) > 0 {
			reason = "lands_on_misspeak_evidence"
		}
		adjacentRanges := []talkingHeadRange{}
		if fragment.leftDelete != nil {
			adjacentRanges = append(adjacentRanges, *fragment.leftDelete)
		}
		if fragment.rightDelete != nil {
			adjacentRanges = append(adjacentRanges, *fragment.rightDelete)
		}
		// 落在口误证据上的孤岛应当被删除而非并入保留内容，因此不给它可机械撤回的
		// 相邻气口；防护循环只对纯粹过短的好台词尝试撤回气口来消解孤岛。
		adjacentPauseIDs := []string{}
		if reason == "too_short" {
			for _, pause := range removedPauses {
				if pause.DeleteEnd == fragment.rangeValue.Start || pause.DeleteStart == fragment.rangeValue.End {
					adjacentPauseIDs = append(adjacentPauseIDs, pause.ID)
				}
			}
		}
		item := map[string]any{
			"source_start_frame": fragment.rangeValue.Start,
			"source_end_frame":   fragment.rangeValue.End,
			"duration_frames":    duration,
			"reason":             reason,
			"retained_text": talkingHeadTranscriptText(
				utterances, fragment.rangeValue.Start, fragment.rangeValue.End,
				removedUtterances, removedWords,
			),
			"adjacent_deleted_ranges": adjacentRanges,
			"adjacent_pause_ids":      adjacentPauseIDs,
		}
		if len(misspeakIDs) > 0 {
			item["matched_evidence_ids"] = misspeakIDs
		}
		result = append(result, item)
	}
	return result
}

func talkingHeadRangeOverlapsAny(target talkingHeadRange, values []talkingHeadRange) bool {
	for _, value := range values {
		if target.Start < value.End && value.Start < target.End {
			return true
		}
	}
	return false
}

func talkingHeadTranscriptText(
	utterances []speechUtterance,
	startFrame, endFrame int,
	removedUtterances map[string]struct{},
	removedWords map[string]struct{},
) string {
	parts := []string{}
	for _, utterance := range utterances {
		if utterance.EndFrame <= startFrame || utterance.StartFrame >= endFrame {
			continue
		}
		if _, removed := removedUtterances[utterance.ID]; removed {
			continue
		}
		if len(utterance.Words) == 0 {
			parts = append(parts, utterance.Text)
			continue
		}
		text := ""
		for _, word := range utterance.Words {
			if word.EndFrame <= startFrame || word.StartFrame >= endFrame {
				continue
			}
			if _, removed := removedWords[word.ID]; removed {
				continue
			}
			text += word.Text + word.Punctuation
		}
		if text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "")
}

func talkingHeadTimelineCoverage(
	document timeline.Document, assetID string, sourceStart, sourceEnd int,
) (talkingHeadRange, error) {
	ranges := []talkingHeadRange{}
	for _, track := range document.Tracks {
		if track.TrackID != "visual_base" {
			continue
		}
		for _, clip := range track.Clips {
			if clip.AssetID != assetID {
				continue
			}
			if start, end, ok := mapSourceRangeToTimelineClip(clip, sourceStart, sourceEnd); ok {
				ranges = append(ranges, talkingHeadRange{Start: start, End: end})
			}
		}
	}
	if len(ranges) == 0 {
		return talkingHeadRange{}, errors.New("语义源区间已经被删除或不在主视频轨")
	}
	sort.Slice(ranges, func(left, right int) bool { return ranges[left].Start < ranges[right].Start })
	for index := 1; index < len(ranges); index++ {
		if ranges[index].Start != ranges[index-1].End {
			return talkingHeadRange{}, errors.New("同一语义源区间在时间线上不是唯一连续区域")
		}
	}
	return talkingHeadRange{Start: ranges[0].Start, End: ranges[len(ranges)-1].End}, nil
}

func talkingHeadOverlayOverlaps(document timeline.Document, target talkingHeadRange) bool {
	for _, track := range document.Tracks {
		if track.TrackID != "visual_overlay" {
			continue
		}
		for _, clip := range track.Clips {
			if clip.TimelineStartFrame < target.End && target.Start < clip.TimelineEndFrame {
				return true
			}
		}
	}
	return false
}

type talkingHeadEvidenceRange struct {
	ID    string
	Start int
	End   int
}

// talkingHeadMisspeakEvidence 汇总落在当前 clip 内的口误证据源区间（句内重复的两遍
// 说法与短语音/重说残片），用于判断保留孤岛本身是否就是一段应删的口误。
func talkingHeadMisspeakEvidence(
	repetitions []rushestools.SpeechRepetitionEvidence,
	fragments []rushestools.SpeechFragmentEvidence,
	clip timeline.Clip,
) []talkingHeadEvidenceRange {
	result := []talkingHeadEvidenceRange{}
	within := func(start, end int) bool {
		return start >= clip.SourceStartFrame && end <= clip.SourceEndFrame && end > start
	}
	for _, repetition := range repetitions {
		if within(repetition.EarlierSourceStartFrame, repetition.EarlierSourceEndFrame) {
			result = append(result, talkingHeadEvidenceRange{
				ID:    repetition.RepetitionID + ":earlier",
				Start: repetition.EarlierSourceStartFrame, End: repetition.EarlierSourceEndFrame,
			})
		}
		if within(repetition.LaterSourceStartFrame, repetition.LaterSourceEndFrame) {
			result = append(result, talkingHeadEvidenceRange{
				ID:    repetition.RepetitionID + ":later",
				Start: repetition.LaterSourceStartFrame, End: repetition.LaterSourceEndFrame,
			})
		}
	}
	for _, fragment := range fragments {
		if within(fragment.SourceStartFrame, fragment.SourceEndFrame) {
			result = append(result, talkingHeadEvidenceRange{
				ID: fragment.FragmentID, Start: fragment.SourceStartFrame, End: fragment.SourceEndFrame,
			})
		}
	}
	return result
}

// talkingHeadIslandMisspeakMatches 返回把该保留孤岛过半覆盖的口误证据 ID：只有当单条
// 证据覆盖孤岛的多数时长时，才认定"孤岛本身就是口误"，避免误伤内部仅含少量重复的
// 完整长句。
func talkingHeadIslandMisspeakMatches(
	island talkingHeadRange,
	evidence []talkingHeadEvidenceRange,
) []string {
	islandDuration := island.End - island.Start
	if islandDuration <= 0 {
		return nil
	}
	ids := []string{}
	for _, candidate := range evidence {
		overlap := min(island.End, candidate.End) - max(island.Start, candidate.Start)
		if overlap > 0 && overlap*2 >= islandDuration {
			ids = append(ids, candidate.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

// talkingHeadIslandCounterProposals 为每个孤岛给出"并入相邻删除"的合并区间与可直接
// 采纳的删除锚点，让模型确认删掉这段碎片，而不是自行发明绕过防护的缩删方式。
func talkingHeadIslandCounterProposals(
	orphanFragments []map[string]any,
	utterances []speechUtterance,
) []map[string]any {
	result := make([]map[string]any, 0, len(orphanFragments))
	for _, orphan := range orphanFragments {
		islandStart, _ := orphan["source_start_frame"].(int)
		islandEnd, _ := orphan["source_end_frame"].(int)
		merged := talkingHeadRange{Start: islandStart, End: islandEnd}
		if adjacent, ok := orphan["adjacent_deleted_ranges"].([]talkingHeadRange); ok {
			for _, deletion := range adjacent {
				merged.Start = min(merged.Start, deletion.Start)
				merged.End = max(merged.End, deletion.End)
			}
		}
		proposal := map[string]any{
			"island_source_start_frame":        islandStart,
			"island_source_end_frame":          islandEnd,
			"island_duration_frames":           orphan["duration_frames"],
			"island_text":                      orphan["retained_text"],
			"reason":                           orphan["reason"],
			"merged_delete_source_start_frame": merged.Start,
			"merged_delete_source_end_frame":   merged.End,
		}
		if startWordID, endWordID, ok := talkingHeadIslandWordRange(utterances, islandStart, islandEnd); ok {
			proposal["island_start_word_id"] = startWordID
			proposal["island_end_word_id"] = endWordID
		}
		if ids, ok := orphan["matched_evidence_ids"].([]string); ok && len(ids) > 0 {
			proposal["matched_evidence_ids"] = ids
		}
		result = append(result, proposal)
	}
	return result
}

// talkingHeadIslandWordRange 返回完整落在孤岛内的首尾 word_id，供模型用 remove_word_ranges
// 直接采纳 counter-proposal；无词级证据时返回 ok=false。
func talkingHeadIslandWordRange(utterances []speechUtterance, start, end int) (string, string, bool) {
	startID, endID := "", ""
	for _, utterance := range utterances {
		for _, word := range utterance.Words {
			if word.StartFrame >= start && word.EndFrame <= end {
				if startID == "" {
					startID = word.ID
				}
				endID = word.ID
			}
		}
	}
	return startID, endID, startID != "" && endID != ""
}
