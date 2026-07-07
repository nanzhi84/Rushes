import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useParams } from "@tanstack/react-router";
import { useCallback, useEffect, useMemo, useState } from "react";
import type { ReactElement } from "react";
import {
  api,
  type CaseMessage,
  type DecisionAnswer,
  type TimelineClipJson,
  type TimelineJson
} from "../api/client";
import { queryKeys } from "../app/query_client";
import { createApiEventSource } from "../auth";
import { AssistantThread } from "../components/Console/AssistantThread";
import { useTurnStream } from "../components/Console/useTurnStream";
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
import {
  useConsoleExternalStoreRuntime,
  type ConsoleMessage,
  type ConsoleMessageRole
} from "../components/Console/runtime";
import { PreviewPlayer } from "../components/PreviewPlayer";
import { TimelineViewer } from "../components/TimelineViewer";

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
  const [selectedClipId, setSelectedClipId] = useState<string | null>(null);
  const [unmatchedClipId, setUnmatchedClipId] = useState<string | null>(null);
  const [highlightedMessageId, setHighlightedMessageId] = useState<string | null>(null);
  const [playheadSec, setPlayheadSec] = useState<number | null>(null);
  const [seekSec, setSeekSec] = useState<number | null>(null);
  const [pxPerSec, setPxPerSec] = useState(DEFAULT_TIMELINE_PX_PER_SEC);

  const messagesQuery = useQuery({
    queryKey: queryKeys.messages(projectId, caseId),
    queryFn: async () => {
      const response = await api.getCaseMessages(projectId, caseId);
      return response.messages.map(toConsoleMessage);
    },
    initialData: [] as ConsoleMessage[]
  });

  const decisionQuery = useQuery({
    queryKey: queryKeys.currentDecision(projectId, caseId),
    queryFn: () => api.currentDecision(projectId, caseId)
  });

  const caseQuery = useQuery({
    queryKey: queryKeys.case(projectId, caseId),
    queryFn: () => api.getCase(projectId, caseId)
  });

  const currentCase = caseQuery.data?.case ?? null;
  const timelineVersion = currentCase?.timeline_current_version ?? null;

  const timelineQuery = useQuery({
    queryKey: queryKeys.timeline(projectId, caseId, timelineVersion),
    queryFn: () => api.fetchCaseTimeline(projectId, caseId, timelineVersion),
    enabled: timelineVersion !== null
  });

  const currentDecision = decisionQuery.data?.decision ?? null;
  const historyMessages = messagesQuery.data ?? [];
  const timelinePayload = timelineQuery.data ?? null;
  const previewSrc = timelinePayload?.preview_id ? api.mediaPreviewUrl(timelinePayload.preview_id) : null;
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
        queryClient.invalidateQueries({ queryKey: ["timeline", projectId, caseId] }),
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

  // turn-stream 订阅置于领域 /events 订阅之后，保证 /events 是首个 EventSource。
  const refreshMessages = useCallback(() => {
    void queryClient.invalidateQueries({ queryKey: queryKeys.messages(projectId, caseId) });
  }, [caseId, projectId, queryClient]);
  const { inProgressMessages, toolSteps } = useTurnStream(projectId, caseId, {
    onTurnEnded: refreshMessages
  });

  // 历史消息为准，流式 buffer 按 message_id 去重后追加，避免落库后与历史重复。
  const messages = useMemo<ConsoleMessage[]>(() => {
    const historyIds = new Set(historyMessages.map((message) => message.id));
    const streaming: ConsoleMessage[] = inProgressMessages
      .filter((message) => !historyIds.has(message.message_id))
      .map((message) => ({
        id: message.message_id,
        role: "assistant",
        kind: message.kind,
        content: message.text,
        createdAt: ""
      }));
    return [...historyMessages, ...streaming];
  }, [historyMessages, inProgressMessages]);

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
  const handlePreviewFirstPlay = useCallback(() => {
    const previewId = timelinePayload?.preview_id;
    if (!previewId) {
      return;
    }
    void api
      .postPreviewViewed(projectId, caseId, previewId)
      .then(() => queryClient.invalidateQueries({ queryKey: queryKeys.case(projectId, caseId) }))
      .catch(() => undefined);
  }, [caseId, projectId, queryClient, timelinePayload?.preview_id]);
  const handlePreviewTimeUpdate = useCallback((sec: number) => {
    setPlayheadSec(sec);
  }, []);
  const handleTimelineSeek = useCallback((sec: number) => {
    setSeekSec(sec);
    setPlayheadSec(sec);
  }, []);
  const zoomOutTimeline = useCallback(() => {
    setPxPerSec((current) => {
      const currentIndex = TIMELINE_ZOOM_LEVELS.indexOf(current);
      return TIMELINE_ZOOM_LEVELS[Math.max(0, currentIndex - 1)] ?? current;
    });
  }, []);
  const zoomInTimeline = useCallback(() => {
    setPxPerSec((current) => {
      const currentIndex = TIMELINE_ZOOM_LEVELS.indexOf(current);
      const nextIndex = currentIndex === -1 ? 0 : Math.min(TIMELINE_ZOOM_LEVELS.length - 1, currentIndex + 1);
      return TIMELINE_ZOOM_LEVELS[nextIndex] ?? current;
    });
  }, []);
  const handleClipClick = useCallback(
    (clipId: string) => {
      setSelectedClipId(clipId);
      const messageMatch = messages.find((message) => message.content.includes(clipId));
      const structuredMatch = renderedStructuredItems.some((item) => JSON.stringify(item).includes(clipId));
      const targetId = messageMatch?.id ?? (structuredMatch ? "structured-interactions" : null);
      setHighlightedMessageId(targetId);
      setUnmatchedClipId(targetId ? null : clipId);
      if (targetId) {
        window.requestAnimationFrame(() => scrollToMessage(targetId));
      }
    },
    [messages, renderedStructuredItems]
  );

  useEffect(() => {
    setPlayheadSec(null);
    setSeekSec(null);
  }, [timelinePayload?.preview_id]);

  const unmatchedClipDetail = useMemo(
    () =>
      unmatchedClipId && timelinePayload
        ? findTimelineClip(timelinePayload.timeline, unmatchedClipId)
        : null,
    [timelinePayload, unmatchedClipId]
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

      <div className="flex min-h-0 flex-1 flex-col gap-4 p-6">
        <div className="grid min-h-0 flex-1 grid-cols-1 gap-4 xl:grid-cols-[minmax(0,1fr)_360px]">
          <div className="flex min-h-[420px] flex-col rounded-lg border border-[#d9dee7] bg-white xl:min-h-0">
            <AssistantThread
              runtime={runtime}
              onAnswerDecision={handleAnswerDecision}
              answerPending={answerDecision.isPending}
              highlightedMessageId={highlightedMessageId}
              toolSteps={toolSteps}
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
              <h2 className="font-semibold">预览</h2>
              <div className="mt-3">
                {timelineVersion === null ? (
                  <p className="text-sm leading-6 text-[#64748b]">暂无时间线。</p>
                ) : timelineQuery.isPending ? (
                  <p className="text-sm leading-6 text-[#64748b]">时间线加载中。</p>
                ) : timelinePayload && previewSrc ? (
                  <PreviewPlayer
                    key={timelinePayload.preview_id}
                    src={previewSrc}
                    fps={timelinePayload.timeline.fps}
                    onFirstPlay={handlePreviewFirstPlay}
                    onTimeUpdate={handlePreviewTimeUpdate}
                    seekSec={seekSec}
                  />
                ) : (
                  <p className="text-sm leading-6 text-[#64748b]">时间线暂不可用。</p>
                )}
              </div>
            </section>

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
          </aside>
        </div>

        <section className="rounded-lg border border-[#d9dee7] bg-white p-4">
          <div className="flex flex-wrap items-center justify-between gap-3 border-b border-[#d9dee7] pb-3">
            <div>
              <h2 className="font-semibold">时间线</h2>
              {timelinePayload ? (
                <p className="mt-1 text-xs text-[#64748b]">
                  版本 {timelinePayload.timeline_version}
                  {timelinePayload.summary ? ` | ${timelinePayload.summary}` : ""}
                </p>
              ) : null}
            </div>
            {timelinePayload ? (
              <div className="flex items-center gap-2">
                <span className="text-xs text-[#64748b]">缩放 {pxPerSec} px/s</span>
                <button
                  type="button"
                  className="grid h-8 w-8 place-items-center rounded-md border border-[#cbd5e1] text-sm font-semibold text-[#334155] hover:bg-[#f1f5f9] disabled:text-[#94a3b8]"
                  aria-label="缩小时间线"
                  onClick={zoomOutTimeline}
                  disabled={pxPerSec === TIMELINE_ZOOM_LEVELS[0]}
                >
                  -
                </button>
                <button
                  type="button"
                  className="grid h-8 w-8 place-items-center rounded-md border border-[#cbd5e1] text-sm font-semibold text-[#334155] hover:bg-[#f1f5f9] disabled:text-[#94a3b8]"
                  aria-label="放大时间线"
                  onClick={zoomInTimeline}
                  disabled={pxPerSec === TIMELINE_ZOOM_LEVELS[TIMELINE_ZOOM_LEVELS.length - 1]}
                >
                  +
                </button>
              </div>
            ) : null}
          </div>
          <div className="mt-3 space-y-3">
            {timelineVersion === null ? (
              <p className="text-sm leading-6 text-[#64748b]">暂无时间线。</p>
            ) : timelineQuery.isPending ? (
              <p className="text-sm leading-6 text-[#64748b]">时间线加载中。</p>
            ) : timelinePayload ? (
              <>
                <TimelineViewer
                  timeline={timelinePayload.timeline}
                  pxPerSec={pxPerSec}
                  playheadSec={playheadSec}
                  selectedClipId={selectedClipId}
                  onClipClick={handleClipClick}
                  onSeek={handleTimelineSeek}
                  waveformSrc={previewSrc}
                />
                {unmatchedClipDetail ? <ClipDetailBar detail={unmatchedClipDetail} /> : null}
              </>
            ) : (
              <p className="text-sm leading-6 text-[#64748b]">时间线暂不可用。</p>
            )}
          </div>
        </section>
      </div>
    </section>
  );
}

