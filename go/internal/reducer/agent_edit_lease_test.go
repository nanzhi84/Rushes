package reducer

import (
	"errors"
	"testing"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

func mutateTestLease(
	t *testing.T,
	database *storage.DB,
	mutation AgentEditLeaseMutation,
) (Result, error) {
	t.Helper()
	return Apply(t.Context(), database, nil, Options{
		Actor:      contracts.ActorAgent,
		ResultRows: ResultRows{AgentEditLeaseMutation: &mutation},
	})
}

func TestAgentEditLeaseCASLifecycle(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	createDraft(t, database, "draft-lease-cas")
	base := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)

	result, err := mutateTestLease(t, database, AgentEditLeaseMutation{
		Operation: AgentEditLeaseAcquire, DraftID: "draft-lease-cas",
		TurnID: "turn-a", LeaseToken: "token-a", Now: base, TTL: 30 * time.Second,
	})
	if err != nil || result.AgentEditLease == nil || result.AgentEditLease.Lease == nil ||
		result.AgentEditLease.Lease.TurnID != "turn-a" {
		t.Fatalf("acquire result=%#v err=%v", result, err)
	}
	if _, err := mutateTestLease(t, database, AgentEditLeaseMutation{
		Operation: AgentEditLeaseAcquire, DraftID: "draft-lease-cas",
		TurnID: "turn-b", LeaseToken: "token-b", Now: base.Add(time.Second), TTL: 30 * time.Second,
	}); !errors.Is(err, storage.ErrTimelineLockedByAgent) {
		t.Fatalf("live lease must reject another turn: %v", err)
	}
	if _, err := mutateTestLease(t, database, AgentEditLeaseMutation{
		Operation: AgentEditLeaseRenew, DraftID: "draft-lease-cas",
		TurnID: "turn-a", LeaseToken: "wrong", Now: base.Add(2 * time.Second), TTL: 30 * time.Second,
	}); !errors.Is(err, storage.ErrAgentEditLeaseLost) {
		t.Fatalf("wrong-token renewal must lose lease: %v", err)
	}
	renewed, err := mutateTestLease(t, database, AgentEditLeaseMutation{
		Operation: AgentEditLeaseRenew, DraftID: "draft-lease-cas",
		TurnID: "turn-a", LeaseToken: "token-a", Now: base.Add(3 * time.Second), TTL: 30 * time.Second,
	})
	if err != nil || renewed.AgentEditLease.Lease.ExpiresAt != base.Add(33*time.Second) {
		t.Fatalf("renew result=%#v err=%v", renewed, err)
	}
	wrongRelease, err := mutateTestLease(t, database, AgentEditLeaseMutation{
		Operation: AgentEditLeaseRelease, DraftID: "draft-lease-cas",
		TurnID: "turn-a", LeaseToken: "wrong", Now: base.Add(4 * time.Second),
	})
	if err != nil || wrongRelease.AgentEditLease.Released {
		t.Fatalf("wrong-token release changed owner: %#v err=%v", wrongRelease, err)
	}
	released, err := mutateTestLease(t, database, AgentEditLeaseMutation{
		Operation: AgentEditLeaseRelease, DraftID: "draft-lease-cas",
		TurnID: "turn-a", LeaseToken: "token-a", Now: base.Add(4 * time.Second),
	})
	if err != nil || !released.AgentEditLease.Released {
		t.Fatalf("exact release result=%#v err=%v", released, err)
	}
	if _, err := storage.GetAgentEditLease(t.Context(), database.Read(), "draft-lease-cas"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("released lease still exists: %v", err)
	}
}

