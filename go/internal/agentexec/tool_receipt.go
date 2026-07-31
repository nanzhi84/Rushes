package agentexec

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func timelineMutationReceipt(
	ctx context.Context,
	draftID string,
	before timelineMutationBase,
	after timeline.Document,
	result rushestools.ToolResult,
) (*reducer.AgentToolReceiptRow, error) {
	turnID, sourceMessageID := rushestools.TurnIdentity(ctx)
	toolCallID := strings.TrimSpace(rushestools.ToolCallID(ctx))
	toolName, fingerprint := rushestools.InvocationFingerprint(ctx)
	if turnID == "" || sourceMessageID == "" || toolCallID == "" ||
		toolName == "" || fingerprint == "" {
		return nil, nil
	}
	if !strings.HasPrefix(toolName, "timeline.") {
		return nil, fmt.Errorf("timeline receipt 工具名无效: %s", toolName)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("编码 timeline receipt 结果: %w", err)
	}
	keyDigest := sha256.Sum256([]byte(turnID + "\x00" + toolCallID))
	var beforeTimelineID *string
	if before.timelineID != "" {
		value := before.timelineID
		beforeTimelineID = &value
	}
	return &reducer.AgentToolReceiptRow{
		InvocationKey:       fmt.Sprintf("sha256:%x", keyDigest[:]),
		DraftID:             draftID,
		SourceMessageID:     sourceMessageID,
		TurnID:              turnID,
		ToolCallID:          toolCallID,
		ToolName:            toolName,
		ArgumentFingerprint: fingerprint,
		BeforeTimelineID:    beforeTimelineID,
		BeforeVersion:       before.timelineVersion,
		AfterTimelineID:     after.TimelineID,
		AfterVersion:        after.Version,
		TerminalStatus:      result.Status,
		ResultJSON:          string(encoded),
	}, nil
}
