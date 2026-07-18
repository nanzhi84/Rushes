package agentexec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/media"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

const (
	MaxASRChunkFrames = 25 * timeline.DefaultFPS
	MaxSimilarPairs   = 24
)

type SpeechUtterance struct {
	ID         string
	StartFrame int
	EndFrame   int
	Text       string
	Language   string
	Emotion    string
	Words      []SpeechWord
}

type SpeechWord struct {
	ID          string
	StartFrame  int
	EndFrame    int
	Text        string
	Punctuation string
}

type SpeechPause struct {
	ID          string
	StartFrame  int
	EndFrame    int
	DeleteStart int
	DeleteEnd   int
	Method      string
}

func BuildASRChunks(durationFrames int, pauses []SpeechPause, maxFrames int) [][2]int {
	if durationFrames <= 0 {
		return nil
	}
	if maxFrames <= 0 {
		maxFrames = MaxASRChunkFrames
	}
	chunks := [][2]int{}
	for start := 0; start < durationFrames; {
		ceiling := min(durationFrames, start+maxFrames)
		end := ceiling
		for _, pause := range pauses {
			midpoint := (pause.StartFrame + pause.EndFrame) / 2
			if midpoint > start+timeline.DefaultFPS*3 && midpoint <= ceiling {
				end = midpoint
			}
		}
		if end <= start {
			end = ceiling
		}
		chunks = append(chunks, [2]int{start, end})
		start = end
	}
	return chunks
}

func AlignRecognizedClauses(
	assetID, text, language, emotion string,
	startFrame, endFrame int,
) []SpeechUtterance {
	clauses := splitSpeechClauses(text)
	if len(clauses) == 0 || endFrame <= startFrame {
		return nil
	}
	totalWeight := 0
	for _, clause := range clauses {
		totalWeight += max(1, utf8.RuneCountInString(clause))
	}
	result := make([]SpeechUtterance, 0, len(clauses))
	cursor, consumedWeight := startFrame, 0
	for index, clause := range clauses {
		weight := max(1, utf8.RuneCountInString(clause))
		consumedWeight += weight
		end := endFrame
		if index+1 < len(clauses) {
			end = startFrame + int(math.Round(
				float64(endFrame-startFrame)*float64(consumedWeight)/float64(totalWeight),
			))
			end = max(cursor+1, min(end, endFrame-(len(clauses)-index-1)))
		}
		result = append(result, SpeechUtterance{
			ID:         StableSpeechID("utt", assetID, cursor, end, clause),
			StartFrame: cursor, EndFrame: end, Text: clause, Language: language, Emotion: emotion,
		})
		cursor = end
	}
	return result
}

func AlignTimestampedRecognition(
	assetID string,
	recognized contracts.SpeechRecognitionResult,
	chunkStartFrame, chunkEndFrame int,
) []SpeechUtterance {
	if chunkEndFrame <= chunkStartFrame || len(recognized.Segments) == 0 {
		return nil
	}
	segmentText := strings.Builder{}
	for _, segment := range recognized.Segments {
		segmentText.WriteString(segment.Text)
	}
	fullLength := utf8.RuneCountInString(NormalizeSpeechText(recognized.Text))
	segmentLength := utf8.RuneCountInString(NormalizeSpeechText(segmentText.String()))
	// 非流式响应有时只携带最后一句的 sentence 详情，而 output.text 是全文。
	// 此时不能用不完整时间戳丢掉前文，回退到完整文本的本地对齐。
	if fullLength > 0 && segmentLength*4 < fullLength*3 {
		return nil
	}
	result := []SpeechUtterance{}
	for _, segment := range recognized.Segments {
		fromWords := timestampedWordsToUtterances(
			assetID, segment.Words, recognized.Language, recognized.Emotion,
			chunkStartFrame, chunkEndFrame,
		)
		if len(fromWords) > 0 {
			result = append(result, fromWords...)
			continue
		}
		start := timestampToSourceFrame(segment.BeginMilliseconds, chunkStartFrame, chunkEndFrame)
		end := timestampToSourceFrame(segment.EndMilliseconds, chunkStartFrame, chunkEndFrame)
		if end <= start {
			continue
		}
		result = append(result, AlignRecognizedClauses(
			assetID, segment.Text, recognized.Language, recognized.Emotion, start, end,
		)...)
	}
	return result
}

func timestampedWordsToUtterances(
	assetID string,
	words []contracts.SpeechRecognitionWord,
	language, emotion string,
	chunkStartFrame, chunkEndFrame int,
) []SpeechUtterance {
	result := []SpeechUtterance{}
	text := strings.Builder{}
	utteranceWords := []SpeechWord{}
	startMilliseconds, endMilliseconds := -1, -1
	flush := func() {
		value := strings.TrimSpace(text.String())
		if value != "" && startMilliseconds >= 0 && endMilliseconds > startMilliseconds {
			start := timestampToSourceFrame(startMilliseconds, chunkStartFrame, chunkEndFrame)
			end := timestampToSourceFrame(endMilliseconds, chunkStartFrame, chunkEndFrame)
			if end > start {
				result = append(result, SpeechUtterance{
					ID:         StableSpeechID("utt", assetID, start, end, value),
					StartFrame: start, EndFrame: end, Text: value,
					Language: language, Emotion: emotion,
					Words: append([]SpeechWord(nil), utteranceWords...),
				})
			}
		}
		text.Reset()
		utteranceWords = utteranceWords[:0]
		startMilliseconds, endMilliseconds = -1, -1
	}
	for _, word := range words {
		if strings.TrimSpace(word.Text) == "" || word.EndMilliseconds <= word.BeginMilliseconds {
			continue
		}
		if startMilliseconds < 0 {
			startMilliseconds = word.BeginMilliseconds
		}
		endMilliseconds = word.EndMilliseconds
		startFrame := timestampToSourceFrame(word.BeginMilliseconds, chunkStartFrame, chunkEndFrame)
		endFrame := timestampToSourceFrame(word.EndMilliseconds, chunkStartFrame, chunkEndFrame)
		if endFrame <= startFrame {
			endFrame = min(chunkEndFrame, startFrame+1)
		}
		if endFrame > startFrame {
			utteranceWords = append(utteranceWords, SpeechWord{
				ID:         StableSpeechID("word", assetID, startFrame, endFrame, word.Text+word.Punctuation),
				StartFrame: startFrame, EndFrame: endFrame, Text: word.Text, Punctuation: word.Punctuation,
			})
		}
		text.WriteString(word.Text)
		text.WriteString(word.Punctuation)
		if strings.ContainsAny(word.Punctuation, "。！？!?；;") ||
			strings.ContainsAny(word.Text, "。！？!?；;\n") {
			flush()
		}
	}
	flush()
	return result
}

func timestampToSourceFrame(milliseconds, chunkStartFrame, chunkEndFrame int) int {
	offset := (max(0, milliseconds)*timeline.DefaultFPS + 500) / 1000
	return max(chunkStartFrame, min(chunkStartFrame+offset, chunkEndFrame))
}

func splitSpeechClauses(text string) []string {
	result := []string{}
	buffer := []rune{}
	flush := func() {
		value := strings.TrimSpace(string(buffer))
		if value != "" {
			result = append(result, value)
		}
		buffer = buffer[:0]
	}
	for _, value := range strings.TrimSpace(text) {
		buffer = append(buffer, value)
		if strings.ContainsRune("。！？!?；;\n", value) {
			flush()
		}
	}
	flush()
	return result
}

