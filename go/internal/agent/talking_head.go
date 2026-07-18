package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
	"github.com/nanzhi84/Rushes/go/internal/understanding"
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
	selectedClip, found := agentexec.TalkingHeadPrimaryClip(document, input.ARollTimelineClipID)
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
	utterances, err := agentexec.DecodeSpeechUtterances(transcript.Utterances)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	pauses, err := agentexec.DecodeSpeechPauses(transcript.VADSegments)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	pauses = agentexec.ClampSpeechPausesToWordBoundaries(asset.ID, pauses, utterances)
	pauseByID := make(map[string]agentexec.SpeechPause, len(pauses))
	for _, pause := range pauses {
		pauseByID[pause.ID] = pause
	}
	var decidedPauseIDs map[string]struct{}
	var invalidPauseDecisions []map[string]any
	input, decidedPauseIDs, invalidPauseDecisions = agentexec.ExpandTalkingHeadPauseDecisions(input, pauseByID)
	if len(invalidPauseDecisions) > 0 {
		return failed("pause_decisions 包含未知、重复、冲突或非法决定", map[string]any{
			"invalid_pause_decisions": invalidPauseDecisions,
			"recovery":                "重新读取 speech.inspect.pauses；每个 pause_id 只提交一次，action 只能是 remove 或 preserve，且 preserve 不能同时出现在 remove_pause_ids。",
		})
	}
	repetitions := agentexec.IntraUtteranceSpeechRepetitions(asset.ID, utterances, agentexec.MaxSimilarPairs)
	repetitionByID := make(map[string]rushestools.SpeechRepetitionEvidence, len(repetitions))
	for _, repetition := range repetitions {
		repetitionByID[repetition.RepetitionID] = repetition
	}
	var decidedRepetitionIDs map[string]struct{}
	var invalidRepetitionDecisions []map[string]any
	input, decidedRepetitionIDs, invalidRepetitionDecisions = agentexec.ExpandTalkingHeadRepetitionDecisions(
		input, repetitionByID,
	)
	if len(invalidRepetitionDecisions) > 0 {
		return failed("repetition_decisions 包含未知、重复或非法决定", map[string]any{
			"invalid_repetition_decisions": invalidRepetitionDecisions,
			"recovery":                     "重新读取 speech.inspect.intra_utterance_repetitions；每个 repetition_id 只提交一次，action 只能是 remove_earlier、remove_later 或 preserve。",
		})
	}
	shortFragments := agentexec.ShortLeadingSpeechFragments(asset.ID, utterances, pauses, agentexec.MaxSimilarPairs)
	fragmentByID := make(map[string]rushestools.SpeechFragmentEvidence, len(shortFragments))
	for _, fragment := range shortFragments {
		fragmentByID[fragment.FragmentID] = fragment
	}
	fragmentExpansion := agentexec.ExpandTalkingHeadFragmentDecisions(input, fragmentByID)
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
		if agentexec.ValidRestartFragmentPreserveReason(fragment, reason) {
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
	utteranceByID := make(map[string]agentexec.SpeechUtterance, len(utterances))
	wordSequence := []agentexec.SpeechWord{}
	for _, utterance := range utterances {
		utteranceByID[utterance.ID] = utterance
		wordSequence = append(wordSequence, utterance.Words...)
	}
	sort.SliceStable(wordSequence, func(left, right int) bool {
		return wordSequence[left].StartFrame < wordSequence[right].StartFrame
	})
	removedUtteranceRanges, invalidUtterances := agentexec.SelectTalkingHeadUtterances(
		input.RemoveUtteranceIDs, utteranceByID, selectedClip,
	)
	removedWordRanges, removedWordIDs, invalidWordRanges := agentexec.SelectTalkingHeadWordRanges(
		input.RemoveWordRanges, wordSequence, selectedClip,
	)
	removedPauses, invalidPauses := agentexec.SelectTalkingHeadPauses(input.RemovePauseIDs, pauseByID, selectedClip)
	if len(invalidUtterances) > 0 || len(invalidWordRanges) > 0 || len(invalidPauses) > 0 {
		data := map[string]any{
			"invalid_utterance_ids": invalidUtterances, "invalid_word_ranges": invalidWordRanges,
			"invalid_pause_ids": invalidPauses,
			"recovery":          "逐条核对：evidence_current_clips 会指出证据当前所属的 timeline_clip_id，请改用该 clip 重新调用；其余为未知 ID，需重新对当前 a_roll_timeline_clip_id 调用 speech.inspect（句内删剪设 include_words=true）。inspect 返回的证据已按该 clip 裁剪，可直接使用其中的 ID。",
		}
		if hints := agentexec.TalkingHeadEvidenceClipHints(
			document, asset.ID, invalidUtterances, utteranceByID,
			invalidWordRanges, wordSequence, invalidPauses, pauseByID,
		); len(hints) > 0 {
			data["evidence_current_clips"] = hints
		}
		return failed("删除项包含未知 ID，或证据完全落在指定 A-roll clip 之外", data)
	}
	removedIDSet := make(map[string]struct{}, len(input.RemoveUtteranceIDs))
	for _, id := range input.RemoveUtteranceIDs {
		removedIDSet[id] = struct{}{}
	}
	removedWordIDSet := make(map[string]struct{}, len(removedWordIDs))
	for _, id := range removedWordIDs {
		removedWordIDSet[id] = struct{}{}
	}
	assignmentSourceRanges := make([]agentexec.TalkingHeadRange, len(input.BrollAssignments))
	invalidAssignments := []map[string]any{}
	for index, assignment := range input.BrollAssignments {
		sourceRange, resolveErr := agentexec.TalkingHeadAssignmentSourceRange(
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
	semanticDeleteRanges := make([]agentexec.TalkingHeadRange, 0, len(removedUtteranceRanges)+len(removedWordRanges))
	semanticDeleteRanges = append(semanticDeleteRanges, removedUtteranceRanges...)
	semanticDeleteRanges = append(semanticDeleteRanges, removedWordRanges...)
	semanticDeleteRanges = agentexec.MergeTalkingHeadRanges(semanticDeleteRanges)
	effectivePauses, _, redundantPauses := agentexec.ResolveTalkingHeadPauseRanges(
		removedPauses, semanticDeleteRanges, agentexec.MinTalkingHeadPauseResidualFrames,
	)
	if len(removedPauses) > 0 && len(effectivePauses) == 0 && len(redundantPauses) > 0 {
		candidates := agentexec.TalkingHeadRetainedPauseCandidates(
			pauses, semanticDeleteRanges, selectedClip, utterances,
			agentexec.MinTalkingHeadPauseCandidateFrames, 8,
		)
		if len(candidates) > 0 {
			return failed("所选气口已被句子或词级删除完整覆盖，本次不会产生任何额外气口清理", map[string]any{
				"redundant_pause_ids":       agentexec.SpeechPauseIDs(redundantPauses),
				"retained_pause_candidates": candidates,
				"recovery":                  "删除这些冗余 pause_id；若创作意图仍要求清理气口，由模型结合 retained_pause_candidates 的两侧原文自主选择后重试。候选只是客观时长与上下文，不代表必须删除。",
			})
		}
	}
	removedPauses = effectivePauses
	retainedSpeech := agentexec.TalkingHeadRetainedSpeechRanges(
		utterances, removedIDSet, removedWordIDSet, selectedClip,
	)
	misspeakEvidence := agentexec.TalkingHeadMisspeakEvidence(repetitions, shortFragments, selectedClip)
	removedPauses, effectivePauseRanges, sourceDeleteRanges, autoPreservedPauses, orphanFragments :=
		agentexec.ProtectTalkingHeadOrphanFragments(
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
			"auto_preserved_pause_ids": agentexec.SpeechPauseIDs(autoPreservedPauses),
			"redundant_pause_ids":      agentexec.SpeechPauseIDs(redundantPauses),
			"recovery":                 recovery,
		})
	}
	if len(orphanFragments) > 0 {
		counterProposals := agentexec.TalkingHeadIslandCounterProposals(orphanFragments, utterances)
		return failed("组合删除会把保留台词夹成不足 2 秒或落在口误证据上的孤立碎片", map[string]any{
			"orphan_fragments":               orphanFragments,
			"island_counter_proposals":       counterProposals,
			"auto_preserved_pause_ids":       agentexec.SpeechPauseIDs(autoPreservedPauses),
			"minimum_retained_island_frames": agentexec.MinTalkingHeadRetainedIslandFrames,
			"recovery":                       "优先采纳 island_counter_proposals：把 merged_delete_source_start_frame..merged_delete_source_end_frame 或 island_start_word_id/island_end_word_id 一并加入删除，清掉这段碎片；若这段其实是你要保留的完整台词，则改为撤回它两侧的相邻删除，让它与前后文连成不小于 2 秒的连续片段。不要原样重试，也不要只把删除缩到刚好过阈值。",
		})
	}
	unresolvedPauses := []rushestools.SpeechPauseEvidence{}
	if speechCleanupRequested {
		unresolvedPauses = agentexec.UnresolvedTalkingHeadPauseDecisions(
			pauses, semanticDeleteRanges, selectedClip, utterances,
			decidedPauseIDs, agentexec.MinTalkingHeadPauseCandidateFrames, agentexec.MaxSimilarPairs,
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
		earlierCovered := agentexec.TalkingHeadRangeCoveredBy(agentexec.TalkingHeadRange{
			Start: repetition.EarlierSourceStartFrame, End: repetition.EarlierSourceEndFrame,
		}, sourceDeleteRanges)
		laterCovered := agentexec.TalkingHeadRangeCoveredBy(agentexec.TalkingHeadRange{
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
			agentexec.TalkingHeadRangeCoveredBy(
				agentexec.TalkingHeadRange{Start: fragment.SourceStartFrame, End: fragment.SourceEndFrame},
				sourceDeleteRanges,
			) {
			continue
		}
		unresolvedFragments = append(unresolvedFragments, fragment)
	}
	// 未处理候选属于供模型继续判断的内容证据，不是参数非法或时间线
	// 不变量错误。局部修正不应为了当前目标以外的候选被迫失败；全量
	// 口播任务仍会在成功结果中拿到这些证据，并可自主决定是否继续编辑。
	deleteRanges := make([]agentexec.TalkingHeadRange, 0, len(sourceDeleteRanges))
	for _, sourceRange := range sourceDeleteRanges {
		start, end, ok := agentexec.MapSourceRangeToTimelineClip(selectedClip, sourceRange.Start, sourceRange.End)
		if ok {
			deleteRanges = append(deleteRanges, agentexec.TalkingHeadRange{Start: start, End: end})
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
		coverage, coverageErr := agentexec.TalkingHeadTimelineCoverage(
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
		if duration < agentexec.MinTalkingHeadBrollDurationFrames {
			shortBrollIssues = append(shortBrollIssues, map[string]any{
				"assignment_index": index, "shot_id": assignment.ShotID,
				"anchor_text":   assignment.AnchorText,
				"start_word_id": assignment.StartWordID, "end_word_id": assignment.EndWordID,
				"b_roll_filename":                    shot.candidate.Filename,
				"placement_duration_frames":          duration,
				"minimum_duration_frames":            agentexec.MinTalkingHeadBrollDurationFrames,
				"semantic_window_source_start_frame": sourceRange.Start,
				"semantic_window_source_end_frame":   sourceRange.End,
				"transcript_text": agentexec.TalkingHeadTranscriptText(
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
				"transcript_text": agentexec.TalkingHeadTranscriptText(
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
		coverage, coverageErr := agentexec.TalkingHeadTimelineCoverage(
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
		placement := agentexec.TalkingHeadRange{Start: coverage.Start, End: coverage.Start + duration}
		if agentexec.TalkingHeadOverlayOverlaps(document, placement) {
			return failed("B-roll 覆盖范围与现有叠加轨片段重叠", map[string]any{
				"assignment_index": index, "timeline_start_frame": placement.Start,
				"timeline_end_frame": placement.End,
				"recovery":           "调整 utterance 范围，或先明确删除/移动现有 visual_overlay 片段",
			})
		}
		clipID := randomID("broll")
		transcriptText := agentexec.TalkingHeadTranscriptText(
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
		"removed_utterance_count":      len(removedUtteranceRanges),
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
		len(removedUtteranceRanges), len(removedWordIDs), len(effectivePauseRanges), len(inserted),
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
	result.Data["removed_pause_ids"] = agentexec.SpeechPauseIDs(removedPauses)
	result.Data["removed_pause_ranges"] = effectivePauseRanges
	result.Data["removed_pause_range_count"] = len(effectivePauseRanges)
	result.Data["removed_pause_evidence_count"] = len(removedPauses)
	result.Data["redundant_pause_ids"] = agentexec.SpeechPauseIDs(redundantPauses)
	result.Data["auto_preserved_pause_ids"] = agentexec.SpeechPauseIDs(autoPreservedPauses)
	result.Data["auto_preserved_pause_count"] = len(autoPreservedPauses)
	result.Data["preserved_speech_fragment_ids"] = append([]string(nil), fragmentExpansion.PreservedIDs...)
	result.Data["preserved_speech_fragment_reasons"] = fragmentExpansion.PreservedReasons
	agentexec.AttachTalkingHeadUnreviewedEvidence(
		&result, unresolvedPauses, unresolvedRepetitions, unresolvedFragments,
	)
	result.Data["deleted_timeline_ranges"] = deleteRanges
	result.Data["b_roll_clips"] = inserted
	if drift := agentexec.TalkingHeadPlanDrift(ctx, autoPreservedPauses, utterances); drift != nil {
		result.Data["plan_drift"] = drift
		result.Observation += " " + drift["summary"].(string)
	}
	// 时间线此时已持久化成功，质检报告只是增强：读取失败时跳过附加，
	// 不把成功的编辑伪装成失败去诱导模型重试（timeline.validate 仍是持久验收面）。
	if quality, qualityErr := service.speechQualityReport(ctx, document); qualityErr == nil {
		result.Data["speech_quality"] = quality
		result.Observation += agentexec.TalkingHeadQualitySummary(quality)
	}
	return result, nil
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
