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
			{Key: "pacing", Kind: "preference", Statement: "成片节奏偏快"},
			{Key: "subtitle_style", Kind: "correction", Statement: "字幕不要遮挡人物面部"},
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
			Key: "pacing", Kind: "preference", Statement: "成片节奏偏快",
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
		Key: "pacing", Kind: "preference", Statement: "成片节奏偏快",
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
			name: "duplicate entry",
			input: rushestools.MemoryUpdateInput{Entries: []rushestools.MemoryEntryInput{
				{Key: "pacing", Kind: "preference", Statement: "偏快"},
				{Key: "pacing", Kind: "correction", Statement: "更快"},
			}},
			code: "memory_key_duplicate",
		},
		{
			name: "entry remove conflict",
			input: rushestools.MemoryUpdateInput{
				Entries: []rushestools.MemoryEntryInput{{
					Key: "pacing", Kind: "preference", Statement: "偏快",
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
