import { render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { api } from "./api/client";
import { AppRoot } from "./main";
import {
  AUTH_REQUIRED_EVENT,
  AUTH_TOKEN_STORAGE_KEY,
  bootstrapAuthFromLaunchUrl,
  getAuthToken,
  listenForLaunchUrlAuth,
  storeAuthToken
} from "./auth";

describe("auth", () => {
  it("无 token 时展示启动引导页", () => {
    render(<AppRoot />);

    expect(screen.getByText("请从后端启动 URL 打开 Rushes")).toBeTruthy();
    expect(screen.getByText(/当前页面没有收到启动 token/)).toBeTruthy();
  });

  it("启动 URL 中的 token 持久化后移除 hash", () => {
    window.history.replaceState(null, document.title, "/#t=persisted-token");

    bootstrapAuthFromLaunchUrl();

    expect(window.localStorage.getItem(AUTH_TOKEN_STORAGE_KEY)).toBe("persisted-token");
    expect(window.sessionStorage.getItem(AUTH_TOKEN_STORAGE_KEY)).toBeNull();
    expect(window.location.hash).toBe("");
    expect(getAuthToken()).toBe("persisted-token");
  });

  it("自动迁移已有的 sessionStorage token", () => {
    window.sessionStorage.setItem(AUTH_TOKEN_STORAGE_KEY, "legacy-token");

    expect(getAuthToken()).toBe("legacy-token");
    expect(window.localStorage.getItem(AUTH_TOKEN_STORAGE_KEY)).toBe("legacy-token");
    expect(window.sessionStorage.getItem(AUTH_TOKEN_STORAGE_KEY)).toBeNull();
  });

  it("已打开页面追加启动 token 时立即完成持久授权", () => {
    const stopListening = listenForLaunchUrlAuth();
    window.history.replaceState(null, document.title, "/#t=same-tab-token");

    window.dispatchEvent(new Event("hashchange"));

    expect(window.localStorage.getItem(AUTH_TOKEN_STORAGE_KEY)).toBe("same-tab-token");
    expect(window.location.hash).toBe("");
    stopListening();
  });

  it("持久化 token 不受会话存储清理影响", () => {
    storeAuthToken("stable-token");
    window.sessionStorage.clear();

    expect(getAuthToken()).toBe("stable-token");
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
    expect(window.localStorage.getItem(AUTH_TOKEN_STORAGE_KEY)).toBeNull();
    expect(window.sessionStorage.getItem(AUTH_TOKEN_STORAGE_KEY)).toBeNull();
    expect(window.location.pathname).toBe("/");
    window.removeEventListener(AUTH_REQUIRED_EVENT, authRequired);
  });
});