func TestExpiredLeaseCanBeReplacedAndStartupCleanupKeepsLiveRows(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	createDraft(t, database, "draft-lease-expired")
	createDraft(t, database, "draft-lease-live")
	base := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	for _, fixture := range []struct {
		draft, turn, token string
		ttl                time.Duration
	}{
		{"draft-lease-expired", "turn-old", "token-old", 5 * time.Second},
		{"draft-lease-live", "turn-live", "token-live", time.Minute},
	} {
		if _, err := mutateTestLease(t, database, AgentEditLeaseMutation{
			Operation: AgentEditLeaseAcquire, DraftID: fixture.draft,
			TurnID: fixture.turn, LeaseToken: fixture.token, Now: base, TTL: fixture.ttl,
		}); err != nil {
			t.Fatal(err)
		}
	}
	cleanup, err := mutateTestLease(t, database, AgentEditLeaseMutation{
		Operation: AgentEditLeaseExpireStale, Now: base.Add(10 * time.Second),
	})
	if err != nil || cleanup.AgentEditLease.ExpiredCount != 1 {
		t.Fatalf("cleanup=%#v err=%v", cleanup, err)
	}
	if _, err := storage.GetAgentEditLease(t.Context(), database.Read(), "draft-lease-live"); err != nil {
		t.Fatalf("startup cleanup deleted live lease: %v", err)
	}
	replacement, err := mutateTestLease(t, database, AgentEditLeaseMutation{
		Operation: AgentEditLeaseAcquire, DraftID: "draft-lease-expired",
		TurnID: "turn-new", LeaseToken: "token-new", Now: base.Add(11 * time.Second), TTL: time.Minute,
	})
	if err != nil || replacement.AgentEditLease.Lease.TurnID != "turn-new" {
		t.Fatalf("expired replacement=%#v err=%v", replacement, err)
	}
}

