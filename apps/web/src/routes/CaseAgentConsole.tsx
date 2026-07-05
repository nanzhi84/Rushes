import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useParams } from "@tanstack/react-router";
import { useCallback, useEffect, useMemo, useState } from "react";
import type { ReactElement } from "react";
import { api, type DecisionAnswer } from "../api/client";
import { queryKeys } from "../app/query_client";
import { createApiEventSource } from "../auth";
import { AssistantThread } from "../components/Console/AssistantThread";
import {
  markDecisionAnswered,
  mergeCurrentDecisionItem,
  reduceStructuredInteractionItems,
  StructuredInteractionRenderer
} from "../components/Console/StructuredInteractionRenderer";
import type {
  DomainSsePayload,
  StructuredInteractionItem
} from "../components/Console/StructuredInteractionRenderer";
import { useConsoleExternalStoreRuntime, type ConsoleMessage } from "../components/Console/runtime";

export function CaseAgentConsolePage(): ReactElement {
  const params = useParams({ strict: false }) as { projectId: string; caseId: string };
  return <CaseConsoleView projectId={params.projectId} caseId={params.caseId} />;
}

export function CaseConsoleView({
  projectId,
  caseId
}: {
  projectId: string;
  caseId: string;
}): ReactElement {
  const queryClient = useQueryClient();
  const [draft, setDraft] = useState("");
  const [awaitingTurnEnd, setAwaitingTurnEnd] = useState(false);
  const [streamState, setStreamState] = useState<"connecting" | "open" | "closed">("connecting");
  const [structuredItems, setStructuredItems] = useState<StructuredInteractionItem[]>([]);

  const messagesQuery = useQuery({
    queryKey: queryKeys.messages(projectId, caseId),
    queryFn: async () => [] as ConsoleMessage[],
    enabled: false,
    initialData: [] as ConsoleMessage[]
  });

  const decisionQuery = useQuery({
    queryKey: queryKeys.currentDecision(projectId, caseId),
    queryFn: () => api.currentDecision(projectId, caseId)
  });

  const currentDecision = decisionQuery.data?.decision ?? null;
  const messages = messagesQuery.data ?? [];
  const renderedStructuredItems = useMemo(
    () => {
      if (
        currentDecision &&
        structuredItems.some(
          (item) => item.kind === "decision" && item.decision_id === currentDecision.decision_id
        )
      ) {
        return mergeCurrentDecisionItem(structuredItems, currentDecision);
      }
      return structuredItems;
    },
    [currentDecision, structuredItems]
  );
  const sideDecisionItem = useMemo(
    () => mergeCurrentDecisionItem([], currentDecision)[0] ?? null,
    [currentDecision]
  );

  const invalidateCaseQueries = useCallback(
    async (payload: DomainSsePayload) => {
      const event = payload.event;
      if (event.event === "TurnEnded") {
        setAwaitingTurnEnd(false);
      }
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: queryKeys.projectTree }),
        queryClient.invalidateQueries({ queryKey: queryKeys.case(projectId, caseId) }),
        queryClient.invalidateQueries({ queryKey: queryKeys.messages(projectId, caseId) }),
        queryClient.invalidateQueries({ queryKey: queryKeys.currentDecision(projectId, caseId) })
      ]);
    },
    [caseId, projectId, queryClient]
  );

  useEffect(() => {
    const source = createApiEventSource(`/api/projects/${projectId}/cases/${caseId}/events`);
    source.onopen = () => setStreamState("open");
    source.onerror = () => setStreamState("closed");
    const handleEvent = (event: Event) => {
      const message = event as MessageEvent<string>;
      const payload = JSON.parse(message.data) as DomainSsePayload;
      setStructuredItems((current) => reduceStructuredInteractionItems(current, payload));
      void invalidateCaseQueries(payload);
    };
    for (const eventName of CASE_EVENT_TYPES) {
      source.addEventListener(eventName, handleEvent);
    }
    return () => {
      source.close();
    };
  }, [caseId, invalidateCaseQueries, projectId]);

  const postMessage = useMutation({
    mutationFn: (content: string) => api.postMessage(projectId, caseId, { content }),
    onMutate: async (content) => {
      setAwaitingTurnEnd(true);
      await queryClient.cancelQueries({ queryKey: queryKeys.messages(projectId, caseId) });
      const optimistic: ConsoleMessage = {
        id: `local_${Date.now()}`,
        role: "user",
        content,
        createdAt: new Date().toISOString()
      };
      queryClient.setQueryData<ConsoleMessage[]>(queryKeys.messages(projectId, caseId), (current) => [
        ...(current ?? []),
        optimistic
      ]);
    },
    onError: () => setAwaitingTurnEnd(false)
  });

  const answerDecision = useMutation({
    mutationFn: ({ decisionId, answer }: { decisionId: string; answer: DecisionAnswer }) =>
      api.answerDecision(decisionId, {
        project_id: projectId,
        case_id: caseId,
        answer
      }),
    onSuccess: async (_data, variables) => {
      setStructuredItems((current) => markDecisionAnswered(current, variables.decisionId, variables.answer));
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: queryKeys.currentDecision(projectId, caseId) }),
        queryClient.invalidateQueries({ queryKey: queryKeys.messages(projectId, caseId) }),
        queryClient.invalidateQueries({ queryKey: queryKeys.case(projectId, caseId) })
      ]);
    }
  });

  const disabled = awaitingTurnEnd || postMessage.isPending;
  const submitMessage = useCallback(
    (content: string) => {
      postMessage.mutate(content);
    },
    [postMessage]
  );
  const handleAnswerDecision = useCallback(
    (decisionId: string, answer: DecisionAnswer) => {
      answerDecision.mutate({ decisionId, answer });
    },
    [answerDecision]
  );
  const runtime = useConsoleExternalStoreRuntime({
    messages,
    structuredItems: renderedStructuredItems,
    isRunning: disabled,
    canSubmit: !disabled,
    submit: submitMessage
  });
  const statusLabel = useMemo(() => {
    if (streamState === "open") {
      return "事件流已连接";
    }
    if (streamState === "closed") {
      return "事件流重连中";
    }
    return "事件流连接中";
  }, [streamState]);

  return (
    <section className="flex h-full min-h-[calc(100vh-3rem)] flex-col">
      <header className="border-b border-[#d9dee7] bg-white px-6 py-4">
        <p className="text-sm font-medium text-[#64748b]">剪辑控制台</p>
        <div className="mt-2 flex flex-wrap items-center justify-between gap-3">
          <h1 className="text-xl font-semibold">{caseId}</h1>
          <span className="rounded bg-[#eef2f7] px-2 py-1 text-xs text-[#475569]">{statusLabel}</span>
        </div>
      </header>

      <div className="grid min-h-0 flex-1 grid-cols-1 gap-4 p-6 xl:grid-cols-[minmax(0,1fr)_340px]">
        <div className="flex min-h-0 flex-col rounded-lg border border-[#d9dee7] bg-white">
          <AssistantThread
            runtime={runtime}
            onAnswerDecision={handleAnswerDecision}
            answerPending={answerDecision.isPending}
          />

          <form
            className="border-t border-[#d9dee7] p-4"
            onSubmit={(event) => {
              event.preventDefault();
              const content = draft.trim();
              if (!content || disabled) {
                return;
              }
              setDraft("");
              runtime.submit(content);
            }}
          >
            <label className="block text-sm font-medium text-[#334155]">
              消息输入
              <textarea
                aria-label="消息输入"
                className="mt-2 h-24 w-full resize-none rounded-md border border-[#cbd5e1] px-3 py-2 outline-none focus:border-[#2563eb] disabled:bg-[#f1f5f9]"
                value={draft}
                onChange={(event) => setDraft(event.target.value)}
                disabled={!runtime.canSubmit}
                placeholder={runtime.isRunning ? "等待本轮结束" : "输入给当前剪辑任务的剪辑指令"}
              />
            </label>
            <div className="mt-3 flex items-center justify-between gap-3">
              <p className="text-sm text-[#64748b]">
                {runtime.isRunning ? "输入框会在本轮结束事件后恢复。" : "消息会进入后端任务队列。"}
              </p>
              <button
                className="rounded-md bg-[#17202a] px-4 py-2 text-sm font-medium text-white disabled:bg-[#94a3b8]"
                type="submit"
                disabled={!runtime.canSubmit || draft.trim().length === 0}
              >
                发送
              </button>
            </div>
          </form>
        </div>

        <aside className="space-y-4">
          <section className="rounded-lg border border-[#d9dee7] bg-white p-4">
            <h2 className="font-semibold">当前确认项</h2>
            <div className="mt-3">
              {sideDecisionItem ? (
                <StructuredInteractionRenderer
                  item={sideDecisionItem}
                  onAnswerDecision={handleAnswerDecision}
                  answerPending={answerDecision.isPending}
                />
              ) : (
                <p className="text-sm text-[#64748b]">暂无待确认项。</p>
              )}
            </div>
          </section>

          <section className="rounded-lg border border-[#d9dee7] bg-white p-4">
            <h2 className="font-semibold">预览与时间线</h2>
            <p className="mt-3 text-sm leading-6 text-[#64748b]">
              预览播放器和只读时间线会在 M6 接入。
            </p>
          </section>
        </aside>
      </div>
    </section>
  );
}

const CASE_EVENT_TYPES = [
  "CaseCreated",
  "CaseRenamed",
  "CaseCopied",
  "CaseMoved",
  "CaseClosed",
  "CaseTrashed",
  "CaseAssetScopeChanged",
  "DecisionCreated",
  "DecisionAnswered",
  "DecisionCancelled",
  "BriefUpdated",
  "ContentPlanUpdated",
  "AudioPlanUpdated",
  "CutPlanUpdated",
  "PostprocessPlanUpdated",
  "CandidatePackCreated",
  "TimelineVersionCreated",
  "TimelineVersionRestored",
  "TimelineValidated",
  "TimelineValidationFailed",
  "PreviewRendered",
  "PreviewViewed",
  "ExportCompleted",
  "MemoryCandidateExtracted",
  "MemoryCandidateDiscarded",
  "JobEnqueued",
  "JobProgress",
  "JobSucceeded",
  "JobFailed",
  "JobCancelled",
  "TurnEnded",
  "CapabilityDegraded"
];