func EncodeSpeechUtterances(values []SpeechUtterance) []map[string]any {
	result := make([]map[string]any, 0, len(values))
	for _, value := range values {
		item := map[string]any{
			"utterance_id": value.ID, "source_start_frame": value.StartFrame,
			"source_end_frame": value.EndFrame, "text": value.Text,
			"words": encodeSpeechWords(value.Words),
		}
		if value.Language != "" {
			item["language"] = value.Language
		}
		if value.Emotion != "" {
			item["emotion"] = value.Emotion
		}
		result = append(result, item)
	}
	return result
}

func encodeSpeechWords(values []SpeechWord) []map[string]any {
	result := make([]map[string]any, 0, len(values))
	for _, value := range values {
		result = append(result, map[string]any{
			"word_id": value.ID, "source_start_frame": value.StartFrame,
			"source_end_frame": value.EndFrame, "text": value.Text,
			"punctuation": value.Punctuation,
		})
	}
	return result
}

func TranscriptHasWordSchema(values []map[string]any) bool {
	for _, value := range values {
		if _, exists := value["words"]; exists {
			return true
		}
	}
	return false
}

func EncodeSpeechPauses(values []SpeechPause) []map[string]any {
	result := make([]map[string]any, 0, len(values))
	for _, value := range values {
		item := map[string]any{
			"pause_id": value.ID, "source_start_frame": value.StartFrame,
			"source_end_frame": value.EndFrame, "delete_start_frame": value.DeleteStart,
			"delete_end_frame": value.DeleteEnd,
		}
		if value.Method != "" {
			item["detection_method"] = value.Method
		}
		result = append(result, item)
	}
	return result
}

func DecodeSpeechUtterances(values []map[string]any) ([]SpeechUtterance, error) {
	result := make([]SpeechUtterance, 0, len(values))
	for _, value := range values {
		start, startOK := NumericValue(value["source_start_frame"])
		end, endOK := NumericValue(value["source_end_frame"])
		item := SpeechUtterance{
			ID: InterfaceString(value["utterance_id"]), StartFrame: int(start), EndFrame: int(end),
			Text: InterfaceString(value["text"]), Language: InterfaceString(value["language"]),
			Emotion: InterfaceString(value["emotion"]),
		}
		if item.ID == "" || item.Text == "" || !startOK || !endOK || item.EndFrame <= item.StartFrame {
			return nil, errors.New("持久化 transcript utterance 无效")
		}
		words, wordsErr := decodeSpeechWords(value["words"])
		if wordsErr != nil {
			return nil, wordsErr
		}
		item.Words = words
		result = append(result, item)
	}
	sort.SliceStable(result, func(left, right int) bool { return result[left].StartFrame < result[right].StartFrame })
	return result, nil
}

func decodeSpeechWords(raw any) ([]SpeechWord, error) {
	if raw == nil {
		return nil, nil
	}
	values := []map[string]any{}
	switch typed := raw.(type) {
	case []map[string]any:
		values = typed
	case []any:
		for _, item := range typed {
			mapped, ok := item.(map[string]any)
			if !ok {
				return nil, errors.New("持久化 transcript word 无效")
			}
			values = append(values, mapped)
		}
	default:
		return nil, errors.New("持久化 transcript words 无效")
	}
	result := make([]SpeechWord, 0, len(values))
	for _, value := range values {
		start, startOK := NumericValue(value["source_start_frame"])
		end, endOK := NumericValue(value["source_end_frame"])
		word := SpeechWord{
			ID: InterfaceString(value["word_id"]), StartFrame: int(start), EndFrame: int(end),
			Text: InterfaceString(value["text"]), Punctuation: InterfaceString(value["punctuation"]),
		}
		if word.ID == "" || word.Text == "" || !startOK || !endOK || word.EndFrame <= word.StartFrame {
			return nil, errors.New("持久化 transcript word 无效")
		}
		result = append(result, word)
	}
	sort.SliceStable(result, func(left, right int) bool { return result[left].StartFrame < result[right].StartFrame })
	return result, nil
}

func DecodeSpeechPauses(values []map[string]any) ([]SpeechPause, error) {
	result := make([]SpeechPause, 0, len(values))
	for _, value := range values {
		start, startOK := NumericValue(value["source_start_frame"])
		end, endOK := NumericValue(value["source_end_frame"])
		deleteStart, deleteStartOK := NumericValue(value["delete_start_frame"])
		deleteEnd, deleteEndOK := NumericValue(value["delete_end_frame"])
		item := SpeechPause{
			ID: InterfaceString(value["pause_id"]), StartFrame: int(start), EndFrame: int(end),
			DeleteStart: int(deleteStart), DeleteEnd: int(deleteEnd),
			Method: InterfaceString(value["detection_method"]),
		}
		if item.ID == "" || !startOK || !endOK || !deleteStartOK || !deleteEndOK ||
			item.EndFrame <= item.StartFrame || item.DeleteEnd <= item.DeleteStart {
			return nil, errors.New("持久化 transcript pause 无效")
		}
		result = append(result, item)
	}
	return result, nil
}

func DeriveASRWordGaps(assetID string, utterances []SpeechUtterance) []SpeechPause {
	words := []SpeechWord{}
	for _, utterance := range utterances {
		words = append(words, utterance.Words...)
	}
	sort.SliceStable(words, func(left, right int) bool {
		return words[left].StartFrame < words[right].StartFrame
	})
	result := []SpeechPause{}
	for index := 1; index < len(words); index++ {
		start, end := words[index-1].EndFrame, words[index].StartFrame
		if end-start < 5 {
			continue
		}
		deleteStart, deleteEnd := start+2, end-2
		if deleteEnd <= deleteStart {
			continue
		}
		result = append(result, SpeechPause{
			ID:         StableSpeechID("pause", assetID, start, end, "asr_word_gap"),
			StartFrame: start, EndFrame: end,
			DeleteStart: deleteStart, DeleteEnd: deleteEnd,
			Method: "asr_word_gap",
		})
	}
	return result
}

func MergeSpeechPauses(assetID string, values []SpeechPause) []SpeechPause {
	if len(values) == 0 {
		return nil
	}
	sort.SliceStable(values, func(left, right int) bool {
		if values[left].StartFrame != values[right].StartFrame {
			return values[left].StartFrame < values[right].StartFrame
		}
		return values[left].EndFrame < values[right].EndFrame
	})
	result := []SpeechPause{values[0]}
	for _, value := range values[1:] {
		last := &result[len(result)-1]
		if value.StartFrame <= last.EndFrame {
			last.StartFrame = min(last.StartFrame, value.StartFrame)
			last.EndFrame = max(last.EndFrame, value.EndFrame)
			last.DeleteStart = min(last.DeleteStart, value.DeleteStart)
			last.DeleteEnd = max(last.DeleteEnd, value.DeleteEnd)
			last.Method = joinSpeechDetectionMethods(last.Method, value.Method)
			continue
		}
		result = append(result, value)
	}
	for index := range result {
		result[index].ID = StableSpeechID(
			"pause", assetID, result[index].StartFrame, result[index].EndFrame, result[index].Method,
		)
	}
	return result
}

