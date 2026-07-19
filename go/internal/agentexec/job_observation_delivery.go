package agentexec

import (
	"context"
	"sync"

	"github.com/nanzhi84/Rushes/go/internal/reducer"
)

type jobObservationDeliveryContextKey struct{}

type jobObservationDeliveryState struct {
	mu        sync.Mutex
	row       reducer.AgentJobObservationDeliveryRow
	delivered bool
}

// WithJobObservationDelivery 让阻塞决策工具可以把 DecisionCreated 与 job observation
// 交付标记放进同一个 reducer 事务，消除“决策已提交、job 仍会被 bridge 重放”的窗口。
func WithJobObservationDelivery(ctx context.Context, jobID, claimToken string) context.Context {
	if jobID == "" || claimToken == "" {
		return ctx
	}
	return context.WithValue(ctx, jobObservationDeliveryContextKey{}, &jobObservationDeliveryState{
		row: reducer.AgentJobObservationDeliveryRow{JobID: jobID, ClaimToken: claimToken},
	})
}

// PendingJobObservationDelivery 返回尚未提交的交付行副本；没有 job 上下文或已提交时为 nil。
func PendingJobObservationDelivery(ctx context.Context) *reducer.AgentJobObservationDeliveryRow {
	row, _ := JobObservationDelivery(ctx)
	return row
}

// JobObservationDelivery 同时返回“是否存在受跟踪的 job 上下文”，让回合收尾区分
// “没有状态”与“已在 DecisionCreated 事务中交付（因此 row=nil）”。
func JobObservationDelivery(
	ctx context.Context,
) (*reducer.AgentJobObservationDeliveryRow, bool) {
	state, _ := ctx.Value(jobObservationDeliveryContextKey{}).(*jobObservationDeliveryState)
	if state == nil {
		return nil, false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.delivered {
		return nil, true
	}
	row := state.row
	return &row, true
}

// MarkJobObservationDelivered 只在包含 delivery 的 reducer 事务成功后调用。
func MarkJobObservationDelivered(ctx context.Context) {
	state, _ := ctx.Value(jobObservationDeliveryContextKey{}).(*jobObservationDeliveryState)
	if state == nil {
		return
	}
	state.mu.Lock()
	state.delivered = true
	state.mu.Unlock()
}
