package reducer

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

func seedUserMemoryMessage(t *testing.T, database *storage.DB, draftID, messageID string) {
	t.Helper()
	result, err := Apply(t.Context(), database, nil, Options{
		Actor: contracts.ActorUser,
		ResultRows: ResultRows{Message: &MessageRow{
			ID: messageID, DraftID: draftID, Role: "user", Kind: "user", Content: "请记住我的偏好",
		}},
	})
	if err != nil || result.Status != StatusApplied {
		t.Fatalf("seed message result=%#v err=%v", result, err)
	}
}

func TestUserMemoryResultRowsUpsertRemoveAndEvictDeterministically(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	createDraft(t, database, "draft_memory")
	seedUserMemoryMessage(t, database, "draft_memory", "message_memory")

	base := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	upserts := make([]UserMemoryRow, 0, storage.UserMemoryLimit+1)
	for index := 0; index <= storage.UserMemoryLimit; index++ {
		confirmedAt := base.Add(time.Duration(index) * time.Second)
		if index == 1 {
			confirmedAt = base.Add(100 * time.Millisecond)
		}
		upserts = append(upserts, UserMemoryRow{
			Key: fmt.Sprintf("memory_%02d", index), Kind: "preference",
			Statement:    fmt.Sprintf("用户偏好编号 %02d", index),
			EvidenceKind: storage.UserMemoryEvidenceMessage,
			EvidenceID:   "message_memory", SourceDraftID: "draft_memory",
			LastConfirmedAt: confirmedAt.Format(time.RFC3339Nano),
		})
	}
	result, err := Apply(t.Context(), database, nil, Options{
		Actor: contracts.ActorAgent, CreatedAt: base,
		ResultRows: ResultRows{UserMemoryUpserts: upserts},
	})
	if err != nil || result.Status != StatusApplied || result.UserMemory == nil {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if result.UserMemory.Total != storage.UserMemoryLimit ||
		len(result.UserMemory.EvictedKeys) != 1 || result.UserMemory.EvictedKeys[0] != "memory_00" {
		t.Fatalf("outcome=%#v", result.UserMemory)
	}
	memories, err := storage.ListUserMemories(t.Context(), database.Read())
	if err != nil || len(memories) != storage.UserMemoryLimit ||
		memories[0].Key != "memory_50" || memories[len(memories)-1].Key != "memory_01" {
		t.Fatalf("memories=%#v len=%d err=%v", memories, len(memories), err)
	}
	if memories[len(memories)-1].LastConfirmedAt != "2026-07-16T12:00:00.100000000Z" {
		t.Fatalf("fractional timestamp was not canonicalized: %#v", memories[len(memories)-1])
	}
	originalCreatedAt := memories[len(memories)-1].CreatedAt

	updatedAt := base.Add(2 * time.Hour).Format(time.RFC3339Nano)
	wantUpdatedAt := base.Add(2 * time.Hour).Format(userMemoryTimestampFormat)
	update, err := Apply(t.Context(), database, nil, Options{
		Actor: contracts.ActorAgent,
		ResultRows: ResultRows{
			UserMemoryUpserts: []UserMemoryRow{{
				Key: "memory_01", Kind: "correction", Statement: "用户纠正后的长期偏好",
				EvidenceKind: storage.UserMemoryEvidenceMessage,
				EvidenceID:   "message_memory", SourceDraftID: "draft_memory",
				CreatedAt: updatedAt, LastConfirmedAt: updatedAt,
			}},
			UserMemoryRemoveKeys: []string{"memory_02", "memory_missing"},
		},
	})
	if err != nil || update.UserMemory == nil || update.UserMemory.Total != storage.UserMemoryLimit-1 ||
		len(update.UserMemory.RemovedKeys) != 1 || update.UserMemory.RemovedKeys[0] != "memory_02" {
		t.Fatalf("update=%#v err=%v", update, err)
	}
	memories, err = storage.ListUserMemories(t.Context(), database.Read())
	if err != nil || len(memories) == 0 || memories[0].Key != "memory_01" ||
		memories[0].CreatedAt != originalCreatedAt || memories[0].LastConfirmedAt != wantUpdatedAt ||
		memories[0].Kind != "correction" {
		t.Fatalf("updated memory=%#v err=%v", memories[0], err)
	}

	removeKeys := make([]string, 0, len(memories))
	for _, memory := range memories {
		removeKeys = append(removeKeys, memory.Key)
	}
	tieUpserts := make([]UserMemoryRow, 0, storage.UserMemoryLimit+1)
	for index := 0; index <= storage.UserMemoryLimit; index++ {
		tieUpserts = append(tieUpserts, UserMemoryRow{
			Key: fmt.Sprintf("tie_%02d", index), Kind: "habit", Statement: "相同确认时间",
			EvidenceKind: storage.UserMemoryEvidenceMessage,
			EvidenceID:   "message_memory", SourceDraftID: "draft_memory",
			LastConfirmedAt: base.Format(time.RFC3339Nano),
		})
	}
	tied, err := Apply(t.Context(), database, nil, Options{
		Actor: contracts.ActorAgent,
		ResultRows: ResultRows{
			UserMemoryRemoveKeys: removeKeys, UserMemoryUpserts: tieUpserts,
		},
	})
	if err != nil || tied.UserMemory == nil || tied.UserMemory.Total != storage.UserMemoryLimit ||
		len(tied.UserMemory.EvictedKeys) != 1 || tied.UserMemory.EvictedKeys[0] != "tie_00" {
		t.Fatalf("tied outcome=%#v err=%v", tied.UserMemory, err)
	}
	memories, err = storage.ListUserMemories(t.Context(), database.Read())
	if err != nil || len(memories) != storage.UserMemoryLimit ||
		memories[0].Key != "tie_01" || memories[len(memories)-1].Key != "tie_50" {
		t.Fatalf("tied memories=%#v err=%v", memories, err)
	}
}

func TestUserMemoryEvidenceValidationCoversMessagesDecisionsAndRemoval(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	createDraft(t, database, "draft_memory_evidence")
	seedUserMemoryMessage(t, database, "draft_memory_evidence", "message_valid")

	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO decisions(
			decision_id,scope_type,draft_id,type,question,options_json,
			allow_free_text,status,answer_json,blocking
		) VALUES('decision_valid','draft','draft_memory_evidence','critical','节奏？','[]',
			1,'answered','{"free_text":"快一点"}',1)`); err != nil {
		t.Fatal(err)
	}
	decisionResult, err := Apply(t.Context(), database, nil, Options{
		Actor: contracts.ActorAgent,
		ResultRows: ResultRows{UserMemoryUpserts: []UserMemoryRow{{
			Key: "pacing", Kind: "preference", Statement: "成片节奏偏快",
			EvidenceKind: storage.UserMemoryEvidenceDecision,
			EvidenceID:   "decision_valid", SourceDraftID: "draft_memory_evidence",
		}}},
	})
	if err != nil || decisionResult.Status != StatusApplied {
		t.Fatalf("decision result=%#v err=%v", decisionResult, err)
	}

	_, err = Apply(t.Context(), database, nil, Options{
		Actor: contracts.ActorAgent,
		ResultRows: ResultRows{UserMemoryUpserts: []UserMemoryRow{{
			Key: "forged", Kind: "habit", Statement: "不应写入",
			EvidenceKind: storage.UserMemoryEvidenceMessage,
			EvidenceID:   "missing", SourceDraftID: "draft_memory_evidence",
		}}},
	})
	if !errors.Is(err, ErrUserMemoryEvidence) {
		t.Fatalf("forged evidence err=%v", err)
	}

	_, err = Apply(t.Context(), database, nil, Options{
		Actor: contracts.ActorAgent,
		ResultRows: ResultRows{
			UserMemoryRemoveKeys: []string{"pacing"},
			UserMemoryMutationEvidence: &UserMemoryEvidenceRow{
				Kind: storage.UserMemoryEvidenceMessage,
				ID:   "missing", SourceDraftID: "draft_memory_evidence",
			},
		},
	})
	if !errors.Is(err, ErrUserMemoryEvidence) {
		t.Fatalf("remove forged evidence err=%v", err)
	}
	memories, err := storage.ListUserMemories(t.Context(), database.Read())
	if err != nil || len(memories) != 1 || memories[0].Key != "pacing" {
		t.Fatalf("memories=%#v err=%v", memories, err)
	}
	if _, err := database.Write().ExecContext(
		t.Context(), "DELETE FROM drafts WHERE draft_id='draft_memory_evidence'",
	); err != nil {
		t.Fatal(err)
	}
	memories, err = storage.ListUserMemories(t.Context(), database.Read())
	if err != nil || len(memories) != 1 || memories[0].Key != "pacing" {
		t.Fatalf("memory should survive source draft cleanup: memories=%#v err=%v", memories, err)
	}
}

func TestUserMemoryAndMessageRollbackTogetherAfterAppendFailure(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	createDraft(t, database, "draft_memory_atomic")

	draft, err := storage.GetDraft(t.Context(), database.Read(), "draft_memory_atomic")
	if err != nil {
		t.Fatal(err)
	}
	_, err = Apply(t.Context(), database, []contracts.Event{{
		Type: "DraftRenamed", DraftID: draft.ID, BaseVersion: &draft.StateVersion,
		Payload: map[string]any{"name": "不应提交", "bad": make(chan int)},
	}}, Options{
		Actor: contracts.ActorUser,
		ResultRows: ResultRows{
			Message: &MessageRow{
				ID: "message_atomic", DraftID: draft.ID, Role: "user", Kind: "user", Content: "记住快节奏",
			},
			UserMemoryUpserts: []UserMemoryRow{{
				Key: "pacing", Kind: "preference", Statement: "成片节奏偏快",
				EvidenceKind: storage.UserMemoryEvidenceMessage,
				EvidenceID:   "message_atomic", SourceDraftID: draft.ID,
			}},
		},
	})
	if err == nil {
		t.Fatal("appendEvents 编码失败应回滚整个 reducer 事务")
	}
	for _, table := range []string{"messages", "user_memories"} {
		var count int
		if err := database.Read().QueryRowContext(t.Context(),
			"SELECT COUNT(*) FROM "+table,
		).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s count=%d err=%v", table, count, err)
		}
	}
}

func TestUserMemoryReducerRejectsAmbiguousMutations(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	createDraft(t, database, "draft_memory_invalid")
	seedUserMemoryMessage(t, database, "draft_memory_invalid", "message_one")
	seedUserMemoryMessage(t, database, "draft_memory_invalid", "message_two")
	valid := UserMemoryRow{
		Key: "pacing", Kind: "preference", Statement: "成片节奏偏快",
		EvidenceKind: storage.UserMemoryEvidenceMessage,
		EvidenceID:   "message_one", SourceDraftID: "draft_memory_invalid",
	}
	cases := []struct {
		name string
		rows ResultRows
		want error
	}{
		{
			name: "invalid upsert fields",
			rows: ResultRows{UserMemoryUpserts: []UserMemoryRow{{
				Key: "Bad-Key", Kind: valid.Kind, Statement: valid.Statement,
				EvidenceKind: valid.EvidenceKind, EvidenceID: valid.EvidenceID,
				SourceDraftID: valid.SourceDraftID,
			}}},
			want: ErrUserMemoryInput,
		},
		{
			name: "duplicate upsert",
			rows: ResultRows{UserMemoryUpserts: []UserMemoryRow{valid, valid}},
			want: ErrUserMemoryInput,
		},
		{
			name: "mutation evidence mismatch",
			rows: ResultRows{
				UserMemoryUpserts: []UserMemoryRow{{
					Key: valid.Key, Kind: valid.Kind, Statement: valid.Statement,
					EvidenceKind: valid.EvidenceKind, EvidenceID: "message_two",
					SourceDraftID: valid.SourceDraftID,
				}},
				UserMemoryMutationEvidence: &UserMemoryEvidenceRow{
					Kind: valid.EvidenceKind, ID: valid.EvidenceID, SourceDraftID: valid.SourceDraftID,
				},
			},
			want: ErrUserMemoryEvidence,
		},
		{
			name: "invalid remove key",
			rows: ResultRows{UserMemoryRemoveKeys: []string{"Bad-Key"}},
			want: ErrUserMemoryInput,
		},
		{
			name: "duplicate remove key",
			rows: ResultRows{UserMemoryRemoveKeys: []string{"pacing", "pacing"}},
			want: ErrUserMemoryInput,
		},
		{
			name: "upsert remove overlap",
			rows: ResultRows{
				UserMemoryUpserts: []UserMemoryRow{valid}, UserMemoryRemoveKeys: []string{"pacing"},
			},
			want: ErrUserMemoryInput,
		},
		{
			name: "incomplete mutation evidence",
			rows: ResultRows{
				UserMemoryRemoveKeys: []string{"pacing"},
				UserMemoryMutationEvidence: &UserMemoryEvidenceRow{
					Kind: "invalid", ID: "message_one", SourceDraftID: "draft_memory_invalid",
				},
			},
			want: ErrUserMemoryEvidence,
		},
		{
			name: "invalid timestamp",
			rows: ResultRows{UserMemoryUpserts: []UserMemoryRow{{
				Key: valid.Key, Kind: valid.Kind, Statement: valid.Statement,
				EvidenceKind: valid.EvidenceKind, EvidenceID: valid.EvidenceID,
				SourceDraftID: valid.SourceDraftID, LastConfirmedAt: "not-a-time",
			}}},
			want: ErrUserMemoryInput,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := Apply(t.Context(), database, nil, Options{
				Actor: contracts.ActorAgent, ResultRows: test.rows,
			})
			if !errors.Is(err, test.want) {
				t.Fatalf("err=%v want=%v", err, test.want)
			}
		})
	}
}
