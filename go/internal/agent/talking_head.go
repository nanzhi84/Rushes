package agent

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

func (service *Service) toolEditTalkingHead(
	ctx context.Context,
	draftID string,
	input rushestools.TalkingHeadEditInput,
) (rushestools.ToolResult, error) {
	speechCleanupRequested := len(input.RemoveUtteranceIDs) > 0 || len(input.RemoveWordRanges) > 0 ||
		len(input.RemovePauseIDs) > 0 || len(input.PauseDecisions) > 0 ||
		len(input.RepetitionDecisions) > 0 || len(input.ShortFragmentDecisions) > 0
	failed := func(message string, data map[string]any) (rushestools.ToolResult, error) {
		if data == nil {
			data = map[string]any{}
		}
		data["current_timeline_unchanged"] = true
		return rushestools.ToolResult{Status: "failed", Observation: message, Data: data}, nil
	}
	if len(input.RemoveUtteranceIDs) == 0 && len(input.RemoveWordRanges) == 0 &&
		len(input.RemovePauseIDs) == 0 && len(input.BrollAssignments) == 0 &&
		len(input.PauseDecisions) == 0 && len(input.RepetitionDecisions) == 0 &&
		len(input.ShortFragmentDecisions) == 0 {
		return failed("timeline.edit_talking_head 至少需要一个删除项或 B-roll 对应关系", map[string]any{
			"recovery": "先调用 speech.inspect 和 media.search_shots，再传模型已选择的稳定 ID",
		})
	}
	document, err := timeline.Latest(ctx, service.database, draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	selectedClip, found := talkingHeadPrimaryClip(document, input.ARollTimelineClipID)
	if !found {
		return failed("a_roll_timeline_clip_id 不存在于主视频轨", map[string]any{
			"recovery": "调用 timeline.inspect 取得当前 visual_base clip ID",
		})
	}
	asset, role, err := service.talkingHeadAsset(ctx, draftID, selectedClip.AssetID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	if selectedClip.Role == "b_roll" || role == "b_roll" {
		return failed("指定片段被素材目录或理解结果识别为 B-roll，不能作为口播主干", map[string]any{
			"asset_id": asset.ID, "semantic_role": role,
			"recovery": "调用 asset.list_assets，选择 suggested_visual_role=a_roll 的主视频 clip",
		})
	}
	transcript, err := storage.LatestTranscript(ctx, service.database.Read(), asset.ID)
	if errors.Is(err, storage.ErrNotFound) {
		return failed("A-roll 尚无持久化逐句索引", map[string]any{
			"asset_id": asset.ID,
			"recovery": "先对该 timeline_clip_id 调用 speech.inspect；它会复用同名 SRT 或调用已配置 ASR",
		})
	}
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	utterances, err := decodeSpeechUtterances(transcript.Utterances)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	pauses, err := decodeSpeechPauses(transcript.VADSegments)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	pauses = clampSpeechPausesToWordBoundaries(asset.ID, pauses, utterances)
	pauseByID := make(map[string]speechPause, len(pauses))
	for _, pause := range pauses {
		pauseByID[pause.ID] = pause
	}
	var decidedPauseIDs map[string]struct{}
	var invalidPauseDecisions []map[string]any
	input, decidedPauseIDs, invalidPauseDecisions = expandTalkingHeadPauseDecisions(input, pauseByID)
	if len(invalidPauseDecisions) > 0 {
		return failed("pause_decisions 包含未知、重复、冲突或非法决定", map[string]any{
			"invalid_pause_decisions": invalidPauseDecisions,
			"recovery":                "重新读取 speech.inspect.pauses；每个 pause_id 只提交一次，action 只能是 remove 或 preserve，且 preserve 不能同时出现在 remove_pause_ids。",
		})
	}
	repetitions := intraUtteranceSpeechRepetitions(asset.ID, utterances, maxSimilarPairs)
	repetitionByID := make(map[string]rushestools.SpeechRepetitionEvidence, len(repetitions))
	for _, repetition := range repetitions {
		repetitionByID[repetition.RepetitionID] = repetition
	}
	var decidedRepetitionIDs map[string]struct{}
	var invalidRepetitionDecisions []map[string]any
	input, decidedRepetitionIDs, invalidRepetitionDecisions = expandTalkingHeadRepetitionDecisions(
		input, repetitionByID,
	)
	if len(invalidRepetitionDecisions) > 0 {
		return failed("repetition_decisions 包含未知、重复或非法决定", map[string]any{
			"invalid_repetition_decisions": invalidRepetitionDecisions,
			"recovery":                     "重新读取 speech.inspect.intra_utterance_repetitions；每个 repetition_id 只提交一次，action 只能是 remove_earlier、remove_later 或 preserve。",
		})
	}
	shortFragments := shortLeadingSpeechFragments(asset.ID, utterances, pauses, maxSimilarPairs)
	fragmentByID := make(map[string]rushestools.SpeechFragmentEvidence, len(shortFragments))
	for _, fragment := range shortFragments {
		fragmentByID[fragment.FragmentID] = fragment
	}
	fragmentExpansion := expandTalkingHeadFragmentDecisions(input, fragmentByID)
	input = fragmentExpansion.Input
	if len(fragmentExpansion.Invalid) > 0 {
		return failed("short_fragment_decisions 包含未知、重复或非法决定", map[string]any{
			"invalid_fragment_decisions": fragmentExpansion.Invalid,
			"recovery":                   "重新读取 speech.inspect.short_speech_fragments；每个 fragment_id 只提交一次，action 只能是 remove 或 preserve。",
		})
	}
	if len(input.RemoveUtteranceIDs) == 0 && len(input.RemoveWordRanges) == 0 &&
		len(input.RemovePauseIDs) == 0 && len(input.BrollAssignments) == 0 {
		return failed("所有重复词/短片段决定均为保留，本次没有实际时间线修改", map[string]any{
			"recovery": "如无需删剪，请添加确有语义对应的 B-roll；否则重新审阅候选并把需删除项标为 remove。",
		})
	}
	preservedFragmentIDs := make(map[string]struct{}, len(fragmentExpansion.PreservedIDs))
	for _, id := range fragmentExpansion.PreservedIDs {
		preservedFragmentIDs[id] = struct{}{}
	}
	preserveReasonRequired := []rushestools.SpeechFragmentEvidence{}
	for id := range preservedFragmentIDs {
		fragment := fragmentByID[id]
		if fragment.Kind != "restart_prefix_before_repeated_take" &&
			fragment.Kind != "earlier_take_before_repeated_phrase_restart" {
			continue
		}
		reason := strings.TrimSpace(fragmentExpansion.PreservedReasons[id])
		if validRestartFragmentPreserveReason(fragment, reason) {
			continue
		}
		preserveReasonRequired = append(preserveReasonRequired, fragment)
	}
	if len(preserveReasonRequired) > 0 {
		return failed("重说接入前缀或重说尾部的保留决策缺少可审计的语义理由", map[string]any{
			"preserve_reason_required": preserveReasonRequired,
			"reason_requirements":      "理由至少 20 个字，必须原样引用 fragment.text 和 restart_anchor_text，并解释 joined_context 的语法与语义为何完整",
			"recovery":                 "逐项读取 previous_context、joined_context 与 matched_earlier_text：若拼接后是病句或重说残片，用 start_word_id/end_word_id 删除；若确应保留，理由必须原样引用 fragment.text 和 restart_anchor_text 并解释完整语义。不要只写“正常”“衔接”或“保留”。",
		})
	}
	utteranceByID := make(map[string]speechUtterance, len(utterances))
	wordSequence := []speechWord{}
	for _, utterance := range utterances {
		utteranceByID[utterance.ID] = utterance
		wordSequence = append(wordSequence, utterance.Words...)
	}
	sort.SliceStable(wordSequence, func(left, right int) bool {
		return wordSequence[left].StartFrame < wordSequence[right].StartFrame
	})
	removedUtterances, invalidUtterances := selectTalkingHeadUtterances(
		input.RemoveUtteranceIDs, utteranceByID, selectedClip,
	)
	removedWordRanges, removedWordIDs, invalidWordRanges := selectTalkingHeadWordRanges(
		input.RemoveWordRanges, wordSequence, selectedClip,
	)
	removedPauses, invalidPauses := selectTalkingHeadPauses(input.RemovePauseIDs, pauseByID, selectedClip)
	if len(invalidUtterances) > 0 || len(invalidWordRanges) > 0 || len(invalidPauses) > 0 {
		return failed("删除项包含未知 ID，或证据范围不完整地落在指定 A-roll clip 内", map[string]any{
			"invalid_utterance_ids": invalidUtterances, "invalid_word_ranges": invalidWordRanges,
			"invalid_pause_ids": invalidPauses,
			"recovery":          "重新对当前 a_roll_timeline_clip_id 调用 speech.inspect；句内删剪需 include_words=true，并只使用本次返回的连续 ID",
		})
	}
	removedIDSet := make(map[string]struct{}, len(input.RemoveUtteranceIDs))
	for _, id := range input.RemoveUtteranceIDs {
		removedIDSet[id] = struct{}{}
	}
	removedWordIDSet := make(map[string]struct{}, len(removedWordIDs))
	for _, id := range removedWordIDs {
		removedWordIDSet[id] = struct{}{}
	}
	assignmentSourceRanges := make([]talkingHeadRange, len(input.BrollAssignments))
	invalidAssignments := []map[string]any{}
	for index, assignment := range input.BrollAssignments {
		sourceRange, resolveErr := talkingHeadAssignmentSourceRange(
			assignment, utteranceByID, wordSequence, removedIDSet, removedWordIDSet, selectedClip,
		)
		if resolveErr != nil {
			startUtterance, startExists := utteranceByID[assignment.StartUtteranceID]
			endUtteranceID := assignment.EndUtteranceID
			if endUtteranceID == "" {
				endUtteranceID = assignment.StartUtteranceID
			}
			endUtterance, endExists := utteranceByID[endUtteranceID]
			_, startRemoved := removedIDSet[assignment.StartUtteranceID]
			_, endRemoved := removedIDSet[endUtteranceID]
			availableText := ""
			if startExists && endExists && startUtterance.StartFrame <= endUtterance.StartFrame {
				parts := []string{}
				for _, utterance := range utterances {
					if utterance.StartFrame >= startUtterance.StartFrame && utterance.EndFrame <= endUtterance.EndFrame {
						parts = append(parts, utterance.Text)
					}
				}
				availableText = strings.Join(parts, "")
			}
			invalidAssignments = append(invalidAssignments, map[string]any{
				"index": index, "shot_id": assignment.ShotID,
				"start_utterance_id": assignment.StartUtteranceID,
				"end_utterance_id":   assignment.EndUtteranceID,
				"anchor_text":        assignment.AnchorText,
				"start_word_id":      assignment.StartWordID, "end_word_id": assignment.EndWordID,
				"start_utterance_exists":   startExists,
				"end_utterance_exists":     endExists,
				"start_utterance_removed":  startRemoved,
				"end_utterance_removed":    endRemoved,
				"available_utterance_text": availableText,
				"reason":                   resolveErr.Error(),
			})
			continue
		}
		assignmentSourceRanges[index] = sourceRange
	}
	if len(invalidAssignments) > 0 {
		return failed("B-roll 对应关系引用了未知、已删除、逆序或不属于该 A-roll clip 的语义 ID", map[string]any{
			"invalid_assignments": invalidAssignments,
			"recovery":            "用 speech.inspect 返回的未删除 utterance_id，并可在其中附带唯一的原文 anchor_text；若原文不唯一则改用连续 word_id。utterance 与 word 两种锚点二选一",
		})
	}
	semanticDeleteRanges := make([]talkingHeadRange, 0, len(removedUtterances)+len(removedWordRanges))
	for _, utterance := range removedUtterances {
		semanticDeleteRanges = append(semanticDeleteRanges, talkingHeadRange{Start: utterance.StartFrame, End: utterance.EndFrame})
	}
	semanticDeleteRanges = append(semanticDeleteRanges, removedWordRanges...)
	semanticDeleteRanges = mergeTalkingHeadRanges(semanticDeleteRanges)
	effectivePauses, _, redundantPauses := resolveTalkingHeadPauseRanges(
		removedPauses, semanticDeleteRanges, minTalkingHeadPauseResidualFrames,
	)
	if len(removedPauses) > 0 && len(effectivePauses) == 0 && len(redundantPauses) > 0 {
		candidates := talkingHeadRetainedPauseCandidates(
			pauses, semanticDeleteRanges, selectedClip, utterances,
			minTalkingHeadPauseCandidateFrames, 8,
		)
		if len(candidates) > 0 {
			return failed("所选气口已被句子或词级删除完整覆盖，本次不会产生任何额外气口清理", map[string]any{
				"redundant_pause_ids":       speechPauseIDs(redundantPauses),
				"retained_pause_candidates": candidates,
				"recovery":                  "删除这些冗余 pause_id；若创作意图仍要求清理气口，由模型结合 retained_pause_candidates 的两侧原文自主选择后重试。候选只是客观时长与上下文，不代表必须删除。",
			})
		}
	}
	removedPauses = effectivePauses
	retainedSpeech := talkingHeadRetainedSpeechRanges(
		utterances, removedIDSet, removedWordIDSet, selectedClip,
	)
	misspeakEvidence := talkingHeadMisspeakEvidence(repetitions, shortFragments, selectedClip)
	removedPauses, effectivePauseRanges, sourceDeleteRanges, autoPreservedPauses, orphanFragments :=
		protectTalkingHeadOrphanFragments(
			semanticDeleteRanges, removedPauses, pauses, retainedSpeech, utterances,
			removedIDSet, removedWordIDSet, selectedClip, misspeakEvidence,
		)
	if len(sourceDeleteRanges) == 0 && len(input.BrollAssignments) == 0 {
		message := "本次没有可安全应用的实际编辑"
		recovery := "重新读取 speech.inspect，只提交会产生实际时间线变化的删除项。"
		if len(autoPreservedPauses) > 0 {
			message = "所选气口均会制造孤立语音，本次没有可安全应用的实际编辑"
			recovery = "结合保留台词重新决定相邻语义是否也应删除；若台词应保留，则无需继续删除这些气口。"
		}
		return failed(message, map[string]any{
			"auto_preserved_pause_ids": speechPauseIDs(autoPreservedPauses),
			"redundant_pause_ids":      speechPauseIDs(redundantPauses),
			"recovery":                 recovery,
		})
	}
	if len(orphanFragments) > 0 {
		counterProposals := talkingHeadIslandCounterProposals(orphanFragments, utterances)
		return failed("组合删除会把保留台词夹成不足 2 秒或落在口误证据上的孤立碎片", map[string]any{
			"orphan_fragments":               orphanFragments,
			"island_counter_proposals":       counterProposals,
			"auto_preserved_pause_ids":       speechPauseIDs(autoPreservedPauses),
			"minimum_retained_island_frames": minTalkingHeadRetainedIslandFrames,
			"recovery":                       "优先采纳 island_counter_proposals：把 merged_delete_source_start_frame..merged_delete_source_end_frame 或 island_start_word_id/island_end_word_id 一并加入删除，清掉这段碎片；若这段其实是你要保留的完整台词，则改为撤回它两侧的相邻删除，让它与前后文连成不小于 2 秒的连续片段。不要原样重试，也不要只把删除缩到刚好过阈值。",
		})
	}
	unresolvedPauses := []rushestools.SpeechPauseEvidence{}
	if speechCleanupRequested {
		unresolvedPauses = unresolvedTalkingHeadPauseDecisions(
			pauses, semanticDeleteRanges, selectedClip, utterances,
			decidedPauseIDs, minTalkingHeadPauseCandidateFrames, maxSimilarPairs,
		)
	}
	unresolvedRepetitions := []rushestools.SpeechRepetitionEvidence{}
	for _, repetition := range repetitions {
		if repetition.EarlierSourceStartFrame < selectedClip.SourceStartFrame ||
			repetition.LaterSourceEndFrame > selectedClip.SourceEndFrame {
			continue
		}
		if _, decided := decidedRepetitionIDs[repetition.RepetitionID]; decided {
			continue
		}
		earlierCovered := talkingHeadRangeCoveredBy(talkingHeadRange{
			Start: repetition.EarlierSourceStartFrame, End: repetition.EarlierSourceEndFrame,
		}, sourceDeleteRanges)
		laterCovered := talkingHeadRangeCoveredBy(talkingHeadRange{
			Start: repetition.LaterSourceStartFrame, End: repetition.LaterSourceEndFrame,
		}, sourceDeleteRanges)
		if earlierCovered || laterCovered {
			continue
		}
		unresolvedRepetitions = append(unresolvedRepetitions, repetition)
	}
	unresolvedFragments := []rushestools.SpeechFragmentEvidence{}
	for _, fragment := range shortFragments {
		if fragment.SourceStartFrame < selectedClip.SourceStartFrame ||
			fragment.SourceEndFrame > selectedClip.SourceEndFrame {
			continue
		}
		if _, preserved := preservedFragmentIDs[fragment.FragmentID]; preserved ||
			talkingHeadRangeCoveredBy(
				talkingHeadRange{Start: fragment.SourceStartFrame, End: fragment.SourceEndFrame},
				sourceDeleteRanges,
			) {
			continue
		}
		unresolvedFragments = append(unresolvedFragments, fragment)
	}
	// 未处理候选属于供模型继续判断的内容证据，不是参数非法或时间线
	// 不变量错误。局部修正不应为了当前目标以外的候选被迫失败；全量
	// 口播任务仍会在成功结果中拿到这些证据，并可自主决定是否继续编辑。
	deleteRanges := make([]talkingHeadRange, 0, len(sourceDeleteRanges))
	for _, sourceRange := range sourceDeleteRanges {
		start, end, ok := mapSourceRangeToTimelineClip(selectedClip, sourceRange.Start, sourceRange.End)
		if ok {
			deleteRanges = append(deleteRanges, talkingHeadRange{Start: start, End: end})
		}
	}
	for index := len(deleteRanges) - 1; index >= 0; index-- {
		document, err = timeline.ApplyPatch(document, map[string]any{
			"kind": "delete_range", "start_frame": deleteRanges[index].Start, "end_frame": deleteRanges[index].End,
		})
		if err != nil {
			return failed("口播波纹删除无法合法应用", map[string]any{
				"reason": err.Error(), "failed_range": deleteRanges[index],
				"recovery": "减少相互覆盖的删除项，或重新调用 speech.inspect 读取当前片段证据",
			})
		}
	}

	shotByID := map[string]indexedShot{}
	if len(input.BrollAssignments) > 0 {
		shots, _, shotErr := service.draftShotIndex(ctx, draftID, nil)
		if shotErr != nil {
			return rushestools.ToolResult{}, shotErr
		}
		for _, shot := range shots {
			shotByID[shot.candidate.ShotID] = shot
		}
	}
	shortBrollIssues := []map[string]any{}
	anchorPrecisionIssues := []map[string]any{}
	for index, assignment := range input.BrollAssignments {
		sourceRange := assignmentSourceRanges[index]
		coverage, coverageErr := talkingHeadTimelineCoverage(
			document, asset.ID, sourceRange.Start, sourceRange.End,
		)
		if coverageErr != nil {
			return failed("删除后无法把 B-roll 语义范围唯一映射到当前时间线", map[string]any{
				"assignment_index": index, "reason": coverageErr.Error(),
				"recovery": "重新调用 timeline.inspect 与 speech.inspect，缩小到连续的未删除台词范围",
			})
		}
		shot, exists := shotByID[assignment.ShotID]
		if !exists || shot.candidate.SemanticRole != "b_roll" {
			return failed("B-roll 对应关系包含不存在、已失效或角色不是 b_roll 的 shot_id", map[string]any{
				"assignment_index": index, "shot_id": assignment.ShotID,
				"recovery": "调用 media.search_shots，并设置 semantic_roles=[\"b_roll\"]",
			})
		}
		duration := min(coverage.End-coverage.Start, shot.candidate.DurationFrames)
		semanticDuration := coverage.End - coverage.Start
		if duration < minTalkingHeadBrollDurationFrames {
			shortBrollIssues = append(shortBrollIssues, map[string]any{
				"assignment_index": index, "shot_id": assignment.ShotID,
				"anchor_text":   assignment.AnchorText,
				"start_word_id": assignment.StartWordID, "end_word_id": assignment.EndWordID,
				"b_roll_filename":                    shot.candidate.Filename,
				"placement_duration_frames":          duration,
				"minimum_duration_frames":            minTalkingHeadBrollDurationFrames,
				"semantic_window_source_start_frame": sourceRange.Start,
				"semantic_window_source_end_frame":   sourceRange.End,
				"transcript_text": talkingHeadTranscriptText(
					utterances, sourceRange.Start, sourceRange.End,
					removedIDSet, removedWordIDSet,
				),
			})
		}
		usesPreciseAnchor := strings.TrimSpace(assignment.AnchorText) != "" ||
			strings.TrimSpace(assignment.StartWordID) != "" ||
			strings.TrimSpace(assignment.EndWordID) != ""
		if !usesPreciseAnchor && semanticDuration > duration*2 && semanticDuration-duration > timeline.DefaultFPS {
			anchorPrecisionIssues = append(anchorPrecisionIssues, map[string]any{
				"assignment_index":                   index,
				"shot_id":                            assignment.ShotID,
				"b_roll_filename":                    shot.candidate.Filename,
				"b_roll_description":                 shot.candidate.Description,
				"b_roll_duration_frames":             duration,
				"semantic_window_source_start_frame": sourceRange.Start,
				"semantic_window_source_end_frame":   sourceRange.End,
				"semantic_window_timeline_frames":    semanticDuration,
				"transcript_text": talkingHeadTranscriptText(
					utterances, sourceRange.Start, sourceRange.End,
					removedIDSet, removedWordIDSet,
				),
			})
		}
	}
	if len(shortBrollIssues) > 0 {
		return failed("B-roll 语义锚点不足半秒，会形成闪帧式孤立片段", map[string]any{
			"short_b_roll_assignments": shortBrollIssues,
			"recovery":                 "在同一保留台词中改用更完整且仍精确对应画面的连续 anchor_text/word_id 范围，使每段至少覆盖 15 帧；不能用无关整句硬凑时长。",
		})
	}
	if len(anchorPrecisionIssues) > 0 {
		return failed("短 B-roll 的逐句语义窗口过宽，无法确定它应落在窗口内的具体台词位置", map[string]any{
			"anchor_precision_required": anchorPrecisionIssues,
			"recovery":                  "在返回的 utterance 范围内原样摘录与 B-roll 画面直接对应且唯一的连续台词作为 anchor_text；若短语不唯一，再调用 speech.inspect(include_words=true) 读取连续 start_word_id/end_word_id。",
		})
	}
	inserted := make([]map[string]any, 0, len(input.BrollAssignments))
	for index, assignment := range input.BrollAssignments {
		sourceRange := assignmentSourceRanges[index]
		coverage, coverageErr := talkingHeadTimelineCoverage(
			document, asset.ID, sourceRange.Start, sourceRange.End,
		)
		if coverageErr != nil {
			return failed("删除后无法把 B-roll 语义范围唯一映射到当前时间线", map[string]any{
				"assignment_index": index, "reason": coverageErr.Error(),
				"recovery": "重新调用 timeline.inspect 与 speech.inspect，缩小到连续的未删除台词范围",
			})
		}
		shot, exists := shotByID[assignment.ShotID]
		if !exists || shot.candidate.SemanticRole != "b_roll" {
			return failed("B-roll 对应关系包含不存在、已失效或角色不是 b_roll 的 shot_id", map[string]any{
				"assignment_index": index, "shot_id": assignment.ShotID,
				"recovery": "调用 media.search_shots，并设置 semantic_roles=[\"b_roll\"]",
			})
		}
		// 语义范围是镜头允许出现的台词窗口，而不是要求 B-roll 必须完整覆盖的
		// 时长。短镜头放在窗口起点，长镜头裁到窗口末尾；这样模型可以用词级
		// ID 表达“这段画面属于哪句话”，无需反向计算镜头长度来制造脆弱参数。
		duration := min(coverage.End-coverage.Start, shot.candidate.DurationFrames)
		placement := talkingHeadRange{Start: coverage.Start, End: coverage.Start + duration}
		if talkingHeadOverlayOverlaps(document, placement) {
			return failed("B-roll 覆盖范围与现有叠加轨片段重叠", map[string]any{
				"assignment_index": index, "timeline_start_frame": placement.Start,
				"timeline_end_frame": placement.End,
				"recovery":           "调整 utterance 范围，或先明确删除/移动现有 visual_overlay 片段",
			})
		}
		clipID := randomID("broll")
		transcriptText := talkingHeadTranscriptText(
			utterances, sourceRange.Start, sourceRange.End,
			removedIDSet, removedWordIDSet,
		)
		semanticAnchor := map[string]any{
			"kind": "b_roll_semantic_anchor", "shot_id": assignment.ShotID,
			"a_roll_asset_id":                asset.ID,
			"a_roll_source_start_frame":      sourceRange.Start,
			"a_roll_source_end_frame":        sourceRange.End,
			"start_utterance_id":             assignment.StartUtteranceID,
			"end_utterance_id":               assignment.EndUtteranceID,
			"anchor_text":                    assignment.AnchorText,
			"start_word_id":                  assignment.StartWordID,
			"end_word_id":                    assignment.EndWordID,
			"transcript_text":                transcriptText,
			"b_roll_asset_id":                shot.candidate.AssetID,
			"b_roll_filename":                shot.candidate.Filename,
			"b_roll_description":             shot.candidate.Description,
			"anchor_timeline_start_frame":    coverage.Start,
			"anchor_timeline_end_frame":      coverage.End,
			"placement_timeline_start_frame": placement.Start,
			"placement_timeline_end_frame":   placement.End,
			"placement_policy":               "fit_within_semantic_window",
		}
		document, err = timeline.ApplyPatch(document, map[string]any{
			"kind": "insert_clip", "timeline_clip_id": clipID, "track_id": "visual_overlay",
			"timeline_start_frame": placement.Start, "asset_id": shot.candidate.AssetID,
			"asset_kind": "video", "role": "b_roll",
			"source_start_frame": shot.candidate.SourceStartFrame,
			"source_end_frame":   shot.candidate.SourceStartFrame + duration,
			"metadata":           semanticAnchor,
		})
		if err != nil {
			return failed("B-roll 叠加无法合法应用", map[string]any{
				"assignment_index": index, "reason": err.Error(),
			})
		}
		inserted = append(inserted, map[string]any{
			"timeline_clip_id": clipID, "shot_id": assignment.ShotID,
			"timeline_start_frame": placement.Start, "timeline_end_frame": placement.End,
			"semantic_window_start_frame": coverage.Start,
			"semantic_window_end_frame":   coverage.End,
			"start_utterance_id":          assignment.StartUtteranceID,
			"end_utterance_id":            assignment.EndUtteranceID,
			"anchor_text":                 assignment.AnchorText,
			"start_word_id":               assignment.StartWordID,
			"end_word_id":                 assignment.EndWordID,
			"semantic_anchor":             semanticAnchor,
		})
	}

	if report := timeline.Validate(document); !report.Valid {
		return failed("口播剪辑结果未通过时间线校验", map[string]any{
			"validation_report": report,
			"recovery":          "根据 issues 修正 ID 或范围；当前时间线尚未写入",
		})
	}
	next, err := timeline.NextVersion(ctx, service.database, draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	document.Version = next
	document.TimelineID = fmt.Sprintf("%s:v%d", draftID, next)
	semanticOperation := map[string]any{
		"kind": "edit_talking_head", "a_roll_asset_id": asset.ID,
		"removed_utterance_count":      len(removedUtterances),
		"removed_word_count":           len(removedWordIDs),
		"removed_pause_count":          len(effectivePauseRanges),
		"removed_pause_evidence_count": len(removedPauses),
		"auto_preserved_pause_count":   len(autoPreservedPauses),
		"b_roll_assignment_count":      len(inserted),
	}
	result, err := service.persistTimeline(
		ctx, draftID, document, "edit_talking_head", []map[string]any{semanticOperation},
	)
	if err != nil || result.Status != "succeeded" {
		return result, err
	}
	result.Observation = fmt.Sprintf(
		"已原子完成口播剪辑：删除 %d 句、%d 个词级片段、%d 个独立气口区间，添加 %d 段独立 B-roll 叠加；主视频原声保持联动。",
		len(removedUtterances), len(removedWordIDs), len(effectivePauseRanges), len(inserted),
	)
	if len(autoPreservedPauses) > 0 {
		result.Observation += fmt.Sprintf(
			" 为避免保留台词变成不足 2 秒的孤片，已保守保留 %d 个相邻气口。",
			len(autoPreservedPauses),
		)
	}
	result.Data["a_roll_asset_id"] = asset.ID
	result.Data["removed_utterance_ids"] = append([]string(nil), input.RemoveUtteranceIDs...)
	result.Data["removed_word_ids"] = append([]string(nil), removedWordIDs...)
	result.Data["removed_word_ranges"] = append([]rushestools.TalkingHeadWordRange(nil), input.RemoveWordRanges...)
	result.Data["repetition_decisions"] = append([]rushestools.TalkingHeadRepetitionDecision(nil), input.RepetitionDecisions...)
	result.Data["short_fragment_decisions"] = append([]rushestools.TalkingHeadFragmentDecision(nil), input.ShortFragmentDecisions...)
	result.Data["pause_decisions"] = append([]rushestools.TalkingHeadPauseDecision(nil), input.PauseDecisions...)
	result.Data["removed_pause_ids"] = speechPauseIDs(removedPauses)
	result.Data["removed_pause_ranges"] = effectivePauseRanges
	result.Data["removed_pause_range_count"] = len(effectivePauseRanges)
	result.Data["removed_pause_evidence_count"] = len(removedPauses)
	result.Data["redundant_pause_ids"] = speechPauseIDs(redundantPauses)
	result.Data["auto_preserved_pause_ids"] = speechPauseIDs(autoPreservedPauses)
	result.Data["auto_preserved_pause_count"] = len(autoPreservedPauses)
	result.Data["preserved_speech_fragment_ids"] = append([]string(nil), fragmentExpansion.PreservedIDs...)
	result.Data["preserved_speech_fragment_reasons"] = fragmentExpansion.PreservedReasons
	attachTalkingHeadUnreviewedEvidence(
		&result, unresolvedPauses, unresolvedRepetitions, unresolvedFragments,
	)
	result.Data["deleted_timeline_ranges"] = deleteRanges
	result.Data["b_roll_clips"] = inserted
	if drift := talkingHeadPlanDrift(ctx, autoPreservedPauses, utterances); drift != nil {
		result.Data["plan_drift"] = drift
		result.Observation += " " + drift["summary"].(string)
	}
	quality, qualityErr := service.speechQualityReport(ctx, document)
	if qualityErr != nil {
		return rushestools.ToolResult{}, qualityErr
	}
	result.Data["speech_quality"] = quality
	result.Observation += talkingHeadQualitySummary(quality)
	return result, nil
}

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

func (service *Service) talkingHeadAsset(
	ctx context.Context, draftID, assetID string,
) (storage.Asset, string, error) {
	assets, err := storage.ListDraftAssets(ctx, service.database.Read(), draftID)
	if err != nil {
		return storage.Asset{}, "", err
	}
	for _, asset := range assets {
		if asset.ID != assetID {
			continue
		}
		relDir := ""
		if asset.RelDir != nil {
			relDir = *asset.RelDir
		}
		understood := ""
		if raw, summaryErr := storage.BestMaterialSummary(ctx, service.database.Read(), asset.ID); summaryErr == nil {
			encoded, _ := json.Marshal(raw)
			var summary understanding.Summary
			if json.Unmarshal(encoded, &summary) == nil {
				understood = summary.SemanticRole
			}
		} else if !errors.Is(summaryErr, storage.ErrNotFound) {
			return storage.Asset{}, "", summaryErr
		}
		return asset, understanding.SuggestVisualRole(asset.Filename, relDir, understood), nil
	}
	return storage.Asset{}, "", errors.New("指定的 A-roll 素材不属于当前草稿")
}

func selectTalkingHeadUtterances(
	ids []string, values map[string]speechUtterance, clip timeline.Clip,
) ([]speechUtterance, []string) {
	selected := []speechUtterance{}
	invalid := []string{}
	seen := map[string]struct{}{}
	for _, id := range ids {
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		value, exists := values[id]
		if !exists || value.StartFrame < clip.SourceStartFrame || value.EndFrame > clip.SourceEndFrame {
			invalid = append(invalid, id)
			continue
		}
		selected = append(selected, value)
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
		if !startOK || !endOK || startIndex > endIndex ||
			startOK && words[startIndex].StartFrame < clip.SourceStartFrame ||
			endOK && words[endIndex].EndFrame > clip.SourceEndFrame {
			invalid = append(invalid, map[string]any{
				"index": index, "start_word_id": startID, "end_word_id": endID,
			})
			continue
		}
		ranges = append(ranges, talkingHeadRange{
			Start: words[startIndex].StartFrame, End: words[endIndex].EndFrame,
		})
		for wordIndex := startIndex; wordIndex <= endIndex; wordIndex++ {
			id := words[wordIndex].ID
			if _, duplicate := seenWords[id]; duplicate {
				continue
			}
			seenWords[id] = struct{}{}
			removedIDs = append(removedIDs, id)
		}
	}
	return mergeTalkingHeadRanges(ranges), removedIDs, invalid
}

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
		if !exists || value.DeleteStart < clip.SourceStartFrame || value.DeleteEnd > clip.SourceEndFrame {
			invalid = append(invalid, id)
			continue
		}
		selected = append(selected, value)
	}
	return selected, invalid
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
