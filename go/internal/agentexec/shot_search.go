package agentexec

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func RankUnderstandingCandidates(
	values []rushestools.ShotSearchUnderstandingCandidate,
	query string,
	tags []string,
	roleFilter map[string]struct{},
	limit int,
) []rushestools.ShotSearchUnderstandingCandidate {
	tokens := SemanticTokens(strings.TrimSpace(query + " " + strings.Join(tags, " ")))
	result := make([]rushestools.ShotSearchUnderstandingCandidate, 0, len(values))
	for _, value := range values {
		// 未理解素材的角色也可能尚未确定；只有目录/文件名已经明确给出
		// 相反角色时才排除，避免 b_roll 检索把最相关的未知素材藏起来。
		if len(roleFilter) > 0 && value.SemanticRole != "" {
			if _, ok := roleFilter[value.SemanticRole]; !ok {
				continue
			}
		}
		filenameText := strings.ToLower(value.Filename)
		catalogText := strings.ToLower(strings.Join([]string{value.Filename, value.RelDir, value.SemanticRole}, " "))
		catalogScore := SemanticMatchScore(tokens, catalogText)
		if len(tokens) > 0 && catalogScore == 0 {
			continue
		}
		filenameScore := SemanticMatchScore(tokens, filenameText)
		value.Score = math.Round((catalogScore*0.7+filenameScore*0.3)*10000) / 10000
		value.MatchedQueryTerms = MatchedSemanticTerms(tokens, catalogText)
		if matched := MatchedSemanticTerms(tokens, filenameText); len(matched) > 0 {
			value.MatchEvidence = append(value.MatchEvidence, "文件名命中: "+strings.Join(matched, "、"))
		}
		if matched := MatchedSemanticTerms(tokens, strings.ToLower(value.RelDir)); len(matched) > 0 {
			value.MatchEvidence = append(value.MatchEvidence, "目录命中: "+strings.Join(matched, "、"))
		}
		result = append(result, value)
	}
	sort.SliceStable(result, func(left, right int) bool {
		if result[left].Score != result[right].Score {
			return result[left].Score > result[right].Score
		}
		return result[left].Filename < result[right].Filename
	})
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result
}

func ShotMatchEvidence(tokens map[string]struct{}, candidate rushestools.ShotCandidate) []string {
	fields := []struct {
		name  string
		value string
	}{
		{name: "文件名", value: candidate.Filename},
		{name: "镜头描述", value: candidate.Description},
		{name: "标签", value: strings.Join(candidate.Tags, " ")},
		{name: "主体/动作", value: strings.Join(append(append([]string{}, candidate.Subjects...), candidate.Actions...), " ")},
		{name: "台词", value: candidate.Transcript},
	}
	result := []string{}
	for _, field := range fields {
		if matched := MatchedSemanticTerms(tokens, strings.ToLower(field.value)); len(matched) > 0 {
			result = append(result, field.name+"命中: "+strings.Join(matched, "、"))
		}
	}
	return result
}

func MatchedSemanticTerms(tokens map[string]struct{}, text string) []string {
	result := []string{}
	for token := range tokens {
		if utf8.RuneCountInString(token) < 2 || !strings.Contains(text, token) {
			continue
		}
		result = append(result, token)
	}
	sort.SliceStable(result, func(left, right int) bool {
		leftSize, rightSize := utf8.RuneCountInString(result[left]), utf8.RuneCountInString(result[right])
		if leftSize != rightSize {
			return leftSize > rightSize
		}
		return result[left] < result[right]
	})
	if len(result) > 12 {
		result = result[:12]
	}
	return result
}

func StableShotID(assetID string, startFrame, endFrame int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%d", assetID, startFrame, endFrame)))
	return "shot_" + hex.EncodeToString(sum[:8])
}

func ShotSemanticText(candidate rushestools.ShotCandidate) string {
	return strings.TrimSpace(ShotAssetSemanticText(candidate) + " " + ShotSegmentSemanticText(candidate))
}

func ShotAssetSemanticText(candidate rushestools.ShotCandidate) string {
	return strings.ToLower(strings.Join([]string{candidate.Filename, candidate.SemanticRole}, " "))
}

func ShotSegmentSemanticText(candidate rushestools.ShotCandidate) string {
	parts := []string{candidate.Description, strings.Join(candidate.Tags, " "),
		strings.Join(candidate.Subjects, " "), strings.Join(candidate.Actions, " "),
		strings.Join(candidate.Setting, " "), candidate.ShotScale, candidate.Composition,
		strings.Join(candidate.Lighting, " "), strings.Join(candidate.Mood, " "),
		strings.Join(candidate.EditHints, " "), candidate.Transcript}
	return strings.ToLower(strings.Join(parts, " "))
}

