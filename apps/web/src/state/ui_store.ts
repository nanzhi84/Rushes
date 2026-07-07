import { create } from "zustand";

export type EntityDialogKind = "renameDraft" | "copyDraft" | "deleteDraft";

export type EntityDialogState = {
  kind: EntityDialogKind;
  draftId: string;
};

type UiState = {
  entityDialog: EntityDialogState | null;
  chatPanelWidth: number;
  materialsPanelWidth: number;
  timelinePanelHeight: number;
  openEntityDialog: (dialog: EntityDialogState) => void;
  closeEntityDialog: () => void;
  setChatPanelWidth: (width: number) => void;
  setMaterialsPanelWidth: (width: number) => void;
  setTimelinePanelHeight: (height: number) => void;
};

const CHAT_WIDTH_KEY = "rushes.ui.chatPanelWidth";
const MATERIALS_WIDTH_KEY = "rushes.ui.materialsPanelWidth";
const TIMELINE_HEIGHT_KEY = "rushes.ui.timelinePanelHeight";

export const DEFAULT_CHAT_PANEL_WIDTH = 400;
export const DEFAULT_MATERIALS_PANEL_WIDTH = 300;
export const DEFAULT_TIMELINE_PANEL_HEIGHT = 260;

export const CHAT_PANEL_WIDTH_RANGE = { min: 300, max: 640 } as const;
export const MATERIALS_PANEL_WIDTH_RANGE = { min: 240, max: 480 } as const;
export const TIMELINE_PANEL_HEIGHT_RANGE = { min: 160, max: 480 } as const;

export const useUiStore = create<UiState>((set) => ({
  entityDialog: null,
  chatPanelWidth: readStoredNumber(CHAT_WIDTH_KEY, DEFAULT_CHAT_PANEL_WIDTH, CHAT_PANEL_WIDTH_RANGE),
  materialsPanelWidth: readStoredNumber(
    MATERIALS_WIDTH_KEY,
    DEFAULT_MATERIALS_PANEL_WIDTH,
    MATERIALS_PANEL_WIDTH_RANGE
  ),
  timelinePanelHeight: readStoredNumber(
    TIMELINE_HEIGHT_KEY,
    DEFAULT_TIMELINE_PANEL_HEIGHT,
    TIMELINE_PANEL_HEIGHT_RANGE
  ),
  openEntityDialog: (dialog) => set({ entityDialog: dialog }),
  closeEntityDialog: () => set({ entityDialog: null }),
  setChatPanelWidth: (width) => {
    const clamped = clamp(width, CHAT_PANEL_WIDTH_RANGE);
    writeStoredNumber(CHAT_WIDTH_KEY, clamped);
    set({ chatPanelWidth: clamped });
  },
  setMaterialsPanelWidth: (width) => {
    const clamped = clamp(width, MATERIALS_PANEL_WIDTH_RANGE);
    writeStoredNumber(MATERIALS_WIDTH_KEY, clamped);
    set({ materialsPanelWidth: clamped });
  },
  setTimelinePanelHeight: (height) => {
    const clamped = clamp(height, TIMELINE_PANEL_HEIGHT_RANGE);
    writeStoredNumber(TIMELINE_HEIGHT_KEY, clamped);
    set({ timelinePanelHeight: clamped });
  }
}));

function clamp(value: number, range: { min: number; max: number }): number {
  return Math.min(range.max, Math.max(range.min, value));
}

function readStoredNumber(
  key: string,
  fallback: number,
  range: { min: number; max: number }
): number {
  if (typeof window === "undefined") {
    return fallback;
  }
  try {
    const raw = window.localStorage.getItem(key);
    const parsed = raw === null ? Number.NaN : Number(raw);
    return Number.isFinite(parsed) ? clamp(parsed, range) : fallback;
  } catch {
    return fallback;
  }
}

function writeStoredNumber(key: string, value: number): void {
  if (typeof window === "undefined") {
    return;
  }
  try {
    window.localStorage.setItem(key, String(value));
  } catch {
    return;
  }
}
