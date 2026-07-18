import { describe, expect, it } from "vitest";
import { INITIAL_STATE, normalizeTurnEndedEvent, reduceTurnStream } from "./useTurnStream";
import type { TurnStreamEvent, TurnStreamState } from "./useTurnStream";

function apply(events: TurnStreamEvent[]): TurnStreamState {
  return events.reduce(reduceTurnStream, INITIAL_STATE);
}

describe("reduceTurnStream · subagent_progress", () => {
  it("上下文压缩失败显示非阻塞降级提示", () => {
    const state = apply([
      { type: "turn_started", turn_id: "turn_1" },
      { type: "model_retry", attempt: 5, max_retries: 5 },
      { type: "context_compaction_failed", reason: "模型不可用" }
    ]);

    expect(state.turnActive).toBe(true);
    expect(state.modelRetry).toBeNull();
    expect(state.items).toContainEqual({
      type: "message",
      message_id: "context_compaction_failed",
      kind: "observation",
      text: "上下文压缩降级：本轮使用确定性摘要"
    });
  });

  it("原样转发回合终态 token usage，并兼容旧事件", () => {
    const token_usage = {
      model_calls: 2,
      prompt_tokens: 100,
      cached_prompt_tokens: 40,
      completion_tokens: 20,
      total_tokens: 120
    };
    expect(normalizeTurnEndedEvent({
      type: "turn_ended", outcome: "finished", reason: null, token_usage
    })).toEqual({ outcome: "finished", reason: null, token_usage });
    expect(normalizeTurnEndedEvent({
      type: "turn_ended", outcome: "cancelled", reason: "用户取消"
    })).toEqual({ outcome: "cancelled", reason: "用户取消" });
  });
  it("显示模型超时重试，并在模型恢复产出后清空", () => {
    const retrying = apply([
      { type: "turn_started", turn_id: "turn_1" },
      {
        type: "model_retry",
        attempt: 2,
        max_retries: 5,
        reason: "模型响应超时",
        next_delay_ms: 500
      }
    ]);
    expect(retrying.turnActive).toBe(true);
    expect(retrying.modelRetry).toEqual({
      attempt: 2,
      maxRetries: 5,
      reason: "模型响应超时",
      nextDelayMs: 500
    });

    const recovered = reduceTurnStream(retrying, {
      type: "text_delta",
      message_id: "m1",
      kind: "assistant",
      delta: "已恢复"
    });
    expect(recovered.modelRetry).toBeNull();
    expect(recovered.items).toHaveLength(1);
  });

  it("本地回退重置会清空整段流式缓冲与运行态", () => {
    const running = apply([
      { type: "turn_started", turn_id: "turn_rewind" },
      { type: "text_delta", message_id: "late-message", delta: "将被撤销" },
      { type: "tool_step_started", step_id: "late-tool", tool: "assets.list" }
    ]);
    const reset = reduceTurnStream(running, { type: "local_reset" });
    expect(reset).toEqual(INITIAL_STATE);
  });

  it("流缺口信号不伪造回合终态", () => {
    const running = apply([
      { type: "turn_started", turn_id: "turn_gap" },
      { type: "text_delta", message_id: "active", delta: "仍在执行" }
    ]);
    expect(reduceTurnStream(running, { type: "stream_gap" })).toBe(running);
    expect(reduceTurnStream(running, { type: "stream_snapshot_truncated" })).toBe(running);
  });

  it("忽略非法重试序号，并在回合结束时清空重试状态", () => {
    const invalid = apply([
      { type: "turn_started", turn_id: "turn_1" },
      { type: "model_retry", attempt: 6, max_retries: 5 }
    ]);
    expect(invalid.modelRetry).toBeNull();

    const ended = apply([
      { type: "turn_started", turn_id: "turn_1" },
      { type: "model_retry", attempt: 5, max_retries: 5 },
      { type: "turn_ended", outcome: "failed", reason: "模型响应超时" }
    ]);
    expect(ended.modelRetry).toBeNull();
    expect(ended.turnActive).toBe(false);
  });

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

  it("回合失败终态复用 message_completed 通道并保留 turn_failure 类型", () => {
    const state = apply([
      { type: "turn_started", turn_id: "turn_1" },
      { type: "text_delta", message_id: "m1", kind: "assistant", delta: "本轮没有完成" },
      {
        type: "message_completed",
        message_id: "m1",
        kind: "turn_failure",
        content: "本轮没有完成：模型响应超时，系统已停止重试。"
      }
    ]);
    // text_delta 阶段以 assistant 流式呈现，完成后整体替换为 turn_failure 终态。
    expect(state.items).toEqual([
      {
        type: "message",
        message_id: "m1",
        kind: "turn_failure",
        text: "本轮没有完成：模型响应超时，系统已停止重试。"
      }
    ]);
  });

  it("不为无素材详情的批次进度创建额外 UI 状态", () => {
    const state = apply([
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
    expect(state.subagentProgress).toEqual([]);
    expect(state.items).toHaveLength(1);
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

describe("reduceTurnStream · memory_updated", () => {
  it("写入成功追加一张记忆卡片项，id 稳定且 replay 可重建", () => {
    const events: TurnStreamEvent[] = [
      { type: "turn_started", turn_id: "turn_1" },
      { type: "memory_updated", written_keys: ["pacing"], removed_keys: [], total: 1 }
    ];
    const first = apply(events);
    const memories = first.items.filter((item) => item.type === "memory");
    expect(memories).toHaveLength(1);
    expect(memories[0]).toEqual({
      type: "memory",
      id: "memory_0",
      written_keys: ["pacing"],
      removed_keys: [],
      total: 1
    });
    // 相同事件序列重放得到同样的 id，保证虚拟化 key 稳定。
    expect(apply(events)).toEqual(first);
  });

  it("既无写入也无移除的记忆事件被忽略", () => {
    const state = apply([
      { type: "turn_started", turn_id: "turn_1" },
      { type: "memory_updated", written_keys: [], removed_keys: [], total: 0 }
    ]);
    expect(state.items.filter((item) => item.type === "memory")).toHaveLength(0);
  });
});
