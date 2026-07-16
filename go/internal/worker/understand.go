package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/understanding"
)

func RegisterUnderstand(
	registry *Registry,
	database *storage.DB,
	analyzer *understanding.Analyzer,
) error {
	if analyzer == nil {
		analyzer = understanding.NewAnalyzer(nil)
	}
	return registry.Register("understand", func(
		ctx context.Context,
		job Job,
		report ProgressReporter,
	) (map[string]any, error) {
		assetIDs := stringSlice(job.Payload["asset_ids"])
		if len(assetIDs) == 0 && job.AssetID != nil {
			assetIDs = []string{*job.AssetID}
		}
		if len(assetIDs) == 0 {
			return nil, errors.New("understand job 缺少 asset_ids")
		}
		focus, _ := job.Payload["focus"].(string)
		depth, _ := job.Payload["depth"].(string)
		maxSteps := understandInt(job.Payload["max_steps_per_asset"])
		forceRefresh, _ := job.Payload["force_refresh"].(bool)
		completedIDs := []string{}
		cacheHitIDs := []string{}
		analyzedIDs := []string{}
		for index, assetID := range assetIDs {
			asset, err := storage.GetAsset(ctx, database.Read(), assetID)
			if err != nil {
				return nil, err
			}
			reportCompleted := func(stage string) error {
				return report(ctx, job, ProgressUpdate{
					Progress:       float64(index+1) / float64(len(assetIDs)),
					CurrentAssetID: assetID, Done: index + 1, Total: len(assetIDs),
					Stage:  stage,
					Detail: fmt.Sprintf("理解素材 %d/%d：%s 已完成", index+1, len(assetIDs), asset.Filename),
				})
			}
			summaryID := fmt.Sprintf("summary_%s_%s", assetID, job.ID)
			var summaryExists int
			if err := database.Read().QueryRowContext(ctx,
				"SELECT COUNT(*) FROM material_summaries WHERE summary_id=?", summaryID,
			).Scan(&summaryExists); err != nil {
				return nil, err
			}
			if summaryExists != 0 {
				completedIDs = append(completedIDs, assetID)
				analyzedIDs = append(analyzedIDs, assetID)
				if err := reportCompleted("already_completed"); err != nil {
					return nil, err
				}
				continue
			}
			options := understanding.NormalizeAnalyzeOptions(asset, understanding.AnalyzeOptions{
				Focus: focus, Depth: depth, MaxStepsPerAsset: maxSteps,
			})
			fingerprint := understanding.AnalysisFingerprint(asset, options)
			if !forceRefresh {
				if _, cacheErr := storage.MaterialSummaryByFingerprint(
					ctx, database.Read(), assetID, fingerprint,
				); cacheErr == nil {
					completedIDs = append(completedIDs, assetID)
					cacheHitIDs = append(cacheHitIDs, assetID)
					if err := reportCompleted("cache_hit"); err != nil {
						return nil, err
					}
					continue
				} else if !errors.Is(cacheErr, storage.ErrNotFound) {
					return nil, cacheErr
				}
			}
			if _, err := reducer.Apply(ctx, database, []contracts.Event{{
				Type: "MaterialUnderstandingStarted", Payload: map[string]any{
					"asset_id": assetID, "job_id": job.ID, "attempt": job.Attempts,
				},
			}}, claimedJobOptions(job, reducer.Options{})); err != nil {
				return nil, err
			}
			var progressErr error
			analyzeCtx, cancelAnalyze := context.WithCancel(ctx)
			summary, err := analyzer.AnalyzeWithOptions(analyzeCtx, database, asset, options, func(note string) {
				if progressErr != nil {
					return
				}
				stage, message := understandingProgressDetail(note)
				progressErr = report(ctx, job, ProgressUpdate{
					Progress:       (float64(index) + understandingStageProgress(stage)) / float64(len(assetIDs)),
					CurrentAssetID: assetID, Done: index, Total: len(assetIDs), Stage: stage,
					Detail: fmt.Sprintf("理解素材 %d/%d：%s %s", index+1, len(assetIDs), asset.Filename, message),
				})
				if progressErr != nil {
					cancelAnalyze()
				}
			})
			cancelAnalyze()
			if progressErr != nil {
				err = progressErr
			}
			if err != nil {
				_, failureErr := reducer.Apply(context.WithoutCancel(ctx), database, []contracts.Event{{
					Type: "MaterialUnderstandingFailed", Payload: map[string]any{
						"asset_id": assetID, "job_id": job.ID, "attempt": job.Attempts,
						"cancelled": errors.Is(err, context.Canceled) || errors.Is(err, reducer.ErrJobCancelled),
						"failure":   map[string]any{"message": err.Error()},
					},
				}}, reducer.Options{Actor: contracts.ActorJob})
				return nil, errors.Join(fmt.Errorf("素材 %s 理解失败: %w", assetID, err), failureErr)
			}
			var summaryMap map[string]any
			data, _ := json.Marshal(summary)
			_ = json.Unmarshal(data, &summaryMap)
			result, err := reducer.Apply(ctx, database, []contracts.Event{{
				Type: "MaterialUnderstandingCompleted", Payload: map[string]any{
					"asset_id": assetID, "job_id": job.ID, "attempt": job.Attempts,
					"summary_id": summaryID,
				},
			}}, claimedJobOptions(job, reducer.Options{
				ResultRows: reducer.ResultRows{MaterialSummaries: []reducer.MaterialSummaryRow{{
					ID: summaryID, AssetID: assetID, Version: 0, Focus: understandStringPointer(options.Focus),
					Status: "ready", Summary: summaryMap,
					Model: understandStringPointer(summary.Model), Fingerprint: understandStringPointer(fingerprint),
					PromptVersion: understandStringPointer(understanding.PromptVersion),
				}}},
			}))
			if err != nil || result.Status != reducer.StatusApplied {
				return nil, errors.Join(err, fmt.Errorf("understand reducer status: %s", result.Status))
			}
			completedIDs = append(completedIDs, assetID)
			analyzedIDs = append(analyzedIDs, assetID)
			if err := reportCompleted("completed"); err != nil {
				return nil, err
			}
		}
		return map[string]any{
			"asset_ids": completedIDs, "cache_hit_asset_ids": cacheHitIDs,
			"analyzed_asset_ids": analyzedIDs, "status": "completed",
		}, nil
	})
}

func understandingProgressDetail(note string) (string, string) {
	stage, message, found := strings.Cut(note, "：")
	if !found {
		stage, message = "analyze", note
	}
	stage = strings.TrimSpace(stage)
	message = strings.TrimSpace(message)
	if message == "" {
		message = "正在分析"
	}
	return stage, message
}

func understandingStageProgress(stage string) float64 {
	switch stage {
	case "audio_probe":
		return 0.1
	case "scene_detect":
		return 0.15
	case "scene_verify":
		return 0.35
	case "view_frames":
		return 0.55
	case "transcribe":
		return 0.8
	case "emit_summary":
		return 0.95
	default:
		return 0.5
	}
}

func stringSlice(value any) []string {
	switch typed := value.(type) {
	case []string:
		return append([]string(nil), typed...)
	case []any:
		result := []string{}
		for _, item := range typed {
			if text, ok := item.(string); ok {
				result = append(result, text)
			}
		}
		return result
	default:
		return nil
	}
}

func understandInt(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	default:
		return 0
	}
}

func understandStringPointer(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
