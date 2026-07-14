import { act, renderHook } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { useDocumentVisibility } from "./use_document_visibility";

const originalVisibilityState = document.visibilityState;

function setVisibility(state: DocumentVisibilityState): void {
  Object.defineProperty(document, "visibilityState", { configurable: true, value: state });
  document.dispatchEvent(new Event("visibilitychange"));
}

afterEach(() => {
  Object.defineProperty(document, "visibilityState", {
    configurable: true,
    value: originalVisibilityState
  });
});

describe("useDocumentVisibility", () => {
  it("随标签页可见性释放并恢复长连接订阅条件", () => {
    setVisibility("visible");
    const { result } = renderHook(() => useDocumentVisibility());
    expect(result.current).toBe(true);

    act(() => setVisibility("hidden"));
    expect(result.current).toBe(false);

    act(() => setVisibility("visible"));
    expect(result.current).toBe(true);
  });
});
