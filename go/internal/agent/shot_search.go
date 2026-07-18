package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
	"github.com/nanzhi84/Rushes/go/internal/understanding"
)

type indexedShot struct {
	candidate rushestools.ShotCandidate
	rangeInfo agentexec.BeatMixSourceRange
}

func (service *Service) toolSearchShots(
	ctx context.Context,
	draftID string,
	input rushestools.ShotSearchInput,
) (rushestools.ShotSearchResult, error) {
	if input.MinDurationFrames < 0 || input.MaxDurationFrames < 0 ||
		input.MaxDurationFrames > 0 && input.MaxDurationFrames < input.MinDurationFrames {
		return rushestools.ShotSearchResult{}, errors.New("镜头时长筛选范围无效")
	}
	roleFilter := map[string]struct{}{}
	for _, role := range input.SemanticRoles {
		role = strings.ToLower(strings.TrimSpace(role))
		if role != "a_roll" && role != "b_roll" {
			return rushestools.ShotSearchResult{}, fmt.Errorf(
				"semantic_roles 只支持 a_roll 或 b_roll，收到 %q", role,
			)
		}
		roleFilter[role] = struct{}{}
	}
	limit := input.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	shots, missing, err := service.draftShotIndex(ctx, draftID, input.AssetIDs)
	if err != nil {
		return rushestools.ShotSearchResult{}, err
	}

	used := map[string][]agentexec.BeatMixSourceRange{}
	if input.ExcludeUsed {
		if document, timelineErr := timeline.Latest(ctx, service.database, draftID); timelineErr == nil {
			for _, track := range document.Tracks {
				for _, clip := range track.Clips {
					if clip.AssetID == "" || clip.SourceEndFrame <= clip.SourceStartFrame {
						continue
					}
					used[clip.AssetID] = append(used[clip.AssetID], agentexec.BeatMixSourceRange{
						StartFrame: clip.SourceStartFrame, EndFrame: clip.SourceEndFrame,
					})
				}
			}
		} else if !errors.Is(timelineErr, storage.ErrNotFound) {
			return rushestools.ShotSearchResult{}, timelineErr
		}
	}

	queryTokens := agentexec.SemanticSearchTokens(input.Query)
	tagTokens := agentexec.SemanticTokens(strings.Join(input.Tags, " "))
	matches := make([]rushestools.ShotCandidate, 0, len(shots))
	for _, shot := range shots {
		candidate := shot.candidate
		if len(roleFilter) > 0 {
			if _, matchesRole := roleFilter[candidate.SemanticRole]; !matchesRole {
				continue
			}
		}
		if candidate.DurationFrames < input.MinDurationFrames ||
			input.MaxDurationFrames > 0 && candidate.DurationFrames > input.MaxDurationFrames {
			continue
		}
		if candidate.Quality == "unusable" || agentexec.OverlapsAny(shot.rangeInfo, used[candidate.AssetID]) {
			continue
		}
		semanticText := agentexec.ShotSemanticText(candidate)
		if len(tagTokens) > 0 && agentexec.WeightedSemanticMatchScore(tagTokens, semanticText) == 0 {
			continue
		}
		segmentText := agentexec.ShotSegmentSemanticText(candidate)
		assetText := agentexec.ShotAssetSemanticText(candidate)
		segmentScore := agentexec.WeightedSemanticMatchScore(queryTokens, segmentText)
		assetScore := agentexec.WeightedSemanticMatchScore(queryTokens, assetText)
		score := segmentScore*0.78 + assetScore*0.22 + agentexec.ExactNumericEvidenceBonus(input.Query, segmentText)
		if len(queryTokens) > 0 && score == 0 {
			continue
		}
		if candidate.Quality == "usable" {
			score += 0.08
		}
		score -= agentexec.ShotQualityPenalty(candidate)
		if candidate.BoundaryVerified {
			score += 0.04
		}
		candidate.MatchedQueryTerms = agentexec.MatchedSemanticTerms(queryTokens, semanticText)
		for _, term := range agentexec.MatchedSemanticTerms(queryTokens, strings.ToLower(candidate.Transcript)) {
			candidate.MatchedQueryTerms = append(candidate.MatchedQueryTerms, "台词:"+term)
		}
		candidate.MatchEvidence = agentexec.ShotMatchEvidence(queryTokens, candidate)
		candidate.SegmentScore = agentexec.RoundScore(segmentScore)
		candidate.AssetScore = agentexec.RoundScore(assetScore)
		candidate.Score = math.Round(score*10000) / 10000
		matches = append(matches, candidate)
	}
	sort.SliceStable(matches, func(left, right int) bool {
		if matches[left].Score != matches[right].Score {
			return matches[left].Score > matches[right].Score
		}
		if matches[left].AssetID != matches[right].AssetID {
			return matches[left].AssetID < matches[right].AssetID
		}
		return matches[left].SourceStartFrame < matches[right].SourceStartFrame
	})
	total := len(matches)
	if len(matches) > limit {
		matches = matches[:limit]
	}
	missingIDs := make([]string, 0, len(missing))
	for _, candidate := range missing {
		missingIDs = append(missingIDs, candidate.AssetID)
	}
	understandingCandidates := agentexec.RankUnderstandingCandidates(
		missing, input.Query, input.Tags, roleFilter, min(limit, 20),
	)
	result := rushestools.ShotSearchResult{
		Query: input.Query, Shots: matches, TotalMatches: total, Truncated: total > len(matches),
		MissingUnderstandingAssetIDs: missingIDs, UnderstandingCandidates: understandingCandidates,
	}
	if len(missing) > 0 {
		result.UnderstandingCoverageNote = fmt.Sprintf(
			"当前草稿关联的可用视频素材中还有 %d 个尚未理解，未纳入本次检索；若目标画面可能在其中，先用 understand.materials 理解后重搜，或明确告知用户当前检索池并不完整。",
			len(missing),
		)
	}
	return result, nil
}

