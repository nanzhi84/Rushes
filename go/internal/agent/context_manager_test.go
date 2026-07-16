package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
)

func TestContextManagerKeepsReferenceSnapshotAndInjectsObjectiveMergePatch(t *testing.T) {
	t.Parallel()
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_context_reference")
	insertContextMessage(t, database, storage.Message{
		ID: "user_reference", DraftID: "draft_context_reference",
		Role: "user", Kind: "user", Content: "做成克制的电影感",
	})
	manager := NewContextManager(database)
	first, err := manager.Build(t.Context(), "draft_context_reference")
	if err != nil {
		t.Fatal(err)
	}
	if first.Manifest.HasWorldStatePatch || first.Manifest.WindowNumber != 1 ||
		len(first.Messages) != 2 || first.Messages[0].Role != schema.System ||
		first.Messages[1].Role != schema.User {
		t.Fatalf("first=%#v messages=%#v", first.Manifest, first.Messages)
	}
	if !strings.Contains(first.Messages[0].Content, `"name":"draft_context_reference"`) {
		t.Fatalf("reference missing initial name: %s", first.Messages[0].Content)
	}

	draft, err := storage.GetDraft(t.Context(), database.Read(), "draft_context_reference")
	if err != nil {
		t.Fatal(err)
	}
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "DraftRenamed", DraftID: draft.ID,
		Payload: map[string]any{"name": "新片名"},
	}}, reducer.Options{Actor: contracts.ActorUser, BaseVersion: &draft.StateVersion})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("rename=%#v err=%v", result, err)
	}
	insertContextMessage(t, database, storage.Message{
		ID: "tool_hidden", DraftID: draft.ID, Role: "system", Kind: "tool",
		Content: `{"step_id":"secret","args_summary":"raw"}`,
	})
	insertContextMessage(t, database, storage.Message{
		ID: "assistant_reference", DraftID: draft.ID,
		Role: "assistant", Kind: "reply", Content: "旧片名已经完成。",
	})

	second, err := manager.Build(t.Context(), draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Manifest.HasWorldStatePatch || second.Manifest.WindowID != first.Manifest.WindowID ||
		second.Manifest.WindowNumber != 1 ||
		second.Manifest.ReferenceHash != first.Manifest.ReferenceHash {
		t.Fatalf("second manifest=%#v first=%#v", second.Manifest, first.Manifest)
	}
	if len(second.Messages) != 4 || second.Messages[1].Extra["context_phase"] != "world_state_update" ||
		second.Messages[3].Role != schema.Assistant ||
		second.Messages[3].Extra["historical_narrative"] != true {
		t.Fatalf("messages=%#v", second.Messages)
	}
	if strings.Contains(second.Messages[0].Content, "新片名") ||
		!strings.Contains(second.Messages[1].Content, "新片名") ||
		strings.Contains(joinMessageContent(second.Messages), "secret") {
		t.Fatalf("reference/update/tool filtering failed: %#v", second.Messages)
	}

	base, err := WorldStateSnapshotFromMap(second.Checkpoint.BaseSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	patch, err := base.MergePatchTo(second.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	baseMap, _ := base.Map()
	currentMap, _ := second.Snapshot.Map()
	if reconstructed := applyMergePatch(baseMap, patch); !reflect.DeepEqual(reconstructed, currentMap) {
		t.Fatalf("merge patch cannot reconstruct current\npatch=%#v\nwant=%#v\ngot=%#v", patch, currentMap, reconstructed)
	}
}

func TestContextManagerRebasesOversizedWorldStatePatch(t *testing.T) {
	t.Parallel()
	database := agentTestDatabase(t)
	const draftID = "draft_context_large_patch"
	createAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)

	selections := make([]timeline.Selection, 0, 51)
	for index := 0; index < 51; index++ {
		selections = append(selections, timeline.Selection{
			AssetID: fmt.Sprintf("asset_large_%02d", index), AssetKind: "video",
			SourceStartFrame: 0, SourceEndFrame: 30, Role: "b_roll",
		})
	}
	document, err := timeline.ComposeInitial(draftID, 1, selections)
	if err != nil {
		t.Fatal(err)
	}
	if persisted, persistErr := service.persistTimeline(
		t.Context(), draftID, document, "large_context_fixture",
	); persistErr != nil || persisted.Status != "succeeded" {
		t.Fatalf("persist initial=%#v err=%v", persisted, persistErr)
	}

	manager := NewContextManager(database)
	first, err := manager.Build(t.Context(), draftID)
	if err != nil {
		t.Fatal(err)
	}
	document.Version = 2
	document.TimelineID = draftID + ":v2"
	document.Tracks[0].Clips[0].GainDB = -6
	if persisted, persistErr := service.persistTimeline(
		t.Context(), draftID, document, "large_context_edit",
	); persistErr != nil || persisted.Status != "succeeded" {
		t.Fatalf("persist edit=%#v err=%v", persisted, persistErr)
	}

	second, err := manager.Build(t.Context(), draftID)
	if err != nil {
		t.Fatal(err)
	}
	if first.Manifest.WindowNumber != 1 || second.Manifest.WindowNumber != 2 ||
		second.Manifest.WindowID == first.Manifest.WindowID || second.Manifest.HasWorldStatePatch ||
		second.Manifest.ReferenceHash != second.Manifest.CurrentHash {
		t.Fatalf("first=%#v second=%#v", first.Manifest, second.Manifest)
	}
	stored, err := storage.GetAgentContextCheckpoint(t.Context(), database.Read(), draftID)
	if err != nil || stored.BaseSnapshotHash != second.Manifest.ReferenceHash ||
		stored.WindowID != second.Manifest.WindowID || stored.WindowNumber != 2 {
		t.Fatalf("stored=%#v second=%#v err=%v", stored, second.Manifest, err)
	}
	storedBase, err := WorldStateSnapshotFromMap(stored.BaseSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	storedHash, err := storedBase.Hash()
	if err != nil || storedHash != second.Manifest.CurrentHash {
		t.Fatalf("stored hash=%s current=%s err=%v", storedHash, second.Manifest.CurrentHash, err)
	}
	third, err := manager.Build(t.Context(), draftID)
	if err != nil {
		t.Fatal(err)
	}
	if third.Manifest.WindowID != second.Manifest.WindowID || third.Manifest.WindowNumber != 2 ||
		third.Manifest.HasWorldStatePatch {
		t.Fatalf("rebase must be idempotent: second=%#v third=%#v", second.Manifest, third.Manifest)
	}
}

func TestContextManagerKeepsSmallTimelinePatchIncremental(t *testing.T) {
	t.Parallel()
	database := agentTestDatabase(t)
	const draftID = "draft_context_small_patch"
	createAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	document, err := timeline.ComposeInitial(draftID, 1, []timeline.Selection{{
		AssetID: "asset_small", AssetKind: "video",
		SourceStartFrame: 0, SourceEndFrame: 30, Role: "a_roll",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if persisted, persistErr := service.persistTimeline(
		t.Context(), draftID, document, "small_context_fixture",
	); persistErr != nil || persisted.Status != "succeeded" {
		t.Fatalf("persist initial=%#v err=%v", persisted, persistErr)
	}
	manager := NewContextManager(database)
	first, err := manager.Build(t.Context(), draftID)
	if err != nil {
		t.Fatal(err)
	}
	document.Version = 2
	document.TimelineID = draftID + ":v2"
	document.Tracks[0].Clips[0].GainDB = -3
	if persisted, persistErr := service.persistTimeline(
		t.Context(), draftID, document, "small_context_edit",
	); persistErr != nil || persisted.Status != "succeeded" {
		t.Fatalf("persist edit=%#v err=%v", persisted, persistErr)
	}
	base, err := WorldStateSnapshotFromMap(first.Checkpoint.BaseSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	current, err := manager.builder.Snapshot(t.Context(), draftID)
	if err != nil {
		t.Fatal(err)
	}
	patch, err := base.MergePatchTo(current)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(patch)
	if err != nil {
		t.Fatal(err)
	}
	if runes := utf8.RuneCount(raw); runes >= contextWorldStatePatchRebaseFloor {
		t.Fatalf("small patch runes=%d patch=%s", runes, raw)
	}
	second, err := manager.Build(t.Context(), draftID)
	if err != nil {
		t.Fatal(err)
	}
	if second.Manifest.WindowID != first.Manifest.WindowID || second.Manifest.WindowNumber != 1 ||
		!second.Manifest.HasWorldStatePatch ||
		second.Manifest.ReferenceHash != first.Manifest.ReferenceHash ||
		second.Manifest.ReferenceHash == second.Manifest.CurrentHash {
		t.Fatalf("first=%#v second=%#v", first.Manifest, second.Manifest)
	}
}

func TestContextManagerRebuildsWorldStateAfterRewindWithoutHashMismatch(t *testing.T) {
	t.Parallel()
	database := agentTestDatabase(t)
	const draftID = "draft_context_rewind"
	createAgentDraft(t, database, draftID)
	insertContextMessage(t, database, storage.Message{
		ID: "rewind_user_one", DraftID: draftID, Role: "user", Kind: "user", Content: "保留第一版",
	})
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	document, err := timeline.ComposeInitial(draftID, 1, []timeline.Selection{{
		AssetID: "asset_rewind", AssetKind: "video",
		SourceStartFrame: 0, SourceEndFrame: 30, Role: "a_roll",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if persisted, persistErr := service.persistTimeline(
		t.Context(), draftID, document, "rewind_context_initial",
	); persistErr != nil || persisted.Status != "succeeded" {
		t.Fatalf("persist initial=%#v err=%v", persisted, persistErr)
	}
	manager := NewContextManager(database)
	before, err := manager.Build(t.Context(), draftID)
	if err != nil {
		t.Fatal(err)
	}
	insertContextMessage(t, database, storage.Message{
		ID: "rewind_user_two", DraftID: draftID, Role: "user", Kind: "user", Content: "把声音压低",
	})
	document.Version = 2
	document.TimelineID = draftID + ":v2"
	document.Tracks[0].Clips[0].GainDB = -3
	if persisted, persistErr := service.persistTimeline(
		t.Context(), draftID, document, "rewind_context_edit",
	); persistErr != nil || persisted.Status != "succeeded" {
		t.Fatalf("persist edit=%#v err=%v", persisted, persistErr)
	}
	if _, err := manager.Build(t.Context(), draftID); err != nil {
		t.Fatalf("build edited state: %v", err)
	}
	checkpoints, err := storage.ListRewindCheckpoints(t.Context(), database.Read(), draftID, 50)
	if err != nil {
		t.Fatal(err)
	}
	var target storage.RewindCheckpoint
	for _, checkpoint := range checkpoints {
		if checkpoint.TriggerKind == "timeline_write" && checkpoint.TimelineVersion != nil &&
			*checkpoint.TimelineVersion == 1 {
			target = checkpoint
			break
		}
	}
	if target.ID == "" {
		t.Fatalf("missing rewind target: %#v", checkpoints)
	}
	draft, err := storage.GetDraft(t.Context(), database.Read(), draftID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "TimelineVersionRestored", DraftID: draftID,
		Payload: map[string]any{
			"checkpoint_id": target.ID, "mode": "both", "timeline_version": 3,
			"restore_checkpoint_id": "rewind_context_restore",
		},
	}}, reducer.Options{
		Actor: contracts.ActorUser, BaseVersion: &draft.StateVersion,
		ResultRows: reducer.ResultRows{Message: &reducer.MessageRow{
			ID: "rewind_context_observation", DraftID: draftID,
			Role: "system_observation", Kind: "rewind", Content: "已恢复第一版",
		}},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("restore=%#v err=%v", result, err)
	}
	after, err := manager.Build(t.Context(), draftID)
	if err != nil {
		t.Fatalf("build restored state: %v", err)
	}
	hash, err := after.Snapshot.Hash()
	if err != nil || hash != after.Manifest.CurrentHash {
		t.Fatalf("restored hash=%s manifest=%s err=%v", hash, after.Manifest.CurrentHash, err)
	}
	raw, err := after.Snapshot.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"gain_db":-3`) ||
		!strings.Contains(string(raw), `"timeline_clip_id":"clip_v1_001"`) {
		t.Fatalf("restored WorldState=%s", raw)
	}
	history := joinMessageContent(after.Messages)
	if strings.Contains(history, "把声音压低") || !strings.Contains(history, "已恢复第一版") {
		t.Fatalf("restored history=%s", history)
	}
	if after.Manifest.WindowID == before.Manifest.WindowID {
		t.Fatalf("conversation rewind must rebuild context window: before=%s after=%s",
			before.Manifest.WindowID, after.Manifest.WindowID)
	}
}

func TestContextManagerKeepsCurrentQueuedUserLastAndHidesFutureUsers(t *testing.T) {
	t.Parallel()
	database := agentTestDatabase(t)
	const draftID = "draft_context_queue_boundary"
	createAgentDraft(t, database, draftID)
	for _, message := range []storage.Message{
		{ID: "user_a", DraftID: draftID, Role: "user", Kind: "user", Content: "第一条"},
		{ID: "user_b", DraftID: draftID, Role: "user", Kind: "user", Content: "当前第二条"},
		{ID: "user_c", DraftID: draftID, Role: "user", Kind: "user", Content: "未来第三条"},
		// FIFO 中第一条的回复可能晚于后续 user 入库；当前 user 仍必须放在模型历史末尾。
		{ID: "assistant_a", DraftID: draftID, Role: "assistant", Kind: "reply", Content: "第一条完成"},
	} {
		insertContextMessage(t, database, message)
	}
	build, err := NewContextManager(database).BuildThroughMessage(t.Context(), draftID, "user_b")
	if err != nil {
		t.Fatal(err)
	}
	if len(build.Messages) != 4 || build.Messages[1].Content != "第一条" ||
		build.Messages[2].Role != schema.Assistant ||
		build.Messages[3].Content != "当前第二条" ||
		strings.Contains(joinMessageContent(build.Messages), "未来第三条") {
		t.Fatalf("queued context order=%#v", build.Messages)
	}
}

func TestServiceCompactionReplacesHistoryAndPreservesPendingUser(t *testing.T) {
	t.Parallel()
	database := agentTestDatabase(t)
	const draftID = "draft_context_compaction"
	createAgentDraft(t, database, draftID)
	for index := 0; index < 25; index++ {
		insertContextMessage(t, database, storage.Message{
			ID: fmt.Sprintf("user_%02d", index), DraftID: draftID,
			Role: "user", Kind: "user", Content: fmt.Sprintf("旧目标 %02d", index),
		})
		insertContextMessage(t, database, storage.Message{
			ID: fmt.Sprintf("assistant_%02d", index), DraftID: draftID,
			Role: "assistant", Kind: "reply", Content: fmt.Sprintf("旧回复 %02d", index),
		})
	}
	insertContextMessage(t, database, storage.Message{
		ID: "user_pending", DraftID: draftID, Role: "user", Kind: "user",
		Content: "最新指令必须留在压缩摘要之外",
	})
	chatModel := &decisionContinuationModel{}
	service, err := NewService(t.Context(), database, chatModel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)

	messages, err := service.modelMessages(t.Context(), draftID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 || messages[0].Role != schema.System ||
		messages[1].Extra["context_phase"] != "compaction_replacement" ||
		messages[2].Role != schema.User || messages[2].Content != "最新指令必须留在压缩摘要之外" {
		t.Fatalf("replacement messages=%#v", messages)
	}
	joined := joinMessageContent(messages)
	if !strings.Contains(joined, "DECISION-CONTINUED") || strings.Contains(joined, "旧目标 24") ||
		strings.Contains(joined, "旧回复 24") {
		t.Fatalf("history was not replaced: %s", joined)
	}
	checkpoint, err := storage.GetAgentContextCheckpoint(t.Context(), database.Read(), draftID)
	if err != nil || checkpoint.WindowNumber != 2 || checkpoint.CompactedThroughMessageID == nil ||
		*checkpoint.CompactedThroughMessageID != "assistant_24" || checkpoint.HistoryVersion != 51 {
		t.Fatalf("checkpoint=%#v err=%v", checkpoint, err)
	}
	visible, err := storage.ListMessages(t.Context(), database.Read(), draftID, 200)
	if err != nil || len(visible) != 51 {
		t.Fatalf("visible history must remain intact: len=%d err=%v", len(visible), err)
	}
}

func TestCJKHistoryTriggersCompactionAndPreservesAssistantBoundary(t *testing.T) {
	t.Parallel()
	database := agentTestDatabase(t)
	const draftID = "draft_context_cjk_compaction"
	createAgentDraft(t, database, draftID)
	content := strings.Repeat("中", contextHistorySoftTokenLimit+1)
	legacyEstimate := (len([]byte(content)) + 3) / 4
	if legacyEstimate >= contextHistorySoftTokenLimit {
		t.Fatalf("fixture must stay below legacy estimate: %d", legacyEstimate)
	}
	insertContextMessage(t, database, storage.Message{
		ID: "assistant_cjk", DraftID: draftID,
		Role: "assistant", Kind: "reply", Content: content,
	})
	insertContextMessage(t, database, storage.Message{
		ID: "user_cjk_pending", DraftID: draftID,
		Role: "user", Kind: "user", Content: "保留在压缩摘要外",
	})
	build, err := NewContextManager(database).Build(t.Context(), draftID)
	if err != nil {
		t.Fatal(err)
	}
	if !build.Manifest.NeedsCompaction {
		t.Fatalf("CJK history should compact: %#v", build.Manifest)
	}
	_, through, ok := build.CompactionSource(true)
	if !ok || through == nil || *through != "assistant_cjk" {
		t.Fatalf("through=%v ok=%v", through, ok)
	}
	chatModel := &decisionContinuationModel{}
	service, err := NewService(t.Context(), database, chatModel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	messages, err := service.modelMessages(t.Context(), draftID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 || messages[1].Extra["context_phase"] != "compaction_replacement" ||
		!strings.Contains(messages[1].Content, "DECISION-CONTINUED") ||
		messages[2].Role != schema.User || messages[2].Content != "保留在压缩摘要外" {
		t.Fatalf("messages=%#v", messages)
	}
	checkpoint, err := storage.GetAgentContextCheckpoint(t.Context(), database.Read(), draftID)
	if err != nil || checkpoint.WindowNumber != 2 || checkpoint.CompactedThroughMessageID == nil ||
		*checkpoint.CompactedThroughMessageID != "assistant_cjk" {
		t.Fatalf("checkpoint=%#v err=%v", checkpoint, err)
	}
}

func TestApproximateTokensCalibratesCJKAndPreservesASCIIEstimate(t *testing.T) {
	t.Parallel()
	cjk := strings.Repeat("中文、かな。Ａ", 100)
	cjkRunes := utf8.RuneCountInString(cjk)
	cjkTokens := approximateTokens(cjk)
	if cjkTokens < cjkRunes || cjkTokens > cjkRunes+cjkRunes/10 {
		t.Fatalf("CJK tokens=%d runes=%d", cjkTokens, cjkRunes)
	}
	ascii := strings.Repeat("abcd", 37) + "x"
	asciiWant := (len([]byte(ascii)) + 3) / 4
	if got := approximateTokens(ascii); got != asciiWant {
		t.Fatalf("ASCII tokens=%d want=%d", got, asciiWant)
	}
	if got, want := approximateTokens(cjk+ascii),
		approximateTokens(cjk)+approximateTokens(ascii); got != want {
		t.Fatalf("mixed tokens=%d want=%d", got, want)
	}
}

func TestConversationClearDeletesContextWindowButPreservesWorldState(t *testing.T) {
	t.Parallel()
	database := agentTestDatabase(t)
	const draftID = "draft_context_clear"
	createAgentDraft(t, database, draftID)
	insertContextMessage(t, database, storage.Message{
		ID: "user_before_clear", DraftID: draftID, Role: "user", Kind: "user", Content: "旧目标",
	})
	manager := NewContextManager(database)
	before, err := manager.Build(t.Context(), draftID)
	if err != nil {
		t.Fatal(err)
	}
	draft, err := storage.GetDraft(t.Context(), database.Read(), draftID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "ConversationContextCleared", DraftID: draftID,
		Payload: map[string]any{"message_id": "context_clear"},
	}}, reducer.Options{
		Actor: contracts.ActorUser, BaseVersion: &draft.StateVersion,
		ResultRows: reducer.ResultRows{Message: &reducer.MessageRow{
			ID: "context_clear", DraftID: draftID, Role: "system_observation",
			Kind: "context_reset", Content: "已清空",
		}},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("clear=%#v err=%v", result, err)
	}
	if _, err := storage.GetAgentContextCheckpoint(t.Context(), database.Read(), draftID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("checkpoint should be deleted: %v", err)
	}
	after, err := manager.Build(t.Context(), draftID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Manifest.WindowID == before.Manifest.WindowID || after.Manifest.HistoryItems != 0 ||
		len(after.Messages) != 1 || !strings.Contains(after.Messages[0].Content, `"reset":true`) {
		t.Fatalf("after clear=%#v messages=%#v", after.Manifest, after.Messages)
	}
}

func TestWorldStateSnapshotRejectsMalformedCheckpoint(t *testing.T) {
	t.Parallel()
	if _, err := WorldStateSnapshotFromMap(map[string]any{"schema_version": 99}); err == nil {
		t.Fatal("invalid world state must fail")
	}
	if _, err := WorldStateSnapshotFromMap(map[string]any{
		"schema_version": "bad", "sections": map[string]any{},
	}); err == nil {
		t.Fatal("wrong schema_version type must fail")
	}
	base := NewWorldStateSnapshot(map[string]any{
		"draft": map[string]any{"name": "a", "obsolete": true},
	})
	current := NewWorldStateSnapshot(map[string]any{
		"draft": map[string]any{"name": "b", "added": true},
	})
	patch, err := base.MergePatchTo(current)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(patch)
	if !strings.Contains(string(raw), `"obsolete":null`) || !strings.Contains(string(raw), `"added":true`) {
		t.Fatalf("deletion must use RFC7396 null: %s", raw)
	}
	baseMap, _ := base.Map()
	currentMap, _ := current.Map()
	if reconstructed := applyMergePatch(baseMap, patch); !reflect.DeepEqual(reconstructed, currentMap) {
		t.Fatalf("deletion patch reconstruction=%#v want=%#v", reconstructed, currentMap)
	}
}

func TestContextManagerRepairsCorruptPersistedCheckpoint(t *testing.T) {
	t.Parallel()
	database := agentTestDatabase(t)
	const draftID = "draft_context_corrupt"
	createAgentDraft(t, database, draftID)
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO agent_context_checkpoints(
			draft_id,window_id,window_number,history_version,
			base_snapshot_json,base_snapshot_hash,summary,created_at,updated_at
		) VALUES(?, 'broken', 7, 9, '{', 'wrong', 'stale', ?, ?)`,
		draftID, "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z",
	); err != nil {
		t.Fatal(err)
	}
	build, err := NewContextManager(database).Build(t.Context(), draftID)
	if err != nil {
		t.Fatal(err)
	}
	if build.Manifest.WindowID == "broken" || build.Manifest.WindowNumber != 1 ||
		build.Checkpoint.Summary != "" {
		t.Fatalf("corrupt checkpoint was not replaced: %#v", build)
	}
	stored, err := storage.GetAgentContextCheckpoint(t.Context(), database.Read(), draftID)
	if err != nil || stored.BaseSnapshotHash != build.Manifest.ReferenceHash {
		t.Fatalf("stored=%#v err=%v", stored, err)
	}
}

func TestContextManagerRebasesCheckpointWithMismatchedHash(t *testing.T) {
	t.Parallel()
	database := agentTestDatabase(t)
	const draftID = "draft_context_hash_mismatch"
	createAgentDraft(t, database, draftID)
	manager := NewContextManager(database)
	first, err := manager.Build(t.Context(), draftID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write().ExecContext(t.Context(), `
		UPDATE agent_context_checkpoints SET base_snapshot_hash='wrong' WHERE draft_id=?`, draftID,
	); err != nil {
		t.Fatal(err)
	}
	second, err := manager.Build(t.Context(), draftID)
	if err != nil {
		t.Fatal(err)
	}
	if second.Manifest.WindowID == first.Manifest.WindowID || second.Manifest.WindowNumber != 2 ||
		second.Manifest.ReferenceHash != second.Manifest.CurrentHash || second.Manifest.HasWorldStatePatch {
		t.Fatalf("checkpoint was not rebased: first=%#v second=%#v", first.Manifest, second.Manifest)
	}
}

func TestContextManagerDefensiveBranchesAndCompactionBudget(t *testing.T) {
	t.Parallel()
	bad := WorldStateSnapshot{
		SchemaVersion: worldStateSchemaVersion,
		Sections:      map[string]any{"bad": make(chan int)},
	}
	valid := NewWorldStateSnapshot(map[string]any{"draft": map[string]any{"name": "ok"}})
	if got := NewWorldStateSnapshot(map[string]any{"bad": make(chan int)}); got.Sections["bad"] == nil {
		t.Fatal("marshal failure must retain original snapshot for caller-visible error")
	}
	if _, err := WorldStateSnapshotFromMap(map[string]any{"bad": make(chan int)}); err == nil {
		t.Fatal("unsupported checkpoint value must fail")
	}
	if _, err := bad.Map(); err == nil {
		t.Fatal("bad snapshot map must fail")
	}
	if _, err := bad.Hash(); err == nil {
		t.Fatal("bad snapshot hash must fail")
	}
	if _, err := bad.MergePatchTo(valid); err == nil {
		t.Fatal("bad base merge patch must fail")
	}
	if _, err := valid.MergePatchTo(bad); err == nil {
		t.Fatal("bad target merge patch must fail")
	}
	if rebased, err := shouldRebaseWorldStatePatch(valid, nil); err != nil || rebased {
		t.Fatalf("empty patch rebased=%v err=%v", rebased, err)
	}
	if _, err := shouldRebaseWorldStatePatch(bad, map[string]any{"changed": true}); err == nil {
		t.Fatal("bad base rebase sizing must fail")
	}
	if _, err := shouldRebaseWorldStatePatch(
		valid, map[string]any{"bad": make(chan int)},
	); err == nil {
		t.Fatal("bad patch rebase sizing must fail")
	}
	largeBase := NewWorldStateSnapshot(map[string]any{
		"large": strings.Repeat("基", 10000),
	})
	ratioPatch := map[string]any{"small": strings.Repeat("补", 2100)}
	if rebased, err := shouldRebaseWorldStatePatch(largeBase, ratioPatch); err != nil || rebased {
		t.Fatalf("sub-ratio patch rebased=%v err=%v", rebased, err)
	}
	if got := applyMergePatch(42, map[string]any{"name": "value"}); !reflect.DeepEqual(got, map[string]any{"name": "value"}) {
		t.Fatalf("non-object target=%#v", got)
	}
	if got := applyMergePatch(map[string]any{"old": true}, "replacement"); got != "replacement" {
		t.Fatalf("scalar patch=%#v", got)
	}

	filtered := normalizeContextHistory([]storage.Message{
		{Role: "user", Kind: "user", Content: "  "},
		{Role: "system", Kind: "observation", Content: "hidden"},
		{Role: "system", Kind: "context_reset", Content: "hidden"},
		{Role: "assistant", Kind: "other", Content: "hidden"},
		{Role: "unknown", Kind: "reply", Content: "hidden"},
	})
	if len(filtered) != 0 {
		t.Fatalf("unexpected normalized history=%#v", filtered)
	}
	checkpoint := storage.AgentContextCheckpoint{
		WindowID: "window", WindowNumber: 1, BaseSnapshotHash: "hash",
	}
	if _, err := renderContextMessages(bad, valid, nil, checkpoint, nil); err == nil {
		t.Fatal("bad reference rendering must fail")
	}
	if _, err := renderContextMessages(valid, bad, nil, checkpoint, nil); err == nil {
		t.Fatal("bad current rendering must fail")
	}
	if _, err := renderContextMessages(
		valid, valid, map[string]any{"bad": make(chan int)}, checkpoint, nil,
	); err == nil {
		t.Fatal("bad patch rendering must fail")
	}

	emptyBuild := ContextBuild{}
	if _, _, ok := emptyBuild.CompactionSource(true); ok {
		t.Fatal("empty history cannot compact")
	}
	onlyPending := ContextBuild{history: []contextHistoryItem{
		{row: storage.Message{ID: "pending_a", Role: "user", Content: "latest a"}},
		{row: storage.Message{ID: "pending_b", Role: "user", Content: "latest b"}},
	}}
	if _, _, ok := onlyPending.CompactionSource(true); ok {
		t.Fatal("pending user must stay outside compaction")
	}
	longHistory := make([]contextHistoryItem, 0, 45)
	for index := 0; index < 45; index++ {
		longHistory = append(longHistory, contextHistoryItem{row: storage.Message{
			ID: fmt.Sprintf("long_%02d", index), Role: "assistant",
			Content: strings.Repeat("镜", 2000),
		}})
	}
	budgeted := ContextBuild{
		Checkpoint: storage.AgentContextCheckpoint{Summary: strings.Repeat("旧", 9000)},
		history:    longHistory,
	}
	source, through, ok := budgeted.CompactionSource(false)
	if !ok || through == nil || *through != "long_44" ||
		len([]rune(source)) > contextCompactionRuneBudget+100 {
		t.Fatalf("budgeted source runes=%d through=%v ok=%v", len([]rune(source)), through, ok)
	}

	database := agentTestDatabase(t)
	manager := NewContextManager(database)
	if _, err := manager.newCheckpoint(t.Context(), "missing", bad, 1, 1, "", nil); err == nil {
		t.Fatal("bad snapshot checkpoint must fail")
	}
	if err := manager.persistCheckpoint(t.Context(), storage.AgentContextCheckpoint{}); err == nil {
		t.Fatal("invalid checkpoint must fail in reducer")
	}
	if err := manager.ReplaceHistory(t.Context(), "missing", ContextBuild{}, "summary", nil); err == nil {
		t.Fatal("nil compaction boundary must fail")
	}
	emptyBoundary := " "
	if err := manager.ReplaceHistory(
		t.Context(), "missing", ContextBuild{}, "summary", &emptyBoundary,
	); err == nil {
		t.Fatal("empty compaction boundary must fail")
	}
	validBoundary := "message"
	missingDraftBuild := ContextBuild{
		Checkpoint: storage.AgentContextCheckpoint{HistoryVersion: 1},
		history: []contextHistoryItem{{row: storage.Message{
			ID: validBoundary, Role: "assistant", Content: "done",
		}}},
	}
	if err := manager.ReplaceHistory(
		t.Context(), "missing", missingDraftBuild, "summary", &validBoundary,
	); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("missing draft replacement error=%v", err)
	}
	unknownBoundary := "unknown"
	if err := manager.ReplaceHistory(
		t.Context(), "missing", missingDraftBuild, "summary", &unknownBoundary,
	); err == nil || !strings.Contains(err.Error(), "不在当前窗口") {
		t.Fatalf("unknown boundary error=%v", err)
	}

	closedDatabase := agentTestDatabase(t)
	if err := closedDatabase.Close(); err != nil {
		t.Fatal(err)
	}
	closedManager := NewContextManager(closedDatabase)
	if _, err := closedManager.newCheckpoint(
		t.Context(), "closed", valid, 1, 1, "", nil,
	); err == nil {
		t.Fatal("closed checkpoint persistence must fail")
	}

	builderDatabase := agentTestDatabase(t)
	createAgentDraft(t, builderDatabase, "draft_separate_builder")
	separate := &ContextManager{
		database: closedDatabase, builder: NewContextBuilder(builderDatabase),
		historyTokenLimit: contextHistorySoftTokenLimit,
		historyItemLimit:  contextHistoryItemLimit,
	}
	if _, err := separate.Build(t.Context(), "draft_separate_builder"); err == nil {
		t.Fatal("checkpoint read failure must surface")
	}
}

func insertContextMessage(t *testing.T, database *storage.DB, message storage.Message) {
	t.Helper()
	actor := contracts.ActorAgent
	if message.Role == "user" {
		actor = contracts.ActorUser
	}
	result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: actor,
		ResultRows: reducer.ResultRows{Message: &reducer.MessageRow{
			ID: message.ID, DraftID: message.DraftID, Role: message.Role,
			Kind: message.Kind, Content: message.Content,
		}},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("insert message=%#v status=%s err=%v", message, result.Status, err)
	}
}

func joinMessageContent(messages []*schema.Message) string {
	parts := make([]string, 0, len(messages))
	for _, message := range messages {
		parts = append(parts, message.Content)
	}
	return strings.Join(parts, "\n")
}

// applyMergePatch 是 mergePatchDifference 的逆运算，仅用于测试中校验
// “基准 + 补丁 == 当前状态” 的往返一致性。
func applyMergePatch(target, patch any) any {
	patchMap, patchObject := patch.(map[string]any)
	if !patchObject {
		return patch
	}
	targetMap, targetObject := target.(map[string]any)
	if !targetObject {
		targetMap = map[string]any{}
	} else {
		copyMap := make(map[string]any, len(targetMap))
		for key, value := range targetMap {
			copyMap[key] = value
		}
		targetMap = copyMap
	}
	for key, value := range patchMap {
		if value == nil {
			delete(targetMap, key)
			continue
		}
		targetMap[key] = applyMergePatch(targetMap[key], value)
	}
	return targetMap
}
