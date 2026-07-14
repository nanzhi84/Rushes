import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { TimelineJson } from "../../api/client";
import { DiffusionPreviewPlayer, PreviewScrubber } from "./DiffusionPreviewPlayer";

const diffusionMock = vi.hoisted(() => {
  const instances: unknown[] = [];

  class Engine {
    readonly composition = { currentTime: 0 };
    playing = false;
    clockState = "running";
    clockTime = 0;
    mount = vi.fn();
    onTime = vi.fn(() => () => undefined);
    sync = vi.fn(async () => undefined);
    seekFrame = vi.fn(async (frame: number) => {
      this.composition.currentTime = frame / 30;
    });
    play = vi.fn(async () => {
      this.playing = true;
    });
    pause = vi.fn(async () => {
      this.playing = false;
    });
    recoverPlayback = vi.fn(async () => {
      this.clockState = "running";
      this.playing = true;
    });
    dispose = vi.fn(async () => undefined);

    constructor() {
      instances.push(this);
    }
  }

  return {
    Engine,
    instances: instances as Engine[]
  };
});

vi.mock("../../editor/diffusion_preview_engine", () => ({
  DiffusionPreviewEngine: diffusionMock.Engine
}));

vi.mock("./PreviewPlayer", () => ({
  PreviewPlayer: ({ src }: { src: string }) => <div data-testid="ffmpeg-preview">{src}</div>
}));

const timeline: TimelineJson = {
  fps: 30,
  duration_frames: 90,
  tracks: []
};

function setLocalPreviewSupport(supported: boolean): void {
  Object.defineProperty(window, "AudioContext", {
    configurable: true,
    value: supported ? class AudioContextStub {} : undefined
  });
  Object.defineProperty(window, "VideoDecoder", {
    configurable: true,
    value: supported ? class VideoDecoderStub {} : undefined
  });
}

