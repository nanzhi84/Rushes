import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { PreviewPlayer } from "./PreviewPlayer";

const vidstackMock = vi.hoisted(() => {
  type State = {
    currentTime: number;
    playing: boolean;
    paused: boolean;
  };
  let state: State = { currentTime: 0, playing: false, paused: true };
  const listeners = new Set<() => void>();
  const emit = () => {
    for (const listener of listeners) {
      listener();
    }
  };
  return {
    reset() {
      state = { currentTime: 0, playing: false, paused: true };
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
    MediaPlayer({ children, src }: { children?: unknown; src: string }) {
      return React.createElement(
        "div",
        { "data-testid": "media-player", "data-src": src },
        children as never
      );
    },
    MediaProvider() {
      return React.createElement("video", { "data-testid": "vidstack-video" });
    },
    useMediaState<T extends "currentTime" | "playing" | "paused">(key: T) {
      const [value, setValue] = React.useState(vidstackMock.get(key));
      React.useEffect(
        () => vidstackMock.subscribe(() => setValue(vidstackMock.get(key))),
        [key]
      );
      return value;
    },
    useMediaRemote() {
      return {
        seek(time: number) {
          vidstackMock.set({ currentTime: time });
        },
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

  it("按 fps 做一帧步进并显示当前帧号", async () => {
    render(<PreviewPlayer src="/api/media/preview/prev_1" fps={30} />);

    expect(screen.getByTestId("media-player").getAttribute("data-src")).toBe(
      "/api/media/preview/prev_1"
    );
    expect(screen.getByText("0")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "前进一帧" }));

    await waitFor(() => expect(vidstackMock.get("currentTime")).toBeCloseTo(1 / 30, 4));
    expect(screen.getByText("1")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "后退一帧" }));

    await waitFor(() => expect(vidstackMock.get("currentTime")).toBeCloseTo(0, 4));
    expect(screen.getByText("0")).toBeTruthy();
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
});
