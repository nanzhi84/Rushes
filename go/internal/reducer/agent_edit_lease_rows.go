package reducer

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/storage"
)

type AgentEditLeaseOperation string

const (
	AgentEditLeaseAcquire     AgentEditLeaseOperation = "acquire"
	AgentEditLeaseRenew       AgentEditLeaseOperation = "renew"
	AgentEditLeaseRelease     AgentEditLeaseOperation = "release"
	AgentEditLeaseExpireStale AgentEditLeaseOperation = "expire_stale"
)

type AgentEditLeaseMutation struct {
	Operation  AgentEditLeaseOperation
	DraftID    string
	TurnID     string
	LeaseToken string
	Now        time.Time
	TTL        time.Duration
}

type AgentEditLeaseOutcome struct {
	Lease        *storage.AgentEditLease
	Released     bool
	ExpiredCount int64
}

const reducerAgentEditLeaseTimeLayout = "2006-01-02T15:04:05.000000000Z"

func editLeaseTimestamp(value time.Time) string {
	return value.UTC().Format(reducerAgentEditLeaseTimeLayout)
}

func persistAgentEditLeaseMutation(
	ctx context.Context,
	tx *sql.Tx,
	mutation AgentEditLeaseMutation,
) (AgentEditLeaseOutcome, error) {
	now := mutation.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	switch mutation.Operation {
	case AgentEditLeaseAcquire:
		if strings.TrimSpace(mutation.DraftID) == "" ||
			strings.TrimSpace(mutation.TurnID) == "" ||
			strings.TrimSpace(mutation.LeaseToken) == "" || mutation.TTL <= 0 {
			return AgentEditLeaseOutcome{}, errors.New("edit lease acquire 字段不完整")
		}
		expiresAt := now.Add(mutation.TTL)
		result, err := tx.ExecContext(ctx, `
			INSERT INTO agent_edit_leases(
				draft_id,turn_id,lease_token,acquired_at,heartbeat_at,expires_at
			) VALUES(?,?,?,?,?,?)
			ON CONFLICT(draft_id) DO UPDATE SET
				turn_id=excluded.turn_id,
				lease_token=excluded.lease_token,
				acquired_at=excluded.acquired_at,
				heartbeat_at=excluded.heartbeat_at,
				expires_at=excluded.expires_at
			WHERE agent_edit_leases.expires_at<=excluded.acquired_at`,
			mutation.DraftID, mutation.TurnID, mutation.LeaseToken,
			editLeaseTimestamp(now), editLeaseTimestamp(now), editLeaseTimestamp(expiresAt),
		)
		if err != nil {
			return AgentEditLeaseOutcome{}, err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return AgentEditLeaseOutcome{}, err
		}
		if rows != 1 {
			return AgentEditLeaseOutcome{}, storage.ErrTimelineLockedByAgent
		}
		lease := storage.AgentEditLease{
			DraftID: mutation.DraftID, TurnID: mutation.TurnID,
			LeaseToken: mutation.LeaseToken, AcquiredAt: now,
			HeartbeatAt: now, ExpiresAt: expiresAt,
		}
		return AgentEditLeaseOutcome{Lease: &lease}, nil

	case AgentEditLeaseRenew:
		if strings.TrimSpace(mutation.DraftID) == "" ||
			strings.TrimSpace(mutation.TurnID) == "" ||
			strings.TrimSpace(mutation.LeaseToken) == "" || mutation.TTL <= 0 {
			return AgentEditLeaseOutcome{}, errors.New("edit lease renew 字段不完整")
		}
		expiresAt := now.Add(mutation.TTL)
		result, err := tx.ExecContext(ctx, `
			UPDATE agent_edit_leases SET heartbeat_at=?,expires_at=?
			WHERE draft_id=? AND turn_id=? AND lease_token=? AND expires_at>?`,
			editLeaseTimestamp(now), editLeaseTimestamp(expiresAt), mutation.DraftID,
			mutation.TurnID, mutation.LeaseToken, editLeaseTimestamp(now),
		)
		if err != nil {
			return AgentEditLeaseOutcome{}, err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return AgentEditLeaseOutcome{}, err
		}
		if rows != 1 {
			return AgentEditLeaseOutcome{}, storage.ErrAgentEditLeaseLost
		}
		lease, err := storage.GetAgentEditLease(ctx, tx, mutation.DraftID)
		if err != nil {
			return AgentEditLeaseOutcome{}, err
		}
		if lease.TurnID != mutation.TurnID || lease.LeaseToken != mutation.LeaseToken {
			return AgentEditLeaseOutcome{}, storage.ErrAgentEditLeaseLost
		}
		return AgentEditLeaseOutcome{Lease: &lease}, nil

	case AgentEditLeaseRelease:
		if strings.TrimSpace(mutation.DraftID) == "" ||
			strings.TrimSpace(mutation.TurnID) == "" ||
			strings.TrimSpace(mutation.LeaseToken) == "" {
			return AgentEditLeaseOutcome{}, errors.New("edit lease release 字段不完整")
		}
		result, err := tx.ExecContext(ctx, `
			DELETE FROM agent_edit_leases
			WHERE draft_id=? AND turn_id=? AND lease_token=?`,
			mutation.DraftID, mutation.TurnID, mutation.LeaseToken,
		)
		if err != nil {
			return AgentEditLeaseOutcome{}, err
		}
		rows, err := result.RowsAffected()
		return AgentEditLeaseOutcome{Released: rows == 1}, err

	case AgentEditLeaseExpireStale:
		result, err := tx.ExecContext(ctx,
			"DELETE FROM agent_edit_leases WHERE expires_at<=?", editLeaseTimestamp(now),
		)
		if err != nil {
			return AgentEditLeaseOutcome{}, err
		}
		count, err := result.RowsAffected()
		return AgentEditLeaseOutcome{ExpiredCount: count}, err
	default:
		return AgentEditLeaseOutcome{}, errors.New("edit lease operation 无效")
	}
}
