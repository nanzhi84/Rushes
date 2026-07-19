import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  forwardRef,
  startTransition,
  useCallback,
  useEffect,
  useImperativeHandle,
  useMemo,
  useRef,
  useState
} from "react";
import type { ReactElement } from "react";
import { ArrowUp, MessageSquareX, Square } from "lucide-react";
import {
  api,
  type AffectedMemory,
  type DecisionAnswer,
  type MessageRecord
} from "../../api/client";
import { DRAFT_EVENT_TYPES } from "../../api/event_types";
import { queryKeys } from "../../app/query_client";
import { useDocumentVisibility } from "../../app/use_document_visibility";
import { acquireApiEventSource, ApiError } from "../../auth";
import { AssistantThread, type MemoryRetractionResult } from "./AssistantThread";
import { AffectedMemoriesCard } from "./MemoryCards";
import { WorkspaceSettingsDialog } from "../Shell/WorkspaceSettingsDialog";
import { useTurnStream, type StreamMemoryEntry } from "./useTurnStream";
import {
  markDecisionAnswered,
  mergeCurrentDecisionItem,
  reduceStructuredInteractionItems,
  StructuredInteractionRenderer
} from "./StructuredInteractionRenderer";
import type {
  DomainSsePayload,
  StructuredInteractionItem
} from "./StructuredInteractionRenderer";
import {
  useConsoleExternalStoreRuntime,
  type ConsoleMessage,
  type ConsoleMessageRole
} from "./runtime";
import {
  conversationClearErrorMessage,
  jobCancelErrorMessage,
  resendErrorMessage
} from "./error_messages";

export type ConsoleConnectionState = "connecting" | "open" | "closed";

// 导出按钮位于顶栏（父组件），却要触发一次消息提交；用 ref 暴露 submit，避免把
// postMessage/awaitingTurnEnd 等会话态重新提回父组件。
export type ConsolePanelHandle = {
  submit: (content: string) => void;
};

export type ConsolePanelProps = {
  draftId: string;
  chatPanelWidth: number;
  highlightedMessageId: string | null;
  onConnectionStateChange: (state: ConsoleConnectionState) => void;
  onTurnBusyChange: (busy: boolean) => void;
};

type ConversationHistory = {
  messages: ConsoleMessage[];
  rewoundMessageCount: number;
};

/**
 * 会话左栏：独立持有 streamItems / structuredItems 等 SSE 高频态，让每个 text_delta
 * 只重渲染这一栏，而右侧时间线 / 预览（已 memo）完全不受影响。父组件（DraftEditorView）
 * 只保留工作区，并通过回调接收低频的连接态与回合忙碌态。
 */