func (service *Service) draftShotIndex(
	ctx context.Context,
	draftID string,
	requestedAssetIDs []string,
) ([]indexedShot, []rushestools.ShotSearchUnderstandingCandidate, error) {
	assets, err := storage.ListDraftAssets(ctx, service.database.Read(), draftID)
	if err != nil {
		return nil, nil, err
	}
	requested := make(map[string]struct{}, len(requestedAssetIDs))
	for _, assetID := range requestedAssetIDs {
		requested[assetID] = struct{}{}
	}
	if len(requested) > 0 {
		for _, asset := range assets {
			if _, ok := requested[asset.ID]; ok && asset.Kind == "video" && asset.Usable {
				delete(requested, asset.ID)
			}
		}
		if len(requested) > 0 {
			invalid := make([]string, 0, len(requested))
			for assetID := range requested {
				invalid = append(invalid, assetID)
			}
			sort.Strings(invalid)
			return nil, nil, fmt.Errorf("asset_ids 包含不存在、不可用或非视频素材: %s", strings.Join(invalid, ", "))
		}
		for _, assetID := range requestedAssetIDs {
			requested[assetID] = struct{}{}
		}
	}

	shots := []indexedShot{}
	missing := []rushestools.ShotSearchUnderstandingCandidate{}
	seen := map[string]struct{}{}
	for _, asset := range assets {
		if asset.Kind != "video" || !asset.Usable {
			continue
		}
		if len(requested) > 0 {
			if _, ok := requested[asset.ID]; !ok {
				continue
			}
		}
		durationSec, _ := agentexec.NumericValue(asset.Probe["duration_sec"])
		availableFrames := int(math.Round(durationSec * timeline.DefaultFPS))
		relDir := ""
		if asset.RelDir != nil {
			relDir = *asset.RelDir
		}
		suggestedRole := understanding.SuggestVisualRole(asset.Filename, relDir, "")
		missingCandidate := rushestools.ShotSearchUnderstandingCandidate{
			AssetID: asset.ID, Filename: asset.Filename, RelDir: relDir,
			DurationFrames: availableFrames, SemanticRole: suggestedRole,
		}
		raw, summaryErr := storage.BestMaterialSummary(ctx, service.database.Read(), asset.ID)
		if errors.Is(summaryErr, storage.ErrNotFound) {
			missing = append(missing, missingCandidate)
			continue
		}
		if summaryErr != nil {
			return nil, nil, summaryErr
		}
		encoded, _ := json.Marshal(raw)
		var summary understanding.Summary
		if err := json.Unmarshal(encoded, &summary); err != nil {
			missing = append(missing, missingCandidate)
			continue
		}
		semanticRole := understanding.SuggestVisualRole(asset.Filename, relDir, summary.SemanticRole)
		transcript := storage.Transcript{}
		if persisted, transcriptErr := storage.LatestTranscript(ctx, service.database.Read(), asset.ID); transcriptErr == nil {
			transcript = persisted
		} else if !errors.Is(transcriptErr, storage.ErrNotFound) {
			return nil, nil, transcriptErr
		}
		for _, segment := range summary.Segments {
			start := max(0, segment.SourceStartFrame)
			end := segment.SourceEndFrame
			if availableFrames > 0 {
				end = min(end, availableFrames)
			}
			if end <= start || segment.Quality == "unusable" {
				continue
			}
			shotID := agentexec.StableShotID(asset.ID, start, end)
			if _, duplicate := seen[shotID]; duplicate {
				continue
			}
			seen[shotID] = struct{}{}
			transcriptText := agentexec.TranscriptTextForSourceRange(transcript.Utterances, start, end)
			shots = append(shots, indexedShot{
				candidate: rushestools.ShotCandidate{
					ShotID: shotID, AssetID: asset.ID, Filename: asset.Filename,
					SourceStartFrame: start, SourceEndFrame: end, DurationFrames: end - start,
					SemanticRole: semanticRole,
					Description:  segment.Description, Tags: append([]string(nil), segment.Tags...),
					Quality: segment.Quality, Subjects: append([]string(nil), segment.Subjects...),
					Actions: append([]string(nil), segment.Actions...), Setting: append([]string(nil), segment.Setting...),
					ShotScale: segment.ShotScale, Composition: segment.Composition,
					Lighting: append([]string(nil), segment.Lighting...), Mood: append([]string(nil), segment.Mood...),
					EditHints:  append([]string(nil), segment.EditHints...),
					Transcript: transcriptText, OverexposedRatio: segment.OverexposedRatio,
					SharpnessScore: segment.SharpnessScore,
					BoundaryKind:   segment.BoundaryKind, BoundaryVerified: segment.BoundaryVerified,
				},
				rangeInfo: agentexec.BeatMixSourceRange{StartFrame: start, EndFrame: end},
			})
		}
	}
	return shots, missing, nil
}
