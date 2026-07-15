package agent

import (
	"context"
	"sync"

	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

type turnInteractionContextKey struct{}

type turnInteractionState struct {
	executionMu      sync.Mutex
	mu               sync.Mutex
	createdDecisions map[string]struct{}
	blockingDecision string
}

func newTurnInteractionState() *turnInteractionState {
	return &turnInteractionState{createdDecisions: map[string]struct{}{}}
}

func withTurnInteractionState(ctx context.Context, state *turnInteractionState) context.Context {
	return context.WithValue(ctx, turnInteractionContextKey{}, state)
}

func interactionStateFromContext(ctx context.Context) *turnInteractionState {
	state, _ := ctx.Value(turnInteractionContextKey{}).(*turnInteractionState)
	return state
}

func markDecisionCreatedThisTurn(ctx context.Context, decisionID string, blocking bool) {
	state := interactionStateFromContext(ctx)
	if state == nil || decisionID == "" {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	state.createdDecisions[decisionID] = struct{}{}
	if blocking {
		state.blockingDecision = decisionID
	}
}

// beginTurnToolCall serializes tool execution within one model turn. This makes
// a blocking decision an actual barrier even when one assistant message emits
// multiple tool calls that the graph runner would otherwise execute in parallel.
func beginTurnToolCall(ctx context.Context) (func(), string) {
	state := interactionStateFromContext(ctx)
	if state == nil {
		return func() {}, ""
	}
	state.executionMu.Lock()
	state.mu.Lock()
	decisionID := state.blockingDecision
	state.mu.Unlock()
	return state.executionMu.Unlock, decisionID
}

func decisionCreatedThisTurn(ctx context.Context, decisionID string) bool {
	state := interactionStateFromContext(ctx)
	if state == nil || decisionID == "" {
		return false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	_, exists := state.createdDecisions[decisionID]
	return exists
}

func nullableToolCallID(ctx context.Context) any {
	if toolCallID := rushestools.ToolCallID(ctx); toolCallID != "" {
		return toolCallID
	}
	return nil
}

func normalizeDecisionType(value string) string {
	switch value {
	case "approve_content_plan", "approve_speech_cut", "approve_rough_cut":
		return value
	default:
		return "generic"
	}
}