export const ConsolePanel = forwardRef<ConsolePanelHandle, ConsolePanelProps>(
  function ConsolePanel(
    { draftId, chatPanelWidth, highlightedMessageId, onConnectionStateChange, onTurnBusyChange },
    ref
  ): ReactElement {
    const queryClient = useQueryClient();
    const [draft, setDraft] = useState("");
    const [awaitingTurnEnd, setAwaitingTurnEnd] = useState(false);
    const [streamState, setStreamState] = useState<ConsoleConnectionState>("connecting");
    const documentVisible = useDocumentVisibility();
    const [structuredItems, setStructuredItems] = useState<StructuredInteractionItem[]>([]);
    const [conversationError, setConversationError] = useState<string | null>(null);
    // 「编辑并重发」回退波及的长期记忆(证据落在被撤回对话里):后端在回退事务内算出并随
    // 响应带回,前端据此渲染「撤回这些记忆」卡片。默认保留,撤回或关闭卡片后清空。
    const [affectedMemories, setAffectedMemories] = useState<AffectedMemory[]>([]);
    const optimisticMessageSequenceRef = useRef(0);
    const draftInvalidationTimerRef = useRef<number | null>(null);

    const messagesQuery = useQuery({
      queryKey: queryKeys.messages(draftId),
      queryFn: async () => {
        const response = await api.getDraftMessages(draftId);
        return {
          messages: response.messages.map(toConsoleMessage),
          rewoundMessageCount: response.rewound_message_count
        };
      },
      initialData: { messages: [] as ConsoleMessage[], rewoundMessageCount: 0 }
    });

    const decisionQuery = useQuery({
      queryKey: queryKeys.currentDecision(draftId),
      queryFn: () => api.currentDecision(draftId)
    });

    const currentDecision = decisionQuery.data?.decision ?? null;
    const historyMessages = messagesQuery.data.messages;
    const rewoundMessageCount = messagesQuery.data.rewoundMessageCount;
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
      () => {
        if (
          !currentDecision ||
          renderedStructuredItems.some(
            (item) => item.kind === "decision" && item.decision_id === currentDecision.decision_id
          )
        ) {
          return null;
        }
        return mergeCurrentDecisionItem([], currentDecision)[0] ?? null;
      },
      [currentDecision, renderedStructuredItems]
    );

    const scheduleDraftQueryInvalidation = useCallback(
      () => {
        if (draftInvalidationTimerRef.current !== null) {
          window.clearTimeout(draftInvalidationTimerRef.current);
        }
        // 首次 SSE 会重放历史领域事件。把同一小段事件突发合并成一次刷新，
        // 避免几十条旧 Timeline 事件反复取消正在加载的最新时间线请求。
        draftInvalidationTimerRef.current = window.setTimeout(() => {
          draftInvalidationTimerRef.current = null;
          void Promise.all([
            queryClient.invalidateQueries({ queryKey: queryKeys.draft(draftId) }),
            queryClient.invalidateQueries({ queryKey: ["timeline", draftId] }),
            queryClient.invalidateQueries({ queryKey: queryKeys.messages(draftId) }),
            queryClient.invalidateQueries({ queryKey: queryKeys.currentDecision(draftId) }),
            queryClient.invalidateQueries({ queryKey: queryKeys.costs(draftId) }),
            queryClient.invalidateQueries({ queryKey: queryKeys.materials(draftId) })
          ]);
        }, 80);
      },
      [draftId, queryClient]
    );

    useEffect(() => {
      if (!documentVisible) {
        setStreamState("connecting");
        return;
      }
      const { source, release } = acquireApiEventSource(`/api/drafts/${draftId}/events`);
      const handleOpen = () => setStreamState("open");
      const handleError = () => setStreamState("closed");
      source.addEventListener("open", handleOpen);
      source.addEventListener("error", handleError);
      const handleEvent = (event: Event) => {
        const message = event as MessageEvent<string>;
        const payload = JSON.parse(message.data) as DomainSsePayload;
        // 结构化交互更新非用户输入驱动，标记为非紧急，让位于流式与滚动等紧急渲染。
        startTransition(() =>
          setStructuredItems((current) => reduceStructuredInteractionItems(current, payload))
        );
        scheduleDraftQueryInvalidation();
      };
      for (const eventName of DRAFT_EVENT_TYPES) {
        source.addEventListener(eventName, handleEvent);
      }
      return () => {
        source.removeEventListener("open", handleOpen);
        source.removeEventListener("error", handleError);
        for (const eventName of DRAFT_EVENT_TYPES) {
          source.removeEventListener(eventName, handleEvent);
        }
        release();
        if (draftInvalidationTimerRef.current !== null) {
          window.clearTimeout(draftInvalidationTimerRef.current);
          draftInvalidationTimerRef.current = null;
        }
      };
    }, [documentVisible, draftId, scheduleDraftQueryInvalidation]);

    // turn-stream 订阅置于领域 /events 订阅之后，保证 /events 是首个 EventSource。
    const refreshMessages = useCallback(() => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.messages(draftId) });
    }, [draftId, queryClient]);
    const finishTurn = useCallback(() => {
      setAwaitingTurnEnd(false);
      refreshMessages();
    }, [refreshMessages]);
    const {
      items: streamItems,
      turnActive,
      modelRetry,
      subagentProgress,
      reset: resetTurnStream
    } = useTurnStream(draftId, {
      onTurnEnded: finishTurn,
      onTurnError: finishTurn,
      onStreamGap: refreshMessages
    });

    // 当前回合以流式列表为准。历史里同 message_id / step_id 的落库副本让位，
    // 回合结束后保留实时顺序，刷新页面后再由持久化消息与工具轨迹接管。
    // 流式文本增量不断换 streamItems 引用，但活跃项 id 集合在增量之间不变。按 id 集合的
    // 稳定签名 memo，避免每个 text_delta 重建 messages —— 否则 useConsoleExternalStoreRuntime
    // 会重建全部消息对象，击穿 AssistantThread 里所有消息行的 memo。
    const liveItemIdsKey = streamItems
      .map((item) => (item.type === "message" ? item.message_id : item.type === "memory" ? item.id : item.step_id))
      .join("\\u0000");
    const messages = useMemo<ConsoleMessage[]>(() => {
      const liveItemIds = new Set(
        streamItems.map((item) => (item.type === "message" ? item.message_id : item.type === "memory" ? item.id : item.step_id))
      );
      return historyMessages.filter((message) => !liveItemIds.has(message.id));
      // liveItemIdsKey 是 streamItems 活跃 id 集合的稳定签名，替代 streamItems 引用依赖。
      // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [historyMessages, liveItemIdsKey]);

    const postMessage = useMutation({
      mutationFn: (content: string) => api.postMessage(draftId, { content }),
      onMutate: async (content) => {
        setAwaitingTurnEnd(true);
        await queryClient.cancelQueries({ queryKey: queryKeys.messages(draftId) });
        optimisticMessageSequenceRef.current += 1;
        const optimistic: ConsoleMessage = {
          id: `local_${Date.now()}_${optimisticMessageSequenceRef.current}`,
          role: "user",
          content,
          createdAt: new Date().toISOString()
        };
        queryClient.setQueryData<ConversationHistory>(queryKeys.messages(draftId), (current) => ({
          messages: [...(current?.messages ?? []), optimistic],
          rewoundMessageCount: current?.rewoundMessageCount ?? 0
        }));
      },
      onError: () => setAwaitingTurnEnd(false)
    });

    const cancelTurn = useMutation({
      mutationFn: () => api.cancelTurn(draftId)
    });

    const cancelJob = useMutation({
      mutationFn: (jobId: string) => api.cancelJob(jobId, "user_cancelled"),
      onMutate: () => setConversationError(null),
      onSuccess: async (_response, jobId) => {
        setStructuredItems((current) =>
          current.map((item) =>
            item.kind === "progress" && item.job_id === jobId
              ? { ...item, status: "cancelled" }
              : item
          )
        );
        await queryClient.invalidateQueries({ queryKey: queryKeys.materials(draftId) });
      },
      onError: async (error) => {
        setConversationError(jobCancelErrorMessage(error));
        await queryClient.invalidateQueries({ queryKey: queryKeys.materials(draftId) });
      }
    });

    // 编辑并重发：回退到该消息之前并以新内容开启新回合。软遮蔽后靠领域 SSE 的
    // query invalidation 让旧消息与旧时间线从 UI 消失，新回合复用现有 turn-stream。
    const resendMessage = useMutation({
      mutationFn: ({ messageId, content }: { messageId: string; content: string }) =>
        api.resendMessage(draftId, messageId, {
          content,
          idempotency_key: crypto.randomUUID()
        }),
      onMutate: () => {
        setConversationError(null);
        setAffectedMemories([]);
        setAwaitingTurnEnd(true);
      },
      onSuccess: async (response) => {
        resetTurnStream();
        setStructuredItems([]);
        setAffectedMemories(response.affected_memories);
        await Promise.all([
          queryClient.invalidateQueries({ queryKey: queryKeys.draft(draftId) }),
          queryClient.invalidateQueries({ queryKey: queryKeys.timeline(draftId) }),
          queryClient.invalidateQueries({ queryKey: queryKeys.messages(draftId) }),
          queryClient.invalidateQueries({ queryKey: queryKeys.currentDecision(draftId) }),
          queryClient.invalidateQueries({ queryKey: queryKeys.materials(draftId) })
        ]);
      },
      onError: (error) => {
        setAwaitingTurnEnd(false);
        setConversationError(resendErrorMessage(error));
      }
    });

    // 撤回被回退对话形成的长期记忆:批量走 Actor=User 删除路径(复用 M3 已有的单键端点)。
    // 已不存在(404)视作已达成目标,只有真失败才报错并保留卡片供重试;成功后清卡并刷新记忆。
    const retractAffectedMemories = useMutation({
      mutationFn: async (keys: string[]) => {
        const outcomes = await Promise.allSettled(keys.map((key) => api.deleteMemory(key)));
        const failure = outcomes.find(
          (outcome) =>
            outcome.status === "rejected" &&
            !(outcome.reason instanceof ApiError && outcome.reason.status === 404)
        );
        if (failure && failure.status === "rejected") {
          throw failure.reason;
        }
      },
      onMutate: () => setConversationError(null),
      onSuccess: async () => {
        setAffectedMemories([]);
        await queryClient.invalidateQueries({ queryKey: ["memories"] });
      },
      onError: () => setConversationError("撤回记忆失败,请稍后重试。")
    });

    // 写入回执卡逐条撤回：复用既有单键删除端点，成功后刷新全局设置面板缓存。
    // 404 表示该键已从其他入口删除，同样视为目标已达成。
    const retractStreamMemory = useMutation({
      mutationFn: async (entry: StreamMemoryEntry): Promise<MemoryRetractionResult> => {
        const response = await api.listMemories();
        const current = response.memories.find((memory) => memory.memory_key === entry.key);
        if (!current) {
          return "retracted";
        }
        // SSE 回执没有新增版本字段（M7 要求 OpenAPI 零变更）。删除前用既有列表端点
        // 核对当前值；同 key 已被后续写入或人工修订时让旧回执失效，绝不误删新值。
        if (
          current.statement !== entry.statement ||
          current.kind !== entry.kind ||
          current.source_draft_id !== draftId
        ) {
          return "stale";
        }
        try {
          await api.deleteMemory(entry.key);
        } catch (error) {
          if (!(error instanceof ApiError && error.status === 404)) {
            throw error;
          }
        }
        return "retracted";
      },
      onSuccess: async () => {
        await queryClient.invalidateQueries({ queryKey: ["memories"] });
      },
      onError: () => setConversationError("撤回记忆失败，请稍后重试。")
    });
    const retractMemoryFromReceipt = useCallback(
      (entry: StreamMemoryEntry) => retractStreamMemory.mutateAsync(entry),
      [retractStreamMemory.mutateAsync]
    );

    const clearConversation = useMutation({
      mutationFn: () => api.clearDraftConversation(draftId),
      onMutate: () => setConversationError(null),
      onSuccess: async () => {
        setDraft("");
        setStructuredItems([]);
        await Promise.all([
          queryClient.invalidateQueries({ queryKey: queryKeys.messages(draftId) }),
          queryClient.invalidateQueries({ queryKey: queryKeys.currentDecision(draftId) }),
          queryClient.invalidateQueries({ queryKey: queryKeys.draft(draftId) })
        ]);
      },
      onError: (error) => setConversationError(conversationClearErrorMessage(error))
    });

    const answerDecision = useMutation({
      mutationFn: ({ decisionId, answer }: { decisionId: string; answer: DecisionAnswer }) =>
        api.answerDecision(decisionId, {
          draft_id: draftId,
          answer
        }),
      onSuccess: async (_data, variables) => {
        setStructuredItems((current) => markDecisionAnswered(current, variables.decisionId, variables.answer));
        await Promise.all([
          queryClient.invalidateQueries({ queryKey: queryKeys.currentDecision(draftId) }),
          queryClient.invalidateQueries({ queryKey: queryKeys.messages(draftId) }),
          queryClient.invalidateQueries({ queryKey: queryKeys.draft(draftId) })
        ]);
      }
    });

    // turnActive 覆盖刷新/断线重连后重放中的回合；不能只依赖本页发消息时设置的 awaitingTurnEnd。
    const turnBusy = awaitingTurnEnd || turnActive;
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
    const handleClearConversation = useCallback(() => {
      if (
        !window.confirm(
          "清空当前对话上下文？素材、素材理解、时间线和预览都会保留，新对话会继承这些客观状态。"
        )
      ) {
        return;
      }
      clearConversation.mutate();
    }, [clearConversation]);
    // 稳定引用：memo 化的消息行只有拿到不变的回调才能在流式高频重渲染中被挡下。
    const cancelJobMutate = cancelJob.mutate;
    const resendMutate = resendMessage.mutate;
    const handleCancelJob = useCallback(
      (jobId: string) => cancelJobMutate(jobId),
      [cancelJobMutate]
    );
    const handleResendMessage = useCallback(
      (messageId: string, content: string) => resendMutate({ messageId, content }),
      [resendMutate]
    );

    const runtime = useConsoleExternalStoreRuntime({
      messages,
      structuredItems: renderedStructuredItems,
      isRunning: turnBusy,
      canSubmit: true,
      submit: submitMessage
    });
    const submitComposer = useCallback(() => {
      const content = draft.trim();
      if (!content) {
        return;
      }
      setDraft("");
      runtime.submit(content);
    }, [draft, runtime]);
    const statusLabel = useMemo(() => {
      if (streamState === "open") {
        return "事件流已连接";
      }
      if (streamState === "closed") {
        return "事件流重连中";
      }
      return "事件流连接中";
    }, [streamState]);

    // 连接态与回合忙碌态是低频信号（连接开合、回合起止），提回父组件供顶栏连接指示与
    // 导出按钮禁用态使用；不会在每个 text_delta 上触发父组件重渲染。
    useEffect(() => {
      onConnectionStateChange(streamState);
    }, [onConnectionStateChange, streamState]);
    useEffect(() => {
      onTurnBusyChange(turnBusy);
    }, [onTurnBusyChange, turnBusy]);
    useImperativeHandle(ref, () => ({ submit: submitMessage }), [submitMessage]);

    const [memorySettingsOpen, setMemorySettingsOpen] = useState(false);
    const openMemorySettings = useCallback(() => setMemorySettingsOpen(true), []);
    const retractMemoriesMutate = retractAffectedMemories.mutate;
    const handleRetractAffectedMemories = useCallback(
      () => retractMemoriesMutate(affectedMemories.map((memory) => memory.key)),
      [retractMemoriesMutate, affectedMemories]
    );
    const handleDismissAffectedMemories = useCallback(() => setAffectedMemories([]), []);

    return (
      <aside
        className="flex min-h-0 shrink-0 flex-col bg-panel"
        style={{ width: chatPanelWidth }}
        aria-label="剪辑对话"
      >
        <div className="flex h-8 shrink-0 items-center justify-between border-b border-line px-3">
          <span className="text-xs font-semibold tracking-wide">AI 剪辑</span>
          <div className="flex items-center gap-2">
            <button
              type="button"
              className="inline-flex h-6 items-center gap-1 rounded-sm px-1.5 text-2xs text-fg-faint hover:bg-hover hover:text-fg disabled:opacity-35"
              aria-label="清空对话上下文"
              title="清空对话；保留素材与时间线"
              disabled={turnBusy || clearConversation.isPending}
              onClick={handleClearConversation}
            >
              <MessageSquareX size={12} strokeWidth={1.7} aria-hidden />
              清空
            </button>
            <span className="inline-flex items-center gap-1.5 text-2xs text-fg-faint">
              <span
                aria-hidden
                className={`size-1.5 rounded-full ${
                  streamState === "open"
                    ? "bg-ok"
                    : streamState === "closed"
                      ? "bg-danger"
                      : "bg-warn"
                }`}
              />
              <span className="sr-only">{statusLabel}</span>
              {streamState === "open" ? "在线" : "连接中"}
            </span>
          </div>
        </div>

        {conversationError ? (
          <div className="shrink-0 border-b border-danger/30 bg-danger/8 px-3 py-1 text-2xs text-danger" role="alert">
            {conversationError}
          </div>
        ) : null}

        {rewoundMessageCount > 0 ? (
          <div
            className="shrink-0 border-b border-line bg-panel px-3 py-1 text-center text-2xs text-fg-faint"
            role="status"
          >
            已回退并折叠 {rewoundMessageCount} 条历史消息
          </div>
        ) : null}

        {affectedMemories.length > 0 ? (
          <div className="shrink-0 border-b border-line px-3 py-2">
            <AffectedMemoriesCard
              memories={affectedMemories}
              onRetract={handleRetractAffectedMemories}
              onDismiss={handleDismissAffectedMemories}
              retracting={retractAffectedMemories.isPending}
            />
          </div>
        ) : null}

        <AssistantThread
          runtime={runtime}
          onAnswerDecision={handleAnswerDecision}
          answerPending={answerDecision.isPending}
          highlightedMessageId={highlightedMessageId}
          streamItems={streamItems}
          modelRetry={modelRetry}
          subagentProgress={subagentProgress}
          onCancelJob={handleCancelJob}
          cancelPendingJobId={cancelJob.isPending ? (cancelJob.variables ?? null) : null}
          onResendMessage={handleResendMessage}
          resendPendingMessageId={
            resendMessage.isPending ? (resendMessage.variables?.messageId ?? null) : null
          }
          onOpenMemorySettings={openMemorySettings}
          onRetractMemory={retractMemoryFromReceipt}
        />

        <WorkspaceSettingsDialog
          open={memorySettingsOpen}
          onClose={() => setMemorySettingsOpen(false)}
        />

        {sideDecisionItem ? (
          <div className="shrink-0 border-t border-line p-2.5" aria-label="当前确认项">
            <StructuredInteractionRenderer
              item={sideDecisionItem}
              onAnswerDecision={handleAnswerDecision}
              answerPending={answerDecision.isPending}
            />
          </div>
        ) : null}

        <form
          className="shrink-0 border-t border-line p-2.5"
          onSubmit={(event) => {
            event.preventDefault();
            submitComposer();
          }}
        >
          <div className="overflow-hidden rounded-md border border-line-strong bg-raised focus-within:border-accent">
            <textarea
              aria-label="消息输入"
              className="h-16 w-full resize-none bg-transparent px-3 pt-2.5 text-[13px] leading-5 text-fg outline-none placeholder:text-fg-faint"
              value={draft}
              onChange={(event) => setDraft(event.target.value)}
              onKeyDown={(event) => {
                if (
                  event.key === "Enter" &&
                  !event.shiftKey &&
                  !event.nativeEvent.isComposing
                ) {
                  event.preventDefault();
                  submitComposer();
                }
              }}
              placeholder={
                runtime.isRunning
                  ? "描述下一步；发送后会排到当前任务之后…"
                  : "描述你想怎样剪辑…"
              }
            />
            <div className="flex items-center justify-between gap-3 border-t border-line px-2 py-1.5">
              <div className="min-w-0 text-2xs text-fg-faint">
                {runtime.isRunning ? (
                  <span className="block truncate text-accent" role="status" aria-live="polite">
                    新消息将按发送顺序排队
                  </span>
                ) : null}
                <span className="block">
                  <kbd className="font-mono">Enter</kbd> 发送　
                  <kbd className="font-mono">Shift+Enter</kbd> 换行
                </span>
              </div>
              <div className="flex shrink-0 items-center gap-1.5">
                {runtime.isRunning ? (
                  <button
                    className="flex size-7 items-center justify-center rounded-md border border-line-strong bg-panel text-fg-muted transition-[transform,background-color] duration-fast hover:bg-hover hover:text-fg active:translate-y-px disabled:opacity-40"
                    type="button"
                    aria-label="停止当前任务"
                    disabled={cancelTurn.isPending}
                    onClick={() => cancelTurn.mutate()}
                  >
                    <Square size={11} fill="currentColor" strokeWidth={1.5} aria-hidden />
                  </button>
                ) : null}
                <button
                  className="flex size-7 items-center justify-center rounded-md bg-accent text-white transition-[transform,background-color] duration-fast hover:bg-accent-strong active:translate-y-px disabled:opacity-40"
                  type="submit"
                  aria-label="发送消息"
                  disabled={!runtime.canSubmit || draft.trim().length === 0}
                >
                  <ArrowUp size={15} strokeWidth={2} aria-hidden />
                  <span className="sr-only">发送</span>
                </button>
              </div>
            </div>
          </div>
        </form>
      </aside>
    );
  }
);

export function toConsoleMessage(message: MessageRecord): ConsoleMessage {
  return {
    id: message.message_id,
    role: normalizeConsoleRole(message.role),
    kind: message.kind,
    content: message.content,
    createdAt: message.created_at
  };
}

export function normalizeConsoleRole(role: string): ConsoleMessageRole {
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
