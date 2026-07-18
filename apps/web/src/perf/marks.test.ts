import { describe, expect, it, vi } from "vitest";
import { markEnd, markStart, perfSpan } from "./marks";

describe("perf marks", () => {
  it("markStart/markEnd 在开发期写入 User Timing 标记与 measure", () => {
    const markSpy = vi.spyOn(performance, "mark");
    const measureSpy = vi.spyOn(performance, "measure");

    markStart("test:action");
    markEnd("test:action");

    expect(markSpy).toHaveBeenCalledWith("test:action:start");
    expect(markSpy).toHaveBeenCalledWith("test:action:end");
    expect(measureSpy).toHaveBeenCalledWith("test:action", "test:action:start", "test:action:end");
  });

  it("perfSpan 返回结束函数，一对区间成对落标记", () => {
    const markSpy = vi.spyOn(performance, "mark");
    const end = perfSpan("test:span");
    expect(typeof end).toBe("function");
    end();
    expect(markSpy).toHaveBeenCalledWith("test:span:start");
    expect(markSpy).toHaveBeenCalledWith("test:span:end");
  });

  it("缺少起始标记或缓冲不可用时 markEnd 静默不抛", () => {
    vi.spyOn(performance, "measure").mockImplementation(() => {
      throw new Error("no start mark");
    });
    expect(() => markEnd("test:missing")).not.toThrow();
  });
});
