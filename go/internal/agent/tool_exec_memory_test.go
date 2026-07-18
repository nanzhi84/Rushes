package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func TestQueueMemoryEvidenceUsesOnlyUserOwnedInputs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		item QueueItem
		kind string
		id   string
		ok   bool
	}{
		{
			name: "user message",
			item: QueueItem{Kind: QueueUserMessage, ItemID: "message_user"},
			kind: storage.UserMemoryEvidenceMessage, id: "message_user", ok: true,
		},
		{
			name: "decision payload rather than replay item id",
			item: QueueItem{
				Kind: QueueUIObservation, ItemID: "replay_wrong",
				Payload: map[string]any{
					"observation_type": "decision_answered", "decision_id": "decision_real",
				},
			},
			kind: storage.UserMemoryEvidenceDecision, id: "decision_real", ok: true,
		},
		{
			name: "other ui observation",
			item: QueueItem{Kind: QueueUIObservation, ItemID: "preview", Payload: map[string]any{
				"observation_type": "preview_viewed",
			}},
		},
		{
			name: "job observation",
			item: QueueItem{Kind: QueueJobObservation, ItemID: "job"},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			evidence, ok := agentexec.MemoryEvidenceFromContext(withQueueMemoryEvidence(t.Context(), test.item))
			if ok != test.ok || evidence.Kind != test.kind || evidence.ID != test.id {
				t.Fatalf("evidence=%#v ok=%v", evidence, ok)
			}
		})
	}
}

func TestMemoryUpdatePersistsCurrentUserEvidenceAndRemovesAtomically(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_memory_tool")
	agenttest.InsertAgentMessage(t, database, "draft_memory_tool", "message_memory_tool", "以后都快一点，字幕别遮脸")
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ctx := rushestools.WithDraftID(
		agentexec.WithMemoryEvidence(t.Context(), storage.UserMemoryEvidenceMessage, "message_memory_tool"),
		"draft_memory_tool",
	)

	raw, err := service.ExecuteTool(ctx, "memory.update", rushestools.MemoryUpdateInput{
		Entries: []rushestools.MemoryEntryInput{
			{Key: "pacing", Kind: "preference", Statement: "成片节奏偏快", EvidenceQuote: "都快一点"},
			{Key: "subtitle_style", Kind: "correction", Statement: "字幕不要遮挡人物面部", EvidenceQuote: "字幕别遮脸"},
		},
	})
	result := raw.(rushestools.ToolResult)
	if err != nil || result.Status != "succeeded" || result.Data["total"] != 2 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	memories, err := storage.ListUserMemories(t.Context(), database.Read())
	if err != nil || len(memories) != 2 {
		t.Fatalf("memories=%#v err=%v", memories, err)
	}
	for _, memory := range memories {
		if memory.EvidenceKind != storage.UserMemoryEvidenceMessage ||
			memory.EvidenceID != "message_memory_tool" || memory.SourceDraftID != "draft_memory_tool" {
			t.Fatalf("memory evidence=%#v", memory)
		}
	}

	raw, err = service.ExecuteTool(ctx, "memory.update", rushestools.MemoryUpdateInput{
		RemoveKeys: []string{"pacing"},
	})
	result = raw.(rushestools.ToolResult)
	if err != nil || result.Status != "succeeded" || result.Data["total"] != 1 {
		t.Fatalf("remove result=%#v err=%v", result, err)
	}
	memories, err = storage.ListUserMemories(t.Context(), database.Read())
	if err != nil || len(memories) != 1 || memories[0].Key != "subtitle_style" {
		t.Fatalf("memories after remove=%#v err=%v", memories, err)
	}
}

