import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { PreviewPlayer } from "./PreviewPlayer";

const vidstackMock = vi.hoisted(() => {
  type State = {
    currentTime: number;
    duration: number;
    bufferedEnd: number;
    playing: boolean;
    paused: boolean;
    volume: number;
    muted: boolean;
    fullscreen: boolean;
    playbackRate: number;
  };
  const initial = (): State => ({
    currentTime: 0,
    duration: 10,
    bufferedEnd: 0,
    playing: false,
    paused: true,
    volume: 1,
    muted: false,
    fullscreen: false,
    playbackRate: 1
  });
  let state: State = initial();
  const listeners = new Set<() => void>();
  const emit = () => {
    for (const listener of listeners) {
      listener();
    }
  };
  const seek = vi.fn((time: number) => {
    state = { ...state, currentTime: time };
    emit();
  });
  const changeVolume = vi.fn((value: number) => {
    state = { ...state, volume: value };
    emit();
  });
  const toggleMuted = vi.fn(() => {
    state = { ...state, muted: !state.muted };
    emit();
  });
  const unmute = vi.fn(() => {
    state = { ...state, muted: false };
    emit();
  });
  const toggleFullscreen = vi.fn(() => {
    state = { ...state, fullscreen: !state.fullscreen };
    emit();
  });
  return {
    seek,
    changeVolume,
    toggleMuted,
    unmute,
    toggleFullscreen,
    reset() {
      state = initial();
      seek.mockClear();
      changeVolume.mockClear();
      toggleMuted.mockClear();
      unmute.mockClear();
      toggleFullscreen.mockClear();
      emit();
    },
    get<T extends keyof State>(key: T): State[T] {
      return state[key];
    },
    set(patch: Partial<State>) {
      state = { ...state, ...patch };
      emit();
    },
    subscribe(listener: () => void) {
      listeners.add(listener);
      return () => {
        listeners.delete(listener);
      };
    }
  };
});

vi.mock("@vidstack/react", async () => {
  const React = await import("react");
  return {
    MediaPlayer({
      children,
      src
    }: {
      children?: unknown;
      src: string | { src: string; type: string };
    }) {
      return React.createElement(
        "div",
        {
          "data-testid": "media-player",
          "data-src": typeof src === "string" ? src : src.src,
          "data-type": typeof src === "string" ? "" : src.type
        },
        children as never
      );
    },
    MediaProvider() {
      return React.createElement("video", { "data-testid": "vidstack-video" });
    },
    useMediaState(key: string) {
      const [value, setValue] = React.useState(() =>
        vidstackMock.get(key as never)
      );
      React.useEffect(
        () => vidstackMock.subscribe(() => setValue(vidstackMock.get(key as never))),
        [key]
      );
      return value;
    },
    useMediaRemote() {
      return {
        seek: vidstackMock.seek,
        changeVolume: vidstackMock.changeVolume,
        toggleMuted: vidstackMock.toggleMuted,
        unmute: vidstackMock.unmute,
        toggleFullscreen: vidstackMock.toggleFullscreen,
        play() {
          vidstackMock.set({ playing: true, paused: false });
        },
        pause() {
          vidstackMock.set({ playing: false, paused: true });
        }
      };
    }
  };
});

