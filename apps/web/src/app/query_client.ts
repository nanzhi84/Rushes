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
  drafts: ["drafts"] as const,
  draft: (draftId: string) => ["draft", draftId] as const,
  materials: (draftId: string) => ["materials", draftId] as const,
  materialSummary: (draftId: string, assetId: string) =>
    ["material-summary", draftId, assetId] as const,
  fsRoots: ["fs-roots"] as const,
  fsList: (path: string) => ["fs-list", path] as const,
  timeline: (draftId: string) => ["timeline", draftId] as const,
  messages: (draftId: string) => ["messages", draftId] as const,
  currentDecision: (draftId: string) => ["current-decision", draftId] as const,
  pendingDecisions: (draftId: string) => ["pending-decisions", draftId] as const,
  costs: (draftId: string) => ["costs", draftId] as const
};