describe("DiffusionPreviewPlayer", () => {
  beforeEach(() => {
    diffusionMock.instances.length = 0;
    setLocalPreviewSupport(false);
  });

  afterEach(() => {
    setLocalPreviewSupport(false);
  });

  it("当前版本已有服务端预览时直接使用单文件稳定播放", async () => {
    render(<DiffusionPreviewPlayer timeline={timeline} fallbackSrc="/preview.mp4" />);

    await waitFor(() => {
      expect(document.querySelector('[data-preview-engine="ffmpeg-current-version"]')).toBeTruthy();
    });
    expect(screen.getByTestId("ffmpeg-preview").textContent).toBe("/preview.mp4");
    expect(screen.getByText("当前时间线 · 稳定预览")).toBeTruthy();
    expect(diffusionMock.instances).toHaveLength(0);
  });

  it("没有旧预览可回退时给出明确错误，不留下永久加载状态", async () => {
    render(<DiffusionPreviewPlayer timeline={timeline} />);

    expect(
      await screen.findByText("当前浏览器无法启动编辑代理预览；最终导出仍会读取原素材。")
    ).toBeTruthy();
    expect(screen.queryByText("正在准备本地即时预览…")).toBeNull();
  });

  it("拖动时先更新进度和时间码，不等待解码器 seek 完成", async () => {
    setLocalPreviewSupport(true);
    render(<DiffusionPreviewPlayer timeline={timeline} />);

    const scrubber = await screen.findByRole("slider", { name: "预览进度" });
    const engine = diffusionMock.instances[0];
    engine.seekFrame.mockImplementationOnce(() => new Promise<void>(() => undefined));

    fireEvent.change(scrubber, { target: { value: "1.5" } });

    expect((scrubber as HTMLInputElement).value).toBe("1.5");
    expect(
      screen.getByText(
        (_content, element) =>
          element?.tagName === "SPAN" && element.textContent === "00:01:15 / 00:03:00"
      )
    ).toBeTruthy();
    expect(engine.seekFrame).toHaveBeenCalledWith(45);
  });

  it("首次音频时钟授权失败后仍可再次播放", async () => {
    setLocalPreviewSupport(true);
    render(<DiffusionPreviewPlayer timeline={timeline} />);

    const playButton = await screen.findByRole("button", { name: "播放" });
    const engine = diffusionMock.instances[0];
    engine.play.mockRejectedValueOnce(new Error("浏览器未允许启动音频预览时钟"));

    fireEvent.click(playButton);

    await waitFor(() => expect(engine.play).toHaveBeenCalledTimes(1));
    expect((screen.getByRole("button", { name: "播放" }) as HTMLButtonElement).disabled).toBe(false);
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("播放中音频时钟被挂起时只自动恢复一次", async () => {
    setLocalPreviewSupport(true);
    const animationFrames: FrameRequestCallback[] = [];
    const requestFrame = vi
      .spyOn(window, "requestAnimationFrame")
      .mockImplementation((callback) => {
        animationFrames.push(callback);
        return 1;
      });
    vi.spyOn(window, "cancelAnimationFrame").mockImplementation(() => undefined);
    render(<DiffusionPreviewPlayer timeline={timeline} />);

    const playButton = await screen.findByRole("button", { name: "播放" });
    const engine = diffusionMock.instances[0];
    fireEvent.click(playButton);
    await waitFor(() => expect(engine.play).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(animationFrames).toHaveLength(1));

    engine.clockState = "suspended";
    await act(async () => {
      animationFrames[0]?.(performance.now());
    });

    await waitFor(() => expect(engine.recoverPlayback).toHaveBeenCalledTimes(1));
    expect(screen.queryByText("浏览器暂停了音频时钟，点击播放即可继续")).toBeNull();
    requestFrame.mockRestore();
  });
});

describe("PreviewScrubber", () => {
  it("用 20px 热区承载细轨道，并持续显示当前位置滑块", () => {
    render(
      <PreviewScrubber
        currentSec={0.75}
        durationSec={3}
        fps={30}
        disabled={false}
        onSeek={() => undefined}
      />
    );

    const hitArea = screen.getByTestId("preview-scrubber-hit-area");
    const input = screen.getByRole("slider", { name: "预览进度" });
    expect(hitArea.className).toContain("h-5");
    expect(input.className).toContain("h-full");
    expect(screen.getByTestId("preview-scrubber-progress").style.width).toBe("25%");
    expect(screen.getByTestId("preview-scrubber-thumb").style.left).toBe("25%");
  });

  it("支持拖动、逐帧方向键以及首尾快捷键", () => {
    const onSeek = vi.fn();
    render(
      <PreviewScrubber
        currentSec={1}
        durationSec={3}
        fps={30}
        disabled={false}
        onSeek={onSeek}
      />
    );

    const input = screen.getByRole("slider", { name: "预览进度" });
    fireEvent.change(input, { target: { value: "2.25" } });
    expect(onSeek).toHaveBeenLastCalledWith(2.25);

    fireEvent.keyDown(input, { key: "ArrowRight" });
    expect(onSeek.mock.calls.at(-1)?.[0]).toBeCloseTo(1 + 1 / 30, 6);

    fireEvent.keyDown(input, { key: "ArrowLeft" });
    expect(onSeek.mock.calls.at(-1)?.[0]).toBeCloseTo(1 - 1 / 30, 6);

    fireEvent.keyDown(input, { key: "Home" });
    expect(onSeek).toHaveBeenLastCalledWith(0);

    fireEvent.keyDown(input, { key: "End" });
    expect(onSeek).toHaveBeenLastCalledWith(3);
  });

  it("点击和 Pointer Capture 拖动按热区宽度换算定位时间", () => {
    const onSeek = vi.fn();
    render(
      <PreviewScrubber
        currentSec={0}
        durationSec={4}
        fps={30}
        disabled={false}
        onSeek={onSeek}
      />
    );

    const hitArea = screen.getByTestId("preview-scrubber-hit-area");
    vi.spyOn(hitArea, "getBoundingClientRect").mockReturnValue({
      bottom: 20,
      height: 20,
      left: 10,
      right: 210,
      top: 0,
      width: 200,
      x: 10,
      y: 0,
      toJSON: () => ({})
    });

    fireEvent.pointerDown(hitArea, { button: 0, clientX: 110, pointerId: 7 });
    expect(onSeek).toHaveBeenLastCalledWith(2);
    expect(document.activeElement).toBe(screen.getByRole("slider", { name: "预览进度" }));

    fireEvent.pointerMove(hitArea, { clientX: 160, pointerId: 7 });
    expect(onSeek).toHaveBeenLastCalledWith(3);

    fireEvent.pointerUp(hitArea, { button: 0, clientX: 210, pointerId: 7 });
    expect(onSeek).toHaveBeenLastCalledWith(4);
  });
});
