import { useCallback, useEffect, useReducer, useRef } from "react";
import { useDocumentVisibility } from "../../app/use_document_visibility";
import { acquireApiEventSource } from "../../auth";

// text_delta 阶段是 assistant；完成后区分叙述、正式回复、后台观察和回合失败终态。
type TurnStreamMessageKind = "assistant" | "narration" | "reply" | "observation" | "turn_failure";

export type StreamMessageItem = {
  type: "message";
  message_id: string;
  kind: TurnStreamMessageKind;
  text: string;
};

export type StreamToolItem = {
  type: "tool";
  step_id: string;
  tool: string;
  status: string;
  argsSummary: string | null;
  observation: string | null;
  progress?: number | null;
  progressNote?: string | null;
  durationMs?: number | null;
  harnessOwned?: boolean;
};

export type StreamStopGateItem = {
  type: "stop_gate";
  gate_id: string;
  traceIds: string[];
  timelineId: string | null;
  status: "checking" | "blocked" | "passed" | "hook_error";
  issues: Array<{ code?: string; message?: string; recovery?: string }>;
  remainingIssueCount: number;
  resultRef: string | null;
  observation: string | null;
  durationMs: number | null;
};

// 长期记忆写入成功的可见卡片：列出已记住/已更新/已移除的记忆键并直链设置面板。
export type StreamMemoryItem = {
  type: "memory";
  id: string;
  written_keys: string[];
  removed_keys: string[];
  entries: StreamMemoryEntry[];
};

export type StreamMemoryEntry = {
  key: string;
  kind: string;
  statement: string;
  evidence_quote: string;
};

// 消息、工具步与记忆卡片合并成按到达顺序排列的单一列表：前端据此把工具行内嵌在
// 叙述之间（对齐 Claude Code 的呈现方式），而不是把工具堆在消息流末尾。
export type TurnStreamItem = StreamMessageItem | StreamToolItem | StreamStopGateItem | StreamMemoryItem;

// 素材理解等子代理执行期间的实时动态：按素材粒度只保留最近一条 note。
export type SubagentProgressEntry = {
  asset_id: string;
  note: string;
};

export type ModelRetryState = {
  attempt: number;
  maxRetries: number;
  reason: string;
  nextDelayMs: number | null;
};

export type TokenUsage = {
  model_calls: number;
  prompt_tokens: number;
  cached_prompt_tokens: number;
  completion_tokens: number;
  total_tokens: number;
};

export type TurnEndedEvent = {
  outcome: string;
  reason: string | null;
  token_usage?: TokenUsage;
};

export type TurnStreamState = {
  items: TurnStreamItem[];
  turnActive: boolean;
  // 模型单次请求超时后的可见恢复状态；成功产出任何新进展后立即清空。
  modelRetry: ModelRetryState | null;
  // 挂在「当前进行中工具行」下方的子代理进度；工具收尾或回合结束即清空。
  subagentProgress: SubagentProgressEntry[];
};

// 服务端统一发送 event: turn_stream；事件 type 由 go/internal/agent 定义。
export const KNOWN_TURN_STREAM_TYPES = [
  "turn_started",
  "text_delta",
  "message_completed",
  "tool_step_started",
  "tool_step_progress",
  "tool_step_finished",
  "stop_gate_started",
  "stop_gate_finished",
  "model_retry",
  "subagent_progress",
  "context_compaction_failed",
  "stream_snapshot_truncated",
  "stream_gap",
  "turn_ended",
  "turn_error",
  "memory_updated"
] as const;

export type TurnStreamEvent =
  | { type: "local_reset" }
  | { type: "turn_started"; turn_id?: string }
  | { type: "text_delta"; message_id: string; kind?: string; delta?: string }
  | {
      type: "message_completed";
      message_id: string;
      kind: "narration" | "reply" | "observation" | "turn_failure";
      content: string;
    }
  | { type: "tool_step_started"; step_id: string; tool: string; args_summary?: string; progress?: number; harness_owned?: boolean }
  | { type: "tool_step_progress"; step_id: string; tool: string; progress?: number; note?: string; harness_owned?: boolean }
  | { type: "tool_step_finished"; step_id: string; tool: string; status: string; observation?: string; progress?: number; duration_ms?: number; harness_owned?: boolean }
  | { type: "stop_gate_started"; gate_id: string; trace_id?: string; timeline_id?: string; status?: "checking" }
  | {
      type: "stop_gate_finished";
      gate_id: string;
      trace_id?: string;
      timeline_id?: string;
      status: "blocked" | "passed" | "hook_error";
      issues?: Array<{ code?: string; message?: string; recovery?: string }>;
      remaining_issue_count?: number;
      result_ref?: string;
      observation?: string;
      duration_ms?: number;
    }
  | {
      type: "model_retry";
      attempt?: number;
      max_retries?: number;
      reason?: string;
      next_delay_ms?: number;
    }
  | {
      type: "subagent_progress";
      asset_id?: string;
      note?: string;
      tool?: string;
      completed?: number;
      total?: number;
    }
  | { type: "context_compaction_failed"; reason?: string; fallback?: string }
  | { type: "stream_snapshot_truncated" }
  | { type: "stream_gap" }
  | ({ type: "turn_ended" } & TurnEndedEvent)
  | { type: "turn_error"; message: string }
  | {
      type: "memory_updated";
      written_keys?: string[];
      removed_keys?: string[];
      entries?: unknown[];
    };

