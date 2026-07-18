import * as Dialog from "@radix-ui/react-dialog";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Pencil, Trash2, X } from "lucide-react";
import { useState, type ReactElement, type ReactNode } from "react";
import { api, type MemoriesResponse, type MemoryRecord } from "../../api/client";

type WorkspaceSettingsDialogProps = {
  open: boolean;
  onClose: () => void;
};

export function WorkspaceSettingsDialog({
  open,
  onClose
}: WorkspaceSettingsDialogProps): ReactElement {
  const queryClient = useQueryClient();
  const [clearConfirmOpen, setClearConfirmOpen] = useState(false);
  const memoriesQuery = useQuery({
    queryKey: ["memories"],
    queryFn: () => api.listMemories(),
    enabled: open
  });
  const deleteMemory = useMutation({
    mutationFn: (memoryKey: string) => api.deleteMemory(memoryKey),
    onSuccess: (_, memoryKey) => {
      queryClient.setQueryData<MemoriesResponse>(["memories"], (current) => ({
        memories: current?.memories.filter((memory) => memory.memory_key !== memoryKey) ?? []
      }));
    }
  });
  const updateMemory = useMutation({
    mutationFn: ({ memoryKey, statement }: { memoryKey: string; statement: string }) =>
      api.updateMemoryStatement(memoryKey, statement),
    onSuccess: (updated) => {
      queryClient.setQueryData<MemoriesResponse>(["memories"], (current) => ({
        memories:
          current?.memories.map((memory) =>
            memory.memory_key === updated.memory_key ? updated : memory
          ) ?? []
      }));
    }
  });
  const clearMemories = useMutation({
    mutationFn: () => api.clearMemories(true),
    onSuccess: () => {
      queryClient.setQueryData<MemoriesResponse>(["memories"], { memories: [] });
      setClearConfirmOpen(false);
    }
  });
  const memories = memoriesQuery.data?.memories ?? [];
  const deleteError = deleteMemory.error;
  const mutationPending = deleteMemory.isPending || clearMemories.isPending || updateMemory.isPending;

  const handleDelete = (memoryKey: string): void => {
    clearMemories.reset();
    deleteMemory.mutate(memoryKey);
  };

  const handleClearConfirmOpenChange = (nextOpen: boolean): void => {
    if (nextOpen) {
      clearMemories.reset();
    }
    setClearConfirmOpen(nextOpen);
  };

  const handleClear = (): void => {
    deleteMemory.reset();
    clearMemories.mutate();
  };

  return (
    <Dialog.Root
      open={open}
      onOpenChange={(next) => {
        if (!next) {
          setClearConfirmOpen(false);
          onClose();
        }
      }}
    >
      <Dialog.Portal>
        <Dialog.Overlay className="rx-overlay fixed inset-0 z-30 bg-black/60 backdrop-blur-sm" />
        <Dialog.Content
          aria-describedby={undefined}
          className="rx-content fixed left-1/2 top-1/2 z-40 max-h-[calc(100vh-2rem)] w-[calc(100%-2rem)] max-w-xl -translate-x-1/2 -translate-y-1/2 overflow-y-auto rounded-xl bg-raised p-5 shadow-overlay focus:outline-none"
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

          <Section title="长期记忆">
            <p className="text-xs leading-5 text-fg-faint">
              代理会在不同草稿间沿用这些稳定偏好。删除会立即清除当前记录；正在进行的回合不受影响。
            </p>
            {memoriesQuery.isPending ? (
              <p className="mt-3 text-sm text-fg-muted">正在读取长期记忆…</p>
            ) : memoriesQuery.isError ? (
              <p className="mt-3 text-sm text-danger" role="alert">
                长期记忆读取失败，请稍后重试。
              </p>
            ) : memories.length === 0 ? (
              <p className="mt-3 rounded-md border border-dashed border-line px-3 py-4 text-center text-sm text-fg-muted">
                还没有长期记忆
              </p>
            ) : (
              <ul className="mt-3 space-y-2" aria-label="长期记忆列表">
                {memories.map((memory) => (
                  <MemoryRow
                    key={memory.memory_key}
                    memory={memory}
                    deleting={deleteMemory.isPending && deleteMemory.variables === memory.memory_key}
                    saving={
                      updateMemory.isPending &&
                      updateMemory.variables?.memoryKey === memory.memory_key
                    }
                    disabled={mutationPending}
                    onDelete={() => handleDelete(memory.memory_key)}
                    onSave={async (statement) => {
                      clearMemories.reset();
                      deleteMemory.reset();
                      await updateMemory.mutateAsync({
                        memoryKey: memory.memory_key,
                        statement
                      });
                    }}
                  />
                ))}
              </ul>
            )}
            {deleteError ? (
              <p className="mt-3 text-sm text-danger" role="alert">
                删除失败，请重新打开设置确认当前内容。
              </p>
            ) : null}
            {updateMemory.isError ? (
              <p className="mt-3 text-sm text-danger" role="alert">
                保存失败，请重试或重新打开设置确认当前内容。
              </p>
            ) : null}
            {memories.length > 0 ? (
              <ClearMemoriesDialog
                open={clearConfirmOpen}
                count={memories.length}
                pending={mutationPending}
                failed={clearMemories.isError}
                onOpenChange={handleClearConfirmOpenChange}
                onConfirm={handleClear}
              />
            ) : null}
          </Section>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}

function ClearMemoriesDialog({
  open,
  count,
  pending,
  failed,
  onOpenChange,
  onConfirm
}: {
  open: boolean;
  count: number;
  pending: boolean;
  failed: boolean;
  onOpenChange: (open: boolean) => void;
  onConfirm: () => void;
}): ReactElement {
  return (
    <Dialog.Root
      open={open}
      onOpenChange={(nextOpen) => {
        if (!pending) {
          onOpenChange(nextOpen);
        }
      }}
    >
      <Dialog.Trigger asChild>
        <button
          className="mt-3 text-xs font-medium text-danger transition-opacity hover:opacity-80 disabled:opacity-40"
          type="button"
          disabled={pending}
        >
          清空全部长期记忆
        </button>
      </Dialog.Trigger>
      <Dialog.Portal>
        <Dialog.Overlay className="rx-overlay fixed inset-0 z-50 bg-black/70 backdrop-blur-sm" />
        <Dialog.Content className="rx-content fixed left-1/2 top-1/2 z-[60] w-[calc(100%-2rem)] max-w-md -translate-x-1/2 -translate-y-1/2 rounded-xl bg-raised p-5 shadow-overlay focus:outline-none">
          <Dialog.Title className="text-lg font-semibold text-fg">
            确认清空全部长期记忆？
          </Dialog.Title>
          <Dialog.Description className="mt-2 text-sm leading-6 text-fg-muted">
            将删除 {count} 条跨草稿偏好；正在进行的回合不受影响。此操作无法在界面中撤销。
          </Dialog.Description>
          {failed ? (
            <p className="mt-4 rounded-lg border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger" role="alert">
              清空失败，请重新打开设置确认当前内容。
            </p>
          ) : null}
          <div className="mt-5 flex justify-end gap-2">
            <button
              className="rounded-md border border-line px-3 py-2 text-sm text-fg-muted transition-colors hover:bg-hover hover:text-fg disabled:opacity-40"
              type="button"
              disabled={pending}
              onClick={() => onOpenChange(false)}
            >
              取消
            </button>
            <button
              className="rounded-md bg-danger px-3 py-2 text-sm font-medium text-white transition-colors hover:bg-danger/80 disabled:opacity-40"
              type="button"
              disabled={pending}
              onClick={onConfirm}
            >
              {pending ? "正在清空" : "确认清空全部长期记忆"}
            </button>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}

function MemoryRow({
  memory,
  deleting,
  saving,
  disabled,
  onDelete,
  onSave
}: {
  memory: MemoryRecord;
  deleting: boolean;
  saving: boolean;
  disabled: boolean;
  onDelete: () => void;
  onSave: (statement: string) => Promise<void>;
}): ReactElement {
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState(memory.statement);
  const trimmed = draft.trim();
  const canSave = trimmed.length > 0 && trimmed.length <= 200 && trimmed !== memory.statement;

  const submit = async (): Promise<void> => {
    if (!canSave) {
      return;
    }
    // 保存期间保持编辑态到 settled，「正在保存」才可见；成功后退出，失败保留草稿文本，
    // 错误提示由父组件的 updateMemory.isError 展示。
    try {
      await onSave(trimmed);
      setEditing(false);
    } catch {
      // 保留编辑态与草稿，供用户重试。
    }
  };

  if (editing) {
    return (
      <li className="rounded-md border border-accent/50 bg-raised px-3 py-2.5">
        <div className="flex flex-wrap items-center gap-2 text-xs text-fg-faint">
          <span className="rounded bg-hover px-1.5 py-0.5">{memoryKindLabel(memory.kind)}</span>
          <code>{memory.memory_key}</code>
        </div>
        <textarea
          aria-label={`编辑长期记忆 ${memory.memory_key}`}
          autoFocus
          maxLength={200}
          className="mt-1.5 h-16 w-full resize-none rounded-sm border border-line bg-panel px-2 py-1.5 text-sm leading-5 text-fg outline-none focus:border-accent"
          value={draft}
          onChange={(event) => setDraft(event.target.value)}
        />
        <div className="mt-1.5 flex items-center justify-end gap-1.5">
          <button
            type="button"
            className="rounded-sm px-2 py-1 text-2xs text-fg-muted transition-colors hover:bg-hover hover:text-fg"
            onClick={() => {
              setDraft(memory.statement);
              setEditing(false);
            }}
          >
            取消
          </button>
          <button
            type="button"
            className="rounded-sm bg-accent px-2 py-1 text-2xs font-medium text-white transition-colors hover:bg-accent-strong disabled:opacity-40"
            disabled={!canSave || saving}
            onClick={() => {
              void submit();
            }}
          >
            {saving ? "正在保存" : "保存"}
          </button>
        </div>
      </li>
    );
  }

  return (
    <li className="flex items-start gap-3 rounded-md border border-line bg-raised px-3 py-2.5">
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-center gap-2 text-xs text-fg-faint">
          <span className="rounded bg-hover px-1.5 py-0.5">{memoryKindLabel(memory.kind)}</span>
          <code>{memory.memory_key}</code>
          {memory.manually_revised_at ? (
            <span className="rounded bg-accent/10 px-1.5 py-0.5 text-accent">手动修订</span>
          ) : null}
        </div>
        <p className="mt-1 text-sm leading-5 text-fg">{memory.statement}</p>
        <p className="mt-1 truncate text-[11px] text-fg-faint" title={memory.source_draft_id}>
          来源草稿 {memory.source_draft_id} · {new Date(memory.last_confirmed_at).toLocaleString()}
        </p>
      </div>
      <div className="flex shrink-0 items-center gap-0.5">
        <button
          className="grid h-7 w-7 place-items-center rounded-md text-fg-muted transition-colors hover:bg-hover hover:text-fg disabled:opacity-40"
          type="button"
          aria-label={`编辑长期记忆 ${memory.memory_key}`}
          disabled={disabled}
          onClick={() => {
            setDraft(memory.statement);
            setEditing(true);
          }}
        >
          <Pencil size={14} strokeWidth={1.75} aria-hidden />
        </button>
        <button
          className="grid h-7 w-7 place-items-center rounded-md text-fg-muted transition-colors hover:bg-danger/10 hover:text-danger disabled:opacity-40"
          type="button"
          aria-label={`删除长期记忆 ${memory.memory_key}`}
          disabled={disabled}
          onClick={onDelete}
        >
          <Trash2 size={14} strokeWidth={1.75} aria-hidden />
          <span className="sr-only">{deleting ? "正在删除" : "删除"}</span>
        </button>
      </div>
    </li>
  );
}

function memoryKindLabel(kind: MemoryRecord["kind"]): string {
  switch (kind) {
    case "correction":
      return "纠正";
    case "habit":
      return "习惯";
    default:
      return "偏好";
  }
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
