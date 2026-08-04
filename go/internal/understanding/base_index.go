package understanding

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"

	"github.com/nanzhi84/Rushes/go/internal/storage"
)

const BaseShotIndexSchemaVersion = 2

type BaseIndexShot struct {
	ShotID               string
	SourceStartFrame     int
	SourceEndFrame       int
	BoundaryVersion      int
	BoundaryKind         string
	BoundaryConfidence   *float64
	LineageParentShotID  *string
	RepresentativeFrames []RepresentativeFrame
	SemanticName         string
	Description          string
	Tags                 []string
	Subjects             []string
	Actions              []string
	Setting              []string
	ShotScale            string
	Composition          string
	Lighting             []string
	Mood                 []string
	EditHints            []string
	Quality              map[string]any
	SearchText           string
	SearchTokens         []string
}

// BaseIndexFingerprint is content-addressed. File path, mtime and one asset ID
// deliberately do not participate: identical bytes in another draft reuse the
// same immutable evidence.
func BaseIndexFingerprint(asset storage.Asset) string {
	payload := strings.Join([]string{
		asset.Hash, asset.Kind, PromptVersion, fmt.Sprint(BaseShotIndexSchemaVersion),
	}, "\x00")
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

func BaseIndexIdempotencyKey(fingerprint string) string {
	return "shot-base:" + strings.TrimSpace(fingerprint)
}

func BaseIndexSnapshotID(contentHash, fingerprint string, generation int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf(
		"%s\x00%s\x00%d", contentHash, fingerprint, generation,
	)))
	return "shot_index_" + hex.EncodeToString(sum[:16])
}

func BuildBaseIndexShots(
	contentHash string,
	generation int,
	summary Summary,
	previous []storage.IndexedShot,
) ([]BaseIndexShot, error) {
	if strings.TrimSpace(contentHash) == "" || generation < 1 {
		return nil, errors.New("基础镜头索引缺少内容哈希或 generation")
	}
	if len(summary.Segments) == 0 {
		return nil, errors.New("基础镜头索引没有镜头范围")
	}
	segments := append([]Segment(nil), summary.Segments...)
	sort.SliceStable(segments, func(left, right int) bool {
		if segments[left].SourceStartFrame == segments[right].SourceStartFrame {
			return segments[left].SourceEndFrame < segments[right].SourceEndFrame
		}
		return segments[left].SourceStartFrame < segments[right].SourceStartFrame
	})
	usedPrevious := map[string]struct{}{}
	result := make([]BaseIndexShot, 0, len(segments))
	for index, segment := range segments {
		if err := validateBaseIndexSegment(segment); err != nil {
			return nil, fmt.Errorf("镜头 %d 不满足 search_ready: %w", index, err)
		}
		matched, overlap := bestPreviousShot(segment, previous, usedPrevious)
		shotID := ""
		boundaryVersion := 1
		var parent *string
		if matched != nil && overlap >= 0.5 {
			shotID = matched.ShotID
			value := matched.ShotID
			parent = &value
			boundaryVersion = matched.BoundaryVersion
			if matched.SourceStartFrame != segment.SourceStartFrame ||
				matched.SourceEndFrame != segment.SourceEndFrame {
				boundaryVersion++
			}
			usedPrevious[matched.ShotID] = struct{}{}
		} else {
			shotID = newPersistentShotID(contentHash, generation, index)
			if matched != nil && overlap > 0 {
				value := matched.ShotID
				parent = &value
			}
		}
		searchText, searchTokens := baseSearchProjection(segment)
		quality := map[string]any{"label": strings.TrimSpace(segment.Quality)}
		if segment.OverexposedRatio != nil {
			quality["overexposed_ratio"] = *segment.OverexposedRatio
		}
		if segment.SharpnessScore != nil {
			quality["sharpness"] = *segment.SharpnessScore
		}
		result = append(result, BaseIndexShot{
			ShotID: shotID, SourceStartFrame: segment.SourceStartFrame,
			SourceEndFrame: segment.SourceEndFrame, BoundaryVersion: boundaryVersion,
			BoundaryKind:        nonEmpty(segment.BoundaryKind, "analysis_window"),
			BoundaryConfidence:  normalizedBoundaryConfidence(segment),
			LineageParentShotID: parent, RepresentativeFrames: segment.RepresentativeFrames,
			SemanticName: semanticName(segment),
			Description:  strings.TrimSpace(segment.Description), Tags: compactStrings(segment.Tags),
			Subjects: compactStrings(segment.Subjects), Actions: compactStrings(segment.Actions),
			Setting: compactStrings(segment.Setting), ShotScale: strings.TrimSpace(segment.ShotScale),
			Composition: strings.TrimSpace(segment.Composition), Lighting: compactStrings(segment.Lighting),
			Mood: compactStrings(segment.Mood), EditHints: compactStrings(segment.EditHints),
			Quality: quality, SearchText: searchText, SearchTokens: searchTokens,
		})
	}
	return result, nil
}

// WithSemanticNames upgrades legacy structured summaries without another VLM
// call. New analyses already provide the field; this fallback keeps existing
// workspaces immediately addressable while preserving all prior evidence.
func WithSemanticNames(summary Summary) Summary {
	summary.Segments = append([]Segment(nil), summary.Segments...)
	for index := range summary.Segments {
		summary.Segments[index].SemanticName = semanticName(summary.Segments[index])
	}
	return summary
}

