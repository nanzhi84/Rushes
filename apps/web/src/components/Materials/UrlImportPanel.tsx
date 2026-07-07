import { useState } from "react";
import type { FormEvent, ReactElement } from "react";

export type UrlImportDraft = {
  url: string;
  filename?: string | null;
};

export type PendingUrlDecision = {
  decisionId: string;
  url: string;
  filename?: string | null;
};

type UrlImportPanelProps = {
  isPending: boolean;
  onCreate: (draft: UrlImportDraft) => void;
};

type UrlDecisionCardProps = {
  decisions: PendingUrlDecision[];
  isPending: boolean;
  onAnswer: (decision: PendingUrlDecision, approved: boolean) => void;
};

export function UrlImportPanel({ isPending, onCreate }: UrlImportPanelProps): ReactElement {
  const [url, setUrl] = useState("");
  const [filename, setFilename] = useState("");

  function submit(event: FormEvent<HTMLFormElement>): void {
    event.preventDefault();
    const trimmedUrl = url.trim();
    if (!trimmedUrl) {
      return;
    }
    onCreate({
      url: trimmedUrl,
      filename: filename.trim() || null
    });
    setUrl("");
    setFilename("");
  }

  return (
    <section className="rounded-lg border border-line bg-panel p-4">
      <h2 className="font-semibold">URL 导入</h2>
      <p className="mt-1 text-sm text-fg-muted">提交后会创建确认项，确认后后端只下载该 URL 指向文件。</p>
      <form className="mt-4 grid gap-3" onSubmit={submit}>
        <label className="block text-sm font-medium text-fg">
          URL
          <input
            className="mt-2 w-full rounded-md border border-line-strong px-3 py-2 outline-none focus:border-accent"
            placeholder="https://example.com/raw.mp4"
            value={url}
            onChange={(event) => setUrl(event.target.value)}
          />
        </label>
        <div className="grid gap-3 sm:grid-cols-[1fr_auto] sm:items-end">
          <label className="block text-sm font-medium text-fg">
            文件名
            <input
              className="mt-2 w-full rounded-md border border-line-strong px-3 py-2 outline-none focus:border-accent"
              placeholder="可选"
              value={filename}
              onChange={(event) => setFilename(event.target.value)}
            />
          </label>
          <button
            className="rounded-md bg-accent px-4 py-2 text-sm font-medium text-white disabled:opacity-40"
            type="submit"
            disabled={isPending || url.trim().length === 0}
          >
            创建确认项
          </button>
        </div>
      </form>
    </section>
  );
}

export function UrlDecisionCards({
  decisions,
  isPending,
  onAnswer
}: UrlDecisionCardProps): ReactElement | null {
  if (decisions.length === 0) {
    return null;
  }
  return (
    <section className="rounded-lg border border-line bg-panel p-4">
      <h2 className="font-semibold">待确认 URL 导入</h2>
      <div className="mt-3 space-y-3">
        {decisions.map((decision) => (
          <article key={decision.decisionId} className="rounded-md border border-line p-3">
            <p className="text-sm font-medium">确认从 URL 导入素材？</p>
            <p className="mt-1 break-all text-sm text-fg-muted">{decision.url}</p>
            {decision.filename ? (
              <p className="mt-1 text-xs text-fg-muted">文件名：{decision.filename}</p>
            ) : null}
            <div className="mt-3 flex gap-2">
              <button
                className="rounded-md bg-accent px-3 py-2 text-sm font-medium text-white disabled:opacity-40"
                type="button"
                disabled={isPending}
                onClick={() => onAnswer(decision, true)}
              >
                确认导入
              </button>
              <button
                className="rounded-md border border-line-strong px-3 py-2 text-sm hover:bg-hover disabled:text-fg-faint"
                type="button"
                disabled={isPending}
                onClick={() => onAnswer(decision, false)}
              >
                取消导入
              </button>
            </div>
          </article>
        ))}
      </div>
    </section>
  );
}
