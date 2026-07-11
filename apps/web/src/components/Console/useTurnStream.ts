import { useEffect, useReducer, useRef } from "react";
import { createDraftTurnStreamSource } from "../../api/client";

// text_delta 阶段是 assistant；完成后区分叙述、正式回复和后台观察。
export type TurnStreamMessageKind = "assistant" | "narration" | "reply" | "observation";

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
};

// 消息与工具步合并成按到达顺序排列的单一列表：前端据此把工具行内嵌在
// 叙述之间（对齐 Claude Code 的呈现方式），而不是把工具堆在消息流末尾。
export type TurnStreamItem = StreamMessageItem | StreamToolItem;

// 素材理解等子代理执行期间的实时动态：按素材粒度只保留最近一条 note。
export type SubagentProgressEntry = {
  asset_id: string;
  note: string;
};

export type UnderstandingProgress = {
  completed: number;
  total: number;
};

export type TurnStreamState = {
  items: TurnStreamItem[];
  turnActive: boolean;
  // 挂在「当前进行中工具行」下方的子代理进度；工具收尾或回合结束即清空。
  subagentProgress: SubagentProgressEntry[];
  understandingProgress: UnderstandingProgress | null;
};

// 服务端统一发送 event: turn_stream；8 种 type 由 go/internal/agent 定义。
export type TurnStreamEvent =
  | { type: "turn_started"; turn_id?: string }
  | { type: "text_delta"; message_id: string; kind?: string; delta?: string }
  | {
      type: "message_completed";
      message_id: string;
      kind: "narration" | "reply" | "observation";
      content: string;
    }
  | { type: "tool_step_started"; step_id: string; tool: string; args_summary?: string }
  | { type: "tool_step_finished"; step_id: string; tool: string; status: string; observation?: string }
  | {
      type: "subagent_progress";
      asset_id?: string;
      note?: string;
      tool?: string;
      completed?: number;
      total?: number;
    }
  | { type: "turn_ended"; outcome: string; reason: string | null }
  | { type: "turn_error"; message: string }
  | { type: string; [key: string]: unknown };

export type UseTurnStreamOptions = {
  onTurnEnded?: (event: { outcome: string; reason: string | null }) => void;
  onTurnError?: (message: string) => void;
};

export const INITIAL_STATE: TurnStreamState = {
  items: [],
  turnActive: false,
  subagentProgress: [],
  understandingProgress: null
};

// 纯 reducer：便于单测，也让重连快照重放（turn_started 起头）能从头重建状态。
export function reduceTurnStream(state: TurnStreamState, event: TurnStreamEvent): TurnStreamState {
  switch (event.type) {
    case "turn_started":
      // 新回合（或重连重放）从零重建，避免 text_delta 被重复追加。
      return { items: [], turnActive: true, subagentProgress: [], understandingProgress: null };
    case "text_delta": {
      if (typeof event.message_id !== "string") {
        return state;
      }
      return {
        ...state,
        turnActive: true,
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
        items: upsertToolStep(state.items, {
          type: "tool",
          step_id: event.step_id,
          tool: event.tool,
          status: "running",
          argsSummary: typeof event.args_summary === "string" && event.args_summary ? event.args_summary : null,
          observation: null
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
          observation: typeof event.observation === "string" && event.observation ? event.observation : null
        }),
        // 工具收尾即清空其子代理进度，避免残留串到下一个进行中工具行上。
        subagentProgress: [],
        understandingProgress:
          event.tool === "understand.materials" ? null : state.understandingProgress
      };
    }
    case "subagent_progress": {
      const understandingProgress = parseUnderstandingProgress(event) ?? state.understandingProgress;
      if (typeof event.asset_id !== "string" || !event.asset_id) {
        return understandingProgress === state.understandingProgress
          ? state
          : { ...state, turnActive: true, understandingProgress };
      }
      const note = typeof event.note === "string" ? event.note : "";
      if (!note) {
        return state;
      }
      return {
        ...state,
        turnActive: true,
        subagentProgress: upsertProgress(state.subagentProgress, { asset_id: event.asset_id, note }),
        understandingProgress
      };
    }
    case "turn_ended":
      // 封口：本回合结束，历史消息会被 refetch 接管；流式 buffer 交给 message_id 去重清理。
      return { ...state, turnActive: false, subagentProgress: [], understandingProgress: null };
    case "turn_error":
      return { ...state, turnActive: false, subagentProgress: [], understandingProgress: null };
    default:
      return state;
  }
}

export function useTurnStream(
  draftId: string,
  options: UseTurnStreamOptions = {}
): TurnStreamState {
  const [state, dispatch] = useReducer(reduceTurnStream, INITIAL_STATE);
  const optionsRef = useRef(options);
  optionsRef.current = options;

  useEffect(() => {
    const source = createDraftTurnStreamSource(draftId);
    const handle = (raw: Event) => {
      const message = raw as MessageEvent<string>;
      let event: TurnStreamEvent;
      try {
        event = JSON.parse(message.data) as TurnStreamEvent;
      } catch {
        return;
      }
      dispatch(event);
      if (event.type === "turn_ended") {
        optionsRef.current.onTurnEnded?.({
          outcome: typeof event.outcome === "string" ? event.outcome : "finished",
          reason: typeof event.reason === "string" ? event.reason : null
        });
      } else if (event.type === "turn_error") {
        optionsRef.current.onTurnError?.(
          typeof event.message === "string" ? event.message : "本轮出错"
        );
      }
    };
    source.addEventListener("turn_stream", handle);
    return () => {
      source.removeEventListener("turn_stream", handle);
      source.close();
    };
  }, [draftId]);

  return state;
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
          observation: next.observation ?? item.observation
        }
      : item
  );
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
  if (kind === "narration" || kind === "observation") {
    return kind;
  }
  return "reply";
}

function parseUnderstandingProgress(event: TurnStreamEvent): UnderstandingProgress | null {
  if (
    event.type !== "subagent_progress" ||
    event.tool !== "understand.materials" ||
    typeof event.completed !== "number" ||
    typeof event.total !== "number"
  ) {
    return null;
  }
  const total = Math.max(0, Math.floor(event.total));
  return {
    completed: Math.max(0, Math.min(total, Math.floor(event.completed))),
    total
  };
}