func ClampSpeechPausesToWordBoundaries(
	assetID string,
	values []SpeechPause,
	utterances []SpeechUtterance,
) []SpeechPause {
	const minimumSafeDeleteFrames = 5
	words := []SpeechWord{}
	for _, utterance := range utterances {
		words = append(words, utterance.Words...)
	}
	sort.SliceStable(words, func(left, right int) bool {
		return words[left].StartFrame < words[right].StartFrame
	})
	result := []SpeechPause{}
	for _, value := range values {
		cursor := value.DeleteStart
		segments := []TalkingHeadRange{}
		clamped := false
		for _, word := range words {
			if word.EndFrame <= cursor {
				continue
			}
			if word.StartFrame >= value.DeleteEnd {
				break
			}
			clamped = true
			if word.StartFrame > cursor {
				segments = append(segments, TalkingHeadRange{
					Start: cursor,
					End:   min(word.StartFrame, value.DeleteEnd),
				})
			}
			cursor = max(cursor, word.EndFrame)
			if cursor >= value.DeleteEnd {
				break
			}
		}
		if cursor < value.DeleteEnd {
			segments = append(segments, TalkingHeadRange{Start: cursor, End: value.DeleteEnd})
		}
		if !clamped && len(segments) == 0 {
			segments = append(segments, TalkingHeadRange{Start: value.DeleteStart, End: value.DeleteEnd})
		}
		for _, segment := range segments {
			if segment.End-segment.Start < minimumSafeDeleteFrames {
				continue
			}
			item := value
			item.DeleteStart, item.DeleteEnd = segment.Start, segment.End
			changed := clamped || segment.Start != value.DeleteStart || segment.End != value.DeleteEnd
			if changed {
				item.Method = joinSpeechDetectionMethods(item.Method, "word_boundary_clamped")
				item.ID = StableSpeechID(
					"pause", assetID, item.StartFrame, item.EndFrame,
					fmt.Sprintf("%s:%d:%d", item.Method, item.DeleteStart, item.DeleteEnd),
				)
			}
			result = append(result, item)
		}
	}
	sort.SliceStable(result, func(left, right int) bool {
		if result[left].DeleteStart == result[right].DeleteStart {
			return result[left].DeleteEnd < result[right].DeleteEnd
		}
		return result[left].DeleteStart < result[right].DeleteStart
	})
	return result
}

func joinSpeechDetectionMethods(left, right string) string {
	methods := map[string]struct{}{}
	for _, value := range strings.Split(left+"+"+right, "+") {
		if value = strings.TrimSpace(value); value != "" {
			methods[value] = struct{}{}
		}
	}
	ordered := make([]string, 0, len(methods))
	for value := range methods {
		ordered = append(ordered, value)
	}
	sort.Strings(ordered)
	return strings.Join(ordered, "+")
}

func PopulateSpeechPauseContext(
	target *rushestools.SpeechPauseEvidence,
	utterances []SpeechUtterance,
) {
	previousEnd := -1
	nextStart := math.MaxInt
	allWords := []SpeechWord{}
	for _, utterance := range utterances {
		if len(utterance.Words) == 0 {
			if utterance.EndFrame <= target.SourceStartFrame && utterance.EndFrame > previousEnd {
				previousEnd = utterance.EndFrame
				target.PreviousText = utterance.Text
				target.PreviousWordID = ""
			}
			if utterance.StartFrame >= target.SourceEndFrame && utterance.StartFrame < nextStart {
				nextStart = utterance.StartFrame
				target.NextText = utterance.Text
				target.NextWordID = ""
			}
			continue
		}
		allWords = append(allWords, utterance.Words...)
		for _, word := range utterance.Words {
			if word.EndFrame <= target.SourceStartFrame && word.EndFrame > previousEnd {
				previousEnd = word.EndFrame
				target.PreviousText = word.Text + word.Punctuation
				target.PreviousWordID = word.ID
			}
			if word.StartFrame >= target.SourceEndFrame && word.StartFrame < nextStart {
				nextStart = word.StartFrame
				target.NextText = word.Text + word.Punctuation
				target.NextWordID = word.ID
			}
		}
	}
	sort.SliceStable(allWords, func(left, right int) bool {
		return allWords[left].StartFrame < allWords[right].StartFrame
	})
	previousWords := []SpeechWord{}
	nextWords := []SpeechWord{}
	for _, word := range allWords {
		if word.EndFrame <= target.SourceStartFrame {
			previousWords = append(previousWords, word)
			if len(previousWords) > 6 {
				previousWords = previousWords[len(previousWords)-6:]
			}
			continue
		}
		if word.StartFrame >= target.SourceEndFrame && len(nextWords) < 6 {
			nextWords = append(nextWords, word)
		}
	}
	if len(previousWords) > 0 {
		target.PreviousContext = joinSpeechWords(previousWords)
		target.PreviousContextStartWordID = previousWords[0].ID
		target.PreviousContextEndWordID = previousWords[len(previousWords)-1].ID
	}
	if len(nextWords) > 0 {
		target.NextContext = joinSpeechWords(nextWords)
		target.NextContextStartWordID = nextWords[0].ID
		target.NextContextEndWordID = nextWords[len(nextWords)-1].ID
	}
	target.JoinedContext = target.PreviousContext + target.NextContext
}

func RankSpeechPauseEvidence(
	values []rushestools.SpeechPauseEvidence,
	limit int,
) ([]rushestools.SpeechPauseEvidence, int, bool) {
	sort.SliceStable(values, func(left, right int) bool {
		if values[left].DeleteDurationFrames == values[right].DeleteDurationFrames {
			return values[left].SourceStartFrame < values[right].SourceStartFrame
		}
		return values[left].DeleteDurationFrames > values[right].DeleteDurationFrames
	})
	if limit <= 0 {
		limit = 24
	}
	limit = min(limit, 100)
	total := len(values)
	truncated := total > limit
	if truncated {
		values = values[:limit]
	}
	return values, total, truncated
}

func joinSpeechWords(words []SpeechWord) string {
	var builder strings.Builder
	for _, word := range words {
		builder.WriteString(word.Text)
		builder.WriteString(word.Punctuation)
	}
	return builder.String()
}

func IntraUtteranceSpeechRepetitions(
	assetID string,
	utterances []SpeechUtterance,
	limit int,
) []rushestools.SpeechRepetitionEvidence {
	if limit <= 0 {
		return nil
	}
	result := []rushestools.SpeechRepetitionEvidence{}
	for _, utterance := range utterances {
		for index := 0; index+1 < len(utterance.Words); index++ {
			left := NormalizeSpeechText(utterance.Words[index].Text)
			right := NormalizeSpeechText(utterance.Words[index+1].Text)
			if left == "" || left != right {
				continue
			}
			result = append(result, buildIntraUtteranceRepetition(
				assetID,
				utterance, "adjacent_word_repeat", index, index, index+1, index+1,
				left, utf8.RuneCountInString(left),
				"相邻 ASR 词完全相同；结合完整句意判断是口吃、强调还是正常表达",
			))
		}
		characters, wordIndexes := speechUtteranceCharacterMap(utterance.Words)
		earlierStart, laterStart, matched := longestNonOverlappingSpeechRepeat(characters)
		if matched < 6 {
			continue
		}
		earlierStartWord := wordIndexes[earlierStart]
		earlierEndWord := wordIndexes[earlierStart+matched-1]
		laterStartWord := wordIndexes[laterStart]
		laterEndWord := wordIndexes[laterStart+matched-1]
		if earlierEndWord >= laterStartWord {
			continue
		}
		matchedText := string(characters[earlierStart : earlierStart+matched])
		result = append(result, buildIntraUtteranceRepetition(
			assetID,
			utterance, "repeated_phrase", earlierStartWord, earlierEndWord,
			laterStartWord, laterEndWord, matchedText, matched,
			"同一句内部两个不重叠位置包含最长重复字符片段；范围只标出共同短语，需结合 context_text 与词级窗口判断完整重说边界",
		))
	}
	sort.SliceStable(result, func(left, right int) bool {
		leftAdjacent := result[left].Kind == "adjacent_word_repeat"
		rightAdjacent := result[right].Kind == "adjacent_word_repeat"
		if leftAdjacent != rightAdjacent {
			return leftAdjacent
		}
		if leftAdjacent && result[left].EarlierSourceStartFrame != result[right].EarlierSourceStartFrame {
			return result[left].EarlierSourceStartFrame < result[right].EarlierSourceStartFrame
		}
		return result[left].MatchedCharacters > result[right].MatchedCharacters
	})
	if len(result) > limit {
		result = result[:limit]
	}
	return result
}

