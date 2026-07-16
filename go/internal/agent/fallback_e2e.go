//go:build e2e_scaffold

package agent

import (
	"context"
	"errors"
	"strings"
	"time"

	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

const (
	e2eBlockUntilCancelMarker    = "E2E_BLOCK_UNTIL_CANCEL"
	e2eCancelUnderstandingMarker = "E2E_CANCEL_UNDERSTANDING"
	e2eFullMainlineMarker        = "E2E_FULL_MAINLINE"
)

type e2eFallbackScaffold struct {
	service *Service
}

func newFallbackScaffold(service *Service) fallbackScaffold {
	return &e2eFallbackScaffold{service: service}
}

func (scaffold *e2eFallbackScaffold) TryHandle(
	ctx context.Context,
	draftID, _ string,
	content string,
) (string, bool, error) {
	switch {
	case strings.Contains(content, e2eBlockUntilCancelMarker):
		<-ctx.Done()
		return "", true, ctx.Err()
	case strings.Contains(content, e2eCancelUnderstandingMarker):
		reply, err := scaffold.cancelDuringUnderstanding(ctx, draftID)
		return reply, true, err
	case strings.Contains(content, e2eFullMainlineMarker):
		reply, err := scaffold.service.fallbackFullMainline(ctx, draftID)
		return reply, true, err
	default:
		return "", false, nil
	}
}

func (scaffold *e2eFallbackScaffold) cancelDuringUnderstanding(
	ctx context.Context,
	draftID string,
) (reply string, resultErr error) {
	listed, err := scaffold.service.toolListAssets(
		ctx,
		draftID,
		rushestools.AssetListInput{OnlyUsable: boolPointer(true)},
	)
	if err != nil {
		return "", err
	}
	if len(listed.Assets) == 0 {
		return "", errors.New("E2E 素材理解取消脚手架缺少可用素材")
	}
	assetIDs := make([]string, 0, len(listed.Assets))
	for _, asset := range listed.Assets {
		assetIDs = append(assetIDs, asset.AssetID)
	}
	logicalInput := rushestools.UnderstandInput{
		AssetIDs: assetIDs, Depth: "scan", MaxStepsPerAsset: 8,
	}
	reporter := scaffold.service.toolReporter(ctx, draftID)
	reporter("understand.materials", "started", logicalInput, nil, nil)
	var output any
	defer func() {
		reporter("understand.materials", "finished", logicalInput, output, resultErr)
	}()
	output, resultErr = scaffold.service.ExecuteTool(ctx, "understand.materials", logicalInput)
	if resultErr != nil {
		return "", resultErr
	}
	// Keep the synthetic Agent turn alive while the real async job runs so E2E
	// can exercise both the per-job cancel action and whole-turn cancellation.
	select {
	case <-ctx.Done():
		resultErr = ctx.Err()
		return "", resultErr
	case <-time.After(30 * time.Second):
	}
	return "素材理解已完成。", nil
}
