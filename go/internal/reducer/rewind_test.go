package reducer

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

func TestRewindEditResendBranchesTimelineAndSoftShadowsConversation(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	draftID := "draft-rewind"
	createDraft(t, database, draftID)
	insertRewindTestMessage(t, database, draftID, "user-1", "第一轮")
	createRewindTestTimeline(t, database, draftID, 1, "clip-1", 30)
	insertRewindTestMessage(t, database, draftID, "user-2", "第二轮")
	createRewindTestTimeline(t, database, draftID, 2, "clip-2", 60)
	insertRewindTestMessage(t, database, draftID, "user-3", "第三轮")

	// 收敛后只保留每条用户消息一个检查点：无 timeline_write 来源。
	checkpoints, err := storage.ListRewindCheckpoints(t.Context(), database.Read(), draftID, 50)
	if err != nil || len(checkpoints) != 3 {
		t.Fatalf("checkpoints=%#v err=%v", checkpoints, err)
	}
	for _, checkpoint := range checkpoints {
		if checkpoint.TriggerKind != "user_message" {
			t.Fatalf("unexpected trigger kind %q in %#v", checkpoint.TriggerKind, checkpoint)
		}
	}
	// user-2 的检查点在 v1 之后、v2 之前记录，故捕获 timeline v1。
	target := messageCheckpoint(t, database, draftID, "user-2")
	if target.TimelineVersion == nil || *target.TimelineVersion != 1 ||
		target.AnchorMessageID == nil || *target.AnchorMessageID != "user-2" {
		t.Fatalf("user-2 checkpoint=%#v", target)
	}
	// 可见集边界取“消息之前”：user-2 自身不在其检查点可见集内。
	assertCheckpointVisibleSet(t, database, target.ID, "user-1")

	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO decisions(
			decision_id,scope_type,draft_id,type,question,options_json,status,blocking
		) VALUES('decision-after','draft',?,'generic','继续吗','[]','pending',1);
		INSERT INTO agent_context_checkpoints(
			draft_id,window_id,window_number,history_version,base_snapshot_json,
			base_snapshot_hash,summary,created_at,updated_at
		) VALUES(?,'window',1,1,'{}','hash','summary','now','now')`, draftID, draftID); err != nil {
		t.Fatal(err)
	}
	draft, _ := storage.GetDraft(t.Context(), database.Read(), draftID)
	base := draft.StateVersion
	result, err := Apply(t.Context(), database, []contracts.Event{{
		Type: "TimelineVersionRestored", DraftID: draftID,
		Payload: map[string]any{
			"checkpoint_id": target.ID, "mode": "both", "timeline_version": 3,
			"restore_checkpoint_id": "rewind:restore:both",
		},
	}}, Options{
		Actor: contracts.ActorUser, BaseVersion: &base,
		ResultRows: ResultRows{Message: &MessageRow{
			ID: "user-2b", DraftID: draftID, Role: "user", Kind: "user", Content: "第二轮改写",
		}},
	})
	if err != nil || result.Status != StatusApplied {
		t.Fatalf("restore both result=%#v err=%v", result, err)
	}
	assertRewindTimeline(t, database, draftID, 3, 1, "clip-1")
	// 恢复到 user-2 之前：user-1 保留，user-2/user-3 遮蔽，新的 user-2b 可见。
	visible, err := storage.ListMessages(t.Context(), database.Read(), draftID, 20)
	if err != nil || len(visible) != 2 || visible[0].ID != "user-1" || visible[1].ID != "user-2b" {
		t.Fatalf("visible messages=%#v err=%v", visible, err)
	}
	for _, shadowed := range []string{"user-2", "user-3"} {
		var rewoundAt *string
		if err := database.Read().QueryRowContext(t.Context(),
			"SELECT rewound_at FROM messages WHERE message_id=?", shadowed,
		).Scan(&rewoundAt); err != nil || rewoundAt == nil {
			t.Fatalf("%s rewound_at=%v err=%v", shadowed, rewoundAt, err)
		}
	}
	var decisionStatus string
	if err := database.Read().QueryRowContext(t.Context(),
		"SELECT status FROM decisions WHERE decision_id='decision-after'",
	).Scan(&decisionStatus); err != nil || decisionStatus != "cancelled" {
		t.Fatalf("decision status=%q err=%v", decisionStatus, err)
	}
	var contextCount int
	if err := database.Read().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM agent_context_checkpoints WHERE draft_id=?", draftID,
	).Scan(&contextCount); err != nil || contextCount != 0 {
		t.Fatalf("context checkpoints=%d err=%v", contextCount, err)
	}
	// 恢复既追加新时间线版本（4 行）也留下 restore 审计检查点。
	var timelineRows, restoreCheckpoints int
	if err := database.Read().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM timeline_versions WHERE draft_id=?", draftID,
	).Scan(&timelineRows); err != nil || timelineRows != 3 {
		t.Fatalf("timeline rows=%d err=%v", timelineRows, err)
	}
	if err := database.Read().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM rewind_checkpoints WHERE draft_id=? AND trigger_kind='restore'", draftID,
	).Scan(&restoreCheckpoints); err != nil || restoreCheckpoints != 1 {
		t.Fatalf("restore checkpoints=%d err=%v", restoreCheckpoints, err)
	}
}

func TestRewindTimelineOnlyAndConversationOnlyModes(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	draftID := "draft-rewind-modes"
	createDraft(t, database, draftID)
	createRewindTestTimeline(t, database, draftID, 1, "clip-1", 30)
	insertRewindTestMessage(t, database, draftID, "mode-1", "第一轮")
	createRewindTestTimeline(t, database, draftID, 2, "clip-2", 60)
	insertRewindTestMessage(t, database, draftID, "mode-2", "第二轮")

	target := messageCheckpoint(t, database, draftID, "mode-1")
	if target.TimelineVersion == nil || *target.TimelineVersion != 1 {
		t.Fatalf("mode-1 checkpoint=%#v", target)
	}

	// timeline-only：时间线回到 v1（新版本 v3），会话不动。
	draft, _ := storage.GetDraft(t.Context(), database.Read(), draftID)
	if result, err := Apply(t.Context(), database, []contracts.Event{{
		Type: "TimelineVersionRestored", DraftID: draftID,
		Payload: map[string]any{
			"checkpoint_id": target.ID, "mode": "timeline", "timeline_version": 3,
			"restore_checkpoint_id": "rewind:restore:timeline-only",
		},
	}}, Options{Actor: contracts.ActorUser, BaseVersion: &draft.StateVersion}); err != nil || result.Status != StatusApplied {
		t.Fatalf("timeline-only result=%#v err=%v", result, err)
	}
	assertRewindTimeline(t, database, draftID, 3, 1, "clip-1")
	visible, _ := storage.ListMessages(t.Context(), database.Read(), draftID, 20)
	if len(visible) != 2 {
		t.Fatalf("timeline-only changed conversation: %#v", visible)
	}

	// conversation-only：会话回到 mode-1 之前（仅 mode-2 遮蔽，mode-1 也遮蔽），时间线不动。
	draft, _ = storage.GetDraft(t.Context(), database.Read(), draftID)
	if result, err := Apply(t.Context(), database, []contracts.Event{{
		Type: "TimelineVersionRestored", DraftID: draftID,
		Payload: map[string]any{
			"checkpoint_id": target.ID, "mode": "conversation",
			"restore_checkpoint_id": "rewind:restore:conversation-only",
		},
	}}, Options{Actor: contracts.ActorUser, BaseVersion: &draft.StateVersion}); err != nil || result.Status != StatusApplied {
		t.Fatalf("conversation-only result=%#v err=%v", result, err)
	}
	draft, _ = storage.GetDraft(t.Context(), database.Read(), draftID)
	if draft.TimelineCurrentVersion == nil || *draft.TimelineCurrentVersion != 3 {
		t.Fatalf("conversation-only changed timeline: %#v", draft.TimelineCurrentVersion)
	}
	visibleAfter, _ := storage.ListMessages(t.Context(), database.Read(), draftID, 20)
	if len(visibleAfter) != 0 {
		t.Fatalf("conversation-only should shadow mode-1 and mode-2: %#v", visibleAfter)
	}
}

func TestConversationRewindDoesNotResurrectMessagesFromAnAbandonedBranch(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	draftID := "draft-rewind-branch"
	createDraft(t, database, draftID)
	insertRewindTestMessage(t, database, draftID, "branch-m1", "第一轮")
	insertRewindTestMessage(t, database, draftID, "branch-m2", "旧分支")

	restoreConversation := func(checkpointID, resultID string) {
		t.Helper()
		draft, _ := storage.GetDraft(t.Context(), database.Read(), draftID)
		result, err := Apply(t.Context(), database, []contracts.Event{{
			Type: "TimelineVersionRestored", DraftID: draftID,
			Payload: map[string]any{
				"checkpoint_id": checkpointID, "mode": "conversation",
				"restore_checkpoint_id": resultID,
			},
		}}, Options{Actor: contracts.ActorUser, BaseVersion: &draft.StateVersion})
		if err != nil || result.Status != StatusApplied {
			t.Fatalf("restore %s result=%#v err=%v", checkpointID, result, err)
		}
	}

	// 回到 branch-m2 之前：branch-m2 遮蔽，branch-m1 保留（不在 m2 的可见集边界内）。
	restoreConversation("rewind:message:branch-m2", "rewind:restore:old-branch")
	insertRewindTestMessage(t, database, draftID, "branch-m3", "新分支")
	// 新检查点的可见集边界排除自身，只含 branch-m1。
	assertCheckpointVisibleSet(t, database, "rewind:message:branch-m3", "branch-m1")
	restoreConversation("rewind:message:branch-m3", "rewind:restore:new-branch")

	visible, err := storage.ListMessages(t.Context(), database.Read(), draftID, 20)
	if err != nil || len(visible) != 1 || visible[0].ID != "branch-m1" {
		t.Fatalf("visible=%#v err=%v", visible, err)
	}
	var hidden int
	if err := database.Read().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM messages WHERE message_id IN ('branch-m2','branch-m3') AND rewound_at IS NOT NULL",
	).Scan(&hidden); err != nil || hidden != 2 {
		t.Fatalf("abandoned branch hidden=%d err=%v", hidden, err)
	}
}

func TestRewindBothRollsBackTimelineWhenConversationMutationFails(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	draftID := "draft-rewind-atomic"
	createDraft(t, database, draftID)
	createRewindTestTimeline(t, database, draftID, 1, "clip-anchor", 30)
	insertRewindTestMessage(t, database, draftID, "user-anchor", "锚点")
	insertRewindTestMessage(t, database, draftID, "user-later", "稍后")
	target := messageCheckpoint(t, database, draftID, "user-anchor")
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO decisions(
			decision_id,scope_type,draft_id,type,question,options_json,status,blocking
		) VALUES('atomic-decision','draft',?,'generic','继续吗','[]','pending',1);
		INSERT INTO jobs(
			job_id,kind,status,draft_id,requested_by_draft_id,idempotency_key,
			payload_json,next_run_at,priority,created_at
		) VALUES('atomic-job','render_preview','pending',?,?,'atomic-job','{}','now',100,'now')`,
		draftID, draftID, draftID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write().ExecContext(t.Context(), `
		CREATE TRIGGER fail_conversation_rewind
		BEFORE UPDATE OF rewound_at ON messages
		WHEN NEW.rewound_at IS NOT NULL
		BEGIN SELECT RAISE(ABORT,'forced rewind failure'); END`); err != nil {
		t.Fatal(err)
	}
	draft, _ := storage.GetDraft(t.Context(), database.Read(), draftID)
	base := draft.StateVersion
	if _, err := Apply(t.Context(), database, []contracts.Event{
		{
			Type: "JobCancelled", DraftID: draftID,
			Payload: map[string]any{
				"job_id": "atomic-job", "kind": "render_preview",
				"requested_by_draft_id": draftID, "reason": "rewind_restored",
			},
		},
		{
			Type: "TimelineVersionRestored", DraftID: draftID,
			Payload: map[string]any{
				"checkpoint_id": target.ID, "mode": "both", "timeline_version": 2,
				"restore_checkpoint_id": "rewind:restore:must-rollback",
			},
		},
	}, Options{
		Actor: contracts.ActorUser, BaseVersion: &base,
		ResultRows: ResultRows{AgentJobObservationSuppressions: []AgentJobObservationSuppressionRow{{
			JobID: "atomic-job",
		}}},
	}); err == nil {
		t.Fatal("forced conversation failure should abort both restore")
	}
	var timelineRows, hidden, restoreEvents int
	if err := database.Read().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM timeline_versions WHERE draft_id=?", draftID).Scan(&timelineRows); err != nil || timelineRows != 1 {
		t.Fatalf("timeline rows=%d err=%v", timelineRows, err)
	}
	if err := database.Read().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM messages WHERE draft_id=? AND rewound_at IS NOT NULL", draftID).Scan(&hidden); err != nil || hidden != 0 {
		t.Fatalf("hidden messages=%d err=%v", hidden, err)
	}
	if err := database.Read().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM event_log WHERE draft_id=? AND event_type='TimelineVersionRestored'", draftID,
	).Scan(&restoreEvents); err != nil || restoreEvents != 0 {
		t.Fatalf("restore events=%d err=%v", restoreEvents, err)
	}
	var jobStatus, decisionStatus string
	if err := database.Read().QueryRowContext(t.Context(),
		"SELECT status FROM jobs WHERE job_id='atomic-job'").Scan(&jobStatus); err != nil || jobStatus != "pending" {
		t.Fatalf("job status=%s err=%v", jobStatus, err)
	}
	if err := database.Read().QueryRowContext(t.Context(),
		"SELECT status FROM decisions WHERE decision_id='atomic-decision'").Scan(&decisionStatus); err != nil || decisionStatus != "pending" {
		t.Fatalf("decision status=%s err=%v", decisionStatus, err)
	}
	var suppressions int
	if err := database.Read().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM agent_job_observation_suppressions WHERE job_id='atomic-job'",
	).Scan(&suppressions); err != nil || suppressions != 0 {
		t.Fatalf("suppressions=%d err=%v", suppressions, err)
	}
}