func buildIntraUtteranceRepetition(
	assetID string,
	utterance SpeechUtterance,
	kind string,
	earlierStart, earlierEnd, laterStart, laterEnd int,
	matchedText string,
	matchedCharacters int,
	evidence string,
) rushestools.SpeechRepetitionEvidence {
	return rushestools.SpeechRepetitionEvidence{
		RepetitionID: StableSpeechID(
			"repetition", assetID,
			utterance.Words[earlierStart].StartFrame,
			utterance.Words[laterEnd].EndFrame,
			kind+":"+utterance.Words[earlierStart].ID+":"+utterance.Words[laterStart].ID,
		),
		UtteranceID: utterance.ID, Kind: kind,
		EarlierStartWordID:      utterance.Words[earlierStart].ID,
		EarlierEndWordID:        utterance.Words[earlierEnd].ID,
		LaterStartWordID:        utterance.Words[laterStart].ID,
		LaterEndWordID:          utterance.Words[laterEnd].ID,
		EarlierSourceStartFrame: utterance.Words[earlierStart].StartFrame,
		EarlierSourceEndFrame:   utterance.Words[earlierEnd].EndFrame,
		LaterSourceStartFrame:   utterance.Words[laterStart].StartFrame,
		LaterSourceEndFrame:     utterance.Words[laterEnd].EndFrame,
		EarlierText:             joinSpeechWords(utterance.Words[earlierStart : earlierEnd+1]),
		LaterText:               joinSpeechWords(utterance.Words[laterStart : laterEnd+1]),
		MatchedText:             matchedText, MatchedCharacters: matchedCharacters,
		ContextText: speechTextPreview(utterance.Text, 320), Evidence: evidence,
	}
}

func speechUtteranceCharacterMap(words []SpeechWord) ([]rune, []int) {
	characters := []rune{}
	wordIndexes := []int{}
	for index, word := range words {
		for _, character := range NormalizeSpeechText(word.Text) {
			characters = append(characters, character)
			wordIndexes = append(wordIndexes, index)
		}
	}
	return characters, wordIndexes
}

func longestNonOverlappingSpeechRepeat(characters []rune) (int, int, int) {
	length := len(characters)
	if length < 2 {
		return 0, 0, 0
	}
	dp := make([][]int, length+1)
	for index := range dp {
		dp[index] = make([]int, length+1)
	}
	bestEarlier, bestLater, bestLength := 0, 0, 0
	for earlier := length - 1; earlier >= 0; earlier-- {
		for later := length - 1; later > earlier; later-- {
			if characters[earlier] != characters[later] {
				continue
			}
			candidateLength := 1 + dp[earlier+1][later+1]
			candidateLength = min(candidateLength, later-earlier)
			dp[earlier][later] = candidateLength
			if candidateLength > bestLength {
				bestEarlier, bestLater, bestLength = earlier, later, candidateLength
			}
		}
	}
	return bestEarlier, bestLater, bestLength
}

func ShortLeadingSpeechFragments(
	assetID string,
	utterances []SpeechUtterance,
	pauses []SpeechPause,
	limit int,
) []rushestools.SpeechFragmentEvidence {
	if limit <= 0 {
		return nil
	}
	const (
		minimumPauseFrames = 18
		maximumWords       = 5
		maximumFrames      = 45
	)
	result := []rushestools.SpeechFragmentEvidence{}
	for utteranceIndex, utterance := range utterances {
		if len(utterance.Words) < 2 {
			continue
		}
		for _, pause := range pauses {
			if pause.EndFrame-pause.StartFrame < minimumPauseFrames ||
				pause.StartFrame <= utterance.StartFrame || pause.EndFrame >= utterance.EndFrame {
				continue
			}
			before := []SpeechWord{}
			after := []SpeechWord{}
			for _, word := range utterance.Words {
				if word.StartFrame < pause.StartFrame {
					before = append(before, word)
					continue
				}
				if word.StartFrame >= pause.EndFrame && len(after) < 6 {
					after = append(after, word)
				}
			}
			if len(before) == 0 || len(before) > maximumWords || len(after) == 0 {
				continue
			}
			last := before[len(before)-1]
			if strings.TrimSpace(last.Punctuation) != "" ||
				last.EndFrame-utterance.StartFrame > maximumFrames {
				continue
			}
			fragmentWords := append([]SpeechWord(nil), before...)
			nextWords := append([]SpeechWord(nil), after...)
			kind := "short_leading_fragment_before_internal_pause"
			restartAnchorText := ""
			matchedEarlierUtteranceID := ""
			matchedEarlierText := ""
			if anchor, ok := speechRestartAnchorAfterPause(
				utterances, utteranceIndex, utterance, pause.EndFrame,
			); ok {
				fragmentWords = append([]SpeechWord(nil), utterance.Words[:anchor.WordIndex]...)
				nextWords = append([]SpeechWord(nil), utterance.Words[anchor.WordIndex:]...)
				if len(nextWords) > 8 {
					nextWords = nextWords[:8]
				}
				kind = "restart_prefix_before_repeated_take"
				restartAnchorText = anchor.AnchorText
				matchedEarlierUtteranceID = anchor.EarlierUtteranceID
				matchedEarlierText = anchor.EarlierText
			}
			if len(fragmentWords) == 0 || len(nextWords) == 0 {
				continue
			}
			last = fragmentWords[len(fragmentWords)-1]
			text := joinSpeechWords(fragmentWords)
			nextContext := joinSpeechWords(nextWords)
			previousContext := ""
			if utteranceIndex > 0 {
				previousContext = trailingSpeechContext(utterances[utteranceIndex-1], 8)
			}
			fragmentID := StableSpeechID(
				"fragment", assetID, fragmentWords[0].StartFrame, last.EndFrame,
				"leading_before_internal_pause:"+pause.ID+":"+text,
			)
			evidence := "同一 ASR 句在开头少量无标点词后出现较长内部停顿；模型必须结合后文明确选择删除或保留"
			if kind == "restart_prefix_before_repeated_take" {
				evidence = "内部停顿后的连续文本从 restart_anchor_text 起重新接入此前台词；fragment 是该接入点之前未对齐的前缀，是否删除由模型结合原文判断"
			}
			result = append(result, rushestools.SpeechFragmentEvidence{
				FragmentID: fragmentID, UtteranceID: utterance.ID, PauseID: pause.ID,
				Kind:        kind,
				StartWordID: fragmentWords[0].ID, EndWordID: last.ID,
				SourceStartFrame: fragmentWords[0].StartFrame, SourceEndFrame: last.EndFrame,
				DurationFrames: last.EndFrame - fragmentWords[0].StartFrame,
				Text:           text, PreviousContext: previousContext,
				NextContext: nextContext, JoinedContext: text + nextContext,
				PauseDurationFrames:       pause.EndFrame - pause.StartFrame,
				NextContextStartWordID:    nextWords[0].ID,
				NextContextEndWordID:      nextWords[len(nextWords)-1].ID,
				RestartAnchorText:         restartAnchorText,
				MatchedEarlierUtteranceID: matchedEarlierUtteranceID,
				MatchedEarlierText:        matchedEarlierText,
				Evidence:                  evidence,
			})
		}
	}
	for _, candidate := range intraUtteranceRetakeTailFragments(assetID, utterances, pauses) {
		overlapsExisting := false
		for _, existing := range result {
			if existing.UtteranceID == candidate.UtteranceID &&
				existing.SourceStartFrame < candidate.SourceEndFrame &&
				candidate.SourceStartFrame < existing.SourceEndFrame {
				overlapsExisting = true
				break
			}
		}
		if !overlapsExisting {
			result = append(result, candidate)
		}
	}
	sort.SliceStable(result, func(left, right int) bool {
		if result[left].DurationFrames == result[right].DurationFrames {
			return result[left].SourceStartFrame < result[right].SourceStartFrame
		}
		return result[left].DurationFrames < result[right].DurationFrames
	})
	if len(result) > limit {
		result = result[:limit]
	}
	return result
}

