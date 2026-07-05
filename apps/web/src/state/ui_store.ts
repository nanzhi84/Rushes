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

export const useUiStore = create<UiState>((set) => ({
  sidebarCollapsed: false,
  expandedProjectIds: {},
  selection: { type: "projects" },
  treeDialog: null,
  setSelection: (selection) => set({ selection }),
  toggleSidebar: () => set((state) => ({ sidebarCollapsed: !state.sidebarCollapsed })),
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
