import { createRootRoute, createRoute, createRouter, Outlet } from "@tanstack/react-router";
import { AppLayout } from "../routes/AppLayout";
import { CaseAgentConsolePage } from "../routes/CaseAgentConsole";
import { ProjectHomePage } from "../routes/ProjectHomePage";
import { ProjectMaterialsPage } from "../routes/ProjectMaterialsPage";
import { ProjectsOverviewPage } from "../routes/ProjectsOverview";

const rootRoute = createRootRoute({
  component: () => (
    <AppLayout>
      <Outlet />
    </AppLayout>
  )
});

const indexRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/",
  component: ProjectsOverviewPage
});

const projectRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/projects/$projectId",
  component: ProjectHomePage
});

const materialsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/projects/$projectId/materials",
  component: ProjectMaterialsPage
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
