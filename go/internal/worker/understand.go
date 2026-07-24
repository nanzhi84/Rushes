package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/understanding"
)

type understandPayload struct {
	AssetID             string `json:"asset_id"`
	Focus               string `json:"focus"`
	Depth               string `json:"depth"`
	MaxStepsPerAsset    int    `json:"max_steps_per_asset"`
	ForceRefresh        bool   `json:"force_refresh"`
	RefreshNonce        string `json:"refresh_nonce"`
	AnalysisFingerprint string `json:"analysis_fingerprint"`
}

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
		payload, err := decodeUnderstandPayload(job.Payload)
		if err != nil {
			return nil, fmt.Errorf("understand job payload 无效: %w", err)
		}
		assetID := strings.TrimSpace(payload.AssetID)
		if assetID == "" {
			return nil, errors.New("understand job 缺少 asset_id")
		}
		if job.AssetID == nil || strings.TrimSpace(*job.AssetID) != assetID {
			return nil, errors.New("understand job 的 payload asset_id 与 job asset_id 不一致")
		}
		focus := payload.Focus
		depth := payload.Depth
		maxSteps := payload.MaxStepsPerAsset
		forceRefresh := payload.ForceRefresh
		asset, err := storage.GetAsset(ctx, database.Read(), assetID)
		if err != nil {
			return nil, err
		}
		reportCompleted := func(stage string) error {
			return report(ctx, job, ProgressUpdate{
				Progress: 1, CurrentAssetID: assetID, Done: 1, Total: 1,
				Stage: stage, Detail: fmt.Sprintf("理解素材：%s 已完成", asset.Filename),
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
			if err := reportCompleted("already_completed"); err != nil {
				return nil, err
			}
			return map[string]any{
				"asset_id": assetID, "cache_hit": false,
				"analyzed": true, "status": "completed",
			}, nil
		}
		options := understanding.NormalizeAnalyzeOptions(asset, understanding.AnalyzeOptions{
			Focus: focus, Depth: depth, MaxStepsPerAsset: maxSteps,
		})
		fingerprint := understanding.AnalysisFingerprint(asset, options)
		if !forceRefresh {
			if _, cacheErr := storage.MaterialSummaryByFingerprint(
				ctx, database.Read(), assetID, fingerprint,
			); cacheErr == nil {
				if err := reportCompleted("cache_hit"); err != nil {
					return nil, err
				}
				return map[string]any{
					"asset_id": assetID, "cache_hit": true,
					"analyzed": false, "status": "completed",
				}, nil
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
				Progress: understandingStageProgress(stage), CurrentAssetID: assetID,
				Done: 0, Total: 1, Stage: stage,
				Detail: fmt.Sprintf("理解素材：%s %s", asset.Filename, message),
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
			}}, claimedJobOptions(job, reducer.Options{}))
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
		if err := reportCompleted("completed"); err != nil {
			return nil, err
		}
		return map[string]any{
			"asset_id": assetID, "cache_hit": false,
			"analyzed": true, "status": "completed",
		}, nil
	})
}

func decodeUnderstandPayload(value map[string]any) (understandPayload, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return understandPayload{}, err
	}
	var payload understandPayload
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return understandPayload{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("包含多个 JSON 值")
		}
		return understandPayload{}, err
	}
	return payload, nil
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

func understandStringPointer(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
