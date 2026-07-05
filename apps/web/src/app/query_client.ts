import { QueryClient } from "@tanstack/react-query";
import { ApiError } from "../auth";

export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      retry: (failureCount, error) => {
        if (error instanceof ApiError && error.status === 401) {
          return false;
        }
        return failureCount < 2;
      },
      staleTime: 3_000
    },
    mutations: {
      retry: false
    }
  }
});

export const queryKeys = {
  projectTree: ["project-tree"] as const,
  projects: ["projects"] as const,
  project: (projectId: string) => ["project", projectId] as const,
  materials: (projectId: string) => ["materials", projectId] as const,
  fsRoots: ["fs-roots"] as const,
  fsList: (path: string) => ["fs-list", path] as const,
  case: (projectId: string, caseId: string) => ["case", projectId, caseId] as const,
  messages: (projectId: string, caseId: string) => ["messages", projectId, caseId] as const,
  currentDecision: (projectId: string, caseId: string) =>
    ["current-decision", projectId, caseId] as const
};
