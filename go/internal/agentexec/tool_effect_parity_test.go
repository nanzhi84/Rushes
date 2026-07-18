package agentexec

import (
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

// TestToolEffectMatchesExecutorWriteFootprint 抽查工具的 Effect 分级与执行器真实写行为一致
// （#103 G1 验收）：只读工具执行后事件日志零增长；可逆工具经 reducer 落盘（事件或 ResultRows）；
// 破坏性 memory.update 的 remove_keys 真正删记忆。Effect 事实源取自由同一执行器构造的注册表，
// 与被执行的方法闭环对拍。
func TestToolEffectMatchesExecutorWriteFootprint(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := rushestools.NewRegistry(database, exec)
	if err != nil {
		t.Fatal(err)
	}
	effectOf := func(name string) rushestools.Effect {
		effect, ok := registry.Effect(name)
		if !ok {
			t.Fatalf("%s 未注册 Effect", name)
		}
		return effect
	}
	countEvents := func(draftID string) int {
		events, err := storage.ListEventsAfter(t.Context(), database.Read(), 0, &draftID, 100000)
		if err != nil {
			t.Fatal(err)
		}
		return len(events)
	}

	t.Run("read-only tools leave the event log untouched", func(t *testing.T) {
		const draftID = "draft_effect_readonly"
		agenttest.CreateAgentDraft(t, database, draftID)
		ctx := rushestools.WithDraftID(t.Context(), draftID)
		for _, name := range []string{"asset.list_assets", "timeline.inspect"} {
			if effect := effectOf(name); effect != rushestools.EffectReadOnly {
				t.Fatalf("%s Effect=%q，期望 read_only", name, effect)
			}
		}
		before := countEvents(draftID)
		if _, err := exec.ToolListAssets(ctx, draftID, rushestools.AssetListInput{}); err != nil {
			t.Fatal(err)
		}
		if _, err := exec.toolInspectTimeline(ctx, draftID, rushestools.TimelineInspectInput{}); err != nil {
			t.Fatal(err)
		}
		if after := countEvents(draftID); after != before {
			t.Fatalf("只读工具写入了事件日志: before=%d after=%d", before, after)
		}
	})

	t.Run("reversible timeline.validate appends a validation event via the reducer", func(t *testing.T) {
		const draftID = "draft_effect_validate"
		agenttest.CreateAgentDraft(t, database, draftID)
		if effect := effectOf("timeline.validate"); effect != rushestools.EffectReversible {
			t.Fatalf("timeline.validate Effect=%q，期望 reversible", effect)
		}
		document, err := timeline.ComposeInitial(draftID, 1, []timeline.Selection{{
			AssetID: "asset_video", AssetKind: "video", SourceStartFrame: 0, SourceEndFrame: 90,
		}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := exec.PersistTimeline(t.Context(), draftID, document, "effect_validate_fixture"); err != nil {
			t.Fatal(err)
		}
		before := countEvents(draftID)
		if _, err := exec.toolValidateTimeline(t.Context(), draftID); err != nil {
			t.Fatal(err)
		}
		if after := countEvents(draftID); after <= before {
			t.Fatalf("timeline.validate 未写入校验事件: before=%d after=%d", before, after)
		}
	})

	t.Run("reversible plan.update persists through the reducer result rows", func(t *testing.T) {
		const draftID = "draft_effect_plan"
		agenttest.CreateAgentDraft(t, database, draftID)
		if effect := effectOf("plan.update"); effect != rushestools.EffectReversible {
			t.Fatalf("plan.update Effect=%q，期望 reversible", effect)
		}
		ctx := rushestools.WithDraftID(t.Context(), draftID)
		if _, err := exec.toolPlanUpdate(ctx, draftID, rushestools.PlanUpdateInput{
			Plan: map[string]any{"pacing": "fast"},
		}); err != nil {
			t.Fatal(err)
		}
		draft, err := storage.GetDraft(t.Context(), database.Read(), draftID)
		if err != nil {
			t.Fatal(err)
		}
		if draft.ContentPlan["pacing"] != "fast" {
			t.Fatalf("plan.update 未落盘创作计划: %#v", draft.ContentPlan)
		}
	})

	t.Run("destructive memory.update remove_keys deletes the stored memory", func(t *testing.T) {
		const draftID = "draft_effect_memory"
		agenttest.CreateAgentDraft(t, database, draftID)
		agenttest.InsertAgentMessage(t, database, draftID, "message_effect_memory", "以后都快一点")
		if effect := effectOf("memory.update"); effect != rushestools.EffectDestructive {
			t.Fatalf("memory.update Effect=%q，期望 destructive", effect)
		}
		ctx := rushestools.WithDraftID(
			WithMemoryEvidence(t.Context(), storage.UserMemoryEvidenceMessage, "message_effect_memory"),
			draftID,
		)
		if _, err := exec.toolMemoryUpdate(ctx, draftID, rushestools.MemoryUpdateInput{
			Entries: []rushestools.MemoryEntryInput{{
				Key: "pacing", Kind: "preference", Statement: "成片节奏偏快", EvidenceQuote: "都快一点",
			}},
		}); err != nil {
			t.Fatal(err)
		}
		if memories, err := storage.ListUserMemories(t.Context(), database.Read()); err != nil || len(memories) != 1 {
			t.Fatalf("新增记忆未落盘: memories=%#v err=%v", memories, err)
		}
		if _, err := exec.toolMemoryUpdate(ctx, draftID, rushestools.MemoryUpdateInput{
			RemoveKeys: []string{"pacing"},
		}); err != nil {
			t.Fatal(err)
		}
		if memories, err := storage.ListUserMemories(t.Context(), database.Read()); err != nil || len(memories) != 0 {
			t.Fatalf("remove_keys 未删除记忆: memories=%#v err=%v", memories, err)
		}
	})
}