type ClipDetail = {
  clipId: string;
  trackId: string;
  startSec: number;
  endSec: number;
  label: string | null;
};

function ClipDetailBar({ detail }: { detail: ClipDetail }): ReactElement {
  return (
    <div className="rounded-md border border-[#d9dee7] bg-[#f8fafc] px-3 py-2 text-xs leading-5 text-[#475569]">
      <span className="font-semibold text-[#17202a]">已选片段：</span>
      <span className="font-mono">{detail.clipId}</span>
      <span className="mx-2 text-[#94a3b8]">|</span>
      <span>轨道 {detail.trackId}</span>
      <span className="mx-2 text-[#94a3b8]">|</span>
      <span>
        {formatSeconds(detail.startSec)} - {formatSeconds(detail.endSec)}
      </span>
      {detail.label ? (
        <>
          <span className="mx-2 text-[#94a3b8]">|</span>
          <span>{detail.label}</span>
        </>
      ) : null}
    </div>
  );
}

function findTimelineClip(timeline: TimelineJson, clipId: string): ClipDetail | null {
  const fps = timeline.fps > 0 ? timeline.fps : 30;
  for (const track of timeline.tracks) {
    for (const clip of track.clips ?? []) {
      if (
        clip.timeline_clip_id !== clipId ||
        typeof clip.timeline_start_frame !== "number" ||
        typeof clip.timeline_end_frame !== "number"
      ) {
        continue;
      }
      return {
        clipId,
        trackId: track.track_id,
        startSec: clip.timeline_start_frame / fps,
        endSec: clip.timeline_end_frame / fps,
        label: clipLabel(clip)
      };
    }
  }
  return null;
}

