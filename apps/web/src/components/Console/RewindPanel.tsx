import { History, RotateCcw, X } from "lucide-react";
import type { ReactElement } from "react";
import type { RewindCheckpoint, RewindRestoreRequest } from "../../api/client";

export function RewindPanel({
  checkpoints,
  selectedCheckpointId,
  loading,
  pending,
  onSelect,
  onRestore,
  onClose
}: {
  checkpoints: RewindCheckpoint[];
  selectedCheckpointId: string | null;
  loading: boolean;
  pending: boolean;
  onSelect: (checkpointId: string) => void;
  onRestore: (mode: RewindRestoreRequest["mode"]) => void;
  onClose: () => void;
}): ReactElement {
  const selected =
    checkpoints.find((checkpoint) => checkpoint.checkpoint_id === selectedCheckpointId) ?? null;

  return (
    <section
      className="shrink-0 border-b border-line bg-raised/70"
      aria-label="回退检查点"
    >
      <header className="flex h-8 items-center gap-2 border-b border-line px-3">
        <History size={13} strokeWidth={1.8} aria-hidden className="text-fg-muted" />
        <span className="min-w-0 flex-1 text-xs font-semibold">检查点</span>
        <span className="text-2xs tabular-nums text-fg-faint">最近 {checkpoints.length}/50</span>
        <button
          type="button"
          className="grid size-5 place-items-center rounded-sm text-fg-faint hover:bg-hover hover:text-fg"
          aria-label="关闭回退面板"
          onClick={onClose}
        >
          <X size={12} strokeWidth={1.8} aria-hidden />
        </button>
      </header>

      <div className="max-h-48 overflow-y-auto p-2">
        {loading ? (
          <p className="px-1 py-3 text-center text-2xs text-fg-faint">正在读取检查点…</p>
        ) : checkpoints.length === 0 ? (
          <p className="px-1 py-3 text-center text-2xs text-fg-faint">
            发送消息或修改时间线后会自动创建检查点。
          </p>
        ) : (
          <ul className="space-y-1">
            {checkpoints.map((checkpoint) => {
              const selectedRow = checkpoint.checkpoint_id === selectedCheckpointId;
              return (
                <li key={checkpoint.checkpoint_id}>
                  <button
                    type="button"
                    className={`w-full rounded-sm border px-2 py-1.5 text-left transition-colors ${
                      selectedRow
                        ? "border-accent/70 bg-accent/8"
                        : "border-transparent hover:border-line hover:bg-hover"
                    }`}
                    aria-label={`选择检查点 ${checkpoint.summary || checkpoint.checkpoint_id}`}
                    aria-pressed={selectedRow}
                    onClick={() => onSelect(checkpoint.checkpoint_id)}
                  >
                    <span className="flex items-center gap-2">
                      <span className="min-w-0 flex-1 truncate text-xs font-medium text-fg">
                        {checkpoint.summary || checkpointLabel(checkpoint.trigger_kind)}
                      </span>
                      <time className="shrink-0 text-2xs tabular-nums text-fg-faint">
                        {formatCheckpointTime(checkpoint.created_at)}
                      </time>
                    </span>
                    <span className="mt-0.5 block text-2xs text-fg-faint">
                      {checkpoint.timeline_version === null ? "无时间线" : `v${checkpoint.timeline_version}`}
                      {` · ${checkpoint.clip_count} 片段 · ${checkpoint.duration_frames} 帧 · ${checkpoint.track_count} 轨`}
                      {checkpointDiff(checkpoint)}
                    </span>
                  </button>
                </li>
              );
            })}
          </ul>
        )}
      </div>

      {selected ? (
        <footer className="border-t border-line px-2 py-2">
          <p className="mb-1.5 truncate px-1 text-2xs text-fg-muted" title={selected.summary}>
            将恢复到：{selected.summary || selected.checkpoint_id}
          </p>
          <div className="grid grid-cols-3 gap-1">
            <RestoreButton
              label="仅时间线"
              disabled={pending || selected.timeline_version === null}
              onClick={() => onRestore("timeline")}
            />
            <RestoreButton
              label="仅对话"
              disabled={pending}
              onClick={() => onRestore("conversation")}
            />
            <RestoreButton
              label="时间线和对话"
              disabled={pending || selected.timeline_version === null}
              onClick={() => onRestore("both")}
            />
          </div>
        </footer>
      ) : null}
    </section>
  );
}

function RestoreButton({
  label,
  disabled,
  onClick
}: {
  label: string;
  disabled: boolean;
  onClick: () => void;
}): ReactElement {
  return (
    <button
      type="button"
      className="inline-flex min-h-7 items-center justify-center gap-1 rounded-sm border border-line-strong bg-panel px-1.5 text-2xs font-medium text-fg-muted hover:bg-hover hover:text-fg disabled:cursor-not-allowed disabled:opacity-35"
      disabled={disabled}
      onClick={onClick}
    >
      <RotateCcw size={11} strokeWidth={1.8} aria-hidden />
      {label}
    </button>
  );
}

function checkpointLabel(trigger: RewindCheckpoint["trigger_kind"]): string {
  if (trigger === "user_message") {
    return "用户消息";
  }
  if (trigger === "timeline_write") {
    return "时间线编辑";
  }
  return "恢复操作";
}

function checkpointDiff(checkpoint: RewindCheckpoint): string {
  const changes = [
    signedChange(checkpoint.clip_count_delta, "片段"),
    signedChange(checkpoint.duration_frames_delta, "帧"),
    signedChange(checkpoint.track_count_delta, "轨")
  ].filter(Boolean);
  return changes.length > 0 ? ` · 较前一检查点 ${changes.join(" / ")}` : "";
}

function signedChange(value: number, unit: string): string {
  if (value === 0) {
    return "";
  }
  return `${value > 0 ? "+" : ""}${value} ${unit}`;
}

function formatCheckpointTime(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) {
    return value;
  }
  return date.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}
