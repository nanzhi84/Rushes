import { describe, expect, it } from "vitest";
import { INITIAL_STATE, reduceTurnStream } from "./useTurnStream";
import type { TurnStreamEvent, TurnStreamState } from "./useTurnStream";

function apply(events: TurnStreamEvent[]): TurnStreamState {
  return events.reduce(reduceTurnStream, INITIAL_STATE);
}

describe("reduceTurnStream · subagent_progress", () => {
  it("后台 observation 完成事件保留专用消息类型", () => {
    const state = apply([
      { type: "turn_started", turn_id: "turn_1" },
      {
        type: "message_completed",
        message_id: "m1",
        kind: "observation",
        content: "后台任务已完成"
      }
    ]);
    expect(state.items).toEqual([
      {
        type: "message",
        message_id: "m1",
        kind: "observation",
        text: "后台任务已完成"
      }
    ]);
  });

  it("理解批次进度按 completed/total 递增并在工具完成后清空", () => {
    const running = apply([
      { type: "turn_started", turn_id: "turn_1" },
      { type: "tool_step_started", step_id: "s1", tool: "understand.materials" },
      {
        type: "subagent_progress",
        tool: "understand.materials",
        completed: 0,
        total: 3,
        note: "理解中 0/3"
      },
      {
        type: "subagent_progress",
        tool: "understand.materials",
        completed: 2,
        total: 3,
        note: "理解中 2/3"
      }
    ]);
    expect(running.understandingProgress).toEqual({ completed: 2, total: 3 });

    const finished = reduceTurnStream(running, {
      type: "tool_step_finished",
      step_id: "s1",
      tool: "understand.materials",
      status: "succeeded"
    });
    expect(finished.understandingProgress).toBeNull();
  });

  it("按 asset_id 记录最近一条 note", () => {
    const state = apply([
      { type: "turn_started", turn_id: "turn_1" },
      { type: "subagent_progress", asset_id: "asset_01a2", note: "正在查看 IMG_2031.mp4 02:10 画面" }
    ]);
    expect(state.subagentProgress).toEqual([
      { asset_id: "asset_01a2", note: "正在查看 IMG_2031.mp4 02:10 画面" }
    ]);
  });

  it("同一 asset 的新 note 覆盖旧的且保持原有顺序", () => {
    const state = apply([
      { type: "turn_started", turn_id: "turn_1" },
      { type: "subagent_progress", asset_id: "asset_01a2", note: "正在查看 IMG_2031.mp4 画面" },
      { type: "subagent_progress", asset_id: "asset_09f3", note: "转写音频中" },
      { type: "subagent_progress", asset_id: "asset_01a2", note: "产出摘要" }
    ]);
    // asset_01a2 覆盖为最新 note，但仍排在 asset_09f3 之前（不跳到末尾）。
    expect(state.subagentProgress).toEqual([
      { asset_id: "asset_01a2", note: "产出摘要" },
      { asset_id: "asset_09f3", note: "转写音频中" }
    ]);
  });

  it("缺 asset_id 或缺 note 的事件被忽略", () => {
    const state = apply([
      { type: "turn_started", turn_id: "turn_1" },
      { type: "subagent_progress", note: "无归属素材" },
      { type: "subagent_progress", asset_id: "asset_x", note: "" },
      { type: "subagent_progress", asset_id: "", note: "空 id" }
    ]);
    expect(state.subagentProgress).toEqual([]);
  });

  it("turn_ended 清空进度", () => {
    const state = apply([
      { type: "turn_started", turn_id: "turn_1" },
      { type: "subagent_progress", asset_id: "asset_01a2", note: "转写音频中" },
      { type: "turn_ended", outcome: "finished", reason: null }
    ]);
    expect(state.subagentProgress).toEqual([]);
    expect(state.turnActive).toBe(false);
  });

  it("turn_error 清空进度", () => {
    const state = apply([
      { type: "turn_started", turn_id: "turn_1" },
      { type: "subagent_progress", asset_id: "asset_01a2", note: "转写音频中" },
      { type: "turn_error", message: "本轮出错" }
    ]);
    expect(state.subagentProgress).toEqual([]);
  });

  it("tool_step_finished 清空进度，避免残留串到下一个工具行", () => {
    const state = apply([
      { type: "turn_started", turn_id: "turn_1" },
      { type: "tool_step_started", step_id: "s1", tool: "understand.materials" },
      { type: "subagent_progress", asset_id: "asset_01a2", note: "转写音频中" },
      { type: "tool_step_finished", step_id: "s1", tool: "understand.materials", status: "succeeded" }
    ]);
    expect(state.subagentProgress).toEqual([]);
  });

  it("turn_started 重置上一回合的残留进度", () => {
    const state = apply([
      { type: "turn_started", turn_id: "turn_1" },
      { type: "subagent_progress", asset_id: "asset_01a2", note: "转写音频中" },
      { type: "turn_started", turn_id: "turn_2" }
    ]);
    expect(state.subagentProgress).toEqual([]);
    expect(state.items).toEqual([]);
  });

  it("text_delta / tool_step_started 不会丢掉已累积的进度", () => {
    const state = apply([
      { type: "turn_started", turn_id: "turn_1" },
      { type: "tool_step_started", step_id: "s1", tool: "understand.materials" },
      { type: "subagent_progress", asset_id: "asset_01a2", note: "转写音频中" },
      { type: "text_delta", message_id: "m1", kind: "assistant", delta: "继续" }
    ]);
    expect(state.subagentProgress).toEqual([{ asset_id: "asset_01a2", note: "转写音频中" }]);
  });
});
