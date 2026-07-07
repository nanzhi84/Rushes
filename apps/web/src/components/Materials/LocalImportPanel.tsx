import { useState } from "react";
import type { ReactElement } from "react";
import { FsBrowserDialog } from "./FsBrowserDialog";

type LocalImportPanelProps = {
  isPending: boolean;
  onImport: (path: string) => void;
};

export function LocalImportPanel({ isPending, onImport }: LocalImportPanelProps): ReactElement {
  const [open, setOpen] = useState(false);

  return (
    <section className="rounded-lg border border-line bg-panel p-4">
      <h2 className="font-semibold">本地路径导入</h2>
      <p className="mt-1 text-sm text-fg-muted">
        通过服务器端目录浏览选择媒体文件，默认 reference，类型由文件后缀自动识别。
      </p>
      <div className="mt-4">
        <button
          className="rounded-md bg-accent px-4 py-2 text-sm font-medium text-white disabled:opacity-40"
          type="button"
          disabled={isPending}
          onClick={() => setOpen(true)}
        >
          本地路径导入
        </button>
      </div>
      <FsBrowserDialog
        open={open}
        title="选择本地素材"
        submitLabel="导入此文件"
        onClose={() => setOpen(false)}
        onSelect={(path) => {
          onImport(path);
          setOpen(false);
        }}
      />
    </section>
  );
}
