package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/telemetry"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

var metricToolReceiptReused = telemetry.NewCounter("agent_tool_receipt_reused_total")

// prepareTimelineMutationReceipt either restores an already committed terminal
// result or enriches the invocation context so the mutation and its receipt can
// be committed atomically by the reducer.
func (service *Service) prepareTimelineMutationReceipt(
	ctx context.Context,
	name string,
	input any,
) (context.Context, any, bool, error) {
	if !isTerminalTimelineMutation(name) {
		return ctx, nil, false, nil
	}
	turnID, sourceMessageID := rushestools.TurnIdentity(ctx)
	toolCallID := rushestools.ToolCallID(ctx)
	draftID, _ := rushestools.DraftID(ctx)
	if turnID == "" || sourceMessageID == "" || toolCallID == "" {
		return ctx, nil, false, nil
	}
	fingerprint, err := canonicalToolInputFingerprint(name, input)
	if err != nil {
		return ctx, nil, false, err
	}
	receipt, lookupErr := storage.GetAgentToolReceipt(
		ctx, service.database.Read(), turnID, toolCallID,
	)
	if lookupErr == nil {
		if receipt.DraftID != draftID || receipt.SourceMessageID != sourceMessageID || receipt.ToolName != name ||
			receipt.ArgumentFingerprint != fingerprint {
			return ctx, nil, false, errors.New("tool receipt 与当前调用不匹配，已拒绝重复执行")
		}
		var result rushestools.ToolResult
		if err := json.Unmarshal([]byte(receipt.ResultJSON), &result); err != nil {
			return ctx, nil, false, fmt.Errorf("读取 tool receipt 终态结果: %w", err)
		}
		metricToolReceiptReused.Inc()
		return ctx, result, true, nil
	}
	if !errors.Is(lookupErr, storage.ErrNotFound) {
		return ctx, nil, false, lookupErr
	}
	return rushestools.WithArgumentFingerprint(ctx, name, fingerprint), nil, false, nil
}

func canonicalToolInputFingerprint(name string, input any) (string, error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return "", fmt.Errorf("计算工具参数指纹: %w", err)
	}
	digest := sha256.Sum256(append(append([]byte(name), 0), encoded...))
	return fmt.Sprintf("sha256:%x", digest[:]), nil
}
