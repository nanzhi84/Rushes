package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

type Asset struct {
	ID                  string
	StorageMode         string
	ObjectHash          *string
	ReferencePath       *string
	Kind                string
	Source              string
	Filename            string
	Hash                string
	MTime               *int64
	Size                int64
	Probe               map[string]any
	ProxyObjectHash     *string
	ThumbnailObjectHash *string
	IngestStatus        string
	UnderstandingStatus string
	Usable              bool
	Failure             map[string]any
	RelDir              *string
}

const assetColumns = `
a.asset_id, a.storage_mode, a.object_hash, a.reference_path, a.kind, a.source,
a.filename, a.hash, a.mtime, a.size, a.probe_json, a.proxy_object_hash,
a.thumbnail_object_hash, a.ingest_status, a.understanding_status, a.usable,
a.failure_json`

func GetAsset(ctx context.Context, query Querier, assetID string) (Asset, error) {
	row := query.QueryRowContext(ctx, "SELECT "+assetColumns+" FROM assets a WHERE a.asset_id=?", assetID)
	return scanAsset(row, nil)
}

func ListDraftAssets(ctx context.Context, query Querier, draftID string) ([]Asset, error) {
	rows, err := query.QueryContext(ctx, `
		SELECT `+assetColumns+`, l.rel_dir
		FROM assets a JOIN draft_asset_links l ON l.asset_id=a.asset_id
		WHERE l.draft_id=? ORDER BY l.linked_at, a.asset_id`, draftID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	assets := []Asset{}
	for rows.Next() {
		var relDir sql.NullString
		asset, err := scanAsset(rows, &relDir)
		if err != nil {
			return nil, err
		}
		asset.RelDir = stringPointer(relDir)
		assets = append(assets, asset)
	}
	return assets, rows.Err()
}

func scanAsset(row rowScanner, relDir *sql.NullString) (Asset, error) {
	var asset Asset
	var objectHash, referencePath, proxyHash, thumbnailHash sql.NullString
	var mtimeInteger sql.NullInt64
	var probe, failure sql.NullString
	var usable int
	destinations := []any{
		&asset.ID, &asset.StorageMode, &objectHash, &referencePath, &asset.Kind, &asset.Source,
		&asset.Filename, &asset.Hash, &mtimeInteger, &asset.Size, &probe, &proxyHash,
		&thumbnailHash, &asset.IngestStatus, &asset.UnderstandingStatus, &usable, &failure,
	}
	if relDir != nil {
		destinations = append(destinations, relDir)
	}
	if err := row.Scan(destinations...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Asset{}, ErrNotFound
		}
		return Asset{}, err
	}
	asset.ObjectHash = stringPointer(objectHash)
	asset.ReferencePath = stringPointer(referencePath)
	asset.ProxyObjectHash = stringPointer(proxyHash)
	asset.ThumbnailObjectHash = stringPointer(thumbnailHash)
	if mtimeInteger.Valid {
		value := mtimeInteger.Int64
		asset.MTime = &value
	}
	asset.Probe = decodeNullMap(probe)
	asset.Failure = decodeNullMap(failure)
	asset.Usable = usable != 0
	return asset, nil
}

type JobSummary struct {
	ID       string
	Kind     string
	Status   string
	Progress *float64
	Error    map[string]any
}

func ListAssetJobs(ctx context.Context, query Querier, assetID string) ([]JobSummary, error) {
	rows, err := query.QueryContext(ctx, `
		SELECT job_id, kind, status, progress, error_json
		FROM jobs WHERE asset_id=? ORDER BY created_at, job_id`, assetID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	jobs := []JobSummary{}
	for rows.Next() {
		var job JobSummary
		var progress sql.NullFloat64
		var errorJSON sql.NullString
		if err := rows.Scan(&job.ID, &job.Kind, &job.Status, &progress, &errorJSON); err != nil {
			return nil, err
		}
		if progress.Valid {
			value := progress.Float64
			job.Progress = &value
		}
		job.Error = decodeNullMap(errorJSON)
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func MaterialSummaryByFingerprint(
	ctx context.Context,
	query Querier,
	assetID, fingerprint string,
) (map[string]any, error) {
	var raw string
	err := query.QueryRowContext(ctx, `
		SELECT summary_json FROM material_summaries
		WHERE asset_id=? AND fingerprint=? AND status='ready'
		ORDER BY version DESC LIMIT 1`, assetID, fingerprint).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(raw), &summary); err != nil {
		return nil, err
	}
	return summary, nil
}

// BestMaterialSummary is quality-monotonic: a later focused or shallow run is
// retained, but cannot hide an older summary with richer semantic evidence.
func BestMaterialSummary(ctx context.Context, query Querier, assetID string) (map[string]any, error) {
	rows, err := query.QueryContext(ctx, `
		SELECT summary_json, version FROM material_summaries
		WHERE asset_id=? AND status='ready'
		ORDER BY version`, assetID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var best map[string]any
	bestScore, bestVersion := -1, -1
	for rows.Next() {
		var raw string
		var version int
		if err := rows.Scan(&raw, &version); err != nil {
			return nil, err
		}
		var summary map[string]any
		if err := json.Unmarshal([]byte(raw), &summary); err != nil {
			continue
		}
		score := materialSummaryQualityScore(summary)
		if score > bestScore || score == bestScore && version > bestVersion {
			best, bestScore, bestVersion = summary, score, version
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if best == nil {
		return nil, ErrNotFound
	}
	return best, nil
}

func materialSummaryQualityScore(summary map[string]any) int {
	segments, _ := summary["segments"].([]any)
	semanticSegments, details := 0, 0
	for _, value := range segments {
		segment, _ := value.(map[string]any)
		description, _ := segment["description"].(string)
		description = strings.TrimSpace(description)
		if description != "" && !strings.Contains(description, "待理解") &&
			!strings.HasPrefix(description, "视频素材，时长约") {
			semanticSegments++
		}
		for _, field := range []string{"tags", "subjects", "actions", "setting", "lighting", "mood", "edit_hints"} {
			if values, ok := segment[field].([]any); ok {
				details += len(values)
			}
		}
	}
	verified := 0
	if value, ok := summary["verified_cut_count"].(float64); ok {
		verified = int(value)
	}
	depthBonus := 0
	if depth, _ := summary["analysis_depth"].(string); depth == "deep" {
		depthBonus = 100
	}
	return semanticSegments*1_000_000 + len(segments)*10_000 + verified*1_000 + details*10 + depthBonus
}

func ObjectPathByHash(paths Paths, hash *string) (string, error) {
	if hash == nil || *hash == "" {
		return "", ErrNotFound
	}
	return paths.ObjectPath(*hash)
}
