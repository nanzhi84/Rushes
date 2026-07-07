import type { ReactElement } from "react";

type StatusTone = "neutral" | "info" | "success" | "danger" | "warning";

type StatusBadgeProps = {
  label: string;
  tone?: StatusTone;
};

export function StatusBadge({ label, tone = "neutral" }: StatusBadgeProps): ReactElement {
  return (
    <span
      className={`inline-flex min-h-6 items-center rounded px-2 text-xs font-medium ${toneClass(tone)}`}
    >
      {label}
    </span>
  );
}

// 素材理解状态 → 徽标文案与色调（Spec C：none/running/ready/failed）。
export function understandingBadgeProps(status: string): { label: string; tone: StatusTone } {
  if (status === "ready") {
    return { label: "已理解", tone: "success" };
  }
  if (status === "running") {
    return { label: "理解中", tone: "info" };
  }
  if (status === "failed") {
    return { label: "理解失败", tone: "danger" };
  }
  return { label: "未理解", tone: "neutral" };
}

function toneClass(tone: StatusTone): string {
  if (tone === "success") {
    return "bg-ok/15 text-ok";
  }
  if (tone === "danger") {
    return "bg-danger/15 text-danger";
  }
  if (tone === "warning") {
    return "bg-warn/15 text-warn";
  }
  if (tone === "info") {
    return "bg-info/15 text-info";
  }
  return "bg-raised text-fg-muted";
}
