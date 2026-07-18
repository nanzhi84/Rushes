package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

const memoryUpdateEntryLimit = 8

type memoryEvidence struct {
	Kind string
	ID   string
}

type memoryEvidenceContextKey struct{}

func withMemoryEvidence(ctx context.Context, kind, id string) context.Context {
	if !storage.ValidUserMemoryEvidenceKind(kind) || strings.TrimSpace(id) == "" {
		return ctx
	}
	return context.WithValue(ctx, memoryEvidenceContextKey{}, memoryEvidence{Kind: kind, ID: id})
}

func memoryEvidenceFromContext(ctx context.Context) (memoryEvidence, bool) {
	evidence, ok := ctx.Value(memoryEvidenceContextKey{}).(memoryEvidence)
	return evidence, ok && storage.ValidUserMemoryEvidenceKind(evidence.Kind) && evidence.ID != ""
}

func withQueueMemoryEvidence(ctx context.Context, item QueueItem) context.Context {
	switch item.Kind {
	case QueueUserMessage:
		return withMemoryEvidence(ctx, storage.UserMemoryEvidenceMessage, item.ItemID)
	case QueueUIObservation:
		if interfaceString(item.Payload["observation_type"]) == "decision_answered" {
			return withMemoryEvidence(
				ctx, storage.UserMemoryEvidenceDecision, interfaceString(item.Payload["decision_id"]),
			)
		}
	}
	return ctx
}

