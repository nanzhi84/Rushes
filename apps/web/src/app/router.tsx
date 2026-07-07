import { createRootRoute, createRoute, createRouter, Outlet, redirect } from "@tanstack/react-router";
import { CaseAgentConsolePage } from "../routes/CaseAgentConsole";
import { ProjectDetailPage } from "../routes/ProjectDetailPage";
import { ProjectsOverviewPage } from "../routes/ProjectsOverview";

const rootRoute = createRootRoute({
  component: Outlet
});

const indexRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/",
  component: ProjectsOverviewPage
});

const projectRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/projects/$projectId",
  component: ProjectDetailPage
});

// 旧素材页路由保留为重定向，指向项目详情的素材 tab。
const materialsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/projects/$projectId/materials",
  beforeLoad: ({ params }) => {
    throw redirect({
      to: "/projects/$projectId",
      params: { projectId: params.projectId },
      search: { tab: "materials" }
    });
  }
});

const caseRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/projects/$projectId/cases/$caseId",
  component: CaseAgentConsolePage
});

const routeTree = rootRoute.addChildren([indexRoute, projectRoute, materialsRoute, caseRoute]);

export const router = createRouter({ routeTree });

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
