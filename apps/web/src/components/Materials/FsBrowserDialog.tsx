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
  onSelect: (path: string) => void;
};

export function FsBrowserDialog({
  open,
  title,
  submitLabel,
  onClose,
  onSelect
}: FsBrowserDialogProps): ReactElement | null {
  const [currentPath, setCurrentPath] = useState<string | null>(null);
  const [selectedFile, setSelectedFile] = useState<FsListEntry | null>(null);

  useEffect(() => {
    if (!open) {
      setCurrentPath(null);
      setSelectedFile(null);
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

  return (
    <div className="fixed inset-0 z-40 grid place-items-center bg-black/30 px-4" role="dialog" aria-modal="true">
      <section className="flex max-h-[82vh] w-full max-w-2xl flex-col rounded-lg border border-[#d9dee7] bg-white shadow-lg">
        <header className="border-b border-[#d9dee7] px-5 py-4">
          <h2 className="text-lg font-semibold">{title}</h2>
          <p className="mt-1 truncate text-sm text-[#64748b]">
            {currentPath ?? "选择一个服务器允许访问的根目录"}
          </p>
        </header>

        <div className="min-h-0 flex-1 overflow-y-auto p-4">
          {currentPath === null ? (
            rootsQuery.isLoading ? (
              <p className="text-sm text-[#64748b]">正在读取根目录</p>
            ) : rootsQuery.error ? (
              <p className="rounded-md bg-[#fee4e2] px-3 py-2 text-sm text-[#b42318]">
                根目录读取失败
              </p>
            ) : (
              <div className="space-y-2">
                {(rootsQuery.data?.roots ?? []).map((root) => (
                  <button
                    key={root.path}
                    className="flex w-full items-center justify-between rounded-md border border-[#d9dee7] px-3 py-2 text-left text-sm hover:bg-[#f1f5f9] disabled:text-[#94a3b8]"
                    type="button"
                    disabled={!root.exists}
                    onClick={() => setCurrentPath(root.path)}
                  >
                    <span>
                      <span className="font-medium">{root.name}</span>
                      <span className="ml-2 text-xs text-[#64748b]">{root.path}</span>
                    </span>
                    <span className="text-xs text-[#64748b]">{root.exists ? "打开" : "不存在"}</span>
                  </button>
                ))}
              </div>
            )
          ) : listQuery.isLoading ? (
            <p className="text-sm text-[#64748b]">正在读取目录</p>
          ) : listQuery.error ? (
            <p className="rounded-md bg-[#fee4e2] px-3 py-2 text-sm text-[#b42318]">目录读取失败</p>
          ) : (
            <div className="space-y-4">
              <div className="flex flex-wrap gap-2">
                <button
                  className="rounded-md border border-[#cbd5e1] px-3 py-1.5 text-sm hover:bg-[#f1f5f9]"
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
                    className="rounded-md border border-[#cbd5e1] px-3 py-1.5 text-sm hover:bg-[#f1f5f9]"
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
                  <h3 className="mb-2 text-xs font-semibold text-[#64748b]">目录</h3>
                  <div className="space-y-1">
                    {directories.map((entry) => (
                      <button
                        key={entry.path}
                        className="block w-full rounded-md px-3 py-2 text-left text-sm hover:bg-[#f1f5f9]"
                        type="button"
                        onClick={() => {
                          setCurrentPath(entry.path);
                          setSelectedFile(null);
                        }}
                      >
                        {entry.name}
                      </button>
                    ))}
                  </div>
                </div>
              ) : null}

              <div>
                <h3 className="mb-2 text-xs font-semibold text-[#64748b]">媒体文件</h3>
                {files.length === 0 ? (
                  <p className="rounded-md border border-dashed border-[#cbd5e1] px-3 py-4 text-sm text-[#64748b]">
                    当前目录没有可导入的媒体文件。
                  </p>
                ) : (
                  <div className="space-y-1">
                    {files.map((entry) => (
                      <button
                        key={entry.path}
                        className={`flex w-full items-center justify-between rounded-md px-3 py-2 text-left text-sm ${
                          selectedFile?.path === entry.path
                            ? "bg-[#dfe7f1] text-[#17202a]"
                            : "hover:bg-[#f1f5f9]"
                        }`}
                        type="button"
                        onClick={() => setSelectedFile(entry)}
                      >
                        <span>{entry.name}</span>
                        <span className="text-xs text-[#64748b]">{formatBytes(entry.size)}</span>
                      </button>
                    ))}
                  </div>
                )}
              </div>
            </div>
          )}
        </div>

        <footer className="flex items-center justify-between gap-3 border-t border-[#d9dee7] px-5 py-4">
          <p className="min-w-0 truncate text-sm text-[#64748b]">
            {selectedFile ? selectedFile.path : "未选择文件"}
          </p>
          <div className="flex shrink-0 gap-2">
            <button
              className="rounded-md border border-[#cbd5e1] px-3 py-2 text-sm hover:bg-[#f1f5f9]"
              type="button"
              onClick={onClose}
            >
              取消
            </button>
            <button
              className="rounded-md bg-[#17202a] px-3 py-2 text-sm font-medium text-white disabled:bg-[#94a3b8]"
              type="button"
              disabled={!selectedFile}
              onClick={() => selectedFile && onSelect(selectedFile.path)}
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
