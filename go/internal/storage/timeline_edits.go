package storage

import (
	"context"
	"encoding/json"
)

// TimelineEditBatch 是提交后的语义操作摘要，不包含时间线快照或版本号。
// 它只用于让下一次 Agent 回合理解最近的人类/Agent 编辑意图。
type TimelineEditBatch struct {
	ID         string
	DraftID    string
	Actor      string
	Origin     string
	Operations []map[string]any
	CreatedAt  string
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
		SELECT edit_batch_id,draft_id,actor,origin,operations_json,created_at FROM (
			SELECT edit_batch_id,draft_id,actor,origin,operations_json,created_at,rowid
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
		var raw string
		if err := rows.Scan(
			&batch.ID, &batch.DraftID, &batch.Actor, &batch.Origin, &raw, &batch.CreatedAt,
		); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &batch.Operations); err != nil {
			return nil, err
		}
		if batch.Operations == nil {
			batch.Operations = []map[string]any{}
		}
		result = append(result, batch)
	}
	return result, rows.Err()
}