func intraUtteranceRetakeTailFragments(
	assetID string,
	utterances []SpeechUtterance,
	pauses []SpeechPause,
) []rushestools.SpeechFragmentEvidence {
	const minimumBoundaryPauseFrames = 6
	result := []rushestools.SpeechFragmentEvidence{}
	for _, repetition := range IntraUtteranceSpeechRepetitions(assetID, utterances, int(^uint(0)>>1)) {
		if repetition.Kind != "repeated_phrase" {
			continue
		}
		utteranceIndex := -1
		for index := range utterances {
			if utterances[index].ID == repetition.UtteranceID {
				utteranceIndex = index
				break
			}
		}
		if utteranceIndex < 0 {
			continue
		}
		utterance := utterances[utteranceIndex]
		earlierStartIndex, earlierEndIndex, laterStartIndex := -1, -1, -1
		for index, word := range utterance.Words {
			if word.ID == repetition.EarlierStartWordID {
				earlierStartIndex = index
			}
			if word.ID == repetition.EarlierEndWordID {
				earlierEndIndex = index
			}
			if word.ID == repetition.LaterStartWordID {
				laterStartIndex = index
			}
		}
		if earlierStartIndex < 0 || earlierEndIndex < earlierStartIndex ||
			laterStartIndex <= earlierEndIndex+1 {
			continue
		}
		var boundary *SpeechPause
		for index := range pauses {
			pause := &pauses[index]
			if pause.EndFrame-pause.StartFrame < minimumBoundaryPauseFrames ||
				pause.StartFrame < utterance.Words[earlierEndIndex].EndFrame ||
				pause.EndFrame > utterance.Words[laterStartIndex].StartFrame {
				continue
			}
			if boundary == nil || pause.EndFrame > boundary.EndFrame {
				boundary = pause
			}
		}
		if boundary == nil {
			continue
		}
		fragmentWords := []SpeechWord{}
		for _, word := range utterance.Words[earlierStartIndex:laterStartIndex] {
			if word.EndFrame <= boundary.StartFrame {
				fragmentWords = append(fragmentWords, word)
			}
		}
		if len(fragmentWords) == 0 {
			continue
		}
		nextWords := []SpeechWord{}
		for _, word := range utterance.Words[earlierEndIndex+1:] {
			if word.StartFrame < boundary.EndFrame {
				continue
			}
			nextWords = append(nextWords, word)
			if len(nextWords) >= 8 {
				break
			}
		}
		if len(nextWords) == 0 {
			continue
		}
		previousStart := max(0, earlierStartIndex-6)
		previousContext := joinSpeechWords(utterance.Words[previousStart:earlierStartIndex])
		last := fragmentWords[len(fragmentWords)-1]
		text := joinSpeechWords(fragmentWords)
		result = append(result, rushestools.SpeechFragmentEvidence{
			FragmentID: StableSpeechID(
				"fragment", assetID, fragmentWords[0].StartFrame, last.EndFrame,
				"earlier_take:"+repetition.RepetitionID+":"+boundary.ID,
			),
			UtteranceID: repetition.UtteranceID, PauseID: boundary.ID,
			Kind:        "earlier_take_before_repeated_phrase_restart",
			StartWordID: fragmentWords[0].ID, EndWordID: last.ID,
			SourceStartFrame: fragmentWords[0].StartFrame, SourceEndFrame: last.EndFrame,
			DurationFrames: last.EndFrame - fragmentWords[0].StartFrame,
			Text:           text, PreviousContext: previousContext,
			NextContext: joinSpeechWords(nextWords), JoinedContext: previousContext + text + joinSpeechWords(nextWords),
			PauseDurationFrames:       boundary.EndFrame - boundary.StartFrame,
			NextContextStartWordID:    nextWords[0].ID,
			NextContextEndWordID:      nextWords[len(nextWords)-1].ID,
			RestartAnchorText:         repetition.LaterText,
			MatchedEarlierUtteranceID: repetition.UtteranceID,
			MatchedEarlierText:        repetition.EarlierText,
			Evidence:                  "同一句的共同短语在停顿后重新出现；该片段覆盖共同短语、随后分叉尾部直到重启停顿，是一遍完整的较早说法候选，模型必须结合完整原文明确删除或保留",
		})
	}
	return result
}

func trailingSpeechContext(utterance SpeechUtterance, maximumWords int) string {
	if maximumWords <= 0 {
		return ""
	}
	if len(utterance.Words) > 0 {
		start := max(0, len(utterance.Words)-maximumWords)
		return joinSpeechWords(utterance.Words[start:])
	}
	return TruncateText(utterance.Text, 80)
}

type speechRestartAnchor struct {
	WordIndex          int
	AnchorText         string
	EarlierUtteranceID string
	EarlierText        string
}

func speechRestartAnchorAfterPause(
	utterances []SpeechUtterance,
	utteranceIndex int,
	utterance SpeechUtterance,
	pauseEndFrame int,
) (speechRestartAnchor, bool) {
	const (
		minimumAnchorRunes = 6
		maximumProbeWords  = 10
	)
	afterIndex := -1
	for index, word := range utterance.Words {
		if word.StartFrame >= pauseEndFrame {
			afterIndex = index
			break
		}
	}
	if afterIndex < 0 || utteranceIndex <= 0 {
		return speechRestartAnchor{}, false
	}
	probeEnd := min(len(utterance.Words), afterIndex+maximumProbeWords)
	for candidate := afterIndex; candidate < probeEnd; candidate++ {
		anchorText, normalizedAnchor := speechWordPrefix(utterance.Words[candidate:], minimumAnchorRunes)
		if utf8.RuneCountInString(normalizedAnchor) < minimumAnchorRunes {
			continue
		}
		for earlierIndex := utteranceIndex - 1; earlierIndex >= 0; earlierIndex-- {
			if !strings.Contains(NormalizeSpeechText(utterances[earlierIndex].Text), normalizedAnchor) {
				continue
			}
			return speechRestartAnchor{
				WordIndex: candidate, AnchorText: anchorText,
				EarlierUtteranceID: utterances[earlierIndex].ID,
				EarlierText:        speechTextPreview(utterances[earlierIndex].Text, 180),
			}, true
		}
	}
	return speechRestartAnchor{}, false
}

