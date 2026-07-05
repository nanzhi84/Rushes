import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useParams } from "@tanstack/react-router";
import { useCallback, useEffect, useMemo, useState } from "react";
import type { ReactElement } from "react";
import { api, type DecisionAnswer } from "../api/client";
import { queryKeys } from "../app/query_client";
import { createApiEventSource } from "../auth";

type ChatMessage = {
  id: string;
  role: "user" | "assistant" | "system";
  content: string;
  createdAt: string;
};

type SsePayload = {
  event_id: number;
  event: {
    event: string;
    project_id?: string | null;
    case_id?: string | null;
    requested_by_case_id?: string | null;
  };
};

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

  const messagesQuery = useQuery({
    queryKey: queryKeys.messages(projectId, caseId),
    queryFn: async () => [] as ChatMessage[],
    enabled: false,
    initialData: [] as ChatMessage[]
  });

  const decisionQuery = useQuery({
    queryKey: queryKeys.currentDecision(projectId, caseId),
    queryFn: () => api.currentDecision(projectId, caseId)
  });

  const currentDecision = decisionQuery.data?.decision ?? null;
  const messages = messagesQuery.data ?? [];

  const invalidateCaseQueries = useCallback(
    async (payload: SsePayload) => {
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
      const payload = JSON.parse(message.data) as SsePayload;
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
      const optimistic: ChatMessage = {
        id: `local_${Date.now()}`,
        role: "user",
        content,
        createdAt: new Date().toISOString()
      };
      queryClient.setQueryData<ChatMessage[]>(queryKeys.messages(projectId, caseId), (current) => [
        ...(current ?? []),
        optimistic
      ]);
    },
    onError: () => setAwaitingTurnEnd(false)
  });

  const answerDecision = useMutation({
    mutationFn: (answer: DecisionAnswer) =>
      api.answerDecision(required(currentDecision?.decision_id), {
        project_id: projectId,
        case_id: caseId,
        answer
      }),
    onSuccess: () =>
      queryClient.invalidateQueries({ queryKey: queryKeys.currentDecision(projectId, caseId) })
  });

  const disabled = awaitingTurnEnd || postMessage.isPending;
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

      <div className="grid min-h-0 flex-1 grid-cols-1 gap-4 p-6 xl:grid-cols-[minmax(0,1fr)_320px]">
        <div className="flex min-h-0 flex-col rounded-lg border border-[#d9dee7] bg-white">
          <div className="min-h-0 flex-1 space-y-3 overflow-y-auto p-4" aria-label="消息列表">
            {messages.length === 0 ? (
              <p className="rounded-md border border-dashed border-[#cbd5e1] px-4 py-6 text-sm text-[#64748b]">
                这里会显示当前剪辑任务的消息流。
              </p>
            ) : (
              messages.map((message) => (
                <article key={message.id} className={messageClass(message.role)}>
                  <span className="text-xs font-medium uppercase text-[#64748b]">{roleLabel(message.role)}</span>
                  <p className="mt-1 whitespace-pre-wrap leading-7">{message.content}</p>
                </article>
              ))
            )}
          </div>

          <form
            className="border-t border-[#d9dee7] p-4"
            onSubmit={(event) => {
              event.preventDefault();
              const content = draft.trim();
              if (!content || disabled) {
                return;
              }
              setDraft("");
              postMessage.mutate(content);
            }}
          >
            <label className="block text-sm font-medium text-[#334155]">
              消息输入
              <textarea
                aria-label="消息输入"
                className="mt-2 h-24 w-full resize-none rounded-md border border-[#cbd5e1] px-3 py-2 outline-none focus:border-[#2563eb] disabled:bg-[#f1f5f9]"
                value={draft}
                onChange={(event) => setDraft(event.target.value)}
                disabled={disabled}
                placeholder={disabled ? "等待本轮结束" : "输入给当前剪辑任务的剪辑指令"}
              />
            </label>
            <div className="mt-3 flex items-center justify-between gap-3">
              <p className="text-sm text-[#64748b]">
                {disabled ? "输入框会在本轮结束事件后恢复。" : "消息会进入后端任务队列。"}
              </p>
              <button
                className="rounded-md bg-[#17202a] px-4 py-2 text-sm font-medium text-white disabled:bg-[#94a3b8]"
                type="submit"
                disabled={disabled || draft.trim().length === 0}
              >
                发送
              </button>
            </div>
          </form>
        </div>

        <aside className="space-y-4">
          <section className="rounded-lg border border-[#d9dee7] bg-white p-4">
            <h2 className="font-semibold">当前确认项</h2>
            {currentDecision ? (
              <div className="mt-3 space-y-3">
                <p className="text-sm leading-6 text-[#475569]">{currentDecision.question}</p>
                {currentDecision.options.map((option) => (
                  <button
                    key={option.option_id}
                    className="block w-full rounded-md border border-[#cbd5e1] px-3 py-2 text-left text-sm hover:bg-[#f1f5f9]"
                    type="button"
                    disabled={answerDecision.isPending}
                    onClick={() =>
                      answerDecision.mutate({
                        option_id: option.option_id,
                        answered_via: "button",
                        payload: (option.payload as Record<string, unknown> | undefined) ?? {}
                      })
                    }
                  >
                    {option.label}
                  </button>
                ))}
              </div>
            ) : (
              <p className="mt-3 text-sm text-[#64748b]">暂无待确认项。</p>
            )}
          </section>

          <section className="rounded-lg border border-[#d9dee7] bg-white p-4">
            <h2 className="font-semibold">预览与时间线</h2>
            <p className="mt-3 text-sm leading-6 text-[#64748b]">
              预览播放器、只读时间线和结构化交互卡片会在后续里程碑接入。
            </p>
          </section>
        </aside>
      </div>
    </section>
  );
}

function messageClass(role: ChatMessage["role"]): string {
  if (role === "user") {
    return "ml-auto max-w-[80%] rounded-lg bg-[#17202a] px-4 py-3 text-white";
  }
  if (role === "assistant") {
    return "mr-auto max-w-[80%] rounded-lg bg-[#eef2f7] px-4 py-3 text-[#17202a]";
  }
  return "mx-auto max-w-[80%] rounded-lg bg-[#fff7ed] px-4 py-3 text-[#7c2d12]";
}

function roleLabel(role: ChatMessage["role"]): string {
  if (role === "user") {
    return "用户";
  }
  if (role === "assistant") {
    return "助手";
  }
  return "系统";
}

function required(value: string | undefined): string {
  if (!value) {
    throw new Error("缺少确认项 ID");
  }
  return value;
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
