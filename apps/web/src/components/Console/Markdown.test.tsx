import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { Markdown } from "./Markdown";

describe("Markdown 时间线引用", () => {
  it("点击 Agent 回复中的语义片段链接会回选精确时间线 clip", async () => {
    const listener = vi.fn();
    window.addEventListener("rushes:select-timeline-clip", listener);
    try {
      render(<Markdown text={'已调整[「海边日落人物」0.00–3.00 秒](#timeline-clip=clip_v1_001)'} />);

      fireEvent.click(await screen.findByRole("link", { name: "「海边日落人物」0.00–3.00 秒" }));

      expect(listener).toHaveBeenCalledTimes(1);
      expect((listener.mock.calls[0]?.[0] as CustomEvent).detail).toEqual({
        clipId: "clip_v1_001"
      });
      expect(window.location.hash).toBe("");
    } finally {
      window.removeEventListener("rushes:select-timeline-clip", listener);
    }
  });
});