func TranscriptTextForSourceRange(utterances []map[string]any, startFrame, endFrame int) string {
	parts := []string{}
	for _, utterance := range utterances {
		startValue, startOK := NumericValue(utterance["source_start_frame"])
		endValue, endOK := NumericValue(utterance["source_end_frame"])
		if !startOK || !endOK || int(startValue) >= endFrame || int(endValue) <= startFrame {
			continue
		}
		if text := strings.TrimSpace(InterfaceString(utterance["text"])); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, " ")
}

func ShotQualityPenalty(candidate rushestools.ShotCandidate) float64 {
	penalty := 0.0
	if candidate.OverexposedRatio != nil && *candidate.OverexposedRatio > 0.10 {
		penalty += min(0.12, (*candidate.OverexposedRatio-0.10)*0.15)
	}
	if candidate.SharpnessScore != nil && *candidate.SharpnessScore < 100 {
		penalty += min(0.10, (100-*candidate.SharpnessScore)/1000)
	}
	return math.Round(penalty*10000) / 10000
}

func SemanticSearchTokens(query string) map[string]struct{} {
	result := SemanticTokens(query)
	lower := strings.ToLower(query)
	if strings.Contains(lower, "无背光") || strings.Contains(lower, "没有背光") ||
		strings.Contains(lower, "没背光") || strings.Contains(lower, "背光缺失") {
		for token := range SemanticTokens("背光关闭 键盘不亮 暗光 黑暗 极暗 低照度 全黑") {
			result[token] = struct{}{}
		}
	}
	return result
}

func WeightedSemanticMatchScore(tokens map[string]struct{}, text string) float64 {
	if len(tokens) == 0 {
		return 0.5
	}
	textTokens := SemanticTokens(text)
	matchedWeight := 0.0
	totalWeight := 0.0
	for token := range tokens {
		weight := semanticTokenWeight(token)
		totalWeight += weight
		if _, ok := textTokens[token]; ok || strings.Contains(text, token) {
			matchedWeight += weight
		}
	}
	if totalWeight == 0 {
		return 0
	}
	return matchedWeight / totalWeight
}

func semanticTokenWeight(token string) float64 {
	containsDigit := false
	onlyCJK := token != ""
	for _, value := range token {
		containsDigit = containsDigit || unicode.IsDigit(value)
		onlyCJK = onlyCJK && unicode.In(value, unicode.Han)
	}
	if containsDigit {
		return 4
	}
	length := utf8.RuneCountInString(token)
	if onlyCJK {
		if length == 1 {
			return 0.15
		}
		return 1
	}
	if length >= 3 {
		return 2
	}
	return 0.5
}

func ExactNumericEvidenceBonus(query, segmentText string) float64 {
	bonus := 0.0
	for _, field := range strings.Fields(strings.ToLower(query)) {
		hasDigit := false
		for _, value := range field {
			hasDigit = hasDigit || unicode.IsDigit(value)
		}
		field = strings.Trim(field, "，。！？、；：,.!?;:()（）[]【】")
		if hasDigit && utf8.RuneCountInString(field) >= 2 && strings.Contains(segmentText, field) {
			bonus += 0.35
		}
	}
	return min(bonus, 0.5)
}

func RoundScore(value float64) float64 {
	return math.Round(value*10000) / 10000
}

func SemanticMatchScore(tokens map[string]struct{}, text string) float64 {
	if len(tokens) == 0 {
		return 0.5
	}
	textTokens := SemanticTokens(text)
	matched := 0
	for token := range tokens {
		if _, ok := textTokens[token]; ok || strings.Contains(text, token) {
			matched++
		}
	}
	return float64(matched) / float64(len(tokens))
}

func SemanticTokens(text string) map[string]struct{} {
	result := map[string]struct{}{}
	lower := strings.ToLower(strings.TrimSpace(text))
	word := []rune{}
	flushWord := func() {
		if len(word) > 0 {
			result[string(word)] = struct{}{}
			word = word[:0]
		}
	}
	cjk := []rune{}
	flushCJK := func() {
		for index, value := range cjk {
			result[string(value)] = struct{}{}
			if index+1 < len(cjk) {
				result[string(cjk[index:index+2])] = struct{}{}
			}
		}
		cjk = cjk[:0]
	}
	for _, value := range lower {
		switch {
		case unicode.In(value, unicode.Han):
			flushWord()
			cjk = append(cjk, value)
		case unicode.IsLetter(value) || unicode.IsDigit(value):
			flushCJK()
			word = append(word, value)
		default:
			flushWord()
			flushCJK()
		}
	}
	flushWord()
	flushCJK()
	return result
}

func OverlapsAny(target BeatMixSourceRange, values []BeatMixSourceRange) bool {
	for _, value := range values {
		if target.StartFrame < value.EndFrame && value.StartFrame < target.EndFrame {
			return true
		}
	}
	return false
}
