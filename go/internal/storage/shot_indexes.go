package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

type ShotIndexSnapshot struct {
	ID                  string
	AssetContentHash    string
	Generation          int
	AnalyzerVersion     string
	OutputSchemaVersion int
	SourceAssetID       *string
	Status              string
	Summary             map[string]any
	Failure             map[string]any
	CreatedAt           string
	PublishedAt         *string
}

type IndexedShot struct {
	SnapshotID           string
	ShotID               string
	AssetContentHash     string
	SourceStartFrame     int
	SourceEndFrame       int
	BoundaryVersion      int
	BoundaryKind         string
	BoundaryConfidence   *float64
	LineageParentShotID  *string
	RepresentativeFrames []map[string]any
	SemanticName         string
	Description          string
	Tags                 []string
	Subjects             []string
	Actions              []string
	Setting              []string
	ShotScale            string
	Composition          string
	Lighting             []string
	Mood                 []string
	EditHints            []string
	Quality              map[string]any
	SearchText           string
	SearchTokens         []string
	DeepCoverage         []string
}

func ReadyShotIndexByContentHash(
	ctx context.Context,
	query Querier,
	contentHash string,
) (ShotIndexSnapshot, error) {
	return scanShotIndexSnapshot(query.QueryRowContext(ctx, `
		SELECT index_snapshot_id,asset_content_hash,generation,analyzer_version,
			output_schema_version,source_asset_id,status,summary_json,failure_json,
			created_at,published_at
		FROM shot_index_snapshots
		WHERE asset_content_hash=? AND status='ready'
		ORDER BY generation DESC LIMIT 1`, contentHash))
}

func ShotIndexSnapshotByID(
	ctx context.Context,
	query Querier,
	snapshotID string,
) (ShotIndexSnapshot, error) {
	return scanShotIndexSnapshot(query.QueryRowContext(ctx, `
		SELECT index_snapshot_id,asset_content_hash,generation,analyzer_version,
			output_schema_version,source_asset_id,status,summary_json,failure_json,
			created_at,published_at
		FROM shot_index_snapshots WHERE index_snapshot_id=?`, snapshotID))
}

func scanShotIndexSnapshot(row rowScanner) (ShotIndexSnapshot, error) {
	var snapshot ShotIndexSnapshot
	var sourceAssetID, failureJSON, publishedAt sql.NullString
	var summaryJSON string
	if err := row.Scan(
		&snapshot.ID, &snapshot.AssetContentHash, &snapshot.Generation,
		&snapshot.AnalyzerVersion, &snapshot.OutputSchemaVersion, &sourceAssetID,
		&snapshot.Status, &summaryJSON, &failureJSON, &snapshot.CreatedAt, &publishedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ShotIndexSnapshot{}, ErrNotFound
		}
		return ShotIndexSnapshot{}, err
	}
	snapshot.SourceAssetID = stringPointer(sourceAssetID)
	snapshot.PublishedAt = stringPointer(publishedAt)
	if err := json.Unmarshal([]byte(summaryJSON), &snapshot.Summary); err != nil {
		return ShotIndexSnapshot{}, err
	}
	snapshot.Failure = decodeNullMap(failureJSON)
	return snapshot, nil
}

