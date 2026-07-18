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
			EvidenceKind: storage.UserMemoryEvidenceMessage, EvidenceQuote: "偏好",
			EvidenceID: "message_memory", SourceDraftID: "draft_memory",
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
				EvidenceKind: storage.UserMemoryEvidenceMessage, EvidenceQuote: "偏好",
				EvidenceID: "message_memory", SourceDraftID: "draft_memory",
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
			EvidenceKind: storage.UserMemoryEvidenceMessage, EvidenceQuote: "偏好",
			EvidenceID: "message_memory", SourceDraftID: "draft_memory",
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
			EvidenceKind: storage.UserMemoryEvidenceDecision, EvidenceQuote: "快一点",
			EvidenceID: "decision_valid", SourceDraftID: "draft_memory_evidence",
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
				EvidenceKind: storage.UserMemoryEvidenceMessage, EvidenceQuote: "快节奏",
				EvidenceID: "message_atomic", SourceDraftID: draft.ID,
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
			name: "clear with upsert",
			rows: ResultRows{
				UserMemoryClearAll: true, UserMemoryUpserts: []UserMemoryRow{valid},
			},
			want: ErrUserMemoryInput,
		},
		{
			name: "clear with remove",
			rows: ResultRows{
				UserMemoryClearAll: true, UserMemoryRemoveKeys: []string{"pacing"},
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
			name: "invalid created timestamp",
			rows: ResultRows{UserMemoryUpserts: []UserMemoryRow{{
				Key: valid.Key, Kind: valid.Kind, Statement: valid.Statement,
				EvidenceKind: valid.EvidenceKind, EvidenceID: valid.EvidenceID,
				SourceDraftID: valid.SourceDraftID, CreatedAt: "not-a-time",
			}}},
			want: ErrUserMemoryInput,
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

func TestUserMemoryDatabaseFailuresRollBackMutations(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	createDraft(t, database, "draft_memory_failure")
	seedUserMemoryMessage(t, database, "draft_memory_failure", "message_failure")
	seed := UserMemoryRow{
		Key: "pacing", Kind: "preference", Statement: "成片节奏偏快",
		EvidenceKind: storage.UserMemoryEvidenceMessage, EvidenceQuote: "偏好",
		EvidenceID: "message_failure", SourceDraftID: "draft_memory_failure",
	}
	if result, err := Apply(t.Context(), database, nil, Options{
		Actor: contracts.ActorAgent, ResultRows: ResultRows{UserMemoryUpserts: []UserMemoryRow{seed}},
	}); err != nil || result.Status != StatusApplied {
		t.Fatalf("seed result=%#v err=%v", result, err)
	}
	if _, err := database.Write().ExecContext(t.Context(), `
		CREATE TRIGGER block_user_memory_delete BEFORE DELETE ON user_memories
		BEGIN SELECT RAISE(ABORT, 'blocked delete'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(t.Context(), database, nil, Options{
		Actor: contracts.ActorUser, ResultRows: ResultRows{
			Message: &MessageRow{
				ID: "message_before_failed_clear", DraftID: "draft_memory_failure",
				Role: "assistant", Kind: "reply", Content: "不应提交",
			},
			UserMemoryClearAll: true,
		},
	}); err == nil {
		t.Fatal("clear-all trigger failure should roll back")
	}
	var messageCount int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM messages WHERE message_id='message_before_failed_clear'`,
	).Scan(&messageCount); err != nil || messageCount != 0 {
		t.Fatalf("failed clear message count=%d err=%v", messageCount, err)
	}
	if _, err := database.Write().ExecContext(t.Context(), `
		DROP TRIGGER block_user_memory_delete;
		CREATE TRIGGER block_user_memory_insert BEFORE INSERT ON user_memories
		WHEN NEW.memory_key = 'blocked_insert'
		BEGIN SELECT RAISE(ABORT, 'blocked insert'); END`); err != nil {
		t.Fatal(err)
	}
	blocked := seed
	blocked.Key = "blocked_insert"
	allowed := seed
	allowed.Key = "allowed_insert"
	if _, err := Apply(t.Context(), database, nil, Options{
		Actor:      contracts.ActorAgent,
		ResultRows: ResultRows{UserMemoryUpserts: []UserMemoryRow{allowed, blocked}},
	}); err == nil {
		t.Fatal("upsert trigger failure should roll back")
	}
	memories, err := storage.ListUserMemories(t.Context(), database.Read())
	if err != nil || len(memories) != 1 || memories[0].Key != "pacing" {
		t.Fatalf("memories after failures=%#v err=%v", memories, err)
	}
}

func TestUserMemoryCapacityEvictionRollsBackWhenDeleteFails(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	createDraft(t, database, "draft_memory_eviction_failure")
	seedUserMemoryMessage(t, database, "draft_memory_eviction_failure", "message_eviction_failure")
	base := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	upserts := make([]UserMemoryRow, 0, storage.UserMemoryLimit)
	for index := range storage.UserMemoryLimit {
		upserts = append(upserts, UserMemoryRow{
			Key: fmt.Sprintf("eviction_%02d", index), Kind: "preference",
			Statement:    fmt.Sprintf("用户偏好编号 %02d", index),
			EvidenceKind: storage.UserMemoryEvidenceMessage, EvidenceQuote: "偏好",
			EvidenceID: "message_eviction_failure", SourceDraftID: "draft_memory_eviction_failure",
			LastConfirmedAt: base.Add(time.Duration(index) * time.Second).Format(time.RFC3339Nano),
		})
	}
	if result, err := Apply(t.Context(), database, nil, Options{
		Actor: contracts.ActorAgent, ResultRows: ResultRows{UserMemoryUpserts: upserts},
	}); err != nil || result.Status != StatusApplied {
		t.Fatalf("seed result=%#v err=%v", result, err)
	}
	if _, err := database.Write().ExecContext(t.Context(), `
		CREATE TRIGGER block_oldest_memory_delete BEFORE DELETE ON user_memories
		WHEN OLD.memory_key = 'eviction_00'
		BEGIN SELECT RAISE(ABORT, 'blocked eviction'); END`); err != nil {
		t.Fatal(err)
	}
	newest := UserMemoryRow{
		Key: "eviction_50", Kind: "preference", Statement: "最新用户偏好",
		EvidenceKind: storage.UserMemoryEvidenceMessage, EvidenceQuote: "偏好",
		EvidenceID: "message_eviction_failure", SourceDraftID: "draft_memory_eviction_failure",
		LastConfirmedAt: base.Add(time.Hour).Format(time.RFC3339Nano),
	}
	if _, err := Apply(t.Context(), database, nil, Options{
		Actor: contracts.ActorAgent, ResultRows: ResultRows{UserMemoryUpserts: []UserMemoryRow{newest}},
	}); err == nil {
		t.Fatal("eviction delete failure should roll back the preceding upsert")
	}
	memories, err := storage.ListUserMemories(t.Context(), database.Read())
	if err != nil || len(memories) != storage.UserMemoryLimit {
		t.Fatalf("memories=%#v err=%v", memories, err)
	}
	keys := map[string]bool{}
	for _, memory := range memories {
		keys[memory.Key] = true
	}
	if !keys["eviction_00"] || keys["eviction_50"] {
		t.Fatalf("eviction failure keys=%v", keys)
	}
}

func TestUserMemoryCorruptRowsFailWithoutCommittingEarlierResultRows(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	createDraft(t, database, "draft_memory_corrupt")
	if _, err := database.Write().ExecContext(t.Context(), `
		DROP TABLE user_memories;
		CREATE TABLE user_memories(
			memory_key TEXT, kind TEXT, statement TEXT, evidence_kind TEXT,
			evidence_id TEXT, source_draft_id TEXT, created_at TEXT, last_confirmed_at TEXT
		);
		INSERT INTO user_memories VALUES(
			NULL,'preference','损坏记录','user_message','message_corrupt',
			'draft_memory_corrupt','2026-07-16T00:00:00Z','2026-07-16T00:00:00Z'
		)`); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ListUserMemories(t.Context(), database.Read()); err == nil {
		t.Fatal("nullable corrupt memory row should fail typed storage scan")
	}
	if _, err := Apply(t.Context(), database, nil, Options{
		Actor: contracts.ActorUser,
		ResultRows: ResultRows{
			Message: &MessageRow{
				ID: "message_before_corrupt_clear", DraftID: "draft_memory_corrupt",
				Role: "assistant", Kind: "reply", Content: "不应提交",
			},
			UserMemoryClearAll: true,
		},
	}); err == nil {
		t.Fatal("corrupt key should fail clear-all scan")
	}
	var messageCount int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM messages WHERE message_id='message_before_corrupt_clear'`,
	).Scan(&messageCount); err != nil || messageCount != 0 {
		t.Fatalf("message count=%d err=%v", messageCount, err)
	}
}

func TestUserMemoryEvidenceQuoteMustBeVerbatimSubstring(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	createDraft(t, database, "draft_quote")
	if _, err := Apply(t.Context(), database, nil, Options{
		Actor: contracts.ActorUser,
		ResultRows: ResultRows{Message: &MessageRow{
			ID: "message_quote", DraftID: "draft_quote", Role: "user", Kind: "user",
			Content: "以后我的视频节奏都要快一点，字幕别遮脸",
		}},
	}); err != nil {
		t.Fatalf("seed message err=%v", err)
	}
	seed := UserMemoryRow{
		Key: "pacing", Kind: "preference", Statement: "用户长期偏好快节奏",
		EvidenceKind: storage.UserMemoryEvidenceMessage,
		EvidenceID:   "message_quote", SourceDraftID: "draft_quote",
	}
	applyQuote := func(quote string) error {
		row := seed
		row.EvidenceQuote = quote
		_, err := Apply(t.Context(), database, nil, Options{
			Actor: contracts.ActorAgent, ResultRows: ResultRows{UserMemoryUpserts: []UserMemoryRow{row}},
		})
		return err
	}
	if err := applyQuote("节奏都要快一点"); err != nil {
		t.Fatalf("逐字子串应通过校验: %v", err)
	}
	memories, err := storage.ListUserMemories(t.Context(), database.Read())
	if err != nil || len(memories) != 1 || memories[0].Key != "pacing" {
		t.Fatalf("memories=%#v err=%v", memories, err)
	}
	for _, test := range []struct {
		name  string
		quote string
	}{
		{"改写非子串", "节奏都要慢一点"},
		{"跨逗号拼接", "快一点字幕别遮脸"},
		{"无关摘录", "封面要用红色"},
		{"过短单字", "快"},
		{"纯空白", "   "},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := applyQuote(test.quote); !errors.Is(err, ErrUserMemoryEvidenceQuoteMismatch) {
				t.Fatalf("污染 quote %q 应判 ErrUserMemoryEvidenceQuoteMismatch，实际 %v", test.quote, err)
			}
		})
	}
	after, err := storage.ListUserMemories(t.Context(), database.Read())
	if err != nil || len(after) != 1 || after[0].Statement != "用户长期偏好快节奏" {
		t.Fatalf("污染 quote 不得改写既有记忆: %#v err=%v", after, err)
	}
}

func TestUserMemoryDecisionAnswerQuoteMatchesFreeTextAndOptionLabel(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	createDraft(t, database, "draft_quote_decision")
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO decisions(
			decision_id,scope_type,draft_id,type,question,options_json,
			allow_free_text,status,answer_json,blocking
		) VALUES('decision_quote','draft','draft_quote_decision','critical','默认节奏？',
			'[{"option_id":"fast","label":"整体更紧凑"}]',
			1,'answered','{"option_id":"fast","free_text":"顺便字幕别太大"}',1)`); err != nil {
		t.Fatal(err)
	}
	seed := UserMemoryRow{
		Key: "pacing", Kind: "preference", Statement: "用户偏好紧凑节奏",
		EvidenceKind: storage.UserMemoryEvidenceDecision,
		EvidenceID:   "decision_quote", SourceDraftID: "draft_quote_decision",
	}
	applyQuote := func(quote string) error {
		row := seed
		row.EvidenceQuote = quote
		_, err := Apply(t.Context(), database, nil, Options{
			Actor: contracts.ActorAgent, ResultRows: ResultRows{UserMemoryUpserts: []UserMemoryRow{row}},
		})
		return err
	}
	if err := applyQuote("整体更紧凑"); err != nil {
		t.Fatalf("所选项标签子串应通过: %v", err)
	}
	if err := applyQuote("字幕别太大"); err != nil {
		t.Fatalf("自由文本子串应通过: %v", err)
	}
	if err := applyQuote("整体要放慢"); !errors.Is(err, ErrUserMemoryEvidenceQuoteMismatch) {
		t.Fatalf("非子串应被拦截，err=%v", err)
	}
}

func TestUserMemoryEvictionValuesRecentUseAndProtectsCorrections(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	createDraft(t, database, "draft_value")
	seedUserMemoryMessage(t, database, "draft_value", "message_value")
	base := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	upserts := make([]UserMemoryRow, 0, storage.UserMemoryLimit)
	for index := range storage.UserMemoryLimit {
		kind := "preference"
		if index == 1 {
			kind = "correction"
		}
		upserts = append(upserts, UserMemoryRow{
			Key: fmt.Sprintf("mem_%02d", index), Kind: kind, Statement: "长期偏好",
			EvidenceKind: storage.UserMemoryEvidenceMessage, EvidenceQuote: "偏好",
			EvidenceID: "message_value", SourceDraftID: "draft_value",
			LastConfirmedAt: base.Add(time.Duration(index) * time.Second).Format(time.RFC3339Nano),
		})
	}
	if result, err := Apply(t.Context(), database, nil, Options{
		Actor: contracts.ActorAgent, ResultRows: ResultRows{UserMemoryUpserts: upserts},
	}); err != nil || result.Status != StatusApplied {
		t.Fatalf("seed result=%#v err=%v", result, err)
	}

	// 老偏好 mem_00 被注入并用于一个成功回合：touch 让它的 last_used_at 变新。
	if _, err := Apply(t.Context(), database, nil, Options{
		Actor: contracts.ActorAgent, CreatedAt: base.Add(10 * time.Hour),
		ResultRows: ResultRows{UserMemoryTouchKeys: []string{"mem_00"}},
	}); err != nil {
		t.Fatalf("touch err=%v", err)
	}

	const newWrites = 5
	for index := range newWrites {
		writtenAt := base.Add(time.Duration(20+index) * time.Hour)
		if _, err := Apply(t.Context(), database, nil, Options{
			Actor: contracts.ActorAgent, CreatedAt: writtenAt,
			ResultRows: ResultRows{UserMemoryUpserts: []UserMemoryRow{{
				Key: fmt.Sprintf("fresh_%02d", index), Kind: "preference", Statement: "新偏好",
				EvidenceKind: storage.UserMemoryEvidenceMessage, EvidenceQuote: "偏好",
				EvidenceID: "message_value", SourceDraftID: "draft_value",
				LastConfirmedAt: writtenAt.Format(time.RFC3339Nano),
			}}},
		}); err != nil {
			t.Fatalf("write %d err=%v", index, err)
		}
	}

	memories, err := storage.ListUserMemories(t.Context(), database.Read())
	if err != nil || len(memories) != storage.UserMemoryLimit {
		t.Fatalf("memories len=%d err=%v", len(memories), err)
	}
	present := map[string]string{}
	for _, memory := range memories {
		present[memory.Key] = memory.LastUsedAt
	}
	if lastUsed, ok := present["mem_00"]; !ok || lastUsed == "" {
		t.Fatalf("被 touch 的稳定偏好 mem_00 应存活且 last_used_at 非空: ok=%v used=%q", ok, lastUsed)
	}
	if _, ok := present["mem_01"]; !ok {
		t.Fatal("correction mem_01 应受保护存活")
	}
	if _, ok := present["mem_02"]; ok {
		t.Fatal("最低价值且未使用的老偏好 mem_02 应最先被淘汰")
	}
	for index := range newWrites {
		if _, ok := present[fmt.Sprintf("fresh_%02d", index)]; !ok {
			t.Fatalf("新写入 fresh_%02d 应存活", index)
		}
	}
}

func TestUserMemoryEvidenceStorageFailureDoesNotWriteMemory(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	createDraft(t, database, "draft_memory_evidence_failure")
	seedUserMemoryMessage(t, database, "draft_memory_evidence_failure", "message_evidence_failure")
	if _, err := database.Write().ExecContext(t.Context(), "DROP TABLE messages"); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(t.Context(), database, nil, Options{
		Actor: contracts.ActorAgent,
		ResultRows: ResultRows{UserMemoryUpserts: []UserMemoryRow{{
			Key: "pacing", Kind: "preference", Statement: "成片节奏偏快",
			EvidenceKind: storage.UserMemoryEvidenceMessage,
			EvidenceID:   "message_evidence_failure", SourceDraftID: "draft_memory_evidence_failure",
		}}},
	}); err == nil {
		t.Fatal("evidence storage failure should reject memory upsert")
	}
	memories, err := storage.ListUserMemories(t.Context(), database.Read())
	if err != nil || len(memories) != 0 {
		t.Fatalf("memories=%#v err=%v", memories, err)
	}
}

func TestUserMemoryManualStatementEditMarksRevisionAndGuardsActor(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	createDraft(t, database, "draft_edit")
	seedUserMemoryMessage(t, database, "draft_edit", "message_edit")
	if _, err := Apply(t.Context(), database, nil, Options{
		Actor: contracts.ActorAgent,
		ResultRows: ResultRows{UserMemoryUpserts: []UserMemoryRow{{
			Key: "pacing", Kind: "preference", Statement: "成片节奏偏快",
			EvidenceKind: storage.UserMemoryEvidenceMessage, EvidenceQuote: "偏好",
			EvidenceID: "message_edit", SourceDraftID: "draft_edit",
		}}},
	}); err != nil {
		t.Fatal(err)
	}
	// Actor=Agent 不得走手动修订路径（绕过证据）。
	if _, err := Apply(t.Context(), database, nil, Options{
		Actor:      contracts.ActorAgent,
		ResultRows: ResultRows{UserMemoryStatementEdit: &UserMemoryStatementEditRow{Key: "pacing", Statement: "越权改写"}},
	}); !errors.Is(err, ErrUserMemoryInput) {
		t.Fatalf("Actor=Agent 手动修订应被拒: %v", err)
	}
	// Actor=User 手动修订：更新 statement、标注 manually_revised_at，无需证据。
	result, err := Apply(t.Context(), database, nil, Options{
		Actor: contracts.ActorUser, CreatedAt: time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC),
		ResultRows: ResultRows{UserMemoryStatementEdit: &UserMemoryStatementEditRow{
			Key: "pacing", Statement: "用户手动改为：成片整体紧凑",
		}},
	})
	if err != nil || result.UserMemory == nil || len(result.UserMemory.EditedKeys) != 1 ||
		result.UserMemory.EditedKeys[0] != "pacing" {
		t.Fatalf("手动修订应成功: result=%#v err=%v", result, err)
	}
	memory, err := storage.GetUserMemory(t.Context(), database.Read(), "pacing")
	if err != nil || memory.Statement != "用户手动改为：成片整体紧凑" || memory.ManuallyRevisedAt == "" {
		t.Fatalf("修订未落库或未标注 manually_revised_at: %#v err=%v", memory, err)
	}
	// 键不存在：EditedKeys 为空，供端点回 404。
	missing, err := Apply(t.Context(), database, nil, Options{
		Actor:      contracts.ActorUser,
		ResultRows: ResultRows{UserMemoryStatementEdit: &UserMemoryStatementEditRow{Key: "absent_key", Statement: "无此键"}},
	})
	if err != nil || missing.UserMemory == nil || len(missing.UserMemory.EditedKeys) != 0 {
		t.Fatalf("不存在的键不应报编辑: result=%#v err=%v", missing, err)
	}
}
