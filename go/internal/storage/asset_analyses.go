package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// AssetAnalysis is an immutable content-addressed analyzer result. AssetID is
// deliberately absent: asset rows are draft-facing handles, while the analysis
// survives and is reused by every handle with the same content hash.
type AssetAnalysis struct {
	ID                    string
	AssetContentHash      string
	AnalysisType          string
	AnalyzerVersion       string
	NormalizedOptionsJSON string
	OutputSchemaVersion   int
	Result                map[string]any
	CreatedAt             string
}

const assetAnalysisColumns = `
analysis_id, asset_content_hash, analysis_type, analyzer_version,
normalized_options_json, output_schema_version, result_json, created_at`

func AssetAnalysisByID(
	ctx context.Context,
	query Querier,
	analysisID string,
) (AssetAnalysis, error) {
	return scanAssetAnalysis(query.QueryRowContext(ctx, `
		SELECT `+assetAnalysisColumns+` FROM asset_analyses WHERE analysis_id=?`, analysisID,
	))
}

func AssetAnalysisByIdentity(
	ctx context.Context,
	query Querier,
	assetContentHash, analysisType, analyzerVersion, normalizedOptionsJSON string,
	outputSchemaVersion int,
) (AssetAnalysis, error) {
	return scanAssetAnalysis(query.QueryRowContext(ctx, `
		SELECT `+assetAnalysisColumns+` FROM asset_analyses
		WHERE asset_content_hash=? AND analysis_type=? AND analyzer_version=?
			AND normalized_options_json=? AND output_schema_version=?`,
		assetContentHash, analysisType, analyzerVersion, normalizedOptionsJSON,
		outputSchemaVersion,
	))
}

// LatestAssetAnalysesForContentHashes returns at most one latest result for
// every content-hash/type pair. It is used only for bounded WorldState
// projection; execution paths always request the exact compound identity.
func LatestAssetAnalysesForContentHashes(
	ctx context.Context,
	query Querier,
	contentHashes []string,
	analysisType string,
) (map[string]AssetAnalysis, error) {
	result := map[string]AssetAnalysis{}
	if len(contentHashes) == 0 {
		return result, nil
	}
	placeholders, args := inClausePlaceholders(contentHashes)
	args = append(args, analysisType)
	rows, err := query.QueryContext(ctx, `
		SELECT `+assetAnalysisColumns+` FROM asset_analyses AS analysis
		WHERE analysis.rowid IN (
			SELECT MAX(candidate.rowid) FROM asset_analyses AS candidate
			WHERE candidate.asset_content_hash IN (`+placeholders+`)
				AND candidate.analysis_type=?
			GROUP BY candidate.asset_content_hash
		)`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		analysis, err := scanAssetAnalysis(rows)
		if err != nil {
			return nil, err
		}
		result[analysis.AssetContentHash] = analysis
	}
	return result, rows.Err()
}

func scanAssetAnalysis(row rowScanner) (AssetAnalysis, error) {
	var analysis AssetAnalysis
	var raw string
	if err := row.Scan(
		&analysis.ID, &analysis.AssetContentHash, &analysis.AnalysisType,
		&analysis.AnalyzerVersion, &analysis.NormalizedOptionsJSON,
		&analysis.OutputSchemaVersion, &raw, &analysis.CreatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AssetAnalysis{}, ErrNotFound
		}
		return AssetAnalysis{}, err
	}
	if err := json.Unmarshal([]byte(raw), &analysis.Result); err != nil {
		return AssetAnalysis{}, err
	}
	if analysis.Result == nil {
		analysis.Result = map[string]any{}
	}
	return analysis, nil
}
