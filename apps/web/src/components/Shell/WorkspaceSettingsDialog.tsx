import * as Dialog from "@radix-ui/react-dialog";
import { X } from "lucide-react";
import type { ReactElement, ReactNode } from "react";

type WorkspaceSettingsDialogProps = {
  open: boolean;
  onClose: () => void;
};

/**
 * 全局设置弹窗（占位）：设置入口从编辑器移出后，只在草稿墙齿轮打开。
 * 内容维持现状占位水平——展示 workspace 默认值 + 成本汇总占位。
 */
export function WorkspaceSettingsDialog({
  open,
  onClose
}: WorkspaceSettingsDialogProps): ReactElement {
  return (
    <Dialog.Root
      open={open}
      onOpenChange={(next) => {
        if (!next) {
          onClose();
        }
      }}
    >
      <Dialog.Portal>
        <Dialog.Overlay className="rx-overlay fixed inset-0 z-30 bg-black/60 backdrop-blur-sm" />
        <Dialog.Content
          aria-describedby={undefined}
          className="rx-content fixed left-1/2 top-1/2 z-40 w-[calc(100%-2rem)] max-w-md -translate-x-1/2 -translate-y-1/2 rounded-xl bg-raised p-5 shadow-overlay focus:outline-none"
        >
          <div className="flex items-center justify-between">
            <Dialog.Title className="text-lg font-semibold text-fg">全局设置</Dialog.Title>
            <Dialog.Close asChild>
              <button
                className="grid h-7 w-7 place-items-center rounded-md text-fg-muted transition-colors ease-standard hover:bg-hover hover:text-fg"
                type="button"
                aria-label="关闭设置"
              >
                <X size={16} strokeWidth={1.75} aria-hidden />
              </button>
            </Dialog.Close>
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
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}

function Section({ title, children }: { title: string; children: ReactNode }): ReactElement {
  return (
    <section className="mt-5 rounded-lg border border-line bg-ink p-4">
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
