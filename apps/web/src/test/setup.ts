import { cleanup } from "@testing-library/react";
import { afterEach, vi } from "vitest";

// Radix UI 弹层（Dialog/DropdownMenu/ContextMenu）在 jsdom 下依赖若干浏览器 API，
// jsdom 未实现——补最小垫片，供壳层弹层组件的单测运行（Popper 用 ResizeObserver、
// 菜单聚焦用 scrollIntoView、DismissableLayer 用 pointer capture）。
class ResizeObserverStub {
  observe(): void {}
  unobserve(): void {}
  disconnect(): void {}
}

const globalStubs = globalThis as unknown as Record<string, unknown>;
if (globalStubs.ResizeObserver === undefined) {
  globalStubs.ResizeObserver = ResizeObserverStub;
}
if (globalStubs.PointerEvent === undefined) {
  globalStubs.PointerEvent = class PointerEventStub extends MouseEvent {};
}

const elementProto = Element.prototype as unknown as Record<string, (...args: unknown[]) => unknown>;
elementProto.scrollIntoView ??= () => {};
elementProto.hasPointerCapture ??= () => false;
elementProto.setPointerCapture ??= () => {};
elementProto.releasePointerCapture ??= () => {};

afterEach(() => {
  cleanup();
  window.sessionStorage.clear();
  window.localStorage.clear();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  window.history.pushState(null, document.title, "/");
});
