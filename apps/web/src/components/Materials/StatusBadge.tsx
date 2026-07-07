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
