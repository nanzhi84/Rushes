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
	return document, nil
}

func Latest(ctx context.Context, database *storage.DB, draftID string) (Document, error) {
	var version sql.NullInt64
	err := database.Read().QueryRowContext(ctx,
		"SELECT timeline_current_version FROM drafts WHERE draft_id=?", draftID).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) || !version.Valid {
		return Document{}, storage.ErrNotFound
	}
	if err != nil {
		return Document{}, err
	}
	return Get(ctx, database, draftID, int(version.Int64))
}

func NextVersion(ctx context.Context, database *storage.DB, draftID string) (int, error) {
	var version int
	err := database.Read().QueryRowContext(ctx, `
		SELECT COALESCE(MAX(version),0)+1 FROM timeline_versions WHERE draft_id=?`, draftID).Scan(&version)
	return version, err
}

type VersionNavigation struct {
	Parent *int
	Redo   *int
	Latest int
}

func Navigation(
	ctx context.Context,
	database *storage.DB,
	draftID string,
	version int,
) (VersionNavigation, error) {
	var parent sql.NullInt64
	if err := database.Read().QueryRowContext(ctx, `
		SELECT parent_version FROM timeline_versions WHERE draft_id=? AND version=?`,
		draftID, version,
	).Scan(&parent); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return VersionNavigation{}, storage.ErrNotFound
		}
		return VersionNavigation{}, err
	}
	var latest int
	if err := database.Read().QueryRowContext(ctx, `
		SELECT COALESCE(MAX(version),0) FROM timeline_versions WHERE draft_id=?`,
		draftID,
	).Scan(&latest); err != nil {
		return VersionNavigation{}, err
	}
	var redo sql.NullInt64
	if err := database.Read().QueryRowContext(ctx, `
		SELECT version FROM timeline_versions
		WHERE draft_id=? AND parent_version=?
		ORDER BY version DESC LIMIT 1`,
		draftID, version,
	).Scan(&redo); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return VersionNavigation{}, err
	}
	result := VersionNavigation{Latest: latest}
	if parent.Valid {
		value := int(parent.Int64)
		result.Parent = &value
	}
	if redo.Valid {
		value := int(redo.Int64)
		result.Redo = &value
	}
	return result, nil
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