export type UseTurnStreamOptions = {
  onTurnEnded?: (event: TurnEndedEvent) => void;
  onTurnError?: (message: string) => void;
  onStreamGap?: () => void;
};

export const INITIAL_STATE: TurnStreamState = {
  items: [],
  turnActive: false,
  modelRetry: null,
  subagentProgress: []
};

// 纯 reducer：便于单测，也让重连快照重放（turn_started 起头）能从头重建状态。
export function reduceTurnStream(state: TurnStreamState, event: TurnStreamEvent): TurnStreamState {
  switch (event.type) {
    case "local_reset":
      return INITIAL_STATE;
    case "turn_started":
      // 新回合（或重连重放）从零重建，避免 text_delta 被重复追加。
      return { items: [], turnActive: true, modelRetry: null, subagentProgress: [] };
    case "model_retry": {
      const attempt = typeof event.attempt === "number" ? event.attempt : 0;
      const maxRetries = typeof event.max_retries === "number" ? event.max_retries : 0;
      if (attempt <= 0 || maxRetries <= 0 || attempt > maxRetries) {
        return state;
      }
      return {
        ...state,
        turnActive: true,
        modelRetry: {
          attempt,
          maxRetries,
          reason:
            typeof event.reason === "string" && event.reason ? event.reason : "模型响应超时",
          nextDelayMs:
            typeof event.next_delay_ms === "number" && event.next_delay_ms >= 0
              ? event.next_delay_ms
              : null
        }
      };
    }
    case "text_delta": {
      if (typeof event.message_id !== "string") {
        return state;
      }
      return {
        ...state,
        turnActive: true,
        modelRetry: null,
        items: appendDelta(
          state.items,
          event.message_id,
          typeof event.delta === "string" ? event.delta : ""
        )
      };
    }
    case "message_completed": {
      if (typeof event.message_id !== "string") {
        return state;
      }
      // content 为全文，整体替换流式 buffer（failover 双发的自愈语义）。
      return {
        ...state,
        turnActive: true,
        modelRetry: null,
        items: upsertMessage(state.items, {
          type: "message",
          message_id: event.message_id,
          kind: normalizeCompletedKind(event.kind),
          text: typeof event.content === "string" ? event.content : ""
        })
      };
    }
    case "tool_step_started": {
      if (typeof event.step_id !== "string" || typeof event.tool !== "string") {
        return state;
      }
      return {
        ...state,
        turnActive: true,
        modelRetry: null,
        items: upsertToolStep(state.items, {
          type: "tool",
          step_id: event.step_id,
          tool: event.tool,
          status: "running",
          argsSummary: typeof event.args_summary === "string" && event.args_summary ? event.args_summary : null,
          observation: null,
          progress: normalizeProgress(event.progress),
          progressNote: null,
          durationMs: null,
          harnessOwned: event.harness_owned === true
        })
      };
    }
    case "tool_step_progress": {
      if (typeof event.step_id !== "string" || typeof event.tool !== "string") {
        return state;
      }
      return {
        ...state,
        turnActive: true,
        items: upsertToolStep(state.items, {
          type: "tool",
          step_id: event.step_id,
          tool: event.tool,
          status: "running",
          argsSummary: null,
          observation: null,
          progress: normalizeProgress(event.progress),
          progressNote: typeof event.note === "string" && event.note ? event.note : null,
          durationMs: null,
          harnessOwned: event.harness_owned === true
        })
      };
    }
    case "tool_step_finished": {
      if (typeof event.step_id !== "string" || typeof event.tool !== "string") {
        return state;
      }
      return {
        ...state,
        items: upsertToolStep(state.items, {
          type: "tool",
          step_id: event.step_id,
          tool: event.tool,
          status: typeof event.status === "string" ? event.status : "succeeded",
          argsSummary: null,
          observation: typeof event.observation === "string" && event.observation ? event.observation : null,
          progress: normalizeProgress(event.progress),
          progressNote: null,
          durationMs: typeof event.duration_ms === "number" && event.duration_ms >= 0 ? event.duration_ms : null,
          harnessOwned: event.harness_owned === true
        }),
        // 工具收尾即清空其子代理进度，避免残留串到下一个进行中工具行上。
        subagentProgress: []
      };
    }
    case "stop_gate_started": {
      if (typeof event.gate_id !== "string") {
        return state;
      }
      return {
        ...state,
        turnActive: true,
        modelRetry: null,
        items: upsertStopGate(state.items, {
          type: "stop_gate",
          gate_id: event.gate_id,
          traceIds: typeof event.trace_id === "string" ? [event.trace_id] : [],
          timelineId: typeof event.timeline_id === "string" ? event.timeline_id : null,
          status: "checking",
          issues: [],
          remainingIssueCount: 0,
          resultRef: null,
          observation: null,
          durationMs: null
        })
      };
    }
    case "stop_gate_finished": {
      if (typeof event.gate_id !== "string") {
        return state;
      }
      return {
        ...state,
        items: upsertStopGate(state.items, {
          type: "stop_gate",
          gate_id: event.gate_id,
          traceIds: typeof event.trace_id === "string" ? [event.trace_id] : [],
          timelineId: typeof event.timeline_id === "string" ? event.timeline_id : null,
          status: event.status,
          issues: Array.isArray(event.issues) ? event.issues : [],
          remainingIssueCount:
            typeof event.remaining_issue_count === "number" ? event.remaining_issue_count : 0,
          resultRef: typeof event.result_ref === "string" ? event.result_ref : null,
          observation: typeof event.observation === "string" ? event.observation : null,
          durationMs:
            typeof event.duration_ms === "number" && event.duration_ms >= 0
              ? event.duration_ms
              : null
        })
      };
    }
    case "memory_updated": {
      const written = Array.isArray(event.written_keys)
        ? event.written_keys.filter((key): key is string => typeof key === "string")
        : [];
      const removed = Array.isArray(event.removed_keys)
        ? event.removed_keys.filter((key): key is string => typeof key === "string")
        : [];
      const entries = Array.isArray(event.entries)
        ? event.entries.flatMap((entry) => {
            if (typeof entry !== "object" || entry === null) {
              return [];
            }
            const value = entry as Record<string, unknown>;
            if (
              typeof value.key !== "string" ||
              typeof value.kind !== "string" ||
              typeof value.statement !== "string" ||
              typeof value.evidence_quote !== "string"
            ) {
              return [];
            }
            return [
              {
                key: value.key,
                kind: value.kind,
                statement: value.statement,
                evidence_quote: value.evidence_quote
              }
            ];
          })
        : [];
      if (written.length === 0 && removed.length === 0) {
        return state;
      }
      // 记忆卡片只追加、不更新；replay 从 turn_started 起头，按已存在记忆卡片数生成
      // 稳定 id，保证虚拟化列表与 memo 的 key 稳定。
      const memoryCount = state.items.filter((item) => item.type === "memory").length;
      return {
        ...state,
        turnActive: true,
        modelRetry: null,
        items: [
          ...state.items,
          {
            type: "memory",
            id: `memory_${memoryCount}`,
            written_keys: written,
            removed_keys: removed,
            entries
          }
        ]
      };
    }
    case "subagent_progress": {
      if (typeof event.asset_id !== "string" || !event.asset_id) {
        return state;
      }
      const note = typeof event.note === "string" ? event.note : "";
      if (!note) {
        return state;
      }
      return {
        ...state,
        turnActive: true,
        subagentProgress: upsertProgress(state.subagentProgress, { asset_id: event.asset_id, note })
      };
    }
    case "context_compaction_failed":
      return {
        ...state,
        turnActive: true,
        modelRetry: null,
        items: upsertMessage(state.items, {
          type: "message",
          message_id: "context_compaction_failed",
          kind: "observation",
          text: "上下文压缩降级：本轮使用确定性摘要"
        })
      };
    case "turn_ended":
      // 封口：本回合结束，历史消息会被 refetch 接管；流式 buffer 交给 message_id 去重清理。
      return { ...state, turnActive: false, modelRetry: null, subagentProgress: [] };
    case "turn_error":
      return { ...state, turnActive: false, modelRetry: null, subagentProgress: [] };
    default:
      return state;
  }
}