func validateBaseIndexSegment(segment Segment) error {
	if segment.SourceStartFrame < 0 || segment.SourceEndFrame <= segment.SourceStartFrame {
		return errors.New("源帧范围无效")
	}
	description := strings.TrimSpace(segment.Description)
	if description == "" || strings.Contains(description, "待理解视频片段") {
		return errors.New("description 为空或仍是 placeholder")
	}
	if len(segment.RepresentativeFrames) == 0 {
		return errors.New("缺少代表帧")
	}
	for _, frame := range segment.RepresentativeFrames {
		if frame.SourceFrame < 0 || len(frame.ObjectHash) != 64 || frame.ObjectSize <= 0 {
			return errors.New("代表帧 manifest 无效")
		}
	}
	if strings.TrimSpace(segment.Quality) == "" {
		return errors.New("缺少质量事实")
	}
	structured := len(compactStrings(segment.Subjects)) + len(compactStrings(segment.Actions)) +
		len(compactStrings(segment.Setting)) + len(compactStrings(segment.Tags))
	if structured == 0 || strings.TrimSpace(segment.ShotScale) == "" ||
		strings.TrimSpace(segment.Composition) == "" {
		return errors.New("结构化标签不完整")
	}
	if semanticName(segment) == "" {
		return errors.New("缺少短语义名称")
	}
	return nil
}

// semanticName keeps the user-facing label stable even when an older provider
// omits the new field. New analyses generate SemanticName in the same VLM call;
// the structured fallback is only for legacy summaries and deterministic tests.
func semanticName(segment Segment) string {
	if value := compactSemanticName(segment.SemanticName); value != "" {
		return value
	}
	parts := make([]string, 0, 4)
	for _, values := range [][]string{segment.Setting, segment.Subjects, segment.Actions} {
		for _, value := range compactStrings(values) {
			parts = append(parts, value)
			break
		}
	}
	if len(parts) == 0 {
		parts = append(parts, segment.ShotScale)
	}
	return compactSemanticName(strings.Join(parts, "·"))
}

func compactSemanticName(value string) string {
	value = strings.Join(strings.Fields(strings.TrimSpace(value)), "")
	value = strings.Trim(value, "，。；：、,.!?！？《》「」[]【】()（）")
	if value == "" {
		return ""
	}
	runes := []rune(value)
	if len(runes) > 18 {
		return string(runes[:18])
	}
	return value
}

func bestPreviousShot(
	segment Segment,
	previous []storage.IndexedShot,
	used map[string]struct{},
) (*storage.IndexedShot, float64) {
	bestIndex, bestOverlap := -1, 0.0
	for index := range previous {
		if _, exists := used[previous[index].ShotID]; exists {
			continue
		}
		overlap := rangeIntersectionOverUnion(
			segment.SourceStartFrame, segment.SourceEndFrame,
			previous[index].SourceStartFrame, previous[index].SourceEndFrame,
		)
		if overlap > bestOverlap || overlap == bestOverlap && bestIndex >= 0 &&
			previous[index].ShotID < previous[bestIndex].ShotID {
			bestIndex, bestOverlap = index, overlap
		}
	}
	if bestIndex < 0 {
		return nil, 0
	}
	return &previous[bestIndex], bestOverlap
}

func rangeIntersectionOverUnion(leftStart, leftEnd, rightStart, rightEnd int) float64 {
	intersection := max(0, min(leftEnd, rightEnd)-max(leftStart, rightStart))
	union := max(leftEnd, rightEnd) - min(leftStart, rightStart)
	if union <= 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

func newPersistentShotID(contentHash string, generation, ordinal int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf(
		"%s\x00shot\x00%d\x00%d", contentHash, generation, ordinal,
	)))
	return "shot_" + hex.EncodeToString(sum[:12])
}

func normalizedBoundaryConfidence(segment Segment) *float64 {
	if segment.BoundaryScore == nil {
		if !segment.BoundaryVerified {
			return nil
		}
		value := 1.0
		return &value
	}
	value := math.Max(0, math.Min(1, *segment.BoundaryScore/100))
	if segment.BoundaryVerified {
		value = math.Max(value, 0.5)
	}
	return &value
}

func baseSearchProjection(segment Segment) (string, []string) {
	values := []string{semanticName(segment), segment.Description}
	values = append(values, segment.Subjects...)
	values = append(values, segment.Actions...)
	values = append(values, segment.Setting...)
	values = append(values, segment.ShotScale, segment.Composition)
	values = append(values, segment.Tags...)
	values = append(values, segment.Lighting...)
	values = append(values, segment.Mood...)
	values = append(values, segment.EditHints...)
	values = compactStrings(values)
	searchText := strings.ToLower(strings.Join(values, " "))
	seen := map[string]struct{}{}
	tokens := []string{}
	for _, value := range values {
		normalized := strings.ToLower(strings.TrimSpace(value))
		if normalized != "" {
			if _, exists := seen[normalized]; !exists {
				seen[normalized] = struct{}{}
				tokens = append(tokens, normalized)
			}
		}
		for _, field := range strings.FieldsFunc(normalized, func(character rune) bool {
			return !unicode.IsLetter(character) && !unicode.IsNumber(character)
		}) {
			if _, exists := seen[field]; field != "" && !exists {
				seen[field] = struct{}{}
				tokens = append(tokens, field)
			}
		}
	}
	sort.Strings(tokens)
	return searchText, tokens
}

func compactStrings(values []string) []string {
	result := []string{}
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.Join(strings.Fields(value), " ")
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func nonEmpty(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}
