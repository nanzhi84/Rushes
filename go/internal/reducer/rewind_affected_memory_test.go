package reducer

import (
	"slices"
	"testing"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

// insertRewindTestMessageAt 插入带显式 created_at 的用户消息,让回退区间的时间边界可控。
func insertRewindTestMessageAt(
	t *testing.T,
	database *storage.DB,
	draftID, messageID, content string,
	at time.Time,
) {
	t.Helper()
	result, err := Apply(t.Context(), database, nil, Options{
		Actor: contracts.ActorUser, CreatedAt: at,
		ResultRows: ResultRows{Message: &MessageRow{
			ID: messageID, DraftID: draftID, Role: "user", Kind: "user", Content: content,
		}},
	})
	if err != nil || result.Status != StatusApplied {
		t.Fatalf("insert message %s at %v result=%#v err=%v", messageID, at, result, err)
	}
}

// upsertRewindTestMemory 以显式 created_at 写入/复写一条记忆;同键第二次调用走 ON CONFLICT
// 更新证据与 last_confirmed_at,但 created_at 保持首次值(与生产 upsert 语义一致)。
func upsertRewindTestMemory(
	t *testing.T,
	database *storage.DB,
	draftID, key, evidenceKind, evidenceID, quote string,
	at time.Time,
) {
	t.Helper()
	result, err := Apply(t.Context(), database, nil, Options{
		Actor: contracts.ActorAgent, CreatedAt: at,
		ResultRows: ResultRows{UserMemoryUpserts: []UserMemoryRow{{
			Key: key, Kind: "preference", Statement: "记忆-" + key,
			EvidenceKind: evidenceKind, EvidenceID: evidenceID,
			EvidenceQuote: quote, SourceDraftID: draftID,
		}}},
	})
	if err != nil || result.Status != StatusApplied {
		t.Fatalf("upsert memory %s result=%#v err=%v", key, result, err)
	}
}

func affectedKeys(memories []storage.RewindAffectedMemory) []string {
	keys := make([]string, 0, len(memories))
	for _, memory := range memories {
		keys = append(keys, memory.Key)
	}
	return keys
}

// 「证据落在回退区间内」与「创建于区间内」是一个 AND:2×2 四种组合里只有同时为真才计入。
// 用一条消息 X(user-2)作锚点,构造四类记忆,断言只有右上格(两者皆真)被算作波及。
func TestRewindCollectsAffectedMemoriesByEvidenceAndCreation(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	draftID := "draft-affected"
	createDraft(t, database, draftID)

	base := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	insertRewindTestMessageAt(t, database, draftID, "user-1", "请记住第一条", base)
	insertRewindTestMessageAt(t, database, draftID, "user-2", "请记住第二条", base.Add(60*time.Second))
	insertRewindTestMessageAt(t, database, draftID, "user-3", "请记住第三条", base.Add(120*time.Second))

	// 证据✓ 创建✓:证据是被遮蔽的 user-2/user-3,且创建于锚点之后 → 波及。
	upsertRewindTestMemory(t, database, draftID, "aa_in_range",
		storage.UserMemoryEvidenceMessage, "user-2", "第二条", base.Add(70*time.Second))
	upsertRewindTestMemory(t, database, draftID, "bb_in_range2",
		storage.UserMemoryEvidenceMessage, "user-3", "第三条", base.Add(130*time.Second))
	// 证据✗ 创建✗:证据是保留的 user-1,创建也早于锚点 → 保留。
	upsertRewindTestMemory(t, database, draftID, "cc_kept_before",
		storage.UserMemoryEvidenceMessage, "user-1", "第一条", base.Add(10*time.Second))
	// 证据✗ 创建✓:创建于区间内,但证据仍指向可见的 user-1 → 保留(证据没被撤回)。
	upsertRewindTestMemory(t, database, draftID, "dd_evidence_kept",
		storage.UserMemoryEvidenceMessage, "user-1", "第一条", base.Add(75*time.Second))
	// 证据✓ 创建✗:先在区间外成立(证据 user-1),再在区间内复写(证据改指被遮蔽的 user-2);
	// created_at 保持区间外 → 保留。这条正是 AND 保护「更早已成立偏好」的关键分支。
	upsertRewindTestMemory(t, database, draftID, "ee_reconfirmed",
		storage.UserMemoryEvidenceMessage, "user-1", "第一条", base.Add(15*time.Second))
	upsertRewindTestMemory(t, database, draftID, "ee_reconfirmed",
		storage.UserMemoryEvidenceMessage, "user-2", "第二条", base.Add(80*time.Second))

	target := messageCheckpoint(t, database, draftID, "user-2")
	draft, _ := storage.GetDraft(t.Context(), database.Read(), draftID)
	baseVersion := draft.StateVersion
	result, err := Apply(t.Context(), database, []contracts.Event{{
		Type: "TimelineVersionRestored", DraftID: draftID,
		Payload: map[string]any{
			"checkpoint_id": target.ID, "mode": "conversation",
			"restore_checkpoint_id": "rewind:restore:affected",
		},
	}}, Options{
		Actor: contracts.ActorUser, BaseVersion: &baseVersion,
		RewindRestore: &RewindRestore{
			DraftID: draftID, IdempotencyKey: "idem-affected", CheckpointID: target.ID,
			Mode: "conversation", NewMessageID: "user-2b", RewoundMessageCount: 2,
		},
		ResultRows: ResultRows{Message: &MessageRow{
			ID: "user-2b", DraftID: draftID, Role: "user", Kind: "user", Content: "第二轮改写",
		}},
	})
	if err != nil || result.Status != StatusApplied {
		t.Fatalf("resend result=%#v err=%v", result, err)
	}

	want := []string{"aa_in_range", "bb_in_range2"}
	if got := affectedKeys(result.RewindAffectedMemories); !slices.Equal(got, want) {
		t.Fatalf("Result affected=%v want %v", got, want)
	}
	if result.RewindAffectedMemories[0].Statement != "记忆-aa_in_range" {
		t.Fatalf("affected statement=%q want 记忆-aa_in_range", result.RewindAffectedMemories[0].Statement)
	}
	// 同一事务内落进幂等结果表:重放读回同一清单,保证重试渲染同一「撤回」卡片。
	stored, err := storage.GetRewindRestoreResult(t.Context(), database.Read(), draftID, "idem-affected")
	if err != nil {
		t.Fatal(err)
	}
	if got := affectedKeys(stored.AffectedMemories); !slices.Equal(got, want) {
		t.Fatalf("persisted affected=%v want %v", got, want)
	}
	// 记忆刻意跨回退存活:清单只是提示,五条记忆一条都不该被回退删除。
	remaining, err := storage.ListUserMemories(t.Context(), database.Read())
	if err != nil || len(remaining) != 5 {
		t.Fatalf("memories must survive rewind: len=%d err=%v", len(remaining), err)
	}
}

// 决策证据走 rowid > 检查点决策边界(已回答的证据决策不会被作废,不能靠 status 判断);
// 且波及判定按 source_draft_id 隔离:另一草稿的记忆不因本草稿回退而入列。
func TestRewindAffectedMemoriesCoverDecisionEvidenceAndSkipOtherDrafts(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	draftID := "draft-affected-dec"
	otherDraftID := "draft-other"
	createDraft(t, database, draftID)
	createDraft(t, database, otherDraftID)

	base := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	insertRewindTestMessageAt(t, database, draftID, "m1", "请记住第一条", base)
	insertRewindTestMessageAt(t, database, draftID, "m2", "锚点消息", base.Add(60*time.Second))

	// m2 检查点之后插入一条已回答决策(rowid 落在其决策边界之后),作为区间内记忆的证据。
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO decisions(
			decision_id,scope_type,draft_id,type,question,options_json,
			allow_free_text,status,answer_json,blocking
		) VALUES('dec-after','draft',?,'critical','节奏?','[]',1,'answered','{"free_text":"快一点"}',1)`,
		draftID); err != nil {
		t.Fatal(err)
	}
	upsertRewindTestMemory(t, database, draftID, "pacing_decision",
		storage.UserMemoryEvidenceDecision, "dec-after", "快一点", base.Add(70*time.Second))
	// 另一草稿的记忆:证据是它自己的消息,本草稿回退不该波及它。
	insertRewindTestMessageAt(t, database, otherDraftID, "o1", "别动我", base.Add(80*time.Second))
	upsertRewindTestMemory(t, database, otherDraftID, "other_pref",
		storage.UserMemoryEvidenceMessage, "o1", "别动", base.Add(90*time.Second))

	target := messageCheckpoint(t, database, draftID, "m2")
	draft, _ := storage.GetDraft(t.Context(), database.Read(), draftID)
	baseVersion := draft.StateVersion
	result, err := Apply(t.Context(), database, []contracts.Event{{
		Type: "TimelineVersionRestored", DraftID: draftID,
		Payload: map[string]any{
			"checkpoint_id": target.ID, "mode": "conversation",
			"restore_checkpoint_id": "rewind:restore:dec",
		},
	}}, Options{
		Actor: contracts.ActorUser, BaseVersion: &baseVersion,
		ResultRows: ResultRows{Message: &MessageRow{
			ID: "m2b", DraftID: draftID, Role: "user", Kind: "user", Content: "改写锚点",
		}},
	})
	if err != nil || result.Status != StatusApplied {
		t.Fatalf("resend result=%#v err=%v", result, err)
	}
	if got := affectedKeys(result.RewindAffectedMemories); !slices.Equal(got, []string{"pacing_decision"}) {
		t.Fatalf("affected=%v want [pacing_decision]", got)
	}
}
