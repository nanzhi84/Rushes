import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { storeAuthToken } from "../auth";
import { useUiStore } from "../state/ui_store";
import { AppLayout } from "./AppLayout";

const navigateMock = vi.hoisted(() => vi.fn());

vi.mock("@tanstack/react-router", () => ({
  useNavigate: () => navigateMock,
  useRouterState: ({
    select
  }: {
    select: (state: { location: { pathname: string } }) => unknown;
  }) => select({ location: { pathname: "/" } })
}));

class MockEventSource {
  onopen: ((event: Event) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;

  addEventListener(): void {
    return;
  }

  close(): void {
    return;
  }
}

describe("AppLayout sidebar", () => {
  beforeEach(() => {
    window.localStorage.clear();
    useUiStore.setState({
      sidebarCollapsed: false,
      expandedProjectIds: {},
      selection: { type: "projects" },
      treeDialog: null
    });
    storeAuthToken("test-token");
    vi.stubGlobal("EventSource", MockEventSource);
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse({ projects: [] })));
  });

  afterEach(() => {
    window.localStorage.clear();
    navigateMock.mockClear();
  });

  it("切换 store 时持久化 sidebarCollapsed", () => {
    useUiStore.getState().toggleSidebar();

    expect(useUiStore.getState().sidebarCollapsed).toBe(true);
    expect(window.localStorage.getItem("rushes.ui.sidebarCollapsed")).toBe("true");

    useUiStore.getState().toggleSidebar();

    expect(useUiStore.getState().sidebarCollapsed).toBe(false);
    expect(window.localStorage.getItem("rushes.ui.sidebarCollapsed")).toBe("false");
  });

  it("渲染时可从展开侧栏折叠为窄条", async () => {
    renderLayout();

    fireEvent.click(screen.getByLabelText("折叠项目导航"));

    await waitFor(() => expect(screen.getByLabelText("展开项目导航")).toBeTruthy());
    expect(screen.queryByLabelText("项目文件树")).toBeNull();
  });
});

function renderLayout(): void {
  render(
    <QueryClientProvider client={testQueryClient()}>
      <AppLayout>
        <div>内容</div>
      </AppLayout>
    </QueryClientProvider>
  );
}

function testQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: { retry: false },
      mutations: { retry: false }
    }
  });
}

function jsonResponse(payload: unknown, status = 200): Response {
  return new Response(JSON.stringify(payload), {
    status,
    headers: { "Content-Type": "application/json" }
  });
}
