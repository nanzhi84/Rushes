import type { ReactElement, ReactNode } from "react";
import { connectionLabel, type ConnectionState } from "../../app/use_workspace_events";

type TopBarProps = {
  connectionState: ConnectionState;
  /** 左侧内容；缺省渲染 Rushes 字标。 */
  leading?: ReactNode;
  /** 连接状态左侧的附加动作（如导出按钮）。 */
  trailing?: ReactNode;
};

export function TopBar({ connectionState, leading, trailing }: TopBarProps): ReactElement {
  return (
    <header className="flex h-12 shrink-0 items-center justify-between border-b border-line bg-panel px-4">
      <div className="flex min-w-0 items-center gap-3">{leading ?? <Wordmark />}</div>
      <div className="flex shrink-0 items-center gap-3 text-sm text-fg-muted">
        {trailing}
        <span className="inline-flex items-center gap-2" data-testid="connection-state">
          <span className={`h-2 w-2 rounded-full ${connectionColor(connectionState)}`} />
          {connectionLabel(connectionState)}
        </span>
        <button className="rounded-md px-2 py-1 hover:bg-hover hover:text-fg" type="button">
          设置
        </button>
      </div>
    </header>
  );
}

export function Wordmark(): ReactElement {
  return (
    <span className="flex items-center gap-2 text-sm font-semibold tracking-wide text-fg">
      <span aria-hidden className="h-4 w-1.5 rounded-full bg-accent" />
      Rushes
    </span>
  );
}

function connectionColor(state: ConnectionState): string {
  if (state === "open") {
    return "bg-ok";
  }
  if (state === "closed") {
    return "bg-danger";
  }
  return "bg-warn";
}
