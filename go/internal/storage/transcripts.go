package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

type Transcript struct {
	ID           string
	AssetID      string
	ProviderID   string
	RawPreserved bool
	Utterances   []map[string]any
	VADSegments  []map[string]any
}

const transcriptColumns = `
transcript_id, asset_id, provider_id, raw_preserved, utterances_json, vad_segments_json`

func LatestTranscript(ctx context.Context, query Querier, assetID string) (Transcript, error) {
	return scanTranscript(query.QueryRowContext(ctx, `
		SELECT `+transcriptColumns+` FROM transcripts
		WHERE asset_id=? ORDER BY rowid DESC LIMIT 1`, assetID,
	))
}

// LatestTranscriptsForAssets 是 LatestTranscript 的批量版:一条查询取回每个 asset 的最新
// 转写(按 rowid 最大,与单条版 ORDER BY rowid DESC LIMIT 1 同义)。无转写的 asset 不入
// map。返回 asset_id→Transcript。
func LatestTranscriptsForAssets(
	ctx context.Context,
	query Querier,
	assetIDs []string,
) (map[string]Transcript, error) {
	result := map[string]Transcript{}
	if len(assetIDs) == 0 {
		return result, nil
	}
	placeholders, args := inClausePlaceholders(assetIDs)
	rows, err := query.QueryContext(ctx, `
		SELECT `+transcriptColumns+` FROM transcripts
		WHERE rowid IN (
			SELECT MAX(rowid) FROM transcripts
			WHERE asset_id IN (`+placeholders+`) GROUP BY asset_id
		)`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		transcript, err := scanTranscript(rows)
		if err != nil {
			return nil, err
		}
		result[transcript.AssetID] = transcript
	}
	return result, rows.Err()
}

func scanTranscript(row rowScanner) (Transcript, error) {
	var transcript Transcript
	var rawPreserved int
	var utterancesJSON, vadJSON string
	if err := row.Scan(
		&transcript.ID, &transcript.AssetID, &transcript.ProviderID, &rawPreserved,
		&utterancesJSON, &vadJSON,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Transcript{}, ErrNotFound
		}
		return Transcript{}, err
	}
	transcript.RawPreserved = rawPreserved != 0
	if err := json.Unmarshal([]byte(utterancesJSON), &transcript.Utterances); err != nil {
		return Transcript{}, err
	}
	if err := json.Unmarshal([]byte(vadJSON), &transcript.VADSegments); err != nil {
		return Transcript{}, err
	}
	if transcript.Utterances == nil {
		transcript.Utterances = []map[string]any{}
	}
	if transcript.VADSegments == nil {
		transcript.VADSegments = []map[string]any{}
	}
	return transcript, nil
}
