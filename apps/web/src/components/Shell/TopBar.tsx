import type { ReactElement, ReactNode } from "react";
import { connectionLabel, type ConnectionState } from "../../app/use_workspace_events";

type TopBarProps = {
  connectionState: ConnectionState;
  /** 左侧内容；缺省渲染 Rushes 字标。 */
  leading?: ReactNode;
  /** 连接状态左侧的附加动作（如导出按钮）。 */
  trailing?: ReactNode;
  /** 是否显示设置按钮；编辑器隐藏（设置只在草稿墙）。缺省保持显示。 */
  showSettings?: boolean;
  /** 设置按钮点击回调（如打开全局设置弹窗）。 */
  onSettingsClick?: () => void;
};

export function TopBar({
  connectionState,
  leading,
  trailing,
  showSettings = true,
  onSettingsClick
}: TopBarProps): ReactElement {
  return (
    <header className="flex h-10 shrink-0 items-center justify-between border-b border-line bg-panel px-3">
      <div className="flex min-w-0 items-center gap-2">{leading ?? <Wordmark />}</div>
      <div className="flex shrink-0 items-center gap-2 text-xs text-fg-muted">
        {trailing}
        <span className="inline-flex items-center gap-1.5" data-testid="connection-state">
          <span className={`size-1.5 rounded-full ${connectionColor(connectionState)}`} />
          {connectionLabel(connectionState)}
        </span>
        {showSettings ? (
          <button
            className="rounded-sm px-2 py-1 hover:bg-hover hover:text-fg"
            type="button"
            onClick={onSettingsClick}
          >
            设置
          </button>
        ) : null}
      </div>
    </header>
  );
}

export function Wordmark(): ReactElement {
  return (
    <span className="flex items-center gap-2 text-[13px] font-semibold tracking-wide text-fg">
      <span aria-hidden className="h-3.5 w-1 rounded-sm bg-accent" />
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
