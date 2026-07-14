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

func GetTranscript(ctx context.Context, query Querier, transcriptID string) (Transcript, error) {
	return scanTranscript(query.QueryRowContext(ctx,
		"SELECT "+transcriptColumns+" FROM transcripts WHERE transcript_id=?", transcriptID,
	))
}

func LatestTranscript(ctx context.Context, query Querier, assetID string) (Transcript, error) {
	return scanTranscript(query.QueryRowContext(ctx, `
		SELECT `+transcriptColumns+` FROM transcripts
		WHERE asset_id=? ORDER BY rowid DESC LIMIT 1`, assetID,
	))
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
