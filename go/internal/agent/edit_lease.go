package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

const (
	agentEditLeaseTTL               = 30 * time.Second
	agentEditLeaseHeartbeatInterval = 10 * time.Second
	agentEditLeaseCleanupTimeout    = 3 * time.Second
)

type timelineEditLeaseSessionContextKey struct{}

type timelineEditLeaseSession struct {
	database *storage.DB
	draftID  string
	turnID   string
	token    string
	cancel   context.CancelCauseFunc

	mu              sync.Mutex
	acquired        bool
	lost            bool
	closed          bool
	heartbeatCancel context.CancelFunc
	closeDone       chan struct{}
}

func newTimelineEditLeaseSession(
	database *storage.DB,
	draftID, turnID string,
	cancel context.CancelCauseFunc,
) *timelineEditLeaseSession {
	return &timelineEditLeaseSession{
		database: database, draftID: draftID, turnID: turnID,
		token: agentexec.RandomID("lease"), cancel: cancel,
		closeDone: make(chan struct{}),
	}
}

func withTimelineEditLeaseSession(
	ctx context.Context,
	session *timelineEditLeaseSession,
) context.Context {
	return context.WithValue(ctx, timelineEditLeaseSessionContextKey{}, session)
}

func timelineEditLeaseSessionFromContext(ctx context.Context) *timelineEditLeaseSession {
	session, _ := ctx.Value(timelineEditLeaseSessionContextKey{}).(*timelineEditLeaseSession)
	return session
}

func (session *timelineEditLeaseSession) ensure(ctx context.Context) error {
	if session == nil {
		return nil
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed || session.lost {
		return storage.ErrAgentEditLeaseLost
	}
	if session.acquired {
		return nil
	}
	result, err := applyAgentEditLeaseMutation(ctx, session.database, reducer.AgentEditLeaseMutation{
		Operation: reducer.AgentEditLeaseAcquire,
		DraftID:   session.draftID, TurnID: session.turnID, LeaseToken: session.token,
		Now: time.Now().UTC(), TTL: agentEditLeaseTTL,
	})
	if err != nil {
		return err
	}
	if result.AgentEditLease == nil || result.AgentEditLease.Lease == nil {
		return errors.New("edit lease acquire 未返回持久化租约")
	}
	session.acquired = true
	metricEditLeaseAcquire.Inc()
	heartbeatCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	session.heartbeatCancel = cancel
	go session.heartbeat(heartbeatCtx)
	// A provider may ignore cancellation long enough for TurnQueue to abandon
	// its worker generation. Release ownership from a watcher as soon as the
	// turn context ends; the normal runTurn defer remains an idempotent fallback.
	go func(turnCtx context.Context) {
		select {
		case <-turnCtx.Done():
			session.close()
		case <-heartbeatCtx.Done():
		}
	}(ctx)
	return nil
}

func (session *timelineEditLeaseSession) heartbeat(ctx context.Context) {
	ticker := time.NewTicker(agentEditLeaseHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			renewCtx, cancel := context.WithTimeout(ctx, agentEditLeaseCleanupTimeout)
			_, err := applyAgentEditLeaseMutation(renewCtx, session.database, reducer.AgentEditLeaseMutation{
				Operation: reducer.AgentEditLeaseRenew,
				DraftID:   session.draftID, TurnID: session.turnID, LeaseToken: session.token,
				Now: time.Now().UTC(), TTL: agentEditLeaseTTL,
			})
			cancel()
			if err != nil {
				if ctx.Err() == nil {
					session.markLost(fmt.Errorf("%w: %v", storage.ErrAgentEditLeaseLost, err))
				}
				return
			}
			metricEditLeaseRenew.Inc()
		}
	}
}

func (session *timelineEditLeaseSession) markLost(err error) {
	if session == nil {
		return
	}
	session.mu.Lock()
	if session.closed || session.lost {
		session.mu.Unlock()
		return
	}
	session.lost = true
	metricEditLeaseLost.Inc()
	cancel := session.cancel
	session.mu.Unlock()
	if err == nil {
		err = storage.ErrAgentEditLeaseLost
	}
	cancel(err)
}

func (session *timelineEditLeaseSession) close() {
	if session == nil {
		return
	}
	session.mu.Lock()
	if session.closed {
		done := session.closeDone
		session.mu.Unlock()
		if done != nil {
			<-done
		}
		return
	}
	if session.closeDone == nil {
		session.closeDone = make(chan struct{})
	}
	done := session.closeDone
	session.closed = true
	acquired := session.acquired
	heartbeatCancel := session.heartbeatCancel
	session.mu.Unlock()
	defer close(done)
	if heartbeatCancel != nil {
		heartbeatCancel()
	}
	if !acquired {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), agentEditLeaseCleanupTimeout)
	defer cancel()
	result, _ := applyAgentEditLeaseMutation(ctx, session.database, reducer.AgentEditLeaseMutation{
		Operation: reducer.AgentEditLeaseRelease,
		DraftID:   session.draftID, TurnID: session.turnID, LeaseToken: session.token,
		Now: time.Now().UTC(),
	})
	if result.AgentEditLease != nil && result.AgentEditLease.Released {
		metricEditLeaseRelease.Inc()
	}
}

func (session *timelineEditLeaseSession) activeTurnID() string {
	if session == nil {
		return ""
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if !session.acquired || session.closed || session.lost {
		return ""
	}
	return session.turnID
}

func applyAgentEditLeaseMutation(
	ctx context.Context,
	database *storage.DB,
	mutation reducer.AgentEditLeaseMutation,
) (reducer.Result, error) {
	return reducer.Apply(ctx, database, nil, reducer.Options{
		Actor:      contracts.ActorAgent,
		ResultRows: reducer.ResultRows{AgentEditLeaseMutation: &mutation},
	})
}

func expireStaleAgentEditLeases(ctx context.Context, database *storage.DB) error {
	_, err := applyAgentEditLeaseMutation(ctx, database, reducer.AgentEditLeaseMutation{
		Operation: reducer.AgentEditLeaseExpireStale, Now: time.Now().UTC(),
	})
	return err
}

func toolRequiresTimelineEditLease(name string) bool {
	switch name {
	case "timeline.insert", "timeline.delete", "timeline.update", "timeline.split", "preview.generate":
		return true
	default:
		return false
	}
}

func specsRequireTimelineEditLease(specs []rushestools.Spec) bool {
	for _, spec := range specs {
		if toolRequiresTimelineEditLease(spec.Name) {
			return true
		}
	}
	return false
}
