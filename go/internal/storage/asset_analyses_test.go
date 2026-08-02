package storage

import (
	"errors"
	"testing"
)

func TestAssetAnalysisExactIdentityAndLatestBatchLookup(t *testing.T) {
	t.Parallel()
	database, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	for _, row := range []struct {
		id, hash, analysisType, analyzer, options, createdAt string
		schema                                               int
		result                                               string
	}{
		{"analysis_old", "hash_a", "beat_grid", "beat-v1", `{"max_beats":512}`, "2026-08-01T00:00:00Z", 1, `{"bpm":90}`},
		{"analysis_new", "hash_a", "beat_grid", "beat-v2", `{"max_beats":2000}`, "2026-08-02T00:00:00Z", 1, `{"bpm":120}`},
		{"analysis_other", "hash_b", "beat_grid", "beat-v1", `{"max_beats":512}`, "2026-08-01T00:00:00Z", 1, `{"bpm":100}`},
		{"analysis_transcript", "hash_a", "transcript", "asr-v1", `{"language":"zh"}`, "2026-08-03T00:00:00Z", 1, `{"provider_id":"fixture"}`},
	} {
		if _, err := database.Write().ExecContext(t.Context(), `
			INSERT INTO asset_analyses(
				analysis_id,asset_content_hash,analysis_type,analyzer_version,
				normalized_options_json,output_schema_version,result_json,created_at
			) VALUES(?,?,?,?,?,?,?,?)`,
			row.id, row.hash, row.analysisType, row.analyzer, row.options,
			row.schema, row.result, row.createdAt,
		); err != nil {
			t.Fatal(err)
		}
	}

	exact, err := AssetAnalysisByIdentity(
		t.Context(), database.Read(), "hash_a", "beat_grid", "beat-v1",
		`{"max_beats":512}`, 1,
	)
	if err != nil || exact.ID != "analysis_old" || exact.Result["bpm"] != float64(90) {
		t.Fatalf("exact=%#v err=%v", exact, err)
	}
	if _, err := AssetAnalysisByID(t.Context(), database.Read(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing err=%v", err)
	}

	latest, err := LatestAssetAnalysesForContentHashes(
		t.Context(), database.Read(), []string{"hash_a", "hash_b"}, "beat_grid",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(latest) != 2 || latest["hash_a"].ID != "analysis_new" ||
		latest["hash_b"].ID != "analysis_other" {
		t.Fatalf("latest=%#v", latest)
	}
}
