package storage

import (
	"context"
	"database/sql"
	"encoding/json"
)

// TimelineEditBatch 是提交后的有序语义操作摘要。before/after version 和
// affected refs 让模型无需猜测某次编辑作用在哪个已提交版本上。
type TimelineEditBatch struct {
	ID            string
	Sequence      int64
	DraftID       string
	Actor         string
	Origin        string
	Operations    []map[string]any
	BeforeVersion int
	AfterVersion  int
	AffectedRefs  []string
	CreatedAt     string
}

func ListTimelineEditBatches(
	ctx context.Context,
	query Querier,
	draftID string,
	limit int,
) ([]TimelineEditBatch, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := query.QueryContext(ctx, `
		SELECT edit_batch_id,rowid,draft_id,actor,origin,operations_json,
			before_version,after_version,affected_refs_json,created_at FROM (
			SELECT edit_batch_id,draft_id,actor,origin,operations_json,
				before_version,after_version,affected_refs_json,created_at,rowid
			FROM timeline_edit_batches WHERE draft_id=?
			ORDER BY rowid DESC LIMIT ?
		) ORDER BY rowid`, draftID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []TimelineEditBatch{}
	for rows.Next() {
		var batch TimelineEditBatch
		var raw, rawAffectedRefs string
		var beforeVersion, afterVersion sql.NullInt64
		if err := rows.Scan(
			&batch.ID, &batch.Sequence, &batch.DraftID, &batch.Actor, &batch.Origin, &raw,
			&beforeVersion, &afterVersion, &rawAffectedRefs, &batch.CreatedAt,
		); err != nil {
			return nil, err
		}
		if beforeVersion.Valid {
			batch.BeforeVersion = int(beforeVersion.Int64)
		}
		if afterVersion.Valid {
			batch.AfterVersion = int(afterVersion.Int64)
		}
		if err := json.Unmarshal([]byte(raw), &batch.Operations); err != nil {
			return nil, err
		}
		if batch.Operations == nil {
			batch.Operations = []map[string]any{}
		}
		if err := json.Unmarshal([]byte(rawAffectedRefs), &batch.AffectedRefs); err != nil {
			return nil, err
		}
		if batch.AffectedRefs == nil {
			batch.AffectedRefs = []string{}
		}
		result = append(result, batch)
	}
	return result, rows.Err()
}
