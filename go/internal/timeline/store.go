package timeline

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/nanzhi84/Rushes/go/internal/storage"
)

func Get(
	ctx context.Context,
	database *storage.DB,
	draftID string,
	version int,
) (Document, error) {
	var raw string
	err := database.Read().QueryRowContext(ctx, `
		SELECT document_json FROM timeline_versions WHERE draft_id=? AND version=?`,
		draftID, version).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Document{}, storage.ErrNotFound
	}
	if err != nil {
		return Document{}, err
	}
	var document Document
	if err := json.Unmarshal([]byte(raw), &document); err != nil {
		return Document{}, err
	}
	ensureRequiredTracks(&document)
	return document, nil
}

// 迁移前创建的时间线按读取时补齐后来新增的空轨，避免 schema 演进使现有草稿失效。
func ensureRequiredTracks(document *Document) {
	existing := make(map[string]struct{}, len(document.Tracks))
	for _, track := range document.Tracks {
		existing[track.TrackID] = struct{}{}
	}
	for _, required := range requiredTracks {
		if _, found := existing[required.ID]; found {
			continue
		}
		document.Tracks = append(document.Tracks, Track{
			TrackID: required.ID, TrackType: required.Type, Clips: []Clip{},
		})
	}
}

func Latest(ctx context.Context, database *storage.DB, draftID string) (Document, error) {
	var version sql.NullInt64
	err := database.Read().QueryRowContext(ctx,
		"SELECT timeline_current_version FROM drafts WHERE draft_id=?", draftID).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return Document{}, storage.ErrNotFound
	}
	if err != nil {
		return Document{}, err
	}
	if !version.Valid {
		return Document{}, storage.ErrNotFound
	}
	return Get(ctx, database, draftID, int(version.Int64))
}

func NextVersion(ctx context.Context, database *storage.DB, draftID string) (int, error) {
	var version int
	err := database.Read().QueryRowContext(ctx, `
		SELECT COALESCE(timeline_current_version,0)+1 FROM drafts WHERE draft_id=?`, draftID).Scan(&version)
	return version, err
}

func LatestPreviewID(
	ctx context.Context,
	database *storage.DB,
	draftID string,
	version int,
) (*string, error) {
	var previewID string
	err := database.Read().QueryRowContext(ctx, `
		SELECT preview_id FROM previews WHERE draft_id=? AND timeline_version=?
		ORDER BY created_at DESC,preview_id DESC LIMIT 1`, draftID, version).Scan(&previewID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &previewID, err
}