func TestTimelineReducerAdmissionFencesManualAndWrongAgentWrites(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	createDraft(t, database, "draft-lease-admission")
	baseTime := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	if _, err := mutateTestLease(t, database, AgentEditLeaseMutation{
		Operation: AgentEditLeaseAcquire, DraftID: "draft-lease-admission",
		TurnID: "turn-owner", LeaseToken: "token-owner", Now: baseTime, TTL: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	timelineEvent := contracts.Event{
		Type: "TimelineVersionCreated", DraftID: "draft-lease-admission",
		Payload: map[string]any{
			"timeline_id": "draft-lease-admission:v1", "timeline_version": 1,
			"patch_id": "patch-1", "edit_origin": "agent",
			"edit_operations": []any{map[string]any{
				"kind": "move_clip", "timeline_clip_id": "clip-a", "target_frame": 10,
			}},
		},
	}
	baseVersion := 0
	if _, err := Apply(t.Context(), database, []contracts.Event{timelineEvent}, Options{
		Actor: contracts.ActorUser, BaseVersion: &baseVersion,
		TimelineWriteAdmission: &TimelineWriteAdmission{Origin: "manual", Now: baseTime.Add(time.Second)},
	}); !errors.Is(err, storage.ErrTimelineLockedByAgent) {
		t.Fatalf("manual write under lease must fail: %v", err)
	}
	if _, err := Apply(t.Context(), database, []contracts.Event{timelineEvent}, Options{
		Actor: contracts.ActorAgent, BaseVersion: &baseVersion,
		TimelineWriteAdmission: &TimelineWriteAdmission{
			Origin: "agent", TurnID: "turn-owner", LeaseToken: "wrong", Now: baseTime.Add(time.Second),
		},
	}); !errors.Is(err, storage.ErrAgentEditLeaseLost) {
		t.Fatalf("wrong-token agent write must fail: %v", err)
	}
	var versions, events, history int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT
			(SELECT COUNT(*) FROM timeline_versions WHERE draft_id=?),
			(SELECT COUNT(*) FROM event_log WHERE draft_id=? AND event_type='TimelineVersionCreated'),
			(SELECT COUNT(*) FROM timeline_edit_batches WHERE draft_id=?)`,
		"draft-lease-admission", "draft-lease-admission", "draft-lease-admission",
	).Scan(&versions, &events, &history); err != nil || versions != 0 || events != 0 || history != 0 {
		t.Fatalf("rejected writes leaked: versions=%d events=%d history=%d err=%v", versions, events, history, err)
	}

	result, err := Apply(t.Context(), database, []contracts.Event{timelineEvent}, Options{
		Actor: contracts.ActorAgent, BaseVersion: &baseVersion,
		TimelineWriteAdmission: &TimelineWriteAdmission{
			Origin: "agent", TurnID: "turn-owner", LeaseToken: "token-owner", Now: baseTime.Add(time.Second),
		},
	})
	if err != nil || result.Status != StatusApplied {
		t.Fatalf("owner write result=%#v err=%v", result, err)
	}
	batches, err := storage.ListTimelineEditBatches(t.Context(), database.Read(), "draft-lease-admission", 20)
	if err != nil || len(batches) != 1 || batches[0].BeforeVersion != 0 || batches[0].AfterVersion != 1 ||
		len(batches[0].AffectedRefs) == 0 || batches[0].AffectedRefs[0] != "timeline_clip_id:clip-a" {
		t.Fatalf("ordered history=%#v err=%v", batches, err)
	}

	if _, err := mutateTestLease(t, database, AgentEditLeaseMutation{
		Operation: AgentEditLeaseRelease, DraftID: "draft-lease-admission",
		TurnID: "turn-owner", LeaseToken: "token-owner", Now: baseTime.Add(2 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	draft, err := storage.GetDraft(t.Context(), database.Read(), "draft-lease-admission")
	if err != nil {
		t.Fatal(err)
	}
	second := timelineEvent
	second.Payload = map[string]any{
		"timeline_id": "draft-lease-admission:v2", "timeline_version": 2,
		"patch_id": "patch-2", "edit_origin": "manual",
		"edit_operations": []any{map[string]any{
			"kind": "delete_clip", "timeline_clip_id": "clip-a",
		}},
	}
	result, err = Apply(t.Context(), database, []contracts.Event{second}, Options{
		Actor: contracts.ActorUser, BaseVersion: &draft.StateVersion,
		TimelineWriteAdmission: &TimelineWriteAdmission{Origin: "manual", Now: baseTime.Add(3 * time.Second)},
	})
	if err != nil || result.Status != StatusApplied {
		t.Fatalf("manual write after release result=%#v err=%v", result, err)
	}
}

func TestTimelineReducerRejectsAgentPointerWritesWithoutOwnedLease(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	const draftID = "draft-agent-admission-required"
	createDraft(t, database, draftID)
	event := contracts.Event{
		Type: "TimelineVersionCreated", DraftID: draftID,
		Payload: map[string]any{
			"timeline_id": draftID + ":v1", "timeline_version": 1,
			"patch_id": "patch-agent-admission-required",
		},
	}
	baseVersion := 0
	for _, test := range []struct {
		name      string
		admission *TimelineWriteAdmission
	}{
		{name: "missing admission"},
		{name: "agent admission without lease", admission: &TimelineWriteAdmission{
			Origin: "agent", TurnID: "turn-missing", LeaseToken: "token-missing",
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Apply(t.Context(), database, []contracts.Event{event}, Options{
				Actor: contracts.ActorAgent, BaseVersion: &baseVersion,
				TimelineWriteAdmission: test.admission,
			}); !errors.Is(err, storage.ErrAgentEditLeaseLost) {
				t.Fatalf("unowned Agent write err=%v", err)
			}
		})
	}
	var versions, events, history int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT
			(SELECT COUNT(*) FROM timeline_versions WHERE draft_id=?),
			(SELECT COUNT(*) FROM event_log WHERE draft_id=? AND event_type='TimelineVersionCreated'),
			(SELECT COUNT(*) FROM timeline_edit_batches WHERE draft_id=?)`,
		draftID, draftID, draftID,
	).Scan(&versions, &events, &history); err != nil || versions != 0 || events != 0 || history != 0 {
		t.Fatalf("rejected Agent writes leaked: versions=%d events=%d history=%d err=%v", versions, events, history, err)
	}
}

func TestTimelineRestoreAdmissionChecksLiveLeaseBeforeRestoreLookup(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	createDraft(t, database, "draft-lease-restore")
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	if _, err := mutateTestLease(t, database, AgentEditLeaseMutation{
		Operation: AgentEditLeaseAcquire, DraftID: "draft-lease-restore",
		TurnID: "turn-owner", LeaseToken: "token-owner", Now: now, TTL: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	baseVersion := 0
	_, err := Apply(t.Context(), database, []contracts.Event{{
		Type: "TimelineVersionRestored", DraftID: "draft-lease-restore",
		Payload: map[string]any{
			"checkpoint_id": "missing", "restore_checkpoint_id": "restore",
			"mode": "timeline", "timeline_version": 1,
		},
	}}, Options{
		Actor: contracts.ActorUser, BaseVersion: &baseVersion,
		TimelineWriteAdmission: &TimelineWriteAdmission{Origin: "manual", Now: now.Add(time.Second)},
	})
	if !errors.Is(err, storage.ErrTimelineLockedByAgent) {
		t.Fatalf("restore pointer write bypassed live lease: %v", err)
	}
}
