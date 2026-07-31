package agentexec

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func manualTimelineMutationContext(ctx context.Context) context.Context {
	return rushestools.WithTimelineMutationOrigin(ctx, "manual")
}

func seedTimelineVersion(
	exec *Executor,
	ctx context.Context,
	draftID string,
	document timeline.Document,
	operation string,
	editOperation map[string]any,
) (rushestools.ToolResult, error) {
	ctx = manualTimelineMutationContext(ctx)
	var stateVersion int
	if err := exec.database.Read().QueryRowContext(
		ctx, "SELECT state_version FROM drafts WHERE draft_id=?", draftID,
	).Scan(&stateVersion); errors.Is(err, sql.ErrNoRows) {
		return rushestools.ToolResult{}, storage.ErrNotFound
	} else if err != nil {
		return rushestools.ToolResult{}, err
	}
	baseVersion := document.Version - 1
	baseTimelineID := ""
	if baseVersion > 0 {
		baseTimelineID = fmt.Sprintf("%s:v%d", draftID, baseVersion)
	}
	return exec.persistTimelineFromSnapshot(
		ctx,
		draftID,
		document,
		operation,
		editOperation,
		timelineMutationBase{
			stateVersion:    stateVersion,
			timelineVersion: baseVersion,
			timelineID:      baseTimelineID,
		},
	)
}
