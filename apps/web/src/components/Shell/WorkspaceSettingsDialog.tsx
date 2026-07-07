import type { ReactElement, ReactNode } from "react";

type WorkspaceSettingsDialogProps = {
  open: boolean;
  onClose: () => void;
};

/**
 * 全局设置弹窗（占位）：设置入口从编辑器移出后，只在草稿墙齿轮打开。
 * 内容维持现状占位水平——展示 workspace 默认值 + 成本汇总占位；
 * 手搓弹窗样式对齐 EntityActionDialog（radix Dialog 统一替换留给 PR-C）。
 */
export function WorkspaceSettingsDialog({
  open,
  onClose
}: WorkspaceSettingsDialogProps): ReactElement | null {
  if (!open) {
    return null;
  }

  return (
    <div
      className="fixed inset-0 z-20 grid place-items-center bg-black/60 px-4"
      role="presentation"
      onClick={onClose}
    >
      <div
        className="w-full max-w-md rounded-lg border border-line bg-raised p-5"
        role="dialog"
        aria-modal="true"
        aria-label="全局设置"
        onClick={(event) => event.stopPropagation()}
      >
        <div className="flex items-center justify-between">
          <h2 className="text-lg font-semibold text-fg">全局设置</h2>
          <button
            className="grid h-7 w-7 place-items-center rounded-md text-fg-muted hover:bg-hover hover:text-fg"
            type="button"
            aria-label="关闭设置"
            onClick={onClose}
          >
            ✕
          </button>
        </div>

        <Section title="全局默认值">
          <dl className="grid grid-cols-[auto_1fr] gap-x-6 gap-y-1.5 text-sm">
            <DefaultRow label="画幅" value="16:9" />
            <DefaultRow label="帧率" value="30 fps" />
            <DefaultRow label="质量" value="标准" />
          </dl>
          <p className="mt-2 text-xs text-fg-faint">
            新建草稿会继承这些默认值；逐草稿改动在对话中告诉代理即可。
          </p>
        </Section>

        <Section title="成本汇总">
          <p className="text-sm text-fg-muted">全局成本汇总后续接入。</p>
        </Section>
      </div>
    </div>
  );
}

function Section({ title, children }: { title: string; children: ReactNode }): ReactElement {
  return (
    <section className="mt-5 rounded-md border border-line bg-ink p-4">
      <h3 className="text-sm font-semibold text-fg">{title}</h3>
      <div className="mt-3">{children}</div>
    </section>
  );
}

function DefaultRow({ label, value }: { label: string; value: string }): ReactElement {
  return (
    <>
      <dt className="text-fg-muted">{label}</dt>
      <dd className="tabular-nums text-fg">{value}</dd>
    </>
  );
}
