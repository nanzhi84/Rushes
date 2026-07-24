package tools

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/nanzhi84/Rushes/go/internal/storage"
)

var PreconditionRegistry = map[string]struct{}{
	"usable_asset_exists": {},
	"timeline_absent":     {},
	"timeline_exists":     {},
	"any_preview_exists":  {},
}

func EvaluatePrecondition(
	ctx context.Context,
	database *storage.DB,
	draftID, predicate string,
) (bool, error) {
	if _, registered := PreconditionRegistry[predicate]; !registered {
		return false, fmt.Errorf("未知工具前置条件: %s", predicate)
	}
	var value int
	var err error
	switch predicate {
	case "usable_asset_exists":
		err = database.Read().QueryRowContext(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM assets a JOIN draft_asset_links l ON l.asset_id=a.asset_id
				WHERE l.draft_id=? AND a.usable=1
			)`, draftID).Scan(&value)
	case "timeline_exists":
		err = database.Read().QueryRowContext(ctx,
			"SELECT timeline_current_version IS NOT NULL FROM drafts WHERE draft_id=?", draftID).Scan(&value)
	case "timeline_absent":
		err = database.Read().QueryRowContext(ctx,
			"SELECT timeline_current_version IS NULL FROM drafts WHERE draft_id=?", draftID).Scan(&value)
	case "any_preview_exists":
		err = database.Read().QueryRowContext(ctx,
			"SELECT EXISTS(SELECT 1 FROM previews WHERE draft_id=?)", draftID).Scan(&value)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return value != 0, err
}