function clipLabel(clip: TimelineClipJson): string | null {
  if (typeof clip.text === "string" && clip.text.trim()) {
    return clip.text;
  }
  if (typeof clip.asset_id === "string" && clip.asset_id.trim()) {
    return clip.asset_id;
  }
  return null;
}

function toConsoleMessage(message: CaseMessage): ConsoleMessage {
  return {
    id: message.message_id,
    role: normalizeConsoleRole(message.role),
    kind: message.kind,
    content: message.content,
    createdAt: message.created_at
  };
}

function normalizeConsoleRole(role: string): ConsoleMessageRole {
  if (
    role === "user" ||
    role === "assistant" ||
    role === "system" ||
    role === "system_observation"
  ) {
    return role;
  }
  return "system";
}

function scrollToMessage(messageId: string): void {
  const selector = `[data-console-message-id="${escapeAttributeValue(messageId)}"]`;
  document.querySelector(selector)?.scrollIntoView({ block: "center", behavior: "smooth" });
}

function escapeAttributeValue(value: string): string {
  return value.replace(/\\/g, "\\\\").replace(/"/g, '\\"');
}

function formatSeconds(value: number): string {
  return `${value.toFixed(2)}s`;
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

const TIMELINE_ZOOM_LEVELS = [24, 48, 96, 192];
const DEFAULT_TIMELINE_PX_PER_SEC = 96;
