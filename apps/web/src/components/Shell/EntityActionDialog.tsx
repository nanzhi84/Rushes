import * as Dialog from "@radix-ui/react-dialog";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { useEffect, useMemo, useRef, useState } from "react";
import type { ReactElement } from "react";
import { api } from "../../api/client";
import { queryKeys } from "../../app/query_client";
import type { EntityDialogState } from "../../state/ui_store";

/** 对话框只需要草稿的 id 与名字；DraftListItem/DraftRecord 都结构兼容。 */
export type DraftDialogEntry = { draft_id: string; name: string };

type EntityActionDialogProps = {
  dialog: EntityDialogState | null;
  drafts: DraftDialogEntry[];
  onClose: () => void;
};

/** 草稿的重命名、复制、删除统一对话框；草稿墙与编辑器共用。 */
export function EntityActionDialog({
  dialog,
  drafts,
  onClose
}: EntityActionDialogProps): ReactElement | null {
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const [name, setName] = useState("");
  const [confirmed, setConfirmed] = useState(false);

  const sourceDraft = useMemo(
    () => drafts.find((draft) => draft.draft_id === dialog?.draftId) ?? null,
    [dialog?.draftId, drafts]
  );

  // 用户开始编辑后不再重置：草稿列表在对话框打开期间刷新（SSE 失效重取）时，
  // 依赖数组里的 drafts 引用会变化，若无守卫会把已输入的名称/确认勾选清掉。
  const dirtyRef = useRef(false);
  const lastDialogRef = useRef<EntityDialogState | null>(null);

  useEffect(() => {
    if (!dialog) {
      lastDialogRef.current = null;
      dirtyRef.current = false;
      return;
    }
    if (lastDialogRef.current !== dialog) {
      lastDialogRef.current = dialog;
      dirtyRef.current = false;
    }
    if (dirtyRef.current) {
      return;
    }
    setName(initialName(dialog.kind, sourceDraft?.name));
    setConfirmed(false);
  }, [dialog, sourceDraft?.name]);

  const markDirty = (): void => {
    dirtyRef.current = true;
  };

  const mutation = useMutation({
    mutationFn: async () => {
      if (!dialog) {
        return null;
      }
      const draftId = dialog.draftId;
      switch (dialog.kind) {
        case "renameDraft":
          return { kind: dialog.kind, draftId, result: await api.renameDraft(draftId, { name }) };
        case "copyDraft":
          return { kind: dialog.kind, draftId, result: await api.copyDraft(draftId, { name }) };
        case "deleteDraft":
          return { kind: dialog.kind, draftId, result: await api.trashDraft(draftId, confirmed) };
      }
    },
    onSuccess: async (payload) => {
      if (!payload) {
        return;
      }
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: queryKeys.drafts }),
        queryClient.invalidateQueries({ queryKey: queryKeys.draft(payload.draftId) })
      ]);
      if (payload.kind === "deleteDraft") {
        await navigate({ to: "/" });
      } else {
        await navigate({
          to: "/drafts/$draftId",
          params: { draftId: payload.result.draft.draft_id }
        });
      }
      onClose();
    }
  });

  if (!dialog) {
    return null;
  }

  const destructive = dialog.kind === "deleteDraft";
  const naming = !destructive;
  const formReady = (destructive && confirmed) || (naming && name.trim().length > 0);

  return (
    <Dialog.Root
      open
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
          <form
            onSubmit={(event) => {
              event.preventDefault();
              if (!formReady || mutation.isPending) {
                return;
              }
              mutation.mutate();
            }}
          >
            <Dialog.Title className="text-lg font-semibold text-fg">
              {dialogTitle(dialog.kind)}
            </Dialog.Title>

            {naming ? (
              <label className="mt-4 block text-sm font-medium text-fg-muted">
                名称
                <input
                  className="mt-2 w-full rounded-md border border-line bg-ink px-3 py-2 text-fg outline-none focus:border-accent"
                  value={name}
                  onChange={(event) => {
                    markDirty();
                    setName(event.target.value);
                  }}
                  autoFocus
                />
              </label>
            ) : null}

            {destructive ? (
              <label className="mt-4 flex items-start gap-3 rounded-lg border border-danger/40 bg-danger/10 p-3 text-sm text-fg">
                <input
                  className="mt-1 accent-[color:var(--color-danger)]"
                  type="checkbox"
                  checked={confirmed}
                  onChange={(event) => {
                    markDirty();
                    setConfirmed(event.target.checked);
                  }}
                />
                确认删除这条草稿。后端会走软删除和同一条归约路径。
              </label>
            ) : null}

            {mutation.error ? (
              <p className="mt-4 rounded-lg border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">
                操作失败，请检查后端响应。
              </p>
            ) : null}

            <div className="mt-5 flex justify-end gap-2">
              <Dialog.Close asChild>
                <button
                  className="rounded-md border border-line px-3 py-2 text-sm text-fg-muted transition-colors ease-standard hover:bg-hover hover:text-fg"
                  type="button"
                >
                  取消
                </button>
              </Dialog.Close>
              <button
                className={`rounded-md px-3 py-2 text-sm font-medium text-white transition-colors ease-standard disabled:opacity-40 ${
                  destructive ? "bg-danger hover:bg-danger/80" : "bg-accent hover:bg-accent-strong"
                }`}
                type="submit"
                disabled={!formReady || mutation.isPending}
              >
                {mutation.isPending ? "处理中" : "确认"}
              </button>
            </div>
          </form>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}

function initialName(kind: EntityDialogState["kind"], draftName?: string): string {
  if (kind === "renameDraft") {
    return draftName ?? "";
  }
  if (kind === "copyDraft") {
    return draftName ? `${draftName} 副本` : "";
  }
  return "";
}

function dialogTitle(kind: EntityDialogState["kind"]): string {
  const titles: Record<EntityDialogState["kind"], string> = {
    renameDraft: "重命名草稿",
    copyDraft: "复制草稿",
    deleteDraft: "删除草稿"
  };
  return titles[kind];
}
