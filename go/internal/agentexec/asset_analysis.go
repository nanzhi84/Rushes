package agentexec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

const (
	BeatAnalysisType           = "beat_grid"
	SpeechPauseAnalysisType    = "speech_pause"
	TranscriptAnalysisType     = "transcript"
	beatAnalyzerVersion        = "aubio-tempo-specflux-waveform-v1"
	speechPauseAnalyzerVersion = "ffmpeg-rms-spectral-breath-v1"
	assetAnalysisOutputSchema  = 1
)

type assetAnalysisIdentity struct {
	ID                    string
	AssetContentHash      string
	AnalysisType          string
	AnalyzerVersion       string
	NormalizedOptionsJSON string
	OutputSchemaVersion   int
}

func newAssetAnalysisIdentity(
	assetContentHash, analysisType, analyzerVersion string,
	options map[string]any,
	outputSchemaVersion int,
) (assetAnalysisIdentity, error) {
	if assetContentHash == "" || analysisType == "" || analyzerVersion == "" ||
		outputSchemaVersion < 1 {
		return assetAnalysisIdentity{}, errors.New("analysis identity 字段不完整")
	}
	normalized, err := json.Marshal(options)
	if err != nil {
		return assetAnalysisIdentity{}, fmt.Errorf("规范化 analysis options: %w", err)
	}
	key := assetContentHash + "\x00" + analysisType + "\x00" + analyzerVersion + "\x00" +
		string(normalized) + "\x00" + strconv.Itoa(outputSchemaVersion)
	digest := sha256.Sum256([]byte(key))
	return assetAnalysisIdentity{
		ID:               "analysis_" + hex.EncodeToString(digest[:16]),
		AssetContentHash: assetContentHash, AnalysisType: analysisType,
		AnalyzerVersion: analyzerVersion, NormalizedOptionsJSON: string(normalized),
		OutputSchemaVersion: outputSchemaVersion,
	}, nil
}

func (exec *Executor) cachedAssetAnalysis(
	ctx context.Context,
	identity assetAnalysisIdentity,
) (storage.AssetAnalysis, error) {
	return storage.AssetAnalysisByIdentity(
		ctx, exec.database.Read(), identity.AssetContentHash, identity.AnalysisType,
		identity.AnalyzerVersion, identity.NormalizedOptionsJSON,
		identity.OutputSchemaVersion,
	)
}

func (exec *Executor) persistAssetAnalyses(
	ctx context.Context,
	analyses []reducer.AssetAnalysisRow,
	transcripts ...reducer.TranscriptRow,
) error {
	result, err := reducer.Apply(ctx, exec.database, nil, reducer.Options{
		Actor: contracts.ActorAgent,
		ResultRows: reducer.ResultRows{
			AssetAnalyses: analyses,
			Transcripts:   transcripts,
		},
	})
	if err != nil {
		return err
	}
	if result.Status != reducer.StatusApplied {
		return fmt.Errorf("asset analysis reducer status: %s", result.Status)
	}
	return nil
}

func assetAnalysisResultMap(value any) (map[string]any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := json.Unmarshal(encoded, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func decodeAssetAnalysisResult[T any](analysis storage.AssetAnalysis) (T, error) {
	var result T
	encoded, err := json.Marshal(analysis.Result)
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal(encoded, &result); err != nil {
		return result, err
	}
	return result, nil
}

func (exec *Executor) withAnalysisSingleflight(
	identity assetAnalysisIdentity,
	fn func() error,
) error {
	coordinator := exec.analysisResources
	if coordinator == nil {
		coordinator = NewIndexedResourceCoordinator()
		exec.analysisResources = coordinator
	}
	release := coordinator.Begin([]IndexedResourceAccess{{
		Domain: "asset_analysis", Resources: []string{identity.ID}, WriteResource: true,
	}})
	defer release()
	return fn()
}

func logAssetAnalysis(
	identity assetAnalysisIdentity,
	cacheHit bool,
	startedAt time.Time,
	err error,
) {
	status := "succeeded"
	if err != nil {
		status = "failed"
	}
	slog.Info(
		"按需素材分析完成",
		"analysis_id", identity.ID,
		"analysis_type", identity.AnalysisType,
		"analyzer_version", identity.AnalyzerVersion,
		"cache_hit", cacheHit,
		"duration_ms", time.Since(startedAt).Milliseconds(),
		"status", status,
	)
}
