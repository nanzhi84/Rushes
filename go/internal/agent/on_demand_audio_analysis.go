package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
	"github.com/nanzhi84/Rushes/go/internal/understanding"
)

func (service *Service) prepareOnDemandAudioAnalysis(
	ctx context.Context,
	draftID, toolName string,
	input any,
) error {
	switch toolName {
	case "timeline.insert", "timeline.update":
		assetID, err := service.bgmAnalysisAssetID(ctx, draftID, toolName, input)
		if err != nil {
			return err
		}
		if assetID == "" {
			return nil
		}
		_, err = service.executeHarnessAnalysisStep(
			ctx, draftID, "audio.analyze_beats", map[string]any{"asset_id": assetID},
			func() (any, error) {
				return service.executor.EnsureBeatAnalysis(ctx, draftID, assetID)
			},
		)
		return err
	case "speech.search":
		typed, ok := speechSearchInputValue(input)
		if !ok {
			return nil
		}
		_, err := service.executeHarnessAnalysisStep(
			ctx, draftID, "speech.transcribe", map[string]any{
				"asset_id": typed.AssetID, "timeline_clip_id": typed.TimelineClipID,
			},
			func() (any, error) {
				return service.executor.EnsureTranscript(
					ctx, draftID, typed.AssetID, typed.TimelineClipID,
				)
			},
		)
		return err
	default:
		return nil
	}
}

func (service *Service) bgmAnalysisAssetID(
	ctx context.Context,
	draftID, toolName string,
	input any,
) (string, error) {
	operation, err := rushestools.TimelineAtomicOperation(toolName, input)
	if err != nil {
		return "", nil
	}
	switch agentexec.StringValue(operation["kind"]) {
	case "insert_clip":
		if agentexec.ValueOr(
			agentexec.StringValue(operation["track_id"]), "visual_base",
		) == "bgm" {
			return agentexec.StringValue(operation["asset_id"]), nil
		}
	case "replace_clip":
		current, timelineErr := timeline.Latest(ctx, service.database, draftID)
		if errors.Is(timelineErr, storage.ErrNotFound) {
			return "", nil
		}
		if timelineErr != nil {
			return "", timelineErr
		}
		targetID := agentexec.StringValue(operation["timeline_clip_id"])
		for _, track := range current.Tracks {
			if track.TrackID != "bgm" {
				continue
			}
			for _, clip := range track.Clips {
				if clip.TimelineClipID == targetID {
					return agentexec.StringValue(operation["asset_id"]), nil
				}
			}
		}
	}
	return "", nil
}

func speechSearchInputValue(input any) (rushestools.SpeechSearchInput, bool) {
	switch typed := input.(type) {
	case rushestools.SpeechSearchInput:
		return typed, true
	case *rushestools.SpeechSearchInput:
		if typed != nil {
			return *typed, true
		}
	}
	return rushestools.SpeechSearchInput{}, false
}

func (service *Service) executeHarnessAnalysisStep(
	ctx context.Context,
	draftID, toolName string,
	arguments any,
	execute func() (any, error),
) (any, error) {
	stepID := agentexec.RandomID("step")
	startedAt := time.Now()
	argsSummary := compactJSON(arguments)
	service.hub.Record(draftID, StreamEvent{
		"type": TurnStreamToolStepStarted, "step_id": stepID, "tool": toolName,
		"args_summary": argsSummary, "harness_owned": true, "progress": 0,
	})
	service.hub.Record(draftID, StreamEvent{
		"type": TurnStreamToolStepProgress, "step_id": stepID, "tool": toolName,
		"harness_owned": true, "progress": 0.5, "note": "正在确保按需分析证据",
	})
	result, err := execute()
	durationMS := time.Since(startedAt).Milliseconds()
	status := "succeeded"
	observation := compactJSON(result)
	if err != nil {
		status, observation = "failed", err.Error()
	}
	service.hub.Record(draftID, StreamEvent{
		"type": TurnStreamToolStepFinished, "step_id": stepID, "tool": toolName,
		"status": status, "observation": observation, "harness_owned": true,
		"progress": 1, "duration_ms": durationMS,
	})
	_ = service.persistToolTrace(
		context.WithoutCancel(ctx), draftID, stepID, toolName, status,
		argsSummary, observation, "", "", map[string]any{
			"harness_owned": true, "progress": 1, "duration_ms": durationMS,
		},
	)
	return result, err
}

// ensureExplicitBeatTaskAnalysis handles the one safe pre-provider case: the
// user has requested beat editing and one BGM is already selected unambiguously
// (named asset, current BGM clip, or exactly one available BGM-role audio). It
// never chooses among multiple creative candidates.
func (service *Service) ensureExplicitBeatTaskAnalysis(
	ctx context.Context,
	draftID, userText string,
) error {
	if !requestsBeatEditWorkflow(userText) {
		return nil
	}
	assetID, err := service.unambiguousBeatAsset(ctx, draftID, userText)
	if err != nil || assetID == "" {
		return err
	}
	_, err = service.executeHarnessAnalysisStep(
		ctx, draftID, "audio.analyze_beats", map[string]any{"asset_id": assetID},
		func() (any, error) {
			return service.executor.EnsureBeatAnalysis(ctx, draftID, assetID)
		},
	)
	return err
}

func (service *Service) unambiguousBeatAsset(
	ctx context.Context,
	draftID, userText string,
) (string, error) {
	assets, err := storage.ListDraftAssets(ctx, service.database.Read(), draftID)
	if err != nil {
		return "", err
	}
	eligible := make([]storage.Asset, 0)
	for _, asset := range assets {
		if asset.Usable && asset.Kind == "audio" {
			eligible = append(eligible, asset)
			if strings.Contains(userText, asset.ID) {
				return asset.ID, nil
			}
		}
	}
	if document, timelineErr := timeline.Latest(ctx, service.database, draftID); timelineErr == nil {
		selected := ""
		for _, track := range document.Tracks {
			if track.TrackID != "bgm" {
				continue
			}
			for _, clip := range track.Clips {
				if selected != "" && selected != clip.AssetID {
					return "", nil
				}
				selected = clip.AssetID
			}
		}
		if selected != "" {
			return selected, nil
		}
	} else if !errors.Is(timelineErr, storage.ErrNotFound) {
		return "", fmt.Errorf("读取当前 BGM: %w", timelineErr)
	}
	bgmCandidates := make([]string, 0)
	for _, asset := range eligible {
		duration, _ := agentexec.NumericValue(asset.Probe["duration_sec"])
		if understanding.ClassifyAudioRole(asset.Filename, duration) == "bgm" {
			bgmCandidates = append(bgmCandidates, asset.ID)
		}
	}
	if len(bgmCandidates) == 1 {
		return bgmCandidates[0], nil
	}
	if len(eligible) == 1 {
		return eligible[0].ID, nil
	}
	return "", nil
}
