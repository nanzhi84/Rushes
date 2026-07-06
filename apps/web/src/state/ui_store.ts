import { create } from "zustand";

export type TreeSelection =
  | { type: "projects" }
  | { type: "project"; projectId: string }
  | { type: "case"; projectId: string; caseId: string };

export type TreeDialogKind =
  | "createProject"
  | "renameProject"
  | "copyProject"
  | "deleteProject"
  | "createCase"
  | "renameCase"
  | "copyCase"
  | "deleteCase"
  | "moveCase";

export type TreeDialogState = {
  kind: TreeDialogKind;
  projectId?: string;
  caseId?: string;
};

type UiState = {
  sidebarCollapsed: boolean;
  expandedProjectIds: Record<string, boolean>;
  selection: TreeSelection;
  treeDialog: TreeDialogState | null;
  setSelection: (selection: TreeSelection) => void;
  toggleSidebar: () => void;
  toggleProjectExpanded: (projectId: string) => void;
  openTreeDialog: (dialog: TreeDialogState) => void;
  closeTreeDialog: () => void;
};

const SIDEBAR_COLLAPSED_KEY = "rushes.ui.sidebarCollapsed";

export const useUiStore = create<UiState>((set) => ({
  sidebarCollapsed: readSidebarCollapsed(),
  expandedProjectIds: {},
  selection: { type: "projects" },
  treeDialog: null,
  setSelection: (selection) => set({ selection }),
  toggleSidebar: () =>
    set((state) => {
      const sidebarCollapsed = !state.sidebarCollapsed;
      writeSidebarCollapsed(sidebarCollapsed);
      return { sidebarCollapsed };
    }),
  toggleProjectExpanded: (projectId) =>
    set((state) => ({
      expandedProjectIds: {
        ...state.expandedProjectIds,
        [projectId]: !(state.expandedProjectIds[projectId] ?? true)
      }
    })),
  openTreeDialog: (dialog) => set({ treeDialog: dialog }),
  closeTreeDialog: () => set({ treeDialog: null })
}));

function readSidebarCollapsed(): boolean {
  if (typeof window === "undefined") {
    return false;
  }
  try {
    return window.localStorage.getItem(SIDEBAR_COLLAPSED_KEY) === "true";
  } catch {
    return false;
  }
}

function writeSidebarCollapsed(value: boolean): void {
  if (typeof window === "undefined") {
    return;
  }
  try {
    window.localStorage.setItem(SIDEBAR_COLLAPSED_KEY, String(value));
  } catch {
    return;
  }
}
