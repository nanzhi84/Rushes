import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider
} from "@tanstack/react-router";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { storeAuthToken } from "../auth";
import { ProjectDetailPage } from "./ProjectDetailPage";

class NoopEventSource {
  onopen: ((event: Event) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;
  addEventListener(): void {}
  removeEventListener(): void {}
  close(): void {}
}

describe("ProjectDetailPage", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    window.sessionStorage.clear();
  });

  it("默认剪辑任务 tab：新建表单 + Case 卡片", async () => {
    renderDetail("/projects/project_1");

    expect(await screen.findByText("新建剪辑任务")).toBeTruthy();
    expect(await screen.findByText("任务一")).toBeTruthy();
    expect(screen.getByText("创建并进入工作台")).toBeTruthy();
  });

  it("tab=materials 时渲染素材视图", async () => {
    renderDetail("/projects/project_1?tab=materials");

    expect(await screen.findByText("重新检测失效")).toBeTruthy();
  });

  it("设置 tab 提供项目操作入口", async () => {
    renderDetail("/projects/project_1");

    const tabs = await screen.findByLabelText("项目页面切换");
    fireEvent.click(within(tabs).getByText("设置"));

    await waitFor(() => expect(screen.getByText("删除项目")).toBeTruthy());
    expect(screen.getByText("重命名项目")).toBeTruthy();
  });
});

function renderDetail(initialPath: string): void {
  storeAuthToken("test-token");
  vi.stubGlobal("EventSource", NoopEventSource);
  vi.stubGlobal("fetch", mockFetch());

  const rootRoute = createRootRoute();
  const indexRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: "/",
    component: () => null
  });
  const projectRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: "/projects/$projectId",
    component: ProjectDetailPage
  });
  const router = createRouter({
    routeTree: rootRoute.addChildren([indexRoute, projectRoute]),
    history: createMemoryHistory({ initialEntries: [initialPath] })
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

function mockFetch(): (input: RequestInfo | URL) => Promise<Response> {
  return (input: RequestInfo | URL) => {
    const url = String(input);
    if (url.includes("/api/project-tree")) {
      return jsonResponse({
        projects: [
          {
            project_id: "project_1",
            name: "项目A",
            status: "active",
            cases: [
              { case_id: "case_1", name: "任务一", project_id: "project_1", status: "active" }
            ]
          }
        ]
      });
    }
    if (url.includes("/materials")) {
      return jsonResponse({ project_id: "project_1", assets: [], invalidated_asset_ids: [] });
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