func TestMemoryUpdateAcceptsAnsweredDecisionEvidence(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_memory_decision")
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO decisions(
			decision_id,scope_type,draft_id,type,question,options_json,
			allow_free_text,status,answer_json,blocking
		) VALUES('decision_memory','draft','draft_memory_decision','critical','默认节奏？','[]',
			1,'answered','{"free_text":"以后都快一点"}',1)`); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ctx := rushestools.WithDraftID(
		agentexec.WithMemoryEvidence(t.Context(), storage.UserMemoryEvidenceDecision, "decision_memory"),
		"draft_memory_decision",
	)
	raw, err := service.ExecuteTool(ctx, "memory.update", rushestools.MemoryUpdateInput{
		Entries: []rushestools.MemoryEntryInput{{
			Key: "pacing", Kind: "preference", Statement: "成片节奏偏快", EvidenceQuote: "都快一点",
		}},
	})
	result := raw.(rushestools.ToolResult)
	if err != nil || result.Status != "succeeded" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	memories, err := storage.ListUserMemories(t.Context(), database.Read())
	if err != nil || len(memories) != 1 ||
		memories[0].EvidenceKind != storage.UserMemoryEvidenceDecision ||
		memories[0].EvidenceID != "decision_memory" {
		t.Fatalf("memories=%#v err=%v", memories, err)
	}
}

func TestMemoryUpdateRejectsMissingOrForgedEvidence(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_memory_reject")
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	input := rushestools.MemoryUpdateInput{Entries: []rushestools.MemoryEntryInput{{
		Key: "pacing", Kind: "preference", Statement: "成片节奏偏快", EvidenceQuote: "偏快",
	}}}

	for _, test := range []struct {
		name string
		ctx  context.Context
		code string
	}{
		{
			name: "job or direct call has no evidence",
			ctx:  rushestools.WithDraftID(t.Context(), "draft_memory_reject"),
			code: "memory_evidence_unavailable",
		},
		{
			name: "forged message id fails reducer validation",
			ctx: rushestools.WithDraftID(
				agentexec.WithMemoryEvidence(t.Context(), storage.UserMemoryEvidenceMessage, "missing"),
				"draft_memory_reject",
			),
			code: "memory_evidence_invalid",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, executeErr := service.ExecuteTool(test.ctx, "memory.update", input)
			result := raw.(rushestools.ToolResult)
			if executeErr != nil || result.Status != "validation_failed" ||
				result.Data["error_code"] != test.code || result.Data["current_memory_unchanged"] != true {
				t.Fatalf("result=%#v err=%v", result, executeErr)
			}
		})
	}
	memories, err := storage.ListUserMemories(t.Context(), database.Read())
	if err != nil || len(memories) != 0 {
		t.Fatalf("memories=%#v err=%v", memories, err)
	}
}

func TestMemoryUpdateValidatesInputBeforeWriting(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_memory_input")
	agenttest.InsertAgentMessage(t, database, "draft_memory_input", "message_memory_input", "记住")
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ctx := rushestools.WithDraftID(
		agentexec.WithMemoryEvidence(t.Context(), storage.UserMemoryEvidenceMessage, "message_memory_input"),
		"draft_memory_input",
	)
	tooManyEntries := make([]rushestools.MemoryEntryInput, agentexec.MemoryUpdateEntryLimit+1)
	for index := range tooManyEntries {
		tooManyEntries[index] = rushestools.MemoryEntryInput{
			Key: "key_" + strings.Repeat("x", index+1), Kind: "habit", Statement: "稳定习惯",
		}
	}
	tooManyRemovals := make([]string, storage.UserMemoryLimit+1)
	for index := range tooManyRemovals {
		tooManyRemovals[index] = "remove_" + strings.Repeat("x", index+1)
	}
	cases := []struct {
		name  string
		input rushestools.MemoryUpdateInput
		code  string
	}{
		{name: "empty", code: "memory_update_empty"},
		{
			name:  "too many entries",
			input: rushestools.MemoryUpdateInput{Entries: tooManyEntries},
			code:  "memory_entries_limit",
		},
		{
			name:  "too many removals",
			input: rushestools.MemoryUpdateInput{RemoveKeys: tooManyRemovals},
			code:  "memory_remove_limit",
		},
		{
			name: "invalid key",
			input: rushestools.MemoryUpdateInput{Entries: []rushestools.MemoryEntryInput{{
				Key: "Bad-Key", Kind: "preference", Statement: "偏快",
			}}},
			code: "memory_key_invalid",
		},
		{
			name: "invalid kind",
			input: rushestools.MemoryUpdateInput{Entries: []rushestools.MemoryEntryInput{{
				Key: "pacing", Kind: "guess", Statement: "偏快",
			}}},
			code: "memory_kind_invalid",
		},
		{
			name: "statement too long",
			input: rushestools.MemoryUpdateInput{Entries: []rushestools.MemoryEntryInput{{
				Key: "pacing", Kind: "preference", Statement: strings.Repeat("快", 201),
			}}},
			code: "memory_statement_invalid",
		},
		{
			name: "missing evidence quote",
			input: rushestools.MemoryUpdateInput{Entries: []rushestools.MemoryEntryInput{{
				Key: "pacing", Kind: "preference", Statement: "偏快",
			}}},
			code: "memory_evidence_quote_invalid",
		},
		{
			name: "duplicate entry",
			input: rushestools.MemoryUpdateInput{Entries: []rushestools.MemoryEntryInput{
				{Key: "pacing", Kind: "preference", Statement: "偏快", EvidenceQuote: "偏快"},
				{Key: "pacing", Kind: "correction", Statement: "更快", EvidenceQuote: "更快"},
			}},
			code: "memory_key_duplicate",
		},
		{
			name: "entry remove conflict",
			input: rushestools.MemoryUpdateInput{
				Entries: []rushestools.MemoryEntryInput{{
					Key: "pacing", Kind: "preference", Statement: "偏快", EvidenceQuote: "偏快",
				}},
				RemoveKeys: []string{"pacing"},
			},
			code: "memory_key_conflict",
		},
		{
			name:  "invalid remove key",
			input: rushestools.MemoryUpdateInput{RemoveKeys: []string{"Bad-Key"}},
			code:  "memory_remove_key_invalid",
		},
		{
			name:  "duplicate remove key",
			input: rushestools.MemoryUpdateInput{RemoveKeys: []string{"pacing", "pacing"}},
			code:  "memory_remove_key_duplicate",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			raw, executeErr := service.ExecuteTool(ctx, "memory.update", test.input)
			result := raw.(rushestools.ToolResult)
			if executeErr != nil || result.Status != "validation_failed" || result.Data["error_code"] != test.code {
				t.Fatalf("result=%#v err=%v", result, executeErr)
			}
		})
	}
}

func TestInjectedMemoryCollectorTouchesLastUsedAtOnSuccess(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_touch")
	agenttest.InsertAgentMessage(t, database, "draft_touch", "message_touch", "以后都快一点")
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ctx := rushestools.WithDraftID(
		agentexec.WithMemoryEvidence(t.Context(), storage.UserMemoryEvidenceMessage, "message_touch"),
		"draft_touch",
	)
	if _, err := service.ExecuteTool(ctx, "memory.update", rushestools.MemoryUpdateInput{
		Entries: []rushestools.MemoryEntryInput{{
			Key: "pacing", Kind: "preference", Statement: "成片节奏偏快", EvidenceQuote: "都快一点",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	before, err := storage.ListUserMemories(t.Context(), database.Read())
	if err != nil || len(before) != 1 || before[0].LastUsedAt != "" {
		t.Fatalf("初始 last_used_at 应为空: %#v err=%v", before, err)
	}

	// 收集器记录本回合注入的键：去重并排序，未挂到 ctx 时为 no-op。
	collectorCtx, collector := withInjectedMemoryCollector(t.Context())
	recordInjectedMemoryKeys(t.Context(), []string{"ignored"})
	recordInjectedMemoryKeys(collectorCtx, []string{"pacing", "pacing", "missing"})
	if keys := collector.snapshot(); len(keys) != 2 || keys[0] != "missing" || keys[1] != "pacing" {
		t.Fatalf("collector snapshot=%v", keys)
	}

	// 成功收尾的 touch 只刷新已存在的键，缺失键静默跳过。
	service.touchInjectedMemories(t.Context(), "draft_touch", collector.snapshot())
	after, err := storage.ListUserMemories(t.Context(), database.Read())
	if err != nil || len(after) != 1 || after[0].Key != "pacing" || after[0].LastUsedAt == "" {
		t.Fatalf("touch 后 pacing 的 last_used_at 应非空: %#v err=%v", after, err)
	}

	// 空键集合是安全的空操作。
	service.touchInjectedMemories(t.Context(), "draft_touch", nil)
}

func TestMemoryUpdateMapsRewrittenQuoteToQuoteInvalidNotEvidenceInvalid(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_quote_map")
	agenttest.InsertAgentMessage(t, database, "draft_quote_map", "message_quote_map", "以后都快一点")
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ctx := rushestools.WithDraftID(
		agentexec.WithMemoryEvidence(t.Context(), storage.UserMemoryEvidenceMessage, "message_quote_map"),
		"draft_quote_map",
	)
	// 引文有 ≥2 字、过工具预检，但被模型改写，不是证据原文子串：由 reducer 拦下。
	// 必须映射到 memory_evidence_quote_invalid（逐字重摘可救回），而非 memory_evidence_invalid
	// （等下一条消息）——后者对最常见的改写失败是反向误导，会让合法记忆静默流失。
	raw, executeErr := service.ExecuteTool(ctx, "memory.update", rushestools.MemoryUpdateInput{
		Entries: []rushestools.MemoryEntryInput{{
			Key: "pacing", Kind: "preference", Statement: "成片节奏偏快", EvidenceQuote: "都慢一点",
		}},
	})
	result := raw.(rushestools.ToolResult)
	if executeErr != nil || result.Status != "validation_failed" ||
		result.Data["error_code"] != "memory_evidence_quote_invalid" ||
		result.Data["current_memory_unchanged"] != true {
		t.Fatalf("result=%#v err=%v", result, executeErr)
	}
	memories, err := storage.ListUserMemories(t.Context(), database.Read())
	if err != nil || len(memories) != 0 {
		t.Fatalf("非子串引文不得落库: %#v err=%v", memories, err)
	}
}

// TestMemoryUpdateNegativeSetLocksEvidenceMappings 是 M5 的确定性负例集：锁住 #113 拆分出的
// 两类证据错误映射不退化——引文语义不符（同义改写 / 跨字段拼接 / 过短）走 quote_mismatch →
// memory_evidence_quote_invalid（逐字重摘可救回），证据缺失 / 伪造走 evidence_invalid /
// unavailable（重试无益）。任何一条都不得落库。
func TestMemoryUpdateNegativeSetLocksEvidenceMappings(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_neg")
	agenttest.InsertAgentMessage(t, database, "draft_neg", "message_neg", "以后节奏都要快一点，字幕别遮脸")
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)

	realCtx := rushestools.WithDraftID(
		agentexec.WithMemoryEvidence(t.Context(), storage.UserMemoryEvidenceMessage, "message_neg"), "draft_neg")
	forgedCtx := rushestools.WithDraftID(
		agentexec.WithMemoryEvidence(t.Context(), storage.UserMemoryEvidenceMessage, "message_absent"), "draft_neg")
	noEvidenceCtx := rushestools.WithDraftID(t.Context(), "draft_neg")
	entry := func(quote string) rushestools.MemoryUpdateInput {
		return rushestools.MemoryUpdateInput{Entries: []rushestools.MemoryEntryInput{{
			Key: "pacing", Kind: "preference", Statement: "成片节奏偏快", EvidenceQuote: quote,
		}}}
	}

	for _, test := range []struct {
		name  string
		ctx   context.Context
		input rushestools.MemoryUpdateInput
		code  string
	}{
		// 引文语义不符 → quote_mismatch → memory_evidence_quote_invalid
		{"同义改写非子串", realCtx, entry("节奏要加快"), "memory_evidence_quote_invalid"},
		{"跨逗号拼接", realCtx, entry("快一点字幕别遮脸"), "memory_evidence_quote_invalid"},
		{"跨子句摘取", realCtx, entry("都要快一点字幕"), "memory_evidence_quote_invalid"},
		{"引文过短单字", realCtx, entry("快"), "memory_evidence_quote_invalid"},
		// 证据缺失 / 伪造 → evidence_invalid / unavailable
		{"伪造消息证据", forgedCtx, entry("节奏都要快一点"), "memory_evidence_invalid"},
		{"无证据上下文", noEvidenceCtx, entry("节奏都要快一点"), "memory_evidence_unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, execErr := service.ExecuteTool(test.ctx, "memory.update", test.input)
			result := raw.(rushestools.ToolResult)
			if execErr != nil || result.Status != "validation_failed" ||
				result.Data["error_code"] != test.code || result.Data["current_memory_unchanged"] != true {
				t.Fatalf("负例 %q 应判 %s：result=%#v err=%v", test.name, test.code, result, execErr)
			}
		})
	}
	memories, err := storage.ListUserMemories(t.Context(), database.Read())
	if err != nil || len(memories) != 0 {
		t.Fatalf("负例集不得落库任何记忆: %#v err=%v", memories, err)
	}
}

func TestMemoryUpdateEmitsMemoryUpdatedTurnStreamEvent(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_evt")
	agenttest.InsertAgentMessage(t, database, "draft_evt", "message_evt", "以后都快一点")
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ctx := rushestools.WithDraftID(
		agentexec.WithMemoryEvidence(t.Context(), storage.UserMemoryEvidenceMessage, "message_evt"), "draft_evt")
	if _, err := service.ExecuteTool(ctx, "memory.update", rushestools.MemoryUpdateInput{
		Entries: []rushestools.MemoryEntryInput{{
			Key: "pacing", Kind: "preference", Statement: "成片节奏偏快", EvidenceQuote: "都快一点",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range service.Hub().Snapshot("draft_evt") {
		if event["type"] != TurnStreamMemoryUpdated {
			continue
		}
		found = true
		keys, ok := event["written_keys"].([]string)
		if !ok || len(keys) != 1 || keys[0] != "pacing" {
			t.Fatalf("memory_updated written_keys=%#v", event["written_keys"])
		}
	}
	if !found {
		t.Fatal("memory.update 成功后应发专门的 memory_updated turn-stream 事件")
	}
}
