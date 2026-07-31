package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrTimelineLockedByAgent means another live Agent turn owns the draft's
	// timeline. Manual writes must fail fast and must not wait for that turn.
	ErrTimelineLockedByAgent = errors.New("timeline_locked_by_agent")
	// ErrAgentEditLeaseLost means the caller no longer owns the exact persisted
	// turn/token pair. The current turn must stop; it must never silently reacquire.
	ErrAgentEditLeaseLost = errors.New("agent_edit_lease_lost")
)

const agentEditLeaseTimeLayout = "2006-01-02T15:04:05.000000000Z"

type AgentEditLease struct {
	DraftID     string
	TurnID      string
	LeaseToken  string
	AcquiredAt  time.Time
	HeartbeatAt time.Time
	ExpiresAt   time.Time
}

func (lease AgentEditLease) LiveAt(now time.Time) bool {
	return lease.DraftID != "" && lease.ExpiresAt.After(now.UTC())
}

func formatAgentEditLeaseTime(value time.Time) string {
	return value.UTC().Format(agentEditLeaseTimeLayout)
}

func parseAgentEditLeaseTime(value string) (time.Time, error) {
	parsed, err := time.Parse(agentEditLeaseTimeLayout, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("解析 edit lease 时间 %q: %w", value, err)
	}
	return parsed, nil
}

func scanAgentEditLease(scanner rowScanner) (AgentEditLease, error) {
	var lease AgentEditLease
	var acquiredAt, heartbeatAt, expiresAt string
	if err := scanner.Scan(
		&lease.DraftID, &lease.TurnID, &lease.LeaseToken,
		&acquiredAt, &heartbeatAt, &expiresAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AgentEditLease{}, ErrNotFound
		}
		return AgentEditLease{}, err
	}
	var err error
	if lease.AcquiredAt, err = parseAgentEditLeaseTime(acquiredAt); err != nil {
		return AgentEditLease{}, err
	}
	if lease.HeartbeatAt, err = parseAgentEditLeaseTime(heartbeatAt); err != nil {
		return AgentEditLease{}, err
	}
	if lease.ExpiresAt, err = parseAgentEditLeaseTime(expiresAt); err != nil {
		return AgentEditLease{}, err
	}
	return lease, nil
}

func GetAgentEditLease(
	ctx context.Context,
	query Querier,
	draftID string,
) (AgentEditLease, error) {
	return scanAgentEditLease(query.QueryRowContext(ctx, `
		SELECT draft_id,turn_id,lease_token,acquired_at,heartbeat_at,expires_at
		FROM agent_edit_leases WHERE draft_id=?`, draftID))
}

func GetLiveAgentEditLease(
	ctx context.Context,
	query Querier,
	draftID string,
	now time.Time,
) (AgentEditLease, error) {
	return scanAgentEditLease(query.QueryRowContext(ctx, `
		SELECT draft_id,turn_id,lease_token,acquired_at,heartbeat_at,expires_at
		FROM agent_edit_leases WHERE draft_id=? AND expires_at>?`,
		draftID, formatAgentEditLeaseTime(now)))
}
