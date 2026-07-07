import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import type { ReactElement } from "react";
import { api, type DecisionAnswer, type MaterialAsset } from "../api/client";
import { queryKeys } from "../app/query_client";
import { FsBrowserDialog } from "../components/Materials/FsBrowserDialog";
import { LocalImportPanel } from "../components/Materials/LocalImportPanel";
import { MaterialSummaryPanel } from "../components/Materials/MaterialSummaryPanel";
import { MaterialsTable } from "../components/Materials/MaterialsTable";
import { UploadDropzone } from "../components/Materials/UploadDropzone";
import {
  type PendingUrlDecision,
  UrlDecisionCards,
  UrlImportPanel,
  type UrlImportDraft
} from "../components/Materials/UrlImportPanel";
import { useMaterialsEvents } from "../components/Materials/useMaterialsEvents";

type ProjectMaterialsViewProps = {
  projectId: string;
  enableEvents?: boolean;
};

export function ProjectMaterialsView({
  projectId,
  enableEvents = true
}: ProjectMaterialsViewProps): ReactElement {
  const queryClient = useQueryClient();
  const [pendingUrlDecisions, setPendingUrlDecisions] = useState<PendingUrlDecision[]>([]);
  const [relocatingAsset, setRelocatingAsset] = useState<MaterialAsset | null>(null);
  const [activeAssetId, setActiveAssetId] = useState<string | null>(null);

  const materialsQuery = useQuery({
    queryKey: queryKeys.materials(projectId),
    queryFn: () => api.listMaterials(projectId),
    refetchInterval: 5_000
  });

  useMaterialsEvents(projectId, enableEvents);

  const invalidateMaterials = async (): Promise<void> => {
    await Promise.all([
      queryClient.invalidateQueries({ queryKey: queryKeys.materials(projectId) }),
      queryClient.invalidateQueries({ queryKey: queryKeys.projectTree })
    ]);
  };

  const importLocal = useMutation({
    mutationFn: ({ path }: { path: string }) =>
      api.importLocalMaterial(projectId, { path, storage_mode: "reference" }),
    onSuccess: invalidateMaterials
  });

  const createUrlDecision = useMutation({
    mutationFn: (draft: UrlImportDraft) =>
      api.importUrlMaterial(projectId, {
        url: draft.url,
        filename: draft.filename
      }),
    onSuccess: (response, draft) => {
      const decisionId = response.decision_id;
      if (!decisionId) {
        return;
      }
      setPendingUrlDecisions((current) => [
        ...current.filter((item) => item.decisionId !== decisionId),
        {
          decisionId,
          url: draft.url,
          filename: draft.filename
        }
      ]);
    }
  });

  const answerUrlDecision = useMutation({
    mutationFn: ({ decision, approved }: { decision: PendingUrlDecision; approved: boolean }) =>
      api.answerDecision(decision.decisionId, {
        project_id: projectId,
        answer: decisionAnswer(approved)
      }),
    onSuccess: async (_response, variables) => {
      setPendingUrlDecisions((current) =>
        current.filter((item) => item.decisionId !== variables.decision.decisionId)
      );
      await invalidateMaterials();
    }
  });

  const unlinkMaterial = useMutation({
    mutationFn: (asset: MaterialAsset) => api.unlinkMaterial(projectId, { asset_id: asset.asset_id }),
    onSuccess: invalidateMaterials
  });

  const patchMaterial = useMutation({
    mutationFn: ({ asset, payload }: { asset: MaterialAsset; payload: { enabled?: boolean; reference_path?: string } }) =>
      api.patchMaterial(projectId, asset.asset_id, payload),
    onSuccess: async () => {
      setRelocatingAsset(null);
      await invalidateMaterials();
    }
  });

  const revalidateMaterials = useMutation({
    mutationFn: () => api.revalidateMaterials(projectId),
    onSuccess: (response) => {
      queryClient.setQueryData(queryKeys.materials(projectId), response);
    }
  });

  const assets = materialsQuery.data?.assets ?? [];
  // 从最新列表按 id 反查，保证摘要面板跟随理解状态刷新。
  const activeAsset = activeAssetId
    ? (assets.find((asset) => asset.asset_id === activeAssetId) ?? null)
    : null;
  const actionPending =
    importLocal.isPending ||
    answerUrlDecision.isPending ||
    unlinkMaterial.isPending ||
    patchMaterial.isPending ||
    revalidateMaterials.isPending;

  return (
    <section className="flex w-full flex-col gap-5">
      <header className="flex flex-wrap items-center justify-between gap-3">
        <p className="text-sm text-fg-muted">项目素材池，导入后可在剪辑任务中使用。</p>
        <button
          className="rounded-md border border-line-strong px-3 py-2 text-sm hover:bg-hover disabled:text-fg-faint"
          type="button"
          disabled={revalidateMaterials.isPending}
          onClick={() => revalidateMaterials.mutate()}
        >
          重新检测失效
        </button>
      </header>

      <div className="grid gap-4 xl:grid-cols-3">
        <UploadDropzone projectId={projectId} onUploaded={invalidateMaterials} />
        <LocalImportPanel
          isPending={importLocal.isPending}
          onImport={(path) => importLocal.mutate({ path })}
        />
        <UrlImportPanel
          isPending={createUrlDecision.isPending}
          onCreate={(draft) => createUrlDecision.mutate(draft)}
        />
      </div>

      <UrlDecisionCards
        decisions={pendingUrlDecisions}
        isPending={answerUrlDecision.isPending}
        onAnswer={(decision, approved) => answerUrlDecision.mutate({ decision, approved })}
      />

      {materialsQuery.isLoading ? (
        <p className="text-sm text-fg-muted">正在读取素材列表</p>
      ) : materialsQuery.error ? (
        <p className="rounded-md bg-danger/15 px-3 py-2 text-sm text-danger">素材列表加载失败</p>
      ) : (
        <MaterialsTable
          assets={assets}
          actionPending={actionPending}
          activeAssetId={activeAssetId}
          onSelect={(asset) => setActiveAssetId(asset.asset_id)}
          onRelocate={setRelocatingAsset}
          onToggleEnabled={(asset) =>
            patchMaterial.mutate({ asset, payload: { enabled: !asset.enabled } })
          }
          onUnlink={(asset) => {
            if (window.confirm(`删除素材引用：${asset.filename || asset.asset_id}？`)) {
              unlinkMaterial.mutate(asset);
            }
          }}
        />
      )}

      {activeAsset ? (
        <MaterialSummaryPanel
          projectId={projectId}
          asset={activeAsset}
          onClose={() => setActiveAssetId(null)}
        />
      ) : null}

      <FsBrowserDialog
        open={relocatingAsset !== null}
        title="重新定位失效素材"
        submitLabel="使用此路径"
        onClose={() => setRelocatingAsset(null)}
        onSelect={(path) => {
          if (relocatingAsset) {
            patchMaterial.mutate({ asset: relocatingAsset, payload: { reference_path: path } });
          }
        }}
      />
    </section>
  );
}

function decisionAnswer(approved: boolean): DecisionAnswer {
  return {
    option_id: approved ? "approve" : "reject",
    answered_via: "button",
    payload: { approved }
  };
}