func TestRewindCheckpointRetentionKeepsLatestFifty(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	draftID := "draft-rewind-retention"
	createDraft(t, database, draftID)
	for index := 1; index <= 55; index++ {
		insertRewindTestMessage(t, database, draftID,
			fmt.Sprintf("user-%02d", index), fmt.Sprintf("第 %d 轮", index))
	}
	checkpoints, err := storage.ListRewindCheckpoints(t.Context(), database.Read(), draftID, 50)
	if err != nil || len(checkpoints) != 50 {
		t.Fatalf("checkpoints=%d err=%v", len(checkpoints), err)
	}
	if checkpoints[0].AnchorMessageID == nil || *checkpoints[0].AnchorMessageID != "user-55" ||
		checkpoints[49].AnchorMessageID == nil || *checkpoints[49].AnchorMessageID != "user-06" {
		t.Fatalf("retained boundaries newest=%#v oldest=%#v", checkpoints[0], checkpoints[49])
	}
}

func TestConversationRewindCanReturnBeforeTheFirstMessage(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	draftID := "draft-rewind-before-first-message"
	createDraft(t, database, draftID)
	insertRewindTestMessage(t, database, draftID, "user-after-empty", "稍后创建的消息")
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO rewind_checkpoints(
			checkpoint_id,draft_id,trigger_kind,decision_boundary,job_boundary,
			summary,clip_count,duration_frames,track_count,created_at
		) VALUES('rewind-before-first',?,'user_message',0,0,'对话开始前',0,0,0,'now')`, draftID,
	); err != nil {
		t.Fatal(err)
	}
	draft, _ := storage.GetDraft(t.Context(), database.Read(), draftID)
	result, err := Apply(t.Context(), database, []contracts.Event{{
		Type: "TimelineVersionRestored", DraftID: draftID,
		Payload: map[string]any{
			"checkpoint_id": "rewind-before-first", "mode": "conversation",
			"restore_checkpoint_id": "rewind-before-first-result",
		},
	}}, Options{Actor: contracts.ActorUser, BaseVersion: &draft.StateVersion})
	if err != nil || result.Status != StatusApplied {
		t.Fatalf("restore result=%#v err=%v", result, err)
	}
	visible, err := storage.ListMessages(t.Context(), database.Read(), draftID, 20)
	if err != nil || len(visible) != 0 {
		t.Fatalf("visible=%#v err=%v", visible, err)
	}
	restored, err := storage.GetRewindCheckpoint(
		t.Context(), database.Read(), draftID, "rewind-before-first-result",
	)
	if err != nil || restored.TimelineVersion != nil || restored.AnchorMessageID != nil {
		t.Fatalf("restored checkpoint=%#v err=%v", restored, err)
	}
}

func TestConversationRewindDropsShadowedContextResetAnchor(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	draftID := "draft-rewind-context-anchor"
	createDraft(t, database, draftID)
	insertRewindTestMessage(t, database, draftID, "anchor-first", "第一轮")
	target := messageCheckpoint(t, database, draftID, "anchor-first")
	// 在锚点消息之后设置上下文重置点，使其落在回退遮蔽范围内。
	insertRewindTestMessage(t, database, draftID, "anchor-reset", "清空点")
	if _, err := database.Write().ExecContext(t.Context(),
		"UPDATE drafts SET messages_tail_ref='anchor-reset' WHERE draft_id=?", draftID,
	); err != nil {
		t.Fatal(err)
	}
	draft, _ := storage.GetDraft(t.Context(), database.Read(), draftID)
	if result, err := Apply(t.Context(), database, []contracts.Event{{
		Type: "TimelineVersionRestored", DraftID: draftID,
		Payload: map[string]any{
			"checkpoint_id": target.ID, "mode": "conversation",
			"restore_checkpoint_id": "rewind:restore:context-anchor",
		},
	}}, Options{Actor: contracts.ActorUser, BaseVersion: &draft.StateVersion}); err != nil || result.Status != StatusApplied {
		t.Fatalf("restore result=%#v err=%v", result, err)
	}
	// 被遮蔽的重置锚点会撤销那次清空，messages_tail_ref 回落 NULL。
	var tailRef *string
	if err := database.Read().QueryRowContext(t.Context(),
		"SELECT messages_tail_ref FROM drafts WHERE draft_id=?", draftID,
	).Scan(&tailRef); err != nil || tailRef != nil {
		t.Fatalf("messages_tail_ref=%v err=%v", tailRef, err)
	}
}

func TestRewindRejectsUnavailableOrCorruptTimelineTargets(t *testing.T) {
	t.Parallel()
	t.Run("checkpoint without timeline", func(t *testing.T) {
		database := openTestDB(t)
		draftID := "draft-rewind-no-timeline"
		createDraft(t, database, draftID)
		insertRewindTestMessage(t, database, draftID, "user-no-timeline", "尚无时间线")
		target := messageCheckpoint(t, database, draftID, "user-no-timeline")
		draft, _ := storage.GetDraft(t.Context(), database.Read(), draftID)
		_, err := Apply(t.Context(), database, []contracts.Event{{
			Type: "TimelineVersionRestored", DraftID: draftID,
			Payload: map[string]any{
				"checkpoint_id": target.ID, "mode": "timeline", "timeline_version": 1,
				"restore_checkpoint_id": "rewind-invalid-no-timeline",
			},
		}}, Options{Actor: contracts.ActorUser, BaseVersion: &draft.StateVersion})
		if err == nil || !strings.Contains(err.Error(), "没有时间线版本") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("corrupt timeline snapshot", func(t *testing.T) {
		database := openTestDB(t)
		draftID := "draft-rewind-corrupt-timeline"
		createDraft(t, database, draftID)
		createRewindTestTimeline(t, database, draftID, 1, "clip-corrupt", 30)
		insertRewindTestMessage(t, database, draftID, "user-corrupt", "创建时间线")
		target := messageCheckpoint(t, database, draftID, "user-corrupt")
		if _, err := database.Write().ExecContext(t.Context(),
			"UPDATE timeline_versions SET document_json='{' WHERE draft_id=? AND version=1", draftID,
		); err != nil {
			t.Fatal(err)
		}
		draft, _ := storage.GetDraft(t.Context(), database.Read(), draftID)
		_, err := Apply(t.Context(), database, []contracts.Event{{
			Type: "TimelineVersionRestored", DraftID: draftID,
			Payload: map[string]any{
				"checkpoint_id": target.ID, "mode": "timeline", "timeline_version": 2,
				"restore_checkpoint_id": "rewind-invalid-corrupt",
			},
		}}, Options{Actor: contracts.ActorUser, BaseVersion: &draft.StateVersion})
		if err == nil || !strings.Contains(err.Error(), "无法解析") {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestRewindCheckpointHelpersRejectInvalidRowsAndBoundSummaries(t *testing.T) {
	t.Parallel()
	if got := truncateCheckpointSummary("  " + strings.Repeat("回", 130) + "  "); !strings.HasSuffix(got, "…") || len([]rune(got)) != 118 {
		t.Fatalf("summary=%q runes=%d", got, len([]rune(got)))
	}
	if got := truncateCheckpointSummary("  简短  "); got != "简短" {
		t.Fatalf("short summary=%q", got)
	}
	database := openTestDB(t)
	tx, err := database.Write().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := recordRewindCheckpoint(t.Context(), tx, rewindCheckpointInput{}); err == nil {
		t.Fatal("字段不完整的 checkpoint 必须拒绝")
	}
}

func TestRewindMessageCheckpointHandlesMissingAndCorruptTimelineState(t *testing.T) {
	t.Parallel()
	// 无时间线时用户消息检查点照常记录，TimelineVersion 为空。
	database := openTestDB(t)
	draftID := "draft-rewind-checkpoint-edge"
	createDraft(t, database, draftID)
	insertRewindTestMessage(t, database, draftID, "user-no-timeline", "尚无时间线")
	checkpoint := messageCheckpoint(t, database, draftID, "user-no-timeline")
	if checkpoint.TimelineVersion != nil || checkpoint.PatchID != nil {
		t.Fatalf("checkpoint=%#v", checkpoint)
	}
	// 当前时间线快照损坏时，记录消息检查点必须失败并回滚整条 Apply。
	createRewindTestTimeline(t, database, draftID, 1, "edge-clip", 30)
	if _, err := database.Write().ExecContext(t.Context(),
		"UPDATE timeline_versions SET document_json='{' WHERE draft_id=? AND version=1", draftID,
	); err != nil {
		t.Fatal(err)
	}
	result, err := Apply(t.Context(), database, nil, Options{
		Actor: contracts.ActorUser,
		ResultRows: ResultRows{Message: &MessageRow{
			ID: "user-after-corruption", DraftID: draftID, Role: "user", Kind: "user", Content: "继续",
		}},
	})
	if err == nil || result.Status == StatusApplied {
		t.Fatalf("corrupt current timeline must reject checkpoint: result=%#v err=%v", result, err)
	}
}

func TestRewindStorageFailuresRollbackWithoutChangingTheDraft(t *testing.T) {
	t.Parallel()
	t.Run("missing target snapshot", func(t *testing.T) {
		database := openTestDB(t)
		draftID := "draft-rewind-missing-snapshot"
		createDraft(t, database, draftID)
		createRewindTestTimeline(t, database, draftID, 1, "missing-snapshot-clip", 30)
		insertRewindTestMessage(t, database, draftID, "missing-snapshot-user", "初版")
		target := messageCheckpoint(t, database, draftID, "missing-snapshot-user")
		if _, err := database.Write().ExecContext(t.Context(),
			"DELETE FROM timeline_versions WHERE draft_id=? AND version=1", draftID,
		); err != nil {
			t.Fatal(err)
		}
		draft, _ := storage.GetDraft(t.Context(), database.Read(), draftID)
		_, err := Apply(t.Context(), database, []contracts.Event{{
			Type: "TimelineVersionRestored", DraftID: draftID,
			Payload: map[string]any{
				"checkpoint_id": target.ID, "mode": "timeline", "timeline_version": 2,
				"restore_checkpoint_id": "rewind-missing-snapshot-result",
			},
		}}, Options{Actor: contracts.ActorUser, BaseVersion: &draft.StateVersion})
		if err == nil {
			t.Fatal("缺失目标快照时恢复必须失败")
		}
	})

	t.Run("duplicate result version", func(t *testing.T) {
		database := openTestDB(t)
		draftID := "draft-rewind-duplicate-version"
		createDraft(t, database, draftID)
		createRewindTestTimeline(t, database, draftID, 1, "duplicate-version-clip", 30)
		insertRewindTestMessage(t, database, draftID, "duplicate-version-user", "初版")
		target := messageCheckpoint(t, database, draftID, "duplicate-version-user")
		draft, _ := storage.GetDraft(t.Context(), database.Read(), draftID)
		_, err := Apply(t.Context(), database, []contracts.Event{{
			Type: "TimelineVersionRestored", DraftID: draftID,
			Payload: map[string]any{
				"checkpoint_id": target.ID, "mode": "timeline", "timeline_version": 1,
				"restore_checkpoint_id": "rewind-duplicate-version-result",
			},
		}}, Options{Actor: contracts.ActorUser, BaseVersion: &draft.StateVersion})
		if err == nil {
			t.Fatal("重复的新版本号必须拒绝")
		}
	})

	t.Run("corrupt current snapshot during conversation restore", func(t *testing.T) {
		database := openTestDB(t)
		draftID := "draft-rewind-corrupt-conversation"
		createDraft(t, database, draftID)
		createRewindTestTimeline(t, database, draftID, 1, "corrupt-conversation-clip", 30)
		insertRewindTestMessage(t, database, draftID, "corrupt-conversation-user", "锚点")
		target := messageCheckpoint(t, database, draftID, "corrupt-conversation-user")
		if _, err := database.Write().ExecContext(t.Context(),
			"UPDATE timeline_versions SET document_json='{' WHERE draft_id=? AND version=1", draftID,
		); err != nil {
			t.Fatal(err)
		}
		draft, _ := storage.GetDraft(t.Context(), database.Read(), draftID)
		_, err := Apply(t.Context(), database, []contracts.Event{{
			Type: "TimelineVersionRestored", DraftID: draftID,
			Payload: map[string]any{
				"checkpoint_id": target.ID, "mode": "conversation",
				"restore_checkpoint_id": "rewind-corrupt-conversation-result",
			},
		}}, Options{Actor: contracts.ActorUser, BaseVersion: &draft.StateVersion})
		if err == nil {
			t.Fatal("当前快照损坏时 conversation checkpoint 物化必须失败")
		}
	})

	t.Run("missing context checkpoint table", func(t *testing.T) {
		database := openTestDB(t)
		draftID := "draft-rewind-missing-context-table"
		createDraft(t, database, draftID)
		insertRewindTestMessage(t, database, draftID, "missing-context-user", "锚点")
		target := messageCheckpoint(t, database, draftID, "missing-context-user")
		if _, err := database.Write().ExecContext(t.Context(), "DROP TABLE agent_context_checkpoints"); err != nil {
			t.Fatal(err)
		}
		draft, _ := storage.GetDraft(t.Context(), database.Read(), draftID)
		_, err := Apply(t.Context(), database, []contracts.Event{{
			Type: "TimelineVersionRestored", DraftID: draftID,
			Payload: map[string]any{
				"checkpoint_id": target.ID, "mode": "conversation",
				"restore_checkpoint_id": "rewind-missing-context-result",
			},
		}}, Options{Actor: contracts.ActorUser, BaseVersion: &draft.StateVersion})
		if err == nil {
			t.Fatal("上下文表缺失时恢复必须失败")
		}
	})
}

func TestRewindInternalGuardsRejectInvalidVersionAndCheckpointRows(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	draftID := "draft-rewind-internal-guards"
	createDraft(t, database, draftID)
	createRewindTestTimeline(t, database, draftID, 1, "internal-guard-clip", 30)
	insertRewindTestMessage(t, database, draftID, "internal-guard-user", "锚点")
	target := messageCheckpoint(t, database, draftID, "internal-guard-user")
	tx, err := database.Write().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	state := &applyState{
		tx: tx, createdAt: "now", originalVersions: map[string]int{}, touched: map[string]struct{}{},
	}
	if err := applyTimelineRestored(t.Context(), state, contracts.Event{
		Type: "TimelineVersionRestored", DraftID: draftID,
		Payload: map[string]any{
			"checkpoint_id": target.ID, "mode": "timeline", "timeline_version": 0,
			"restore_checkpoint_id": "rewind-invalid-version",
		},
	}); err == nil {
		t.Fatal("非正版本号必须拒绝")
	}
	_ = tx.Rollback()

	tx, err = database.Write().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := recordRewindCheckpoint(t.Context(), tx, rewindCheckpointInput{
		id: "rewind-invalid-trigger", draftID: draftID, triggerKind: "invalid",
		summary: "invalid", createdAt: "now",
	}); err == nil {
		t.Fatal("非法 trigger_kind 必须被 schema 约束拒绝")
	}
}

func TestRewindInternalGuardsRejectIncompleteRestorePayloads(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	draftID := "draft-rewind-incomplete"
	createDraft(t, database, draftID)
	createRewindTestTimeline(t, database, draftID, 1, "guard-clip", 30)
	insertRewindTestMessage(t, database, draftID, "guard-anchor", "锚点")
	target := messageCheckpoint(t, database, draftID, "guard-anchor")
	tx, err := database.Write().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	state := &applyState{
		tx: tx, createdAt: time.Now().UTC().Format(time.RFC3339Nano),
		originalVersions: map[string]int{}, touched: map[string]struct{}{},
	}
	checks := []struct {
		name    string
		payload map[string]any
	}{
		{"invalid mode", map[string]any{"checkpoint_id": target.ID, "mode": "invalid"}},
		{"missing new version", map[string]any{"checkpoint_id": target.ID, "mode": "timeline"}},
		{"missing restore checkpoint", map[string]any{"checkpoint_id": target.ID, "mode": "conversation"}},
	}
	for _, check := range checks {
		if err := applyTimelineRestored(t.Context(), state, contracts.Event{
			DraftID: draftID, Payload: check.payload,
		}); err == nil {
			t.Fatalf("%s should fail", check.name)
		}
	}
}

func TestRewindPropagatesCorruptPersistentState(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(t *testing.T, database *storage.DB, draftID, checkpointID string)
	}{
		{"current timeline snapshot", func(t *testing.T, database *storage.DB, draftID, _ string) {
			_, err := database.Write().ExecContext(t.Context(),
				"UPDATE timeline_versions SET document_json='{' WHERE draft_id=? AND version=1", draftID)
			if err != nil {
				t.Fatal(err)
			}
		}},
		{"context checkpoint table", func(t *testing.T, database *storage.DB, _, _ string) {
			if _, err := database.Write().ExecContext(t.Context(), "DROP TABLE agent_context_checkpoints"); err != nil {
				t.Fatal(err)
			}
		}},
		{"decisions table", func(t *testing.T, database *storage.DB, _, _ string) {
			if _, err := database.Write().ExecContext(t.Context(), "DROP TABLE decisions"); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			database := openTestDB(t)
			draftID := "draft-corrupt-" + strings.ReplaceAll(test.name, " ", "-")
			createDraft(t, database, draftID)
			createRewindTestTimeline(t, database, draftID, 1, "corrupt-clip", 30)
			insertRewindTestMessage(t, database, draftID, "corrupt-anchor", "锚点")
			target := messageCheckpoint(t, database, draftID, "corrupt-anchor")
			test.mutate(t, database, draftID, target.ID)
			draft, _ := storage.GetDraft(t.Context(), database.Read(), draftID)
			_, err := Apply(t.Context(), database, []contracts.Event{{
				Type: "TimelineVersionRestored", DraftID: draftID,
				Payload: map[string]any{
					"checkpoint_id": target.ID, "mode": "conversation",
					"restore_checkpoint_id": "restore-corrupt-" + test.name,
				},
			}}, Options{Actor: contracts.ActorUser, BaseVersion: &draft.StateVersion})
			if err == nil {
				t.Fatal("corrupt persistent state should abort restore")
			}
		})
	}

	t.Run("message checkpoint reads corrupt current timeline", func(t *testing.T) {
		database := openTestDB(t)
		draftID := "draft-corrupt-message-checkpoint"
		createDraft(t, database, draftID)
		createRewindTestTimeline(t, database, draftID, 1, "corrupt-message-clip", 30)
		if _, err := database.Write().ExecContext(t.Context(),
			"UPDATE timeline_versions SET document_json='{' WHERE draft_id=? AND version=1", draftID); err != nil {
			t.Fatal(err)
		}
		_, err := Apply(t.Context(), database, nil, Options{
			Actor: contracts.ActorUser,
			ResultRows: ResultRows{Message: &MessageRow{
				ID: "corrupt-message", DraftID: draftID, Role: "user", Kind: "user", Content: "消息",
			}},
		})
		if err == nil {
			t.Fatal("corrupt current timeline should abort message checkpoint")
		}
	})
}

func TestRewindCheckpointBoundaryQueriesPropagateFailures(t *testing.T) {
	t.Parallel()
	for _, table := range []string{"jobs", "event_log"} {
		t.Run(table, func(t *testing.T) {
			database := openTestDB(t)
			if _, err := database.Write().ExecContext(t.Context(), "DROP TABLE "+table); err != nil {
				t.Fatal(err)
			}
			tx, err := database.Write().BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			if err := recordRewindCheckpoint(t.Context(), tx, rewindCheckpointInput{
				id: "checkpoint", draftID: "draft", createdAt: "now",
			}); err == nil {
				t.Fatalf("missing %s should fail boundary capture", table)
			}
		})
	}
}

func TestRewindRestoreRollsBackOnPersistenceFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		setup      func(t *testing.T, database *storage.DB)
		mode       string
		newVersion int
	}{
		{
			name: "duplicate timeline version", mode: "timeline", newVersion: 1,
			setup: func(*testing.T, *storage.DB) {},
		},
		{
			name: "timeline pointer update", mode: "timeline", newVersion: 2,
			setup: func(t *testing.T, database *storage.DB) {
				if _, err := database.Write().ExecContext(t.Context(), `
					CREATE TRIGGER fail_rewind_timeline_pointer
					BEFORE UPDATE OF timeline_current_version ON drafts
					BEGIN SELECT RAISE(ABORT,'forced timeline pointer failure'); END`); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "draft state touch", mode: "conversation",
			setup: func(t *testing.T, database *storage.DB) {
				if _, err := database.Write().ExecContext(t.Context(), `
					CREATE TRIGGER fail_rewind_state_touch
					BEFORE UPDATE OF state_version ON drafts
					BEGIN SELECT RAISE(ABORT,'forced state touch failure'); END`); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "conversation visibility", mode: "conversation",
			setup: func(t *testing.T, database *storage.DB) {
				if _, err := database.Write().ExecContext(t.Context(), `
					CREATE TRIGGER fail_rewind_visibility
					BEFORE UPDATE OF rewound_at ON messages
					BEGIN SELECT RAISE(ABORT,'forced visibility failure'); END`); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "restore checkpoint audit", mode: "timeline", newVersion: 2,
			setup: func(t *testing.T, database *storage.DB) {
				if _, err := database.Write().ExecContext(t.Context(), `
					CREATE TRIGGER fail_restore_checkpoint_audit
					BEFORE INSERT ON rewind_checkpoints WHEN NEW.trigger_kind='restore'
					BEGIN SELECT RAISE(ABORT,'forced checkpoint failure'); END`); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			database := openTestDB(t)
			draftID := "draft-rewind-failure-" + strings.ReplaceAll(test.name, " ", "-")
			createDraft(t, database, draftID)
			createRewindTestTimeline(t, database, draftID, 1, "failure-clip", 30)
			insertRewindTestMessage(t, database, draftID, "failure-anchor", "锚点")
			target := messageCheckpoint(t, database, draftID, "failure-anchor")
			test.setup(t, database)
			draft, _ := storage.GetDraft(t.Context(), database.Read(), draftID)
			payload := map[string]any{
				"checkpoint_id": target.ID, "mode": test.mode,
				"restore_checkpoint_id": "restore-failure-" + test.name,
			}
			if test.newVersion > 0 {
				payload["timeline_version"] = test.newVersion
			}
			if _, err := Apply(t.Context(), database, []contracts.Event{{
				Type: "TimelineVersionRestored", DraftID: draftID, Payload: payload,
			}}, Options{Actor: contracts.ActorUser, BaseVersion: &draft.StateVersion}); err == nil {
				t.Fatal("persistence failure should abort restore")
			}
		})
	}
}

func insertRewindTestMessage(
	t *testing.T,
	database *storage.DB,
	draftID string,
	messageID string,
	content string,
) {
	t.Helper()
	result, err := Apply(t.Context(), database, nil, Options{
		Actor: contracts.ActorUser,
		ResultRows: ResultRows{Message: &MessageRow{
			ID: messageID, DraftID: draftID, Role: "user", Kind: "user", Content: content,
		}},
	})
	if err != nil || result.Status != StatusApplied {
		t.Fatalf("insert message %s result=%#v err=%v", messageID, result, err)
	}
}

func createRewindTestTimeline(
	t *testing.T,
	database *storage.DB,
	draftID string,
	version int,
	clipID string,
	duration int,
) {
	t.Helper()
	draft, _ := storage.GetDraft(t.Context(), database.Read(), draftID)
	base := draft.StateVersion
	document := map[string]any{
		"timeline_id": fmt.Sprintf("%s:v%d", draftID, version), "draft_id": draftID,
		"version": version, "fps": 30, "duration_frames": duration,
		"tracks": []any{map[string]any{
			"track_id": "visual_base", "track_type": "video",
			"clips": []any{map[string]any{"timeline_clip_id": clipID}},
		}},
	}
	result, err := Apply(t.Context(), database, []contracts.Event{{
		Type: "TimelineVersionCreated", DraftID: draftID,
		Payload: map[string]any{
			"timeline_id":      fmt.Sprintf("%s:v%d", draftID, version),
			"timeline_version": version, "patch_id": fmt.Sprintf("patch-%d", version),
			"document_json": document,
		},
	}}, Options{Actor: contracts.ActorAgent, BaseVersion: &base})
	if err != nil || result.Status != StatusApplied {
		t.Fatalf("timeline v%d result=%#v err=%v", version, result, err)
	}
}

func messageCheckpoint(
	t *testing.T,
	database *storage.DB,
	draftID string,
	messageID string,
) storage.RewindCheckpoint {
	t.Helper()
	checkpoint, err := storage.GetRewindCheckpoint(
		t.Context(), database.Read(), draftID, "rewind:message:"+messageID)
	if err != nil {
		t.Fatalf("checkpoint for %s: %v", messageID, err)
	}
	return checkpoint
}

func assertCheckpointVisibleSet(
	t *testing.T,
	database *storage.DB,
	checkpointID string,
	wantMessageIDs ...string,
) {
	t.Helper()
	rows, err := database.Read().QueryContext(t.Context(),
		"SELECT message_id FROM rewind_checkpoint_messages WHERE checkpoint_id=? ORDER BY message_id", checkpointID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		got = append(got, id)
	}
	if strings.Join(got, ",") != strings.Join(wantMessageIDs, ",") {
		t.Fatalf("checkpoint %s visible set=%v want=%v", checkpointID, got, wantMessageIDs)
	}
}

func assertRewindTimeline(
	t *testing.T,
	database *storage.DB,
	draftID string,
	version int,
	parent int,
	clipID string,
) {
	t.Helper()
	var current, storedParent int
	var document string
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT d.timeline_current_version,t.parent_version,t.document_json
		FROM drafts d JOIN timeline_versions t
		ON t.draft_id=d.draft_id AND t.version=d.timeline_current_version
		WHERE d.draft_id=?`, draftID).Scan(&current, &storedParent, &document); err != nil {
		t.Fatal(err)
	}
	if current != version || storedParent != parent || !containsAll(document,
		fmt.Sprintf(`"version":%d`, version), `"timeline_clip_id":"`+clipID+`"`) {
		t.Fatalf("current=%d parent=%d document=%s", current, storedParent, document)
	}
}

func containsAll(value string, fragments ...string) bool {
	for _, fragment := range fragments {
		if !strings.Contains(value, fragment) {
			return false
		}
	}
	return true
}
