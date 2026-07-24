package agentexec

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

const MemorySetEntryLimit = 8

type memoryEvidence struct {
	Kind string
	ID   string
}

type memoryEvidenceContextKey struct{}

func WithMemoryEvidence(ctx context.Context, kind, id string) context.Context {
	if !storage.ValidUserMemoryEvidenceKind(kind) || strings.TrimSpace(id) == "" {
		return ctx
	}
	return context.WithValue(ctx, memoryEvidenceContextKey{}, memoryEvidence{Kind: kind, ID: id})
}

func MemoryEvidenceFromContext(ctx context.Context) (memoryEvidence, bool) {
	evidence, ok := ctx.Value(memoryEvidenceContextKey{}).(memoryEvidence)
	return evidence, ok && storage.ValidUserMemoryEvidenceKind(evidence.Kind) && evidence.ID != ""
}

func (exec *Executor) toolMemorySet(
	ctx context.Context,
	draftID string,
	input rushestools.MemorySetInput,
) (rushestools.ToolResult, error) {
	return exec.toolMutateMemory(ctx, draftID, input.Entries, nil)
}

func (exec *Executor) toolMemoryRemove(
	ctx context.Context,
	draftID string,
	input rushestools.MemoryRemoveInput,
) (rushestools.ToolResult, error) {
	return exec.toolMutateMemory(ctx, draftID, nil, input.Keys)
}

