package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

var ErrNotFound = errors.New("记录不存在")

type Querier interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type Draft struct {
	ID                     string
	Name                   string
	StateVersion           int
	Status                 string
	Defaults               map[string]any
	PendingDecisionID      *string
	RunningJobs            []map[string]any
	LastError              map[string]any
	Brief                  map[string]any
	ContentPlan            map[string]any
	TimelineCurrentVersion *int
	TimelineValidated      bool
	PreviewCurrentID       *string
	LastViewedPreviewID    *string
	ExportCurrentID        *string
	ScratchMemory          map[string]any
	MessagesTailRef        *string
	CreatedAt              string
	UpdatedAt              string
}

const draftColumns = `
draft_id, name, state_version, status, defaults_json, pending_decision_id,
running_jobs_json, last_error_json, brief_json, content_plan_json,
timeline_current_version, timeline_validated, preview_current_id,
last_viewed_preview_id, export_current_id, scratch_memory_json,
messages_tail_ref, created_at, updated_at`

func GetDraft(ctx context.Context, query Querier, draftID string) (Draft, error) {
	row := query.QueryRowContext(ctx, "SELECT "+draftColumns+" FROM drafts WHERE draft_id=?", draftID)
	return scanDraft(row)
}

type rowScanner interface{ Scan(...any) error }

func scanDraft(row rowScanner) (Draft, error) {
	var draft Draft
	var defaults, runningJobs, brief, scratch string
	var pendingDecision, lastError, contentPlan, previewID, lastViewed, exportID, tail sql.NullString
	var timelineVersion sql.NullInt64
	var timelineValidated int
	if err := row.Scan(
		&draft.ID, &draft.Name, &draft.StateVersion, &draft.Status, &defaults, &pendingDecision,
		&runningJobs, &lastError, &brief, &contentPlan, &timelineVersion, &timelineValidated,
		&previewID, &lastViewed, &exportID, &scratch, &tail, &draft.CreatedAt, &draft.UpdatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Draft{}, ErrNotFound
		}
		return Draft{}, err
	}
	draft.Defaults = decodeMap(defaults)
	draft.RunningJobs = decodeMapSlice(runningJobs)
	draft.LastError = decodeNullMap(lastError)
	draft.Brief = decodeMap(brief)
	draft.ContentPlan = decodeNullMap(contentPlan)
	draft.ScratchMemory = decodeMap(scratch)
	draft.PendingDecisionID = stringPointer(pendingDecision)
	draft.PreviewCurrentID = stringPointer(previewID)
	draft.LastViewedPreviewID = stringPointer(lastViewed)
	draft.ExportCurrentID = stringPointer(exportID)
	draft.MessagesTailRef = stringPointer(tail)
	if timelineVersion.Valid {
		value := int(timelineVersion.Int64)
		draft.TimelineCurrentVersion = &value
	}
	draft.TimelineValidated = timelineValidated != 0
	return draft, nil
}

func ListDrafts(ctx context.Context, query Querier) ([]Draft, error) {
	rows, err := query.QueryContext(ctx,
		"SELECT "+draftColumns+" FROM drafts WHERE status != 'trashed' ORDER BY updated_at DESC, draft_id",
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var drafts []Draft
	for rows.Next() {
		draft, err := scanDraft(rows)
		if err != nil {
			return nil, err
		}
		drafts = append(drafts, draft)
	}
	return drafts, rows.Err()
}

func DraftAssetIDs(ctx context.Context, query Querier, draftID string, limit int) ([]string, error) {
	rows, err := query.QueryContext(ctx, `
		SELECT asset_id FROM draft_asset_links
		WHERE draft_id=? ORDER BY linked_at, asset_id LIMIT ?`, draftID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func DraftMaterialCount(ctx context.Context, query Querier, draftID string) (int, error) {
	var count int
	err := query.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM draft_asset_links WHERE draft_id=?", draftID,
	).Scan(&count)
	return count, err
}

type EventRow struct {
	ID           int64
	Type         string
	Actor        string
	DraftID      *string
	PayloadJSON  []byte
	StateVersion *int
	CreatedAt    string
}

func ListEventsAfter(
	ctx context.Context,
	query Querier,
	after int64,
	draftID *string,
	limit int,
) ([]EventRow, error) {
	statement := `
		SELECT event_id, event_type, actor, draft_id, payload_json, state_version, created_at
		FROM event_log WHERE event_id > ?`
	arguments := []any{after}
	if draftID != nil {
		statement += " AND draft_id = ?"
		arguments = append(arguments, *draftID)
	}
	statement += " ORDER BY event_id LIMIT ?"
	arguments = append(arguments, limit)
	rows, err := query.QueryContext(ctx, statement, arguments...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var events []EventRow
	for rows.Next() {
		var event EventRow
		var draft sql.NullString
		var version sql.NullInt64
		if err := rows.Scan(
			&event.ID, &event.Type, &event.Actor, &draft, &event.PayloadJSON, &version, &event.CreatedAt,
		); err != nil {
			return nil, err
		}
		event.DraftID = stringPointer(draft)
		if version.Valid {
			value := int(version.Int64)
			event.StateVersion = &value
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func decodeMap(raw string) map[string]any {
	result := map[string]any{}
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &result)
	}
	return result
}

func decodeNullMap(raw sql.NullString) map[string]any {
	if !raw.Valid || raw.String == "" {
		return nil
	}
	return decodeMap(raw.String)
}

func decodeMapSlice(raw string) []map[string]any {
	var result []map[string]any
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &result)
	}
	if result == nil {
		result = []map[string]any{}
	}
	return result
}

func stringPointer(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}
