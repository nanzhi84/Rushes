import { act, render } from "@testing-library/react";
import { Profiler, useCallback, useEffect, useRef, useState } from "react";
import type { ProfilerOnRenderCallback, ReactElement } from "react";
import { describe, expect, it, vi } from "vitest";
import { TimelineViewer } from "./TimelineViewer";
import type { TimelineJson } from "./TimelineViewer";
import { DiffusionPreviewPlayer } from "../PreviewPlayer";

// wavesurfer 在 jsdom 下需打桩，避免真实音频解码。
const waveSurferMock = vi.hoisted(() => ({
  create: vi.fn(() => ({
    on: vi.fn(),
    un: vi.fn(),
    exportPeaks: vi.fn(() => [[]]),
    getDuration: vi.fn(() => 3),
    destroy: vi.fn()
  }))
}));

vi.mock("wavesurfer.js", () => ({
  default: { create: waveSurferMock.create }
}));

function timelineFixture(): TimelineJson {
  return {
    fps: 30,
    duration_frames: 90,
    tracks: [
      {
        track_id: "visual_base",
        clips: [
          {
            timeline_clip_id: "tc_a",
            track_id: "visual_base",
            timeline_start_frame: 0,
            timeline_end_frame: 30,
            asset_id: "asset_a"
          },
          {
            timeline_clip_id: "tc_b",
            track_id: "visual_base",
            timeline_start_frame: 30,
            timeline_end_frame: 90,
            asset_id: "asset_b"
          }
        ]
      }
    ]
  };
}

// StreamConsole 内部持有“流式对话”高频态，模拟 SSE text_delta 只更新左栏子树。
// 它把自己的 setState 通过 register 暴露给测试驱动——对应真实结构里 streamItems 下沉到
// ConsolePanel：高频更新只发生在这一子树，不会向上重渲染工作区与时间线。
function StreamConsole({
  register
}: {
  register: (setter: (text: string) => void) => void;
}): ReactElement {
  const [text, setText] = useState("");
  const registerRef = useRef(register);
  registerRef.current = register;
  useEffect(() => {
    registerRef.current(setText);
  }, []);
  return <div data-testid="stream-console">{text}</div>;
}

// Workspace 复现 DraftEditorView 的同层结构：左侧高频对话子树 + 右侧时间线是兄弟节点。
// 时间线的 props 全部引用稳定（timeline 取自 useState、回调走 useCallback、pxPerSec 为字面量）。
function Workspace({
  selectedClipId,
  onTimelineRender,
  register
}: {
  selectedClipId: string | null;
  onTimelineRender: ProfilerOnRenderCallback;
  register: (setter: (text: string) => void) => void;
}): ReactElement {
  const [timeline] = useState(timelineFixture);
  const onClipClick = useCallback(() => {}, []);
  const onSeek = useCallback(() => {}, []);
  return (
    <>
      <StreamConsole register={register} />
      <Profiler id="timeline" onRender={onTimelineRender}>
        <TimelineViewer
          timeline={timeline}
          pxPerSec={60}
          selectedClipId={selectedClipId}
          onClipClick={onClipClick}
          onSeek={onSeek}
        />
      </Profiler>
    </>
  );
}

describe("时间线 memo 边界与流式高频态隔离", () => {
  it("流式高频态只重渲染对话子树，时间线 Profiler onRender 为 0；时间线交互 props 变化时照常重渲染", () => {
    let streamSetter: ((text: string) => void) | null = null;
    const onTimelineRender = vi.fn();
    const { rerender } = render(
      <Workspace
        selectedClipId={null}
        onTimelineRender={onTimelineRender}
        register={(setter) => {
          streamSetter = setter;
        }}
      />
    );

    // 忽略挂载阶段（含挂载期布局 effect）的渲染，只观测其后。
    onTimelineRender.mockClear();

    // 模拟 50 个 text_delta：只更新对话子树，工作区与时间线子树不应有任何提交。
    for (let i = 1; i <= 50; i += 1) {
      act(() => streamSetter?.("响".repeat(i)));
    }
    expect(onTimelineRender).toHaveBeenCalledTimes(0);

    // 选中片段属于时间线交互 props：memo 不能把这类真实更新挡掉，必须照常重渲染。
    rerender(
      <Workspace
        selectedClipId="tc_b"
        onTimelineRender={onTimelineRender}
        register={() => {}}
      />
    );
    expect(onTimelineRender).toHaveBeenCalled();
  });

  it("TimelineViewer 与 DiffusionPreviewPlayer 都以 React.memo 包裹（移除 memo 即失败）", () => {
    const memoTag = Symbol.for("react.memo");
    expect((TimelineViewer as unknown as { $$typeof?: symbol }).$$typeof).toBe(memoTag);
    expect((DiffusionPreviewPlayer as unknown as { $$typeof?: symbol }).$$typeof).toBe(memoTag);
  });
});