export function useTurnStream(
  draftId: string,
  options: UseTurnStreamOptions = {}
): TurnStreamState & { reset: () => void } {
  const [state, dispatch] = useReducer(reduceTurnStream, INITIAL_STATE);
  const optionsRef = useRef(options);
  optionsRef.current = options;
  const documentVisible = useDocumentVisibility();
  const reset = useCallback(() => dispatch({ type: "local_reset" }), []);

  useEffect(() => {
    if (!documentVisible) {
      return;
    }
    const { source, release } = acquireApiEventSource(`/api/drafts/${draftId}/events`);
    const handle = (raw: Event) => {
      const message = raw as MessageEvent<string>;
      let event: TurnStreamEvent;
      try {
        event = JSON.parse(message.data) as TurnStreamEvent;
      } catch {
        return;
      }
      dispatch(event);
      if (event.type === "stream_snapshot_truncated" || event.type === "stream_gap") {
        optionsRef.current.onStreamGap?.();
      } else if (event.type === "turn_ended") {
        optionsRef.current.onTurnEnded?.(normalizeTurnEndedEvent(event));
      } else if (event.type === "turn_error") {
        optionsRef.current.onTurnError?.(
          typeof event.message === "string" ? event.message : "本轮出错"
        );
      }
    };
    const handleError = () => optionsRef.current.onStreamGap?.();
    source.addEventListener("turn_stream", handle);
    source.addEventListener("error", handleError);
    return () => {
      source.removeEventListener("turn_stream", handle);
      source.removeEventListener("error", handleError);
      release();
    };
  }, [documentVisible, draftId]);

  return { ...state, reset };
}