describe("PreviewPlayer", () => {
  beforeEach(() => {
    vidstackMock.reset();
  });

  it("按 fps 做一帧步进并以 mm:ss:ff 时间码显示进度与总长", async () => {
    render(<PreviewPlayer src="/api/media/preview/prev_1" fps={30} />);

    expect(screen.getByTestId("media-player").getAttribute("data-src")).toBe(
      "/api/media/preview/prev_1"
    );
    expect(screen.getByTestId("media-player").getAttribute("data-type")).toBe("video/mp4");
    expect(screen.getByText("00:00:00")).toBeTruthy();
    // 总长 10s @30fps → mm:ss:ff = 00:10:00
    expect(screen.getByText("00:10:00")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "前进一帧" }));

    await waitFor(() => expect(vidstackMock.get("currentTime")).toBeCloseTo(1 / 30, 4));
    expect(screen.getByText("00:00:01")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "后退一帧" }));

    await waitFor(() => expect(vidstackMock.get("currentTime")).toBeCloseTo(0, 4));
    expect(screen.getByText("00:00:00")).toBeTruthy();
  });

  it("播放/暂停按钮在两态间切换并驱动 remote", async () => {
    render(<PreviewPlayer src="/api/media/preview/prev_1" fps={30} />);

    fireEvent.click(screen.getByRole("button", { name: "播放" }));

    await waitFor(() => expect(vidstackMock.get("playing")).toBe(true));
    const pauseButton = screen.getByRole("button", { name: "暂停" });

    fireEvent.click(pauseButton);
    await waitFor(() => expect(vidstackMock.get("paused")).toBe(true));
    expect(screen.getByRole("button", { name: "播放" })).toBeTruthy();
  });

  it("拖动进度条触发受控 seek 并刷新时间码", async () => {
    render(<PreviewPlayer src="/api/media/preview/prev_1" fps={30} />);

    const scrub = screen.getByRole("slider", { name: "播放进度" });
    fireEvent.change(scrub, { target: { value: "5" } });

    await waitFor(() => expect(vidstackMock.seek).toHaveBeenLastCalledWith(5, expect.anything()));
    // 5s @30fps → mm:ss:ff = 00:05:00
    expect(screen.getByText("00:05:00")).toBeTruthy();
  });

  it("静音按钮切换 muted，音量滑杆改变音量并取消静音", async () => {
    render(<PreviewPlayer src="/api/media/preview/prev_1" fps={30} />);

    fireEvent.click(screen.getByRole("button", { name: "静音" }));
    await waitFor(() => expect(vidstackMock.toggleMuted).toHaveBeenCalledTimes(1));
    expect(screen.getByRole("button", { name: "取消静音" })).toBeTruthy();

    const volume = screen.getByRole("slider", { name: "音量" });
    fireEvent.change(volume, { target: { value: "0.3" } });

    await waitFor(() =>
      expect(vidstackMock.changeVolume).toHaveBeenLastCalledWith(0.3, expect.anything())
    );
    expect(vidstackMock.unmute).toHaveBeenCalledTimes(1);
  });

  it("全屏按钮切换全屏并随状态更新标签", async () => {
    render(<PreviewPlayer src="/api/media/preview/prev_1" fps={30} />);

    fireEvent.click(screen.getByRole("button", { name: "全屏" }));
    await waitFor(() => expect(vidstackMock.toggleFullscreen).toHaveBeenCalledTimes(1));

    act(() => {
      vidstackMock.set({ fullscreen: true });
    });
    expect(screen.getByRole("button", { name: "退出全屏" })).toBeTruthy();
  });

  it("首次进入 playing 状态时只触发一次 onFirstPlay", async () => {
    const onFirstPlay = vi.fn();
    render(<PreviewPlayer src="/api/media/preview/prev_1" fps={30} onFirstPlay={onFirstPlay} />);

    act(() => {
      vidstackMock.set({ playing: true, paused: false });
    });

    await waitFor(() => expect(onFirstPlay).toHaveBeenCalledTimes(1));

    act(() => {
      vidstackMock.set({ playing: false, paused: true });
      vidstackMock.set({ playing: true, paused: false });
    });

    await waitFor(() => expect(onFirstPlay).toHaveBeenCalledTimes(1));
  });

  it("seekSec 变化时只触发一次受控 seek", async () => {
    const { rerender } = render(
      <PreviewPlayer src="/api/media/preview/prev_1" fps={30} seekSec={null} />
    );

    expect(vidstackMock.seek).not.toHaveBeenCalled();

    rerender(<PreviewPlayer src="/api/media/preview/prev_1" fps={30} seekSec={1.25} />);

    await waitFor(() => expect(vidstackMock.seek).toHaveBeenCalledTimes(1));
    expect(vidstackMock.seek).toHaveBeenLastCalledWith(1.25);

    rerender(<PreviewPlayer src="/api/media/preview/prev_1" fps={30} seekSec={1.25} />);

    await waitFor(() => expect(vidstackMock.seek).toHaveBeenCalledTimes(1));
  });

  it("currentTime 变化时通过 onTimeUpdate 上报秒数", async () => {
    const onTimeUpdate = vi.fn();
    render(
      <PreviewPlayer
        src="/api/media/preview/prev_1"
        fps={30}
        onTimeUpdate={onTimeUpdate}
      />
    );

    act(() => {
      vidstackMock.set({ currentTime: 1.5 });
    });

    await waitFor(() => expect(onTimeUpdate).toHaveBeenCalledWith(1.5));
  });

  it("播放期间在离散 timeupdate 之间按动画帧连续上报时间", async () => {
    const frames: FrameRequestCallback[] = [];
    vi.stubGlobal(
      "requestAnimationFrame",
      vi.fn((callback: FrameRequestCallback) => {
        frames.push(callback);
        return frames.length;
      })
    );
    vi.stubGlobal("cancelAnimationFrame", vi.fn());
    vi.spyOn(performance, "now").mockReturnValue(1_000);
    const onTimeUpdate = vi.fn();
    render(
      <PreviewPlayer
        src="/api/media/preview/prev_1"
        fps={30}
        onTimeUpdate={onTimeUpdate}
      />
    );

    act(() => {
      vidstackMock.set({ currentTime: 2, playing: true, paused: false });
    });
    await waitFor(() => expect(frames.length).toBeGreaterThan(0));

    act(() => {
      frames.shift()?.(1_100);
    });

    expect(onTimeUpdate.mock.calls.at(-1)?.[0]).toBeCloseTo(2.1, 5);
  });
});
