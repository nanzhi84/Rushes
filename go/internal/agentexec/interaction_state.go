package agentexec

import (
	"context"
	"sync"

	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

type turnInteractionContextKey struct{}

type turnInteractionState struct {
	// executionMu 序列化工具执行:副作用工具独占 Lock,只读工具并发 RLock（#103 G3b）。
	// 写工具的相互顺序由 eino 串行节点保证,这把锁只负责互斥、不负责排序。
	executionMu      sync.RWMutex
	mu               sync.Mutex
	createdDecisions map[string]struct{}
	blockingDecision string
}

func NewTurnInteractionState() *turnInteractionState {
	return &turnInteractionState{createdDecisions: map[string]struct{}{}}
}

func WithTurnInteractionState(ctx context.Context, state *turnInteractionState) context.Context {
	return context.WithValue(ctx, turnInteractionContextKey{}, state)
}

func InteractionStateFromContext(ctx context.Context) *turnInteractionState {
	state, _ := ctx.Value(turnInteractionContextKey{}).(*turnInteractionState)
	return state
}

func MarkDecisionCreatedThisTurn(ctx context.Context, decisionID string, blocking bool) {
	state := InteractionStateFromContext(ctx)
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

func decisionCreatedThisTurn(ctx context.Context, decisionID string) bool {
	state := InteractionStateFromContext(ctx)
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

func NormalizeDecisionType(value string) string {
	switch value {
	case "critical", "approve_content_plan", "approve_speech_cut", "approve_rough_cut":
		return value
	default:
		return "generic"
	}
}

// BeginToolCall 取得本回合工具执行互斥,并返回释放函数与当前阻塞决策 ID。readOnly 为真时
// 取共享 RLock(只读工具可并发),否则取独占 Lock(副作用工具串行,#103 G3b)。屏障检查始终
// 进行:在 state.mu 下读 blockingDecision 供引擎短路。引擎侧装饰器 beginTurnToolCall 读取
// ctx 后调用它,把决策屏障语义留在引擎、状态内部字段留在领域包。
func (state *turnInteractionState) BeginToolCall(readOnly bool) (func(), string) {
	var release func()
	if readOnly {
		state.executionMu.RLock()
		release = state.executionMu.RUnlock
	} else {
		state.executionMu.Lock()
		release = state.executionMu.Unlock
	}
	state.mu.Lock()
	decisionID := state.blockingDecision
	state.mu.Unlock()
	return release, decisionID
}
