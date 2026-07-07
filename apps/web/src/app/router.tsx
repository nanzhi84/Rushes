import { createRootRoute, createRoute, createRouter, Outlet } from "@tanstack/react-router";
import { DraftEditorPage } from "../routes/DraftEditor";
import { DraftsHomePage } from "../routes/DraftsHome";

const rootRoute = createRootRoute({
  component: Outlet
});

const indexRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/",
  component: DraftsHomePage
});

const draftRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/drafts/$draftId",
  component: DraftEditorPage
});

const routeTree = rootRoute.addChildren([indexRoute, draftRoute]);

export const router = createRouter({ routeTree });

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