func speechWordPrefix(words []SpeechWord, minimumRunes int) (string, string) {
	parts := []SpeechWord{}
	for _, word := range words {
		parts = append(parts, word)
		normalized := NormalizeSpeechText(joinSpeechWords(parts))
		if utf8.RuneCountInString(normalized) >= minimumRunes {
			characters := []rune(normalized)
			return joinSpeechWords(parts), string(characters[:minimumRunes])
		}
	}
	return joinSpeechWords(parts), NormalizeSpeechText(joinSpeechWords(parts))
}

func SimilarSpeechPairs(values []SpeechUtterance, limit int) []rushestools.SpeechSimilarityEvidence {
	if limit <= 0 || len(values) < 2 {
		return nil
	}
	singleCandidates := []speechSimilarityCandidate{}
	for left := 0; left < len(values); left++ {
		for right := left + 1; right < len(values) && right <= left+12; right++ {
			score := speechTextSimilarity(values[left].Text, values[right].Text)
			if score < 0.8 {
				continue
			}
			singleCandidates = append(singleCandidates, speechSimilarityCandidate{
				evidence: buildSpeechSimilarityEvidence(
					values, left, left, right, right, score, 0,
					"character_bigram_jaccard", "归一化字符二元组重合度；仅供模型比较原句语义",
				),
				earlierStart: left, earlierEnd: left + 1, laterStart: right, laterEnd: right + 1,
				rank: score,
			})
		}
	}
	sequenceCandidates := speechSequenceSimilarityCandidates(values)
	sort.SliceStable(sequenceCandidates, func(left, right int) bool {
		if sequenceCandidates[left].rank == sequenceCandidates[right].rank {
			return sequenceCandidates[left].evidence.Similarity > sequenceCandidates[right].evidence.Similarity
		}
		return sequenceCandidates[left].rank > sequenceCandidates[right].rank
	})
	sequenceLimit := max(1, limit*2/3)
	selectedSequences := make([]speechSimilarityCandidate, 0, sequenceLimit)
	for _, candidate := range sequenceCandidates {
		duplicate := false
		for _, selected := range selectedSequences {
			if speechWindowOverlapRatio(
				candidate.earlierStart, candidate.earlierEnd, selected.earlierStart, selected.earlierEnd,
			) >= 0.67 && speechWindowOverlapRatio(
				candidate.laterStart, candidate.laterEnd, selected.laterStart, selected.laterEnd,
			) >= 0.67 {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		selectedSequences = append(selectedSequences, candidate)
		if len(selectedSequences) >= sequenceLimit {
			break
		}
	}
	sort.SliceStable(singleCandidates, func(left, right int) bool {
		return singleCandidates[left].evidence.Similarity > singleCandidates[right].evidence.Similarity
	})
	result := make([]rushestools.SpeechSimilarityEvidence, 0, limit)
	for _, candidate := range selectedSequences {
		result = append(result, candidate.evidence)
	}
	for _, candidate := range singleCandidates {
		if len(result) >= limit {
			break
		}
		result = append(result, candidate.evidence)
	}
	return result
}

type speechSimilarityCandidate struct {
	evidence                 rushestools.SpeechSimilarityEvidence
	earlierStart, earlierEnd int
	laterStart, laterEnd     int
	rank                     float64
}

func speechSequenceSimilarityCandidates(values []SpeechUtterance) []speechSimilarityCandidate {
	const (
		maxWindowUtterances = 4
		minSequenceRunes    = 18
		minMatchedRunes     = 18
		minLengthRatio      = 0.45
		minLCSDice          = 0.46
	)
	candidates := []speechSimilarityCandidate{}
	for earlierStart := 0; earlierStart < len(values); earlierStart++ {
		for earlierEnd := earlierStart; earlierEnd < len(values) && earlierEnd < earlierStart+maxWindowUtterances; earlierEnd++ {
			earlierText := NormalizeSpeechText(joinSpeechUtteranceText(values, earlierStart, earlierEnd))
			earlierLength := utf8.RuneCountInString(earlierText)
			if earlierLength < minSequenceRunes {
				continue
			}
			for laterStart := earlierEnd + 1; laterStart < len(values) && laterStart <= earlierStart+12; laterStart++ {
				for laterEnd := laterStart; laterEnd < len(values) && laterEnd < laterStart+maxWindowUtterances; laterEnd++ {
					if earlierStart == earlierEnd && laterStart == laterEnd {
						continue
					}
					laterText := NormalizeSpeechText(joinSpeechUtteranceText(values, laterStart, laterEnd))
					laterLength := utf8.RuneCountInString(laterText)
					if laterLength < minSequenceRunes {
						continue
					}
					shorter, longer := min(earlierLength, laterLength), max(earlierLength, laterLength)
					if float64(shorter)/float64(longer) < minLengthRatio {
						continue
					}
					matched := speechLCSLength([]rune(earlierText), []rune(laterText))
					score := 2 * float64(matched) / float64(earlierLength+laterLength)
					if matched < minMatchedRunes || score < minLCSDice {
						continue
					}
					candidates = append(candidates, speechSimilarityCandidate{
						evidence: buildSpeechSimilarityEvidence(
							values, earlierStart, earlierEnd, laterStart, laterEnd, score, matched,
							"normalized_character_lcs_dice",
							"连续台词块的归一化字符最长公共子序列；用于发现跨句重说，是否删除及保留哪一遍由模型判断",
						),
						earlierStart: earlierStart, earlierEnd: earlierEnd + 1,
						laterStart: laterStart, laterEnd: laterEnd + 1,
						// 相似密度优先，同时给共同字符数一个小权重：完整重说段能胜过短窗口，
						// 但在窗口两侧混入无关句子会因相似度下降而被惩罚。
						rank: score + float64(matched)/1000,
					})
				}
			}
		}
	}
	return candidates
}

func buildSpeechSimilarityEvidence(
	values []SpeechUtterance,
	earlierStart, earlierEnd, laterStart, laterEnd int,
	score float64,
	matched int,
	method, evidence string,
) rushestools.SpeechSimilarityEvidence {
	return rushestools.SpeechSimilarityEvidence{
		EarlierUtteranceID:      values[earlierStart].ID,
		LaterUtteranceID:        values[laterStart].ID,
		EarlierEndUtteranceID:   values[earlierEnd].ID,
		LaterEndUtteranceID:     values[laterEnd].ID,
		EarlierSourceStartFrame: values[earlierStart].StartFrame,
		EarlierSourceEndFrame:   values[earlierEnd].EndFrame,
		LaterSourceStartFrame:   values[laterStart].StartFrame,
		LaterSourceEndFrame:     values[laterEnd].EndFrame,
		EarlierText:             speechTextPreview(joinSpeechUtteranceText(values, earlierStart, earlierEnd), 240),
		LaterText:               speechTextPreview(joinSpeechUtteranceText(values, laterStart, laterEnd), 240),
		Similarity:              math.Round(score*1000) / 1000,
		MatchedCharacters:       matched,
		Method:                  method,
		Evidence:                evidence,
	}
}

func joinSpeechUtteranceText(values []SpeechUtterance, start, end int) string {
	parts := make([]string, 0, end-start+1)
	for index := start; index <= end; index++ {
		parts = append(parts, strings.TrimSpace(values[index].Text))
	}
	return strings.Join(parts, "")
}

func speechTextPreview(value string, limit int) string {
	characters := []rune(strings.TrimSpace(value))
	if len(characters) <= limit {
		return string(characters)
	}
	return string(characters[:limit]) + "…"
}

func speechLCSLength(left, right []rune) int {
	if len(left) > len(right) {
		left, right = right, left
	}
	previous := make([]int, len(left)+1)
	current := make([]int, len(left)+1)
	for _, rightCharacter := range right {
		for leftIndex, leftCharacter := range left {
			if leftCharacter == rightCharacter {
				current[leftIndex+1] = previous[leftIndex] + 1
			} else {
				current[leftIndex+1] = max(previous[leftIndex+1], current[leftIndex])
			}
		}
		previous, current = current, previous
		clear(current)
	}
	return previous[len(left)]
}

func speechWindowOverlapRatio(leftStart, leftEnd, rightStart, rightEnd int) float64 {
	overlap := max(0, min(leftEnd, rightEnd)-max(leftStart, rightStart))
	shorter := min(leftEnd-leftStart, rightEnd-rightStart)
	if shorter <= 0 {
		return 0
	}
	return float64(overlap) / float64(shorter)
}

func speechTextSimilarity(left, right string) float64 {
	left = NormalizeSpeechText(left)
	right = NormalizeSpeechText(right)
	if utf8.RuneCountInString(left) < 4 || utf8.RuneCountInString(right) < 4 {
		return 0
	}
	if left == right {
		return 1
	}
	leftPairs, rightPairs := speechBigrams(left), speechBigrams(right)
	intersection := 0
	for value := range leftPairs {
		if _, exists := rightPairs[value]; exists {
			intersection++
		}
	}
	union := len(leftPairs) + len(rightPairs) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

func NormalizeSpeechText(value string) string {
	result := []rune{}
	for _, character := range strings.ToLower(value) {
		if unicode.IsLetter(character) || unicode.IsDigit(character) || unicode.In(character, unicode.Han) {
			result = append(result, character)
		}
	}
	return string(result)
}

func speechBigrams(value string) map[string]struct{} {
	characters := []rune(value)
	result := map[string]struct{}{}
	for index := 0; index+1 < len(characters); index++ {
		result[string(characters[index:index+2])] = struct{}{}
	}
	return result
}

func StableSpeechID(prefix, assetID string, startFrame, endFrame int, text string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%d:%d:%s", prefix, assetID, startFrame, endFrame, text)))
	return prefix + "_" + hex.EncodeToString(sum[:8])
}

func MapSourceRangeToTimelineClip(clip timeline.Clip, startFrame, endFrame int) (int, int, bool) {
	start := max(startFrame, clip.SourceStartFrame)
	end := min(endFrame, clip.SourceEndFrame)
	if end <= start {
		return 0, 0, false
	}
	rate := clip.PlaybackRate
	if rate <= 0 {
		rate = 1
	}
	timelineStart := clip.TimelineStartFrame + int(math.Round(float64(start-clip.SourceStartFrame)/rate))
	timelineEnd := clip.TimelineStartFrame + int(math.Round(float64(end-clip.SourceStartFrame)/rate))
	timelineStart = max(clip.TimelineStartFrame, timelineStart)
	timelineEnd = min(clip.TimelineEndFrame, timelineEnd)
	return timelineStart, timelineEnd, timelineEnd > timelineStart
}

func SourceRangesOverlap(leftStart, leftEnd, rightStart, rightEnd int) bool {
	return leftStart < rightEnd && rightStart < leftEnd
}

// clampSpeechRangeToClip 把源帧证据区间裁剪到 clip 的已裁剪源区间，返回裁剪后的
// 区间与是否发生了裁剪。调用方对交集为空（end <= start）的项自行决定跳过或判非法，
// 使 speech.inspect 返回的证据坐标与 timeline.edit_talking_head 的交集校验一致。
func ClampSpeechRangeToClip(clip timeline.Clip, start, end int) (int, int, bool) {
	clampedStart := max(start, clip.SourceStartFrame)
	clampedEnd := min(end, clip.SourceEndFrame)
	return clampedStart, clampedEnd, clampedStart != start || clampedEnd != end
}

func (exec *Executor) toolInspectSpeech(
	ctx context.Context,
	draftID string,
	input rushestools.SpeechInspectInput,
) (rushestools.SpeechInspectResult, error) {
	includeWords := input.IncludeWords || strings.TrimSpace(input.Query) != "" ||
		input.SourceStartFrame != nil || input.SourceEndFrame != nil
	asset, timelineClip, err := exec.resolveSpeechAsset(ctx, draftID, input.AssetID, input.TimelineClipID)
	if err != nil {
		return rushestools.SpeechInspectResult{}, err
	}
	transcript, cacheHit, err := exec.loadOrBuildSpeechTranscript(
		ctx, draftID, asset, strings.TrimSpace(input.Language), input.ForceRefresh, includeWords,
	)
	if err != nil {
		return rushestools.SpeechInspectResult{}, err
	}
	utterances, err := DecodeSpeechUtterances(transcript.Utterances)
	if err != nil {
		return rushestools.SpeechInspectResult{}, err
	}
	pauses, err := DecodeSpeechPauses(transcript.VADSegments)
	if err != nil {
		return rushestools.SpeechInspectResult{}, err
	}
	pauses = ClampSpeechPausesToWordBoundaries(asset.ID, pauses, utterances)

	queryTokens := SemanticTokens(input.Query)
	selected := make([]SpeechUtterance, 0, len(utterances))
	for _, utterance := range utterances {
		if timelineClip != nil && !SourceRangesOverlap(
			utterance.StartFrame, utterance.EndFrame,
			timelineClip.SourceStartFrame, timelineClip.SourceEndFrame,
		) {
			continue
		}
		if input.SourceStartFrame != nil && utterance.EndFrame <= *input.SourceStartFrame ||
			input.SourceEndFrame != nil && utterance.StartFrame >= *input.SourceEndFrame {
			continue
		}
		if len(queryTokens) > 0 && SemanticMatchScore(queryTokens, strings.ToLower(utterance.Text)) == 0 {
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
			sourceStart, sourceEnd, clamped = ClampSpeechRangeToClip(*timelineClip, sourceStart, sourceEnd)
		}
		item := rushestools.SpeechUtteranceEvidence{
			UtteranceID: utterance.ID, SourceStartFrame: sourceStart,
			SourceEndFrame: sourceEnd, Text: utterance.Text,
			Language: utterance.Language, Emotion: utterance.Emotion, Clamped: clamped,
		}
		if timelineClip != nil {
			if start, end, ok := MapSourceRangeToTimelineClip(*timelineClip, utterance.StartFrame, utterance.EndFrame); ok {
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
					wordStart, wordEnd, wordClamped = ClampSpeechRangeToClip(*timelineClip, wordStart, wordEnd)
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
					if start, end, ok := MapSourceRangeToTimelineClip(*timelineClip, word.StartFrame, word.EndFrame); ok {
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
			if timelineClip != nil && !SourceRangesOverlap(
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
				deleteStart, deleteEnd, clamped = ClampSpeechRangeToClip(*timelineClip, deleteStart, deleteEnd)
			}
			item := rushestools.SpeechPauseEvidence{
				PauseID: pause.ID, SourceStartFrame: pause.StartFrame, SourceEndFrame: pause.EndFrame,
				DeleteStartFrame: deleteStart, DeleteEndFrame: deleteEnd,
				DurationFrames:       pause.EndFrame - pause.StartFrame,
				DeleteDurationFrames: deleteEnd - deleteStart,
				DetectionMethod:      pause.Method, Clamped: clamped,
			}
			PopulateSpeechPauseContext(&item, utterances)
			if timelineClip != nil {
				if start, end, ok := MapSourceRangeToTimelineClip(*timelineClip, deleteStart, deleteEnd); ok {
					item.TimelineStartFrame, item.TimelineEndFrame = &start, &end
				}
			}
			pauseEvidence = append(pauseEvidence, item)
		}
	}
	pauseEvidence, pauseTotal, pausesTruncated := RankSpeechPauseEvidence(pauseEvidence, input.MaxPauses)
	includeSimilar := input.IncludeSimilar == nil || *input.IncludeSimilar
	similar := []rushestools.SpeechSimilarityEvidence{}
	repetitions := []rushestools.SpeechRepetitionEvidence{}
	repetitionTotal, repetitionsTruncated := 0, false
	if includeSimilar {
		similar = SimilarSpeechPairs(utterances, MaxSimilarPairs)
		repetitions = IntraUtteranceSpeechRepetitions(asset.ID, selected, int(^uint(0)>>1))
		repetitionTotal = len(repetitions)
		if repetitionsTruncated = len(repetitions) > MaxSimilarPairs; repetitionsTruncated {
			repetitions = repetitions[:MaxSimilarPairs]
		}
	}
	allShortFragments := ShortLeadingSpeechFragments(asset.ID, utterances, pauses, int(^uint(0)>>1))
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
	shortFragmentsTruncated := shortFragmentTotal > MaxSimilarPairs
	if shortFragmentsTruncated {
		shortFragments = shortFragments[:MaxSimilarPairs]
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

func (exec *Executor) resolveSpeechAsset(
	ctx context.Context,
	draftID, requestedAssetID, timelineClipID string,
) (storage.Asset, *timeline.Clip, error) {
	assetID := strings.TrimSpace(requestedAssetID)
	var selectedClip *timeline.Clip
	if strings.TrimSpace(timelineClipID) != "" {
		document, err := timeline.Latest(ctx, exec.database, draftID)
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
	assets, err := storage.ListDraftAssets(ctx, exec.database.Read(), draftID)
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

func (exec *Executor) loadOrBuildSpeechTranscript(
	ctx context.Context,
	draftID string,
	asset storage.Asset,
	language string,
	forceRefresh bool,
	requireWordSchema bool,
) (storage.Transcript, bool, error) {
	if !forceRefresh {
		if cached, err := storage.LatestTranscript(ctx, exec.database.Read(), asset.ID); err == nil {
			if !requireWordSchema || TranscriptHasWordSchema(cached.Utterances) ||
				cached.ProviderID == "sidecar-srt" || exec.speechRecognizer == nil {
				return cached, true, nil
			}
		} else if !errors.Is(err, storage.ErrNotFound) {
			return storage.Transcript{}, false, err
		}
	}
	source, _, err := media.ResolveAssetSource(ctx, exec.database, asset.ID)
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
	pauses := make([]SpeechPause, 0, len(pauseAnalysis.Pauses))
	for _, pause := range pauseAnalysis.Pauses {
		pauses = append(pauses, SpeechPause{
			ID:         StableSpeechID("pause", asset.ID, pause.SourceStartFrame, pause.SourceEndFrame, ""),
			StartFrame: pause.SourceStartFrame, EndFrame: pause.SourceEndFrame,
			DeleteStart: pause.DeleteStartFrame, DeleteEnd: pause.DeleteEndFrame,
			Method: "rms_silence",
		})
	}

	providerID := ""
	utterances := []SpeechUtterance{}
	if sidecar := media.FindSidecarSRT(source); sidecar != "" {
		cues, parseErr := media.ParseSRT(sidecar, timeline.DefaultFPS)
		if parseErr != nil {
			return storage.Transcript{}, false, parseErr
		}
		providerID = "sidecar-srt"
		for _, cue := range cues {
			utterances = append(utterances, SpeechUtterance{
				ID:         StableSpeechID("utt", asset.ID, cue.StartFrame, cue.EndFrame, cue.Text),
				StartFrame: cue.StartFrame, EndFrame: cue.EndFrame, Text: cue.Text, Language: language,
			})
		}
	} else {
		if exec.speechRecognizer == nil {
			return storage.Transcript{}, false, errors.New(
				"素材没有同名 SRT，且当前环境未配置云端 ASR；请配置 RUSHES_DASHSCOPE_API_KEY 后重试",
			)
		}
		chunks := BuildASRChunks(durationFrames, pauses, MaxASRChunkFrames)
		for index, chunk := range chunks {
			exec.recordProgress(draftID, map[string]any{
				"type": contracts.TurnStreamSubagentProgress, "tool": "speech.inspect",
				"asset_id": asset.ID, "note": fmt.Sprintf("ASR 转写 %d/%d", index+1, len(chunks)),
				"completed": index, "total": len(chunks),
			})
			path, extractErr := media.ExtractAudioSegmentMP3(
				ctx, exec.database.Paths.Temporary, source, chunk[0], chunk[1], timeline.DefaultFPS,
			)
			if extractErr != nil {
				return storage.Transcript{}, false, extractErr
			}
			recognized, recognizeErr := exec.speechRecognizer.Recognize(ctx, contracts.SpeechRecognitionRequest{
				AudioPath: path, Language: language,
			})
			_ = os.Remove(path)
			if errors.Is(recognizeErr, contracts.ErrSpeechNoWords) {
				exec.recordProgress(draftID, map[string]any{
					"type": contracts.TurnStreamSubagentProgress, "tool": "speech.inspect",
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
			chunkUtterances := AlignTimestampedRecognition(asset.ID, recognized, chunk[0], chunk[1])
			if len(chunkUtterances) == 0 {
				alignmentID = "local-frame-alignment"
				chunkUtterances = AlignRecognizedClauses(
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
	pauses = ClampSpeechPausesToWordBoundaries(
		asset.ID,
		MergeSpeechPauses(asset.ID, append(pauses, DeriveASRWordGaps(asset.ID, utterances)...)),
		utterances,
	)
	utteranceMaps := EncodeSpeechUtterances(utterances)
	pauseMaps := EncodeSpeechPauses(pauses)
	fingerprint := StableSpeechID("transcript", asset.Hash, 0, durationFrames, providerID)
	transcriptID := fingerprint
	if forceRefresh {
		transcriptID += "_" + RandomID("run")
	}
	result, err := reducer.Apply(ctx, exec.database, nil, reducer.Options{
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
