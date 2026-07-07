import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider
} from "@tanstack/react-router";
import { render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { storeAuthToken } from "../auth";
import { ProjectsOverviewPage } from "./ProjectsOverview";

class NoopEventSource {
  onopen: ((event: Event) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;
  addEventListener(): void {}
  removeEventListener(): void {}
  close(): void {}
}

describe("ProjectsOverviewPage", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    window.sessionStorage.clear();
  });

  it("渲染项目卡片墙：项目名、任务/素材计数与新建入口", async () => {
    renderHome();

    expect(await screen.findByText("项目A")).toBeTruthy();
    expect(await screen.findByText("项目B")).toBeTruthy();
    expect(screen.getByText("＋ 新建项目")).toBeTruthy();
    // 项目A 有 2 个 case、1 个素材（缩略图就绪）
    await waitFor(() =>
      expect(screen.getByText(/2 个剪辑任务 · 1 个素材/)).toBeTruthy()
    );
  });

  it("空项目列表显示空态引导", async () => {
    renderHome({ projects: [] });

    expect(await screen.findByText("还没有项目")).toBeTruthy();
  });

  it("点击更多操作展开卡片菜单", async () => {
    renderHome();

    await screen.findByText("项目A");
    (await screen.findByLabelText("项目 项目A 更多操作")).click();

    await waitFor(() => expect(screen.getByText("重命名")).toBeTruthy());
    expect(screen.getByText("复制")).toBeTruthy();
    expect(screen.getByText("删除")).toBeTruthy();
  });
});

type HomeFixture = {
  projects?: Array<{ project_id: string; name: string }>;
};

function renderHome(fixture: HomeFixture = {}): void {
  storeAuthToken("test-token");
  vi.stubGlobal("EventSource", NoopEventSource);
  vi.stubGlobal("fetch", mockFetch(fixture));

  const rootRoute = createRootRoute();
  const indexRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: "/",
    component: ProjectsOverviewPage
  });
  const projectRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: "/projects/$projectId",
    component: () => null
  });
  const router = createRouter({
    routeTree: rootRoute.addChildren([indexRoute, projectRoute]),
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

function mockFetch(fixture: HomeFixture): (input: RequestInfo | URL) => Promise<Response> {
  const projects = fixture.projects ?? [
    { project_id: "project_1", name: "项目A" },
    { project_id: "project_2", name: "项目B" }
  ];
  return (input: RequestInfo | URL) => {
    const url = String(input);
    if (url.includes("/api/project-tree")) {
      return jsonResponse({
        projects: projects.map((project, index) => ({
          project_id: project.project_id,
          name: project.name,
          status: "active",
          cases:
            index === 0
              ? [
                  { case_id: "case_1", name: "任务一", project_id: project.project_id, status: "active" },
                  { case_id: "case_2", name: "任务二", project_id: project.project_id, status: "active" }
                ]
              : []
        }))
      });
    }
    if (url.includes("/materials")) {
      return jsonResponse({
        project_id: "project_1",
        assets: [
          {
            asset_id: "asset_1",
            storage_mode: "reference",
            kind: "video",
            source: "local",
            filename: "a.mp4",
            hash: "h",
            size: 1,
            mtime: null,
            ingest_status: "ready",
            understanding_status: "none",
            usable: true,
            enabled: true,
            probe: null,
            duration_sec: 12,
            proxy_object_hash: null,
            proxy_ready: false,
            thumbnail_ready: true,
            invalid: false,
            failure: null,
            jobs: []
          }
        ],
        invalidated_asset_ids: []
      });
    }
    if (url.includes("/api/projects")) {
      return jsonResponse({
        projects: projects.map((project) => ({
          project_id: project.project_id,
          name: project.name,
          status: "active",
          created_at: "2026-07-01T00:00:00Z",
          defaults: {}
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
