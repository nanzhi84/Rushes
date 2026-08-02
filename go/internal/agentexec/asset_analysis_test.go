package agentexec

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func TestAssetAnalysisIdentityNormalizesOptionsAndVersionsEveryDimension(t *testing.T) {
	first, err := newAssetAnalysisIdentity(
		"hash", BeatAnalysisType, "analyzer-v1",
		map[string]any{"waveform_points": 96, "max_beats": 2000}, 1,
	)
	if err != nil {
		t.Fatal(err)
	}
	second, err := newAssetAnalysisIdentity(
		"hash", BeatAnalysisType, "analyzer-v1",
		map[string]any{"max_beats": 2000, "waveform_points": 96}, 1,
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || first.NormalizedOptionsJSON != second.NormalizedOptionsJSON {
		t.Fatalf("options were not canonical: first=%#v second=%#v", first, second)
	}
	for name, candidate := range map[string]assetAnalysisIdentity{
		"content":  mustAssetAnalysisIdentity(t, "other", BeatAnalysisType, "analyzer-v1", map[string]any{"max_beats": 2000, "waveform_points": 96}, 1),
		"type":     mustAssetAnalysisIdentity(t, "hash", SpeechPauseAnalysisType, "analyzer-v1", map[string]any{"max_beats": 2000, "waveform_points": 96}, 1),
		"analyzer": mustAssetAnalysisIdentity(t, "hash", BeatAnalysisType, "analyzer-v2", map[string]any{"max_beats": 2000, "waveform_points": 96}, 1),
		"options":  mustAssetAnalysisIdentity(t, "hash", BeatAnalysisType, "analyzer-v1", map[string]any{"max_beats": 512, "waveform_points": 96}, 1),
		"schema":   mustAssetAnalysisIdentity(t, "hash", BeatAnalysisType, "analyzer-v1", map[string]any{"max_beats": 2000, "waveform_points": 96}, 2),
	} {
		if candidate.ID == first.ID {
			t.Fatalf("%s dimension did not invalidate identity: %#v", name, candidate)
		}
	}
}

func mustAssetAnalysisIdentity(
	t *testing.T,
	hash, analysisType, analyzer string,
	options map[string]any,
	schema int,
) assetAnalysisIdentity {
	t.Helper()
	identity, err := newAssetAnalysisIdentity(hash, analysisType, analyzer, options, schema)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func TestBeatAnalysisSingleflightsByContentHashAcrossDrafts(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const (
		firstDraft  = "draft_beat_hash_a"
		secondDraft = "draft_beat_hash_b"
		firstAsset  = "asset_beat_hash_a"
		secondAsset = "asset_beat_hash_b"
	)
	agenttest.CreateAgentDraft(t, database, firstDraft)
	agenttest.CreateAgentDraft(t, database, secondDraft)
	audio := createSpeechFixtureAudio(t, database.Paths.Temporary, "shared-beat-content")
	agenttest.InsertSpeechFixtureAsset(t, database, firstDraft, firstAsset, audio)
	agenttest.InsertSpeechFixtureAsset(t, database, secondDraft, secondAsset, audio)
	if _, err := database.Write().ExecContext(t.Context(), `
		UPDATE assets SET hash='shared_beat_content_hash'
		WHERE asset_id IN (?, ?)`, firstAsset, secondAsset,
	); err != nil {
		t.Fatal(err)
	}

	fakeBin := t.TempDir()
	counterPath := filepath.Join(t.TempDir(), "aubiotrack-calls")
	for name, body := range map[string]string{
		"aubiotrack": "#!/bin/sh\nprintf x >> \"$BEAT_ANALYSIS_COUNT_FILE\"\nprintf '0.25\\n0.50\\n0.75\\n1.00\\n1.25\\n1.50\\n'\n",
		"aubioonset": "#!/bin/sh\nprintf '0.25\\n0.75\\n1.25\\n'\n",
	} {
		if err := os.WriteFile(filepath.Join(fakeBin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("BEAT_ANALYSIS_COUNT_FILE", counterPath)
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	executor, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		assetID string
		result  rushestools.AudioBeatAnalysisResult
		err     error
	}
	started := make(chan struct{})
	outcomes := make(chan outcome, 2)
	var workers sync.WaitGroup
	for _, pair := range []struct{ draftID, assetID string }{
		{firstDraft, firstAsset}, {secondDraft, secondAsset},
	} {
		pair := pair
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-started
			result, ensureErr := executor.EnsureBeatAnalysis(
				t.Context(), pair.draftID, pair.assetID,
			)
			outcomes <- outcome{assetID: pair.assetID, result: result, err: ensureErr}
		}()
	}
	close(started)
	workers.Wait()
	close(outcomes)

	analysisID := ""
	cacheHits := 0
	for item := range outcomes {
		if item.err != nil {
			t.Fatal(item.err)
		}
		if item.result.AssetID != item.assetID || len(item.result.BeatFrames) < 2 {
			t.Fatalf("result=%#v want_asset=%s", item.result, item.assetID)
		}
		if analysisID == "" {
			analysisID = item.result.AnalysisID
		} else if item.result.AnalysisID != analysisID {
			t.Fatalf("analysis ids differ: %s != %s", item.result.AnalysisID, analysisID)
		}
		if item.result.CacheHit {
			cacheHits++
		}
	}
	calls, err := os.ReadFile(counterPath)
	if err != nil {
		t.Fatal(err)
	}
	var stored int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM asset_analyses WHERE analysis_type=?`, BeatAnalysisType,
	).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if analysisID == "" || cacheHits != 1 || len(calls) != 1 || stored != 1 {
		t.Fatalf(
			"analysis_id=%q cache_hits=%d analyzer_calls=%d stored=%d",
			analysisID, cacheHits, len(calls), stored,
		)
	}
	if _, err := storage.AssetAnalysisByID(t.Context(), database.Read(), analysisID); err != nil {
		t.Fatal(err)
	}
}
