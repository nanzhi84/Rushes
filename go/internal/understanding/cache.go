package understanding

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"strings"

	"github.com/nanzhi84/Rushes/go/internal/storage"
)

// PromptVersion participates in the persistent material-understanding cache key.
// Bump it whenever the stored semantic shape or VLM instructions materially change.
const PromptVersion = "go-shot-context-v4"

// NormalizeAnalyzeOptions keeps long videos from being accidentally reduced to a
// handful of windows by a shallow model-authored max_steps_per_asset value.
func NormalizeAnalyzeOptions(asset storage.Asset, options AnalyzeOptions) AnalyzeOptions {
	depth := strings.ToLower(strings.TrimSpace(options.Depth))
	if depth != "deep" {
		depth = "scan"
	}
	options.Depth = depth
	options.Focus = strings.Join(strings.Fields(options.Focus), " ")
	if asset.Kind != "video" {
		options.MaxStepsPerAsset = 0
		return options
	}

	durationSec := numeric(asset.Probe["duration_sec"])
	durationFloor := max(3, min(8, int(math.Ceil(durationSec/12))))
	defaultSteps := 8
	if depth == "deep" {
		defaultSteps = 16
		durationFloor = max(durationFloor, 8)
	}
	if options.MaxStepsPerAsset <= 0 {
		options.MaxStepsPerAsset = defaultSteps
	} else {
		options.MaxStepsPerAsset = max(durationFloor, min(24, options.MaxStepsPerAsset))
	}
	return options
}

// AnalysisFingerprint makes understanding idempotent across chat clears, process
// restarts and drafts that reference the same unchanged asset.
func AnalysisFingerprint(asset storage.Asset, options AnalyzeOptions) string {
	return analysisFingerprint(asset, options, PromptVersion)
}

func analysisFingerprint(asset storage.Asset, options AnalyzeOptions, promptVersion string) string {
	payload := struct {
		AssetHash string         `json:"asset_hash"`
		Kind      string         `json:"kind"`
		MTime     *int64         `json:"mtime,omitempty"`
		Size      int64          `json:"size"`
		Prompt    string         `json:"prompt_version"`
		Options   AnalyzeOptions `json:"options"`
	}{
		AssetHash: asset.Hash,
		Kind:      asset.Kind,
		MTime:     asset.MTime,
		Size:      asset.Size,
		Prompt:    promptVersion,
		Options:   options,
	}
	encoded, _ := json.Marshal(payload)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}
