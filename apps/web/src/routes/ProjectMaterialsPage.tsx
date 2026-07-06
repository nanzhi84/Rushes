import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useParams } from "@tanstack/react-router";
import { useEffect, useState } from "react";
import type { ReactElement } from "react";
import { api, type DecisionAnswer, type MaterialAsset } from "../api/client";
import { queryKeys } from "../app/query_client";
import { FsBrowserDialog } from "../components/Materials/FsBrowserDialog";
import { LocalImportPanel } from "../components/Materials/LocalImportPanel";
import { MaterialsTable } from "../components/Materials/MaterialsTable";
import { UploadDropzone } from "../components/Materials/UploadDropzone";
import {
  type PendingUrlDecision,
  UrlDecisionCards,
  UrlImportPanel,
  type UrlImportDraft
} from "../components/Materials/UrlImportPanel";
import { createApiEventSource } from "../auth";

type ProjectMaterialsViewProps = {
  projectId: string;
  enableEvents?: boolean;
};

type MaterialsSsePayload = {
  event_id: number;
  event: {
    event: string;
    project_id?: string | null;
  };
};

export function ProjectMaterialsPage(): ReactElement {
  const params = useParams({ strict: false }) as { projectId: string };
  return <ProjectMaterialsView projectId={params.projectId} />;
}

export function ProjectMaterialsView({
  projectId,
  enableEvents = true
}: ProjectMaterialsViewProps): ReactElement {
  const queryClient = useQueryClient();
  const [pendingUrlDecisions, setPendingUrlDecisions] = useState<PendingUrlDecision[]>([]);
  const [relocatingAsset, setRelocatingAsset] = useState<MaterialAsset | null>(null);

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
  const actionPending =
    importLocal.isPending ||
    answerUrlDecision.isPending ||
    unlinkMaterial.isPending ||
    patchMaterial.isPending ||
    revalidateMaterials.isPending;

  return (
    <section className="mx-auto flex w-full max-w-7xl flex-col gap-5 p-6">
      <header className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <p className="text-sm font-medium text-[#64748b]">素材管理</p>
          <h1 className="mt-2 text-2xl font-semibold">项目级素材页</h1>
          <p className="mt-2 text-sm text-[#64748b]">Project：{projectId}</p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <span className="rounded bg-[#eef2f7] px-3 py-2 text-sm text-[#475569]">
            累计标注成本：暂无接口
          </span>
          <button
            className="rounded-md border border-[#cbd5e1] px-3 py-2 text-sm hover:bg-[#f1f5f9] disabled:text-[#94a3b8]"
            type="button"
            disabled={revalidateMaterials.isPending}
            onClick={() => revalidateMaterials.mutate()}
          >
            重新检测失效
          </button>
          <a
            className="rounded-md border border-[#cbd5e1] px-3 py-2 text-sm hover:bg-[#f1f5f9]"
            href={`/projects/${encodeURIComponent(projectId)}`}
          >
            返回项目首页
          </a>
        </div>
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
        <p className="text-sm text-[#64748b]">正在读取素材列表</p>
      ) : materialsQuery.error ? (
        <p className="rounded-md bg-[#fee4e2] px-3 py-2 text-sm text-[#b42318]">素材列表加载失败</p>
      ) : (
        <MaterialsTable
          assets={assets}
          actionPending={actionPending}
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

function useMaterialsEvents(projectId: string, enabled: boolean): void {
  const queryClient = useQueryClient();

  useEffect(() => {
    if (!enabled) {
      return;
    }
    const source = createApiEventSource("/api/events");
    const handleEvent = (event: Event) => {
      const message = event as MessageEvent<string>;
      const payload = JSON.parse(message.data) as MaterialsSsePayload;
      const eventProjectId = payload.event.project_id;
      if (!eventProjectId || eventProjectId === projectId) {
        void queryClient.invalidateQueries({ queryKey: queryKeys.materials(projectId) });
      }
    };
    for (const eventName of MATERIAL_EVENT_TYPES) {
      source.addEventListener(eventName, handleEvent);
    }
    return () => {
      source.close();
    };
  }, [enabled, projectId, queryClient]);
}

function decisionAnswer(approved: boolean): DecisionAnswer {
  return {
    option_id: approved ? "approve" : "reject",
    answered_via: "button",
    payload: { approved }
  };
}

const MATERIAL_EVENT_TYPES = [
  "AssetImported",
  "AssetProbed",
  "ProxyGenerated",
  "AnnotationCompleted",
  "AnnotationFailed",
  "AssetInvalidated",
  "AssetLinked",
  "AssetUnlinked",
  "JobEnqueued",
  "JobProgress",
  "JobSucceeded",
  "JobFailed",
  "JobCancelled",
  "DecisionAnswered"
];