func (exec *Executor) toolMutateMemory(
	ctx context.Context,
	draftID string,
	entries []rushestools.MemoryEntryInput,
	removeKeys []string,
) (rushestools.ToolResult, error) {
	evidence, ok := MemoryEvidenceFromContext(ctx)
	if !ok {
		return memoryMutationFailure(
			"长期记忆只能锚定当前真实用户消息或当前决策回答；后台续跑和其他 UI 观察不得修改记忆。",
			rushestools.ErrCodeMemoryEvidenceUnavailable,
			"等待下一条真实用户消息，或在用户刚回答决策后的继续回合再调用记忆工具。",
			nil,
		), nil
	}
	if len(entries) == 0 && len(removeKeys) == 0 {
		return memoryMutationFailure(
			"记忆工具至少需要一个条目或删除键。",
			rushestools.ErrCodeMemoryMutationEmpty,
			"memory.set 提交 entries；memory.remove 提交用户明确要求忘记的 keys。",
			nil,
		), nil
	}
	if len(entries) > MemorySetEntryLimit {
		return memoryMutationFailure(
			fmt.Sprintf("单次最多写入 %d 条长期记忆。", MemorySetEntryLimit),
			rushestools.ErrCodeMemoryEntriesLimit,
			"合并语义重复项，只保留用户明确表达的稳定偏好后重试。",
			map[string]any{"entry_count": len(entries), "limit": MemorySetEntryLimit},
		), nil
	}
	if len(removeKeys) > storage.UserMemoryLimit {
		return memoryMutationFailure(
			fmt.Sprintf("单次最多删除 %d 条长期记忆。", storage.UserMemoryLimit),
			rushestools.ErrCodeMemoryRemoveLimit,
			"删除键不得超过当前记忆容量上限。",
			map[string]any{"remove_count": len(removeKeys), "limit": storage.UserMemoryLimit},
		), nil
	}

	rows := make([]reducer.UserMemoryRow, 0, len(entries))
	seenEntries := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		statement := strings.TrimSpace(entry.Statement)
		if !storage.ValidUserMemoryKey(entry.Key) {
			return memoryMutationFailure(
				fmt.Sprintf("长期记忆键 %q 无效。", entry.Key),
				rushestools.ErrCodeMemoryKeyInvalid,
				"key 必须匹配 [a-z0-9_]{2,40}，并表达稳定语义，例如 pacing 或 subtitle_style。",
				map[string]any{"memory_key": entry.Key},
			), nil
		}
		if _, duplicate := seenEntries[entry.Key]; duplicate {
			return memoryMutationFailure(
				fmt.Sprintf("entries 中重复出现长期记忆键 %q。", entry.Key),
				rushestools.ErrCodeMemoryKeyDuplicate,
				"同一请求中每个 key 只能出现一次。",
				map[string]any{"memory_key": entry.Key},
			), nil
		}
		seenEntries[entry.Key] = struct{}{}
		if !storage.ValidUserMemoryKind(entry.Kind) {
			return memoryMutationFailure(
				fmt.Sprintf("长期记忆 %q 的 kind 无效。", entry.Key),
				rushestools.ErrCodeMemoryKindInvalid,
				"kind 只能是 preference、correction 或 habit。",
				map[string]any{"memory_key": entry.Key, "kind": entry.Kind},
			), nil
		}
		if !storage.ValidUserMemoryStatement(statement) {
			return memoryMutationFailure(
				fmt.Sprintf("长期记忆 %q 的 statement 为空或超过 200 字。", entry.Key),
				rushestools.ErrCodeMemoryStatementInvalid,
				"用一句不超过 200 字的简体中文陈述用户明确表达的稳定偏好，不要写模型判断。",
				map[string]any{
					"memory_key": entry.Key, "limit_runes": storage.UserMemoryStatementRuneLimit,
				},
			), nil
		}
		quote := strings.TrimSpace(entry.EvidenceQuote)
		if !storage.ValidUserMemoryEvidenceQuote(quote) {
			return memoryMutationFailure(
				fmt.Sprintf("长期记忆 %q 缺少有效的 evidence_quote。", entry.Key),
				rushestools.ErrCodeMemoryEvidenceQuoteInvalid,
				"evidence_quote 必须从当前这条用户消息或决策回答里逐字摘录一段原文（至少两个字），用来佐证该记忆确有用户依据；改写或拼接都会被拒绝。",
				map[string]any{"memory_key": entry.Key},
			), nil
		}
		rows = append(rows, reducer.UserMemoryRow{
			Key: entry.Key, Kind: entry.Kind, Statement: statement, EvidenceQuote: quote,
			EvidenceKind: evidence.Kind, EvidenceID: evidence.ID, SourceDraftID: draftID,
		})
	}
	seenRemovals := make(map[string]struct{}, len(removeKeys))
	for _, key := range removeKeys {
		if !storage.ValidUserMemoryKey(key) {
			return memoryMutationFailure(
				fmt.Sprintf("待删除的长期记忆键 %q 无效。", key),
				rushestools.ErrCodeMemoryRemoveKeyInvalid,
				"keys 中的键必须匹配 [a-z0-9_]{2,40}。",
				map[string]any{"memory_key": key},
			), nil
		}
		if _, duplicate := seenRemovals[key]; duplicate {
			return memoryMutationFailure(
				fmt.Sprintf("keys 中重复出现长期记忆键 %q。", key),
				rushestools.ErrCodeMemoryRemoveKeyDuplicate,
				"同一请求中每个删除键只能出现一次。",
				map[string]any{"memory_key": key},
			), nil
		}
		seenRemovals[key] = struct{}{}
	}

	result, err := reducer.Apply(ctx, exec.database, nil, reducer.Options{
		Actor: contracts.ActorAgent,
		ResultRows: reducer.ResultRows{
			UserMemoryUpserts: rows, UserMemoryRemoveKeys: removeKeys,
			UserMemoryMutationEvidence: &reducer.UserMemoryEvidenceRow{
				Kind: evidence.Kind, ID: evidence.ID, SourceDraftID: draftID,
			},
		},
	})
	if errors.Is(err, reducer.ErrUserMemoryEvidenceQuoteMismatch) {
		return memoryMutationFailure(
			"长期记忆 evidence_quote 未能在证据原文中逐字匹配，未修改任何记忆。",
			"memory_evidence_quote_invalid",
			"evidence_quote 必须从当前这条用户消息或决策回答里逐字摘录原文片段；改写、拼接或无关摘录都会被拒绝。改成逐字原文后可立即重试，不要放弃这条记忆。",
			nil,
		), nil
	}
	if errors.Is(err, reducer.ErrUserMemoryEvidence) {
		return memoryMutationFailure(
			"当前长期记忆证据未通过数据库核验，未修改任何记忆。",
			rushestools.ErrCodeMemoryEvidenceInvalid,
			"不要重试或替换 evidence；等待下一条真实用户消息后再依据新消息调用。",
			nil,
		), nil
	}
	if errors.Is(err, reducer.ErrUserMemoryInput) {
		return memoryMutationFailure(
			"长期记忆请求未通过持久化约束，未修改任何记忆。",
			rushestools.ErrCodeMemoryInputInvalid,
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
	// 写入成功发专门 turn-stream 事件，前端据此渲染「已记住/已更新长期记忆」卡片并直链设置面板。
	// 执行器不依赖引擎 hub，经注入的 recordProgress 上报;引擎装配把它接到 hub.Record。
	if len(result.UserMemory.WrittenKeys) > 0 || len(result.UserMemory.RemovedKeys) > 0 {
		written := make(map[string]struct{}, len(result.UserMemory.WrittenKeys))
		for _, key := range result.UserMemory.WrittenKeys {
			written[key] = struct{}{}
		}
		entries := make([]map[string]any, 0, len(written))
		for _, row := range rows {
			if _, ok := written[row.Key]; !ok {
				continue
			}
			entries = append(entries, map[string]any{
				"key": row.Key, "kind": row.Kind,
				"statement": row.Statement, "evidence_quote": row.EvidenceQuote,
			})
		}
		exec.recordProgress(draftID, map[string]any{
			"type":         contracts.TurnStreamMemoryUpdated,
			"written_keys": result.UserMemory.WrittenKeys,
			"removed_keys": result.UserMemory.RemovedKeys,
			"entries":      entries,
		})
	}
	return rushestools.ToolResult{
		Status:      string(rushestools.StatusSucceeded),
		Observation: "已按当前真实用户证据更新并持久保存长期记忆。",
		Data: map[string]any{
			"written_keys": result.UserMemory.WrittenKeys,
			"removed_keys": result.UserMemory.RemovedKeys,
			"evicted_keys": result.UserMemory.EvictedKeys,
			"total":        result.UserMemory.Total,
		},
	}, nil
}

func memoryMutationFailure(
	observation string,
	errorCode rushestools.ToolErrorCode,
	recovery string,
	extra map[string]any,
) rushestools.ToolResult {
	data := map[string]any{"current_memory_unchanged": true}
	for key, value := range extra {
		data[key] = value
	}
	return rushestools.ToolFailure(rushestools.StatusValidationFailed, observation, errorCode, recovery, data)
}