func (service *Service) toolMemoryUpdate(
	ctx context.Context,
	draftID string,
	input rushestools.MemoryUpdateInput,
) (rushestools.ToolResult, error) {
	evidence, ok := memoryEvidenceFromContext(ctx)
	if !ok {
		return memoryUpdateFailure(
			"长期记忆只能锚定当前真实用户消息或当前决策回答；后台续跑和其他 UI 观察不得修改记忆。",
			"memory_evidence_unavailable",
			"等待下一条真实用户消息，或在用户刚回答决策后的继续回合再调用 memory.update。",
			nil,
		), nil
	}
	if len(input.Entries) == 0 && len(input.RemoveKeys) == 0 {
		return memoryUpdateFailure(
			"memory.update 至少需要一条 entries 或 remove_keys。",
			"memory_update_empty",
			"只提交用户本回合明确表达的稳定偏好，或用户明确要求忘记的已有键。",
			nil,
		), nil
	}
	if len(input.Entries) > memoryUpdateEntryLimit {
		return memoryUpdateFailure(
			fmt.Sprintf("单次最多写入 %d 条长期记忆。", memoryUpdateEntryLimit),
			"memory_entries_limit",
			"合并语义重复项，只保留用户明确表达的稳定偏好后重试。",
			map[string]any{"entry_count": len(input.Entries), "limit": memoryUpdateEntryLimit},
		), nil
	}
	if len(input.RemoveKeys) > storage.UserMemoryLimit {
		return memoryUpdateFailure(
			fmt.Sprintf("单次最多删除 %d 条长期记忆。", storage.UserMemoryLimit),
			"memory_remove_limit",
			"删除键不得超过当前记忆容量上限。",
			map[string]any{"remove_count": len(input.RemoveKeys), "limit": storage.UserMemoryLimit},
		), nil
	}

	rows := make([]reducer.UserMemoryRow, 0, len(input.Entries))
	seenEntries := make(map[string]struct{}, len(input.Entries))
	for _, entry := range input.Entries {
		statement := strings.TrimSpace(entry.Statement)
		if !storage.ValidUserMemoryKey(entry.Key) {
			return memoryUpdateFailure(
				fmt.Sprintf("长期记忆键 %q 无效。", entry.Key),
				"memory_key_invalid",
				"key 必须匹配 [a-z0-9_]{2,40}，并表达稳定语义，例如 pacing 或 subtitle_style。",
				map[string]any{"memory_key": entry.Key},
			), nil
		}
		if _, duplicate := seenEntries[entry.Key]; duplicate {
			return memoryUpdateFailure(
				fmt.Sprintf("entries 中重复出现长期记忆键 %q。", entry.Key),
				"memory_key_duplicate",
				"同一请求中每个 key 只能出现一次。",
				map[string]any{"memory_key": entry.Key},
			), nil
		}
		seenEntries[entry.Key] = struct{}{}
		if !storage.ValidUserMemoryKind(entry.Kind) {
			return memoryUpdateFailure(
				fmt.Sprintf("长期记忆 %q 的 kind 无效。", entry.Key),
				"memory_kind_invalid",
				"kind 只能是 preference、correction 或 habit。",
				map[string]any{"memory_key": entry.Key, "kind": entry.Kind},
			), nil
		}
		if !storage.ValidUserMemoryStatement(statement) {
			return memoryUpdateFailure(
				fmt.Sprintf("长期记忆 %q 的 statement 为空或超过 200 字。", entry.Key),
				"memory_statement_invalid",
				"用一句不超过 200 字的简体中文陈述用户明确表达的稳定偏好，不要写模型判断。",
				map[string]any{
					"memory_key": entry.Key, "limit_runes": storage.UserMemoryStatementRuneLimit,
				},
			), nil
		}
		quote := strings.TrimSpace(entry.EvidenceQuote)
		if !storage.ValidUserMemoryEvidenceQuote(quote) {
			return memoryUpdateFailure(
				fmt.Sprintf("长期记忆 %q 缺少有效的 evidence_quote。", entry.Key),
				"memory_evidence_quote_invalid",
				"evidence_quote 必须从当前这条用户消息或决策回答里逐字摘录一段原文（至少两个字），用来佐证该记忆确有用户依据；改写或拼接都会被拒绝。",
				map[string]any{"memory_key": entry.Key},
			), nil
		}
		rows = append(rows, reducer.UserMemoryRow{
			Key: entry.Key, Kind: entry.Kind, Statement: statement, EvidenceQuote: quote,
			EvidenceKind: evidence.Kind, EvidenceID: evidence.ID, SourceDraftID: draftID,
		})
	}
	seenRemovals := make(map[string]struct{}, len(input.RemoveKeys))
	for _, key := range input.RemoveKeys {
		if !storage.ValidUserMemoryKey(key) {
			return memoryUpdateFailure(
				fmt.Sprintf("待删除的长期记忆键 %q 无效。", key),
				"memory_remove_key_invalid",
				"remove_keys 中的键必须匹配 [a-z0-9_]{2,40}。",
				map[string]any{"memory_key": key},
			), nil
		}
		if _, duplicate := seenRemovals[key]; duplicate {
			return memoryUpdateFailure(
				fmt.Sprintf("remove_keys 中重复出现长期记忆键 %q。", key),
				"memory_remove_key_duplicate",
				"同一请求中每个删除键只能出现一次。",
				map[string]any{"memory_key": key},
			), nil
		}
		if _, overlap := seenEntries[key]; overlap {
			return memoryUpdateFailure(
				fmt.Sprintf("长期记忆键 %q 不能在同一请求中同时写入和删除。", key),
				"memory_key_conflict",
				"根据用户本回合的最终表达，只保留 entries 或 remove_keys 中的一种操作。",
				map[string]any{"memory_key": key},
			), nil
		}
		seenRemovals[key] = struct{}{}
	}

	result, err := reducer.Apply(ctx, service.database, nil, reducer.Options{
		Actor: contracts.ActorAgent,
		ResultRows: reducer.ResultRows{
			UserMemoryUpserts: rows, UserMemoryRemoveKeys: input.RemoveKeys,
			UserMemoryMutationEvidence: &reducer.UserMemoryEvidenceRow{
				Kind: evidence.Kind, ID: evidence.ID, SourceDraftID: draftID,
			},
		},
	})
	if errors.Is(err, reducer.ErrUserMemoryEvidenceQuoteMismatch) {
		return memoryUpdateFailure(
			"长期记忆 evidence_quote 未能在证据原文中逐字匹配，未修改任何记忆。",
			"memory_evidence_quote_invalid",
			"evidence_quote 必须从当前这条用户消息或决策回答里逐字摘录原文片段；改写、拼接或无关摘录都会被拒绝。改成逐字原文后可立即重试，不要放弃这条记忆。",
			nil,
		), nil
	}
	if errors.Is(err, reducer.ErrUserMemoryEvidence) {
		return memoryUpdateFailure(
			"当前长期记忆证据未通过数据库核验，未修改任何记忆。",
			"memory_evidence_invalid",
			"不要重试或替换 evidence；等待下一条真实用户消息后再依据新消息调用。",
			nil,
		), nil
	}
	if errors.Is(err, reducer.ErrUserMemoryInput) {
		return memoryUpdateFailure(
			"长期记忆请求未通过持久化约束，未修改任何记忆。",
			"memory_input_invalid",
			"按工具 schema 修正 key、kind、statement 与重复键后重试。",
			nil,
		), nil
	}
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	if result.Status != reducer.StatusApplied || result.UserMemory == nil {
		return rushestools.ToolResult{}, fmt.Errorf("长期记忆写入状态异常: %s", result.Status)
	}
	return rushestools.ToolResult{
		Status:      "succeeded",
		Observation: "已按当前真实用户证据更新并持久保存长期记忆。",
		Data: map[string]any{
			"written_keys": result.UserMemory.WrittenKeys,
			"removed_keys": result.UserMemory.RemovedKeys,
			"evicted_keys": result.UserMemory.EvictedKeys,
			"total":        result.UserMemory.Total,
		},
	}, nil
}

func memoryUpdateFailure(
	observation string,
	errorCode string,
	recovery string,
	extra map[string]any,
) rushestools.ToolResult {
	data := map[string]any{
		"error_code": errorCode, "recovery": recovery, "current_memory_unchanged": true,
	}
	for key, value := range extra {
		data[key] = value
	}
	return rushestools.ToolResult{
		Status: "validation_failed", Observation: observation, Data: data,
	}
}
