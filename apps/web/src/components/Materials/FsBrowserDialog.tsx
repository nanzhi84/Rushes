import { useQuery } from "@tanstack/react-query";
import { useEffect, useMemo, useState } from "react";
import type { ReactElement } from "react";
import { api, type FsListEntry } from "../../api/client";
import { queryKeys } from "../../app/query_client";

type FsBrowserDialogProps = {
  open: boolean;
  title: string;
  submitLabel: string;
  onClose: () => void;
  /** 单选文件（重新定位失效素材等旧场景）。 */
  onSelect?: (path: string) => void;
  /** 多选文件/文件夹（本地导入）；传入即启用多选模式。 */
  onSelectMany?: (paths: string[]) => void;
};

/** 服务端目录浏览对话框：单选文件，或多选文件与整个文件夹。 */
export function FsBrowserDialog({
  open,
  title,
  submitLabel,
  onClose,
  onSelect,
  onSelectMany
}: FsBrowserDialogProps): ReactElement | null {
  const multi = onSelectMany !== undefined;
  const [currentPath, setCurrentPath] = useState<string | null>(null);
  const [selectedFile, setSelectedFile] = useState<FsListEntry | null>(null);
  const [selectedPaths, setSelectedPaths] = useState<Map<string, string>>(new Map());

  useEffect(() => {
    if (!open) {
      setCurrentPath(null);
      setSelectedFile(null);
      setSelectedPaths(new Map());
    }
  }, [open]);

  const rootsQuery = useQuery({
    queryKey: queryKeys.fsRoots,
    queryFn: api.fsRoots,
    enabled: open
  });

  const listQuery = useQuery({
    queryKey: queryKeys.fsList(currentPath ?? ""),
    queryFn: () => api.fsList(requiredPath(currentPath)),
    enabled: open && currentPath !== null
  });

  const directories = useMemo(
    () => (listQuery.data?.entries ?? []).filter((entry) => entry.type === "directory"),
    [listQuery.data?.entries]
  );
  const files = useMemo(
    () => (listQuery.data?.entries ?? []).filter((entry) => entry.type === "file"),
    [listQuery.data?.entries]
  );

  if (!open) {
    return null;
  }

  const toggleEntry = (entry: FsListEntry): void => {
    setSelectedPaths((current) => {
      const next = new Map(current);
      if (next.has(entry.path)) {
        next.delete(entry.path);
      } else {
        next.set(entry.path, entry.name);
      }
      return next;
    });
  };

  const submitReady = multi ? selectedPaths.size > 0 : selectedFile !== null;
  const submit = (): void => {
    if (multi) {
      onSelectMany?.([...selectedPaths.keys()]);
      return;
    }
    if (selectedFile) {
      onSelect?.(selectedFile.path);
    }
  };

  return (
    <div className="fixed inset-0 z-40 grid place-items-center bg-black/60 px-4" role="dialog" aria-modal="true">
      <section className="flex max-h-[82vh] w-full max-w-2xl flex-col rounded-lg border border-line bg-panel">
        <header className="border-b border-line px-5 py-4">
          <h2 className="text-lg font-semibold">{title}</h2>
          <p className="mt-1 truncate text-sm text-fg-muted">
            {currentPath ?? "选择一个服务器允许访问的根目录"}
          </p>
        </header>

        <div className="min-h-0 flex-1 overflow-y-auto p-4">
          {currentPath === null ? (
            rootsQuery.isLoading ? (
              <p className="text-sm text-fg-muted">正在读取根目录</p>
            ) : rootsQuery.error ? (
              <p className="rounded-md bg-danger/15 px-3 py-2 text-sm text-danger">
                根目录读取失败
              </p>
            ) : (
              <div className="space-y-2">
                {(rootsQuery.data?.roots ?? []).map((root) => (
                  <button
                    key={root.path}
                    className="flex w-full items-center justify-between rounded-md border border-line px-3 py-2 text-left text-sm hover:bg-hover disabled:text-fg-faint"
                    type="button"
                    disabled={!root.exists}
                    onClick={() => setCurrentPath(root.path)}
                  >
                    <span>
                      <span className="font-medium">{root.name}</span>
                      <span className="ml-2 text-xs text-fg-muted">{root.path}</span>
                    </span>
                    <span className="text-xs text-fg-muted">{root.exists ? "打开" : "不存在"}</span>
                  </button>
                ))}
              </div>
            )
          ) : listQuery.isLoading ? (
            <p className="text-sm text-fg-muted">正在读取目录</p>
          ) : listQuery.error ? (
            <p className="rounded-md bg-danger/15 px-3 py-2 text-sm text-danger">目录读取失败</p>
          ) : (
            <div className="space-y-4">
              <div className="flex flex-wrap gap-2">
                <button
                  className="rounded-md border border-line-strong px-3 py-1.5 text-sm hover:bg-hover"
                  type="button"
                  onClick={() => {
                    setCurrentPath(null);
                    setSelectedFile(null);
                  }}
                >
                  根目录
                </button>
                {parentPath(currentPath) ? (
                  <button
                    className="rounded-md border border-line-strong px-3 py-1.5 text-sm hover:bg-hover"
                    type="button"
                    onClick={() => {
                      setCurrentPath(parentPath(currentPath));
                      setSelectedFile(null);
                    }}
                  >
                    上一级
                  </button>
                ) : null}
              </div>

              {directories.length > 0 ? (
                <div>
                  <h3 className="mb-2 text-xs font-semibold text-fg-muted">
                    目录{multi ? "（勾选整个文件夹连同子文件夹一起导入）" : ""}
                  </h3>
                  <div className="space-y-1">
                    {directories.map((entry) => (
                      <div key={entry.path} className="flex items-center gap-2">
                        {multi ? (
                          <input
                            aria-label={`选择文件夹 ${entry.name}`}
                            checked={selectedPaths.has(entry.path)}
                            className="accent-[color:var(--color-accent)]"
                            type="checkbox"
                            onChange={() => toggleEntry(entry)}
                          />
                        ) : null}
                        <button
                          className="block min-w-0 flex-1 rounded-md px-3 py-2 text-left text-sm hover:bg-hover"
                          type="button"
                          onClick={() => {
                            setCurrentPath(entry.path);
                            setSelectedFile(null);
                          }}
                        >
                          {entry.name}
                        </button>
                      </div>
                    ))}
                  </div>
                </div>
              ) : null}

              <div>
                <h3 className="mb-2 text-xs font-semibold text-fg-muted">媒体文件</h3>
                {files.length === 0 ? (
                  <p className="rounded-md border border-dashed border-line-strong px-3 py-4 text-sm text-fg-muted">
                    当前目录没有可导入的媒体文件。
                  </p>
                ) : (
                  <div className="space-y-1">
                    {files.map((entry) => {
                      const active = multi
                        ? selectedPaths.has(entry.path)
                        : selectedFile?.path === entry.path;
                      return (
                        <button
                          key={entry.path}
                          className={`flex w-full items-center justify-between rounded-md px-3 py-2 text-left text-sm ${
                            active ? "bg-accent/20 text-fg" : "hover:bg-hover"
                          }`}
                          type="button"
                          onClick={() => (multi ? toggleEntry(entry) : setSelectedFile(entry))}
                        >
                          <span className="flex min-w-0 items-center gap-2">
                            {multi ? (
                              <input
                                aria-hidden
                                checked={active}
                                className="pointer-events-none accent-[color:var(--color-accent)]"
                                readOnly
                                tabIndex={-1}
                                type="checkbox"
                              />
                            ) : null}
                            <span className="truncate">{entry.name}</span>
                          </span>
                          <span className="text-xs text-fg-muted">{formatBytes(entry.size)}</span>
                        </button>
                      );
                    })}
                  </div>
                )}
              </div>
            </div>
          )}
        </div>

        <footer className="flex items-center justify-between gap-3 border-t border-line px-5 py-4">
          <p className="min-w-0 truncate text-sm text-fg-muted">
            {multi
              ? selectedPaths.size > 0
                ? `已选 ${selectedPaths.size} 项：${[...selectedPaths.values()].join("、")}`
                : "未选择"
              : (selectedFile?.path ?? "未选择文件")}
          </p>
          <div className="flex shrink-0 gap-2">
            <button
              className="rounded-md border border-line-strong px-3 py-2 text-sm hover:bg-hover"
              type="button"
              onClick={onClose}
            >
              取消
            </button>
            <button
              className="rounded-md bg-accent px-3 py-2 text-sm font-medium text-white hover:bg-accent-strong disabled:opacity-40"
              type="button"
              disabled={!submitReady}
              onClick={submit}
            >
              {submitLabel}
            </button>
          </div>
        </footer>
      </section>
    </div>
  );
}

function requiredPath(path: string | null): string {
  if (path === null) {
    throw new Error("missing current path");
  }
  return path;
}

function parentPath(path: string): string | null {
  const trimmed = path.replace(/\/+$/, "");
  if (!trimmed || trimmed === "/") {
    return null;
  }
  const index = trimmed.lastIndexOf("/");
  if (index <= 0) {
    return "/";
  }
  return trimmed.slice(0, index);
}

function formatBytes(size: number | null | undefined): string {
  if (typeof size !== "number") {
    return "";
  }
  if (size < 1024) {
    return `${size} B`;
  }
  if (size < 1024 * 1024) {
    return `${(size / 1024).toFixed(1)} KB`;
  }
  return `${(size / 1024 / 1024).toFixed(1)} MB`;
}
