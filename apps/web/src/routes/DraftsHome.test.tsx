import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
  useParams
} from "@tanstack/react-router";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { ReactElement } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { storeAuthToken } from "../auth";
import { DraftsHomePage } from "./DraftsHome";

class NoopEventSource {
  onopen: ((event: Event) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;
  addEventListener(): void {}
  removeEventListener(): void {}
  close(): void {}
}

describe("DraftsHomePage", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    window.sessionStorage.clear();
  });

  it("渲染草稿卡片墙：草稿名、素材计数与开始创作入口", async () => {
    renderHome();

    expect(await screen.findByText("7月7日")).toBeTruthy();
    expect(await screen.findByText("旅行 Vlog")).toBeTruthy();
    expect(screen.getByText("开始创作")).toBeTruthy();
    await waitFor(() => expect(screen.getByText(/3 个素材/)).toBeTruthy());
  });

  it("空草稿列表显示空态引导", async () => {
    renderHome({ drafts: [] });

    expect(await screen.findByText("还没有草稿")).toBeTruthy();
  });

  it("点击更多操作展开卡片下拉菜单", async () => {
    renderHome();

    await screen.findByText("7月7日");
    // radix DropdownMenu 由键盘/指针打开（并非裸 click），用 Enter 键触发 Trigger。
    const trigger = await screen.findByLabelText("草稿 7月7日 更多操作");
    fireEvent.keyDown(trigger, { key: "Enter" });

    await waitFor(() => expect(screen.getByText("重命名")).toBeTruthy());
    expect(screen.getByText("复制")).toBeTruthy();
    expect(screen.getByText("删除")).toBeTruthy();
  });

  it("右键卡片打开上下文菜单", async () => {
    renderHome();

    const title = await screen.findByText("7月7日");
    fireEvent.contextMenu(title);

    await waitFor(() => expect(screen.getByText("重命名")).toBeTruthy());
    expect(screen.getByText("复制")).toBeTruthy();
    expect(screen.getByText("删除")).toBeTruthy();
  });

  it("点击开始创作：新建草稿并跳转编辑器", async () => {
    renderHome();

    await screen.findByText("7月7日");
    screen.getByText("开始创作").click();

    expect(await screen.findByText("编辑器:draft_new")).toBeTruthy();
  });

  it("点击设置打开全局设置弹窗，关闭后消失", async () => {
    renderHome();

    await screen.findByText("7月7日");
    expect(screen.queryByRole("dialog", { name: "全局设置" })).toBeNull();

    screen.getByRole("button", { name: "设置" }).click();
    expect(await screen.findByRole("dialog", { name: "全局设置" })).toBeTruthy();

    screen.getByRole("button", { name: "关闭设置" }).click();
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "全局设置" })).toBeNull());
  });
});

type DraftFixture = {
  draft_id: string;
  name: string;
  material_count: number;
  cover_asset_ids?: string[];
};

type HomeFixture = {
  drafts?: DraftFixture[];
};

function DraftMarker(): ReactElement {
  const params = useParams({ strict: false }) as { draftId?: string };
  return <div>编辑器:{params.draftId}</div>;
}

function renderHome(fixture: HomeFixture = {}): void {
  storeAuthToken("test-token");
  vi.stubGlobal("EventSource", NoopEventSource);
  vi.stubGlobal("fetch", mockFetch(fixture));

  const rootRoute = createRootRoute();
  const indexRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: "/",
    component: DraftsHomePage
  });
  const draftRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: "/drafts/$draftId",
    component: DraftMarker
  });
  const router = createRouter({
    routeTree: rootRoute.addChildren([indexRoute, draftRoute]),
    history: createMemoryHistory({ initialEntries: ["/"] })
  });

  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } }
  });
  render(
    <QueryClientProvider client={queryClient}>
      {/* eslint-disable-next-line @typescript-eslint/no-explicit-any */}
      <RouterProvider router={router as any} />
    </QueryClientProvider>
  );
}

function mockFetch(
  fixture: HomeFixture
): (input: RequestInfo | URL, init?: RequestInit) => Promise<Response> {
  const drafts = fixture.drafts ?? [
    { draft_id: "draft_1", name: "7月7日", material_count: 3, cover_asset_ids: ["asset_1", "asset_2"] },
    { draft_id: "draft_2", name: "旅行 Vlog", material_count: 0, cover_asset_ids: [] }
  ];
  return (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    const method = (init?.method ?? "GET").toUpperCase();
    if (url.includes("/api/drafts") && method === "POST") {
      return jsonResponse({ draft: { draft_id: "draft_new" }, event_ids: [] });
    }
    if (url.includes("/api/drafts")) {
      return jsonResponse({
        drafts: drafts.map((draft) => ({
          draft_id: draft.draft_id,
          name: draft.name,
          status: "active",
          updated_at: "2026-07-07T00:00:00Z",
          material_count: draft.material_count,
          cover_asset_ids: draft.cover_asset_ids ?? []
        }))
      });
    }
    return jsonResponse({});
  };
}

function jsonResponse(payload: unknown): Promise<Response> {
  return Promise.resolve(
    new Response(JSON.stringify(payload), {
      status: 200,
      headers: { "Content-Type": "application/json" }
    })
  );
}
