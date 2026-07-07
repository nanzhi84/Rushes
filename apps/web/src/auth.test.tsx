import { render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { api } from "./api/client";
import { AppRoot } from "./main";
import { AUTH_REQUIRED_EVENT, getAuthToken, storeAuthToken } from "./auth";

describe("auth", () => {
  it("无 token 时展示启动引导页", () => {
    render(<AppRoot />);

    expect(screen.getByText("请从后端启动 URL 打开 Rushes")).toBeTruthy();
    expect(screen.getByText(/当前页面没有收到启动 token/)).toBeTruthy();
  });

  it("client 对 401 响应执行统一跳转到引导页逻辑", async () => {
    storeAuthToken("bad-token");
    window.history.pushState(null, document.title, "/drafts/draft_1");
    const authRequired = vi.fn();
    window.addEventListener(AUTH_REQUIRED_EVENT, authRequired);
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response(JSON.stringify({ reason: "bad_token" }), { status: 401 }))
    );

    await expect(api.listDrafts()).rejects.toMatchObject({ status: 401 });

    await waitFor(() => expect(authRequired).toHaveBeenCalled());
    expect(getAuthToken()).toBeNull();
    expect(window.location.pathname).toBe("/");
    window.removeEventListener(AUTH_REQUIRED_EVENT, authRequired);
  });
});
