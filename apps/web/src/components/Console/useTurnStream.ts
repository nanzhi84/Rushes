import { useEffect, useReducer, useRef } from "react";
import { createDraftTurnStreamSource } from "../../api/client";

// 流式消息 kind：text_delta 阶段是 assistant（尚未定型），message_completed 后定为 narration/reply。
export type TurnStreamMessageKind = "assistant" | "narration" | "reply";

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

export type TurnStreamState = {
  items: TurnStreamItem[];
  turnActive: boolean;
};

// 服务端 event: turn_stream，data 里以 type 区分。字段见 packages/agent_harness/loop.py。
export type TurnStreamEvent =
  | { type: "turn_started"; turn_id?: string }
  | { type: "text_delta"; message_id: string; kind?: string; delta?: string }
  | { type: "message_completed"; message_id: string; kind: "narration" | "reply"; content: string }
  | { type: "tool_step_started"; step_id: string; tool: string; args_summary?: string }
  | { type: "tool_step_finished"; step_id: string; tool: string; status: string; observation?: string }
  | { type: "turn_ended"; outcome: string; reason: string | null }
  | { type: "turn_error"; message: string }
  | { type: string; [key: string]: unknown };

export type UseTurnStreamOptions = {
  onTurnEnded?: (event: { outcome: string; reason: string | null }) => void;
  onTurnError?: (message: string) => void;
};

const INITIAL_STATE: TurnStreamState = {
  items: [],
  turnActive: false
};

// 纯 reducer：便于单测，也让重连快照重放（turn_started 起头）能从头重建状态。
export function reduceTurnStream(state: TurnStreamState, event: TurnStreamEvent): TurnStreamState {
  switch (event.type) {
    case "turn_started":
      // 新回合（或重连重放）从零重建，避免 text_delta 被重复追加。
      return { items: [], turnActive: true };
    case "text_delta": {
      if (typeof event.message_id !== "string") {
        return state;
      }
      return {
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
        })
      };
    }
    case "turn_ended":
      // 封口：本回合结束，历史消息会被 refetch 接管；流式 buffer 交给 message_id 去重清理。
      return { ...state, turnActive: false };
    case "turn_error":
      return { ...state, turnActive: false };
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

function normalizeCompletedKind(kind: unknown): TurnStreamMessageKind {
  return kind === "narration" ? "narration" : "reply";
}