func ListShotIndexShots(
	ctx context.Context,
	query Querier,
	snapshotID string,
) ([]IndexedShot, error) {
	rows, err := query.QueryContext(ctx, `
		SELECT index_snapshot_id,shot_id,asset_content_hash,source_start_frame,
			source_end_frame,boundary_version,boundary_kind,boundary_confidence,
			lineage_parent_shot_id,representative_frames_json,semantic_name,description,tags_json,
			subjects_json,actions_json,setting_json,shot_scale,composition,lighting_json,
			mood_json,edit_hints_json,quality_json,search_text,search_tokens_json,
			deep_coverage_json
		FROM shots WHERE index_snapshot_id=?
		ORDER BY source_start_frame,source_end_frame,shot_id`, snapshotID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []IndexedShot{}
	for rows.Next() {
		var shot IndexedShot
		var confidence sql.NullFloat64
		var parent sql.NullString
		var framesJSON, tagsJSON, subjectsJSON, actionsJSON, settingJSON string
		var lightingJSON, moodJSON, hintsJSON, qualityJSON, tokensJSON, coverageJSON string
		if err := rows.Scan(
			&shot.SnapshotID, &shot.ShotID, &shot.AssetContentHash,
			&shot.SourceStartFrame, &shot.SourceEndFrame, &shot.BoundaryVersion,
			&shot.BoundaryKind, &confidence, &parent, &framesJSON, &shot.SemanticName, &shot.Description,
			&tagsJSON, &subjectsJSON, &actionsJSON, &settingJSON, &shot.ShotScale,
			&shot.Composition, &lightingJSON, &moodJSON, &hintsJSON, &qualityJSON,
			&shot.SearchText, &tokensJSON, &coverageJSON,
		); err != nil {
			return nil, err
		}
		if confidence.Valid {
			value := confidence.Float64
			shot.BoundaryConfidence = &value
		}
		shot.LineageParentShotID = stringPointer(parent)
		if err := decodeShotJSON(framesJSON, &shot.RepresentativeFrames); err != nil {
			return nil, err
		}
		for _, value := range []struct {
			raw         string
			destination *[]string
		}{
			{tagsJSON, &shot.Tags}, {subjectsJSON, &shot.Subjects},
			{actionsJSON, &shot.Actions}, {settingJSON, &shot.Setting},
			{lightingJSON, &shot.Lighting}, {moodJSON, &shot.Mood},
			{hintsJSON, &shot.EditHints}, {tokensJSON, &shot.SearchTokens},
			{coverageJSON, &shot.DeepCoverage},
		} {
			if err := decodeShotJSON(value.raw, value.destination); err != nil {
				return nil, err
			}
		}
		if err := decodeShotJSON(qualityJSON, &shot.Quality); err != nil {
			return nil, err
		}
		result = append(result, shot)
	}
	return result, rows.Err()
}

type DraftIndexedShot struct {
	AssetID  string
	Filename string
	Shot     IndexedShot
}

// ListReadyIndexedShotsForDraft returns every ready base-index shot in one query
// so the timeline polling endpoint can annotate clips without an N+1 query loop.
func ListReadyIndexedShotsForDraft(
	ctx context.Context,
	query Querier,
	draftID string,
) ([]DraftIndexedShot, error) {
	rows, err := query.QueryContext(ctx, `
		SELECT a.asset_id,a.filename,s.index_snapshot_id,s.shot_id,s.asset_content_hash,
			s.source_start_frame,s.source_end_frame,s.boundary_version,s.boundary_kind,
			s.boundary_confidence,s.lineage_parent_shot_id,s.representative_frames_json,
			s.semantic_name,s.description,s.tags_json,s.subjects_json,s.actions_json,
			s.setting_json,s.shot_scale,s.composition,s.lighting_json,s.mood_json,
			s.edit_hints_json,s.quality_json,s.search_text,s.search_tokens_json,
			s.deep_coverage_json
		FROM draft_asset_links l
		JOIN assets a ON a.asset_id=l.asset_id
		JOIN shot_index_snapshots i ON i.asset_content_hash=a.hash AND i.status='ready'
		JOIN shots s ON s.index_snapshot_id=i.index_snapshot_id
		WHERE l.draft_id=?
		ORDER BY a.asset_id,s.source_start_frame,s.source_end_frame,s.shot_id`, draftID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []DraftIndexedShot{}
	for rows.Next() {
		var item DraftIndexedShot
		var shot IndexedShot
		var confidence sql.NullFloat64
		var parent sql.NullString
		var framesJSON, tagsJSON, subjectsJSON, actionsJSON, settingJSON string
		var lightingJSON, moodJSON, hintsJSON, qualityJSON, tokensJSON, coverageJSON string
		if err := rows.Scan(
			&item.AssetID, &item.Filename, &shot.SnapshotID, &shot.ShotID,
			&shot.AssetContentHash, &shot.SourceStartFrame, &shot.SourceEndFrame,
			&shot.BoundaryVersion, &shot.BoundaryKind, &confidence, &parent, &framesJSON,
			&shot.SemanticName, &shot.Description, &tagsJSON, &subjectsJSON, &actionsJSON,
			&settingJSON, &shot.ShotScale, &shot.Composition, &lightingJSON, &moodJSON,
			&hintsJSON, &qualityJSON, &shot.SearchText, &tokensJSON, &coverageJSON,
		); err != nil {
			return nil, err
		}
		if confidence.Valid {
			value := confidence.Float64
			shot.BoundaryConfidence = &value
		}
		shot.LineageParentShotID = stringPointer(parent)
		if err := decodeShotJSON(framesJSON, &shot.RepresentativeFrames); err != nil {
			return nil, err
		}
		for _, value := range []struct {
			raw         string
			destination *[]string
		}{
			{tagsJSON, &shot.Tags}, {subjectsJSON, &shot.Subjects},
			{actionsJSON, &shot.Actions}, {settingJSON, &shot.Setting},
			{lightingJSON, &shot.Lighting}, {moodJSON, &shot.Mood},
			{hintsJSON, &shot.EditHints}, {tokensJSON, &shot.SearchTokens},
			{coverageJSON, &shot.DeepCoverage},
		} {
			if err := decodeShotJSON(value.raw, value.destination); err != nil {
				return nil, err
			}
		}
		if err := decodeShotJSON(qualityJSON, &shot.Quality); err != nil {
			return nil, err
		}
		item.Shot = shot
		result = append(result, item)
	}
	return result, rows.Err()
}

func decodeShotJSON(raw string, destination any) error {
	if err := json.Unmarshal([]byte(raw), destination); err != nil {
		return err
	}
	return nil
}
