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

export function annotationLabel(status: string): string {
  const labels: Record<string, string> = {
    pending: "待标注",
    analyzing: "标注中",
    completed: "已完成",
    failed: "失败"
  };
  return labels[status] ?? status;
}

export function annotationTone(status: string): StatusTone {
  if (status === "completed") {
    return "success";
  }
  if (status === "failed") {
    return "danger";
  }
  if (status === "analyzing") {
    return "info";
  }
  return "warning";
}

export function indexLabel(status: string): string {
  const labels: Record<string, string> = {
    none: "未索引",
    partial: "部分索引",
    ready: "已索引"
  };
  return labels[status] ?? status;
}

export function indexTone(status: string): StatusTone {
  if (status === "ready") {
    return "success";
  }
  if (status === "partial") {
    return "info";
  }
  return "neutral";
}

function toneClass(tone: StatusTone): string {
  if (tone === "success") {
    return "bg-[#dcfce7] text-[#166534]";
  }
  if (tone === "danger") {
    return "bg-[#fee4e2] text-[#b42318]";
  }
  if (tone === "warning") {
    return "bg-[#fef3c7] text-[#92400e]";
  }
  if (tone === "info") {
    return "bg-[#dbeafe] text-[#1d4ed8]";
  }
  return "bg-[#eef2f7] text-[#475569]";
}