export function normalizeTurnEndedEvent(event: TurnStreamEvent): TurnEndedEvent {
  const raw = event as Record<string, unknown>;
  const result: TurnEndedEvent = {
    outcome: typeof raw.outcome === "string" ? raw.outcome : "finished",
    reason: typeof raw.reason === "string" ? raw.reason : null
  };
  if (event.type !== "turn_ended" || !isTokenUsage(raw.token_usage)) {
    return result;
  }
  return { ...result, token_usage: raw.token_usage };
}

function isTokenUsage(value: unknown): value is TokenUsage {
  if (value === null || typeof value !== "object") {
    return false;
  }
  const usage = value as Record<string, unknown>;
  return [
    "model_calls",
    "prompt_tokens",
    "cached_prompt_tokens",
    "completion_tokens",
    "total_tokens"
  ].every((key) => typeof usage[key] === "number" && Number.isFinite(usage[key]));
}

function appendDelta(
  items: TurnStreamItem[],
  messageId: string,
  delta: string
): TurnStreamItem[] {
  const index = items.findIndex(
    (item) => item.type === "message" && item.message_id === messageId
  );
  if (index < 0) {
    return [...items, { type: "message", message_id: messageId, kind: "assistant", text: delta }];
  }
  return items.map((item, current) =>
    current === index && item.type === "message" ? { ...item, text: item.text + delta } : item
  );
}

function upsertMessage(items: TurnStreamItem[], next: StreamMessageItem): TurnStreamItem[] {
  const index = items.findIndex(
    (item) => item.type === "message" && item.message_id === next.message_id
  );
  if (index < 0) {
    return [...items, next];
  }
  return items.map((item, current) => (current === index ? next : item));
}

function upsertToolStep(items: TurnStreamItem[], next: StreamToolItem): TurnStreamItem[] {
  const index = items.findIndex((item) => item.type === "tool" && item.step_id === next.step_id);
  if (index < 0) {
    return [...items, next];
  }
  // started 带 argsSummary、finished 带 observation：两次事件各补一半，合并保留已知字段。
  return items.map((item, current) =>
    current === index && item.type === "tool"
      ? {
          ...item,
          status: next.status,
          argsSummary: next.argsSummary ?? item.argsSummary,
          observation: next.observation ?? item.observation,
          progress: next.progress ?? item.progress,
          progressNote: next.progressNote ?? item.progressNote,
          durationMs: next.durationMs ?? item.durationMs,
          harnessOwned: next.harnessOwned || item.harnessOwned
        }
      : item
  );
}

function upsertStopGate(items: TurnStreamItem[], next: StreamStopGateItem): TurnStreamItem[] {
  const index = items.findIndex(
    (item) => item.type === "stop_gate" && item.gate_id === next.gate_id
  );
  if (index < 0) {
    return [...items, next];
  }
  return items.map((item, current) =>
    current === index && item.type === "stop_gate"
      ? {
          ...next,
          traceIds: [...new Set([...item.traceIds, ...next.traceIds])]
        }
      : item
  );
}

function normalizeProgress(value: unknown): number | null {
  return typeof value === "number" && value >= 0 && value <= 1 ? value : null;
}

function upsertProgress(
  entries: SubagentProgressEntry[],
  next: SubagentProgressEntry
): SubagentProgressEntry[] {
  const index = entries.findIndex((entry) => entry.asset_id === next.asset_id);
  if (index < 0) {
    return [...entries, next];
  }
  // 同一素材的新 note 覆盖旧的，保持原有顺序稳定（不因刷新跳到末尾）。
  return entries.map((entry, current) => (current === index ? next : entry));
}

function normalizeCompletedKind(kind: unknown): TurnStreamMessageKind {
  if (kind === "narration" || kind === "observation" || kind === "turn_failure") {
    return kind;
  }
  return "reply";
}
