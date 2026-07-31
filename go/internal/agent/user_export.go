package agent

import (
	"context"

	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/telemetry"
)

// CreateUserExport 是最终成片的用户动作入口。它直达领域执行器，不经过 Tool
// Registry、模型工具 trace 或 ReAct 回合。
func (service *Service) CreateUserExport(
	ctx context.Context,
	draftID, timelineID, orientation string,
) (agentexec.UserExportRecord, error) {
	record, err := service.executor.CreateUserExport(ctx, draftID, timelineID, orientation)
	if err == nil {
		telemetry.RecordUserExportRequested()
	}
	return record, err
}

func (service *Service) ListUserExports(
	ctx context.Context,
	draftID string,
) ([]agentexec.UserExportRecord, error) {
	return service.executor.ListUserExports(ctx, draftID)
}

func (service *Service) RetryUserExport(
	ctx context.Context,
	draftID, jobID string,
) (agentexec.UserExportRecord, error) {
	record, err := service.executor.RetryUserExport(ctx, draftID, jobID)
	if err == nil {
		telemetry.RecordUserExportRequested()
	}
	return record, err
}
