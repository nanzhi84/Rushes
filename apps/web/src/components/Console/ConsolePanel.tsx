import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  forwardRef,
  useCallback,
  useEffect,
  useImperativeHandle,
  useMemo,
  useRef,
  useState
} from "react";
import type { ReactElement } from "react";
import { ArrowUp, History, MessageSquareX, Square } from "lucide-react";
import {
  api,
  type DecisionAnswer,
  type MessageRecord,
  type RewindRestoreRequest
} from "../../api/client";
import { DRAFT_EVENT_TYPES } from "../../api/event_types";
import { queryKeys } from "../../app/query_client";
import { useDocumentVisibility } from "../../app/use_document_visibility";
import { acquireApiEventSource } from "../../auth";
import { AssistantThread } from "./AssistantThread";
import { RewindPanel } from "./RewindPanel";
import { useTurnStream } from "./useTurnStream";
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
  rewindErrorMessage
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
    const [rewindOpen, setRewindOpen] = useState(false);
    const [selectedRewindCheckpointId, setSelectedRewindCheckpointId] = useState<string | null>(null);
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

    const rewindQuery = useQuery({
      queryKey: queryKeys.rewindCheckpoints(draftId),
      queryFn: () => api.rewindCheckpoints(draftId)
    });

    const decisionQuery = useQuery({
      queryKey: queryKeys.currentDecision(draftId),
      queryFn: () => api.currentDecision(draftId)
    });

    const currentDecision = decisionQuery.data?.decision ?? null;
    const historyMessages = messagesQuery.data.messages;
    const rewoundMessageCount = messagesQuery.data.rewoundMessageCount;
    const rewindCheckpoints = rewindQuery.data?.checkpoints ?? [];
    const rewindCheckpointByItem = useMemo(() => {
      const result: Record<string, string> = {};
      for (const checkpoint of rewindCheckpoints) {
        const messageEntry = checkpoint.trigger_kind === "user_message";
        const attachedToolEntry =
          checkpoint.trigger_kind === "timeline_write" &&
          checkpoint.anchor_message_id !== checkpoint.anchor_turn_id;
        if (
          checkpoint.anchor_message_id &&
          (messageEntry || attachedToolEntry) &&
          result[checkpoint.anchor_message_id] === undefined
        ) {
          result[checkpoint.anchor_message_id] = checkpoint.checkpoint_id;
        }
      }
      return result;
    }, [rewindCheckpoints]);
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
            queryClient.invalidateQueries({ queryKey: queryKeys.rewindCheckpoints(draftId) }),
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
        setStructuredItems((current) => reduceStructuredInteractionItems(current, payload));
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
    const messages = useMemo<ConsoleMessage[]>(() => {
      const liveItemIds = new Set(
        streamItems.map((item) => (item.type === "message" ? item.message_id : item.step_id))
      );
      return historyMessages.filter((message) => !liveItemIds.has(message.id));
    }, [historyMessages, streamItems]);

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

    const restoreRewind = useMutation({
      mutationFn: ({ checkpointId, mode, idempotencyKey }: {
        checkpointId: string;
        mode: RewindRestoreRequest["mode"];
        idempotencyKey: string;
      }) => api.restoreRewindCheckpoint(draftId, {
        checkpoint_id: checkpointId,
        idempotency_key: idempotencyKey,
        mode
      }),
      onMutate: () => setConversationError(null),
      onSuccess: async () => {
        resetTurnStream();
        setAwaitingTurnEnd(false);
        setStructuredItems([]);
        await Promise.all([
          queryClient.invalidateQueries({ queryKey: queryKeys.draft(draftId) }),
          queryClient.invalidateQueries({ queryKey: queryKeys.timeline(draftId) }),
          queryClient.invalidateQueries({ queryKey: queryKeys.messages(draftId) }),
          queryClient.invalidateQueries({ queryKey: queryKeys.currentDecision(draftId) }),
          queryClient.invalidateQueries({ queryKey: queryKeys.rewindCheckpoints(draftId) }),
          queryClient.invalidateQueries({ queryKey: queryKeys.materials(draftId) })
        ]);
      },
      onError: (error) => setConversationError(rewindErrorMessage(error))
    });

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
              className="inline-flex h-6 items-center gap-1 rounded-sm px-1.5 text-2xs text-fg-faint hover:bg-hover hover:text-fg"
              aria-label="打开回退检查点"
              aria-expanded={rewindOpen}
              onClick={() => setRewindOpen((current) => !current)}
            >
              <History size={12} strokeWidth={1.7} aria-hidden />
              回退
            </button>
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

        {rewindOpen ? (
          <RewindPanel
            checkpoints={rewindCheckpoints}
            selectedCheckpointId={selectedRewindCheckpointId}
            loading={rewindQuery.isLoading}
            pending={restoreRewind.isPending}
            onSelect={setSelectedRewindCheckpointId}
            onRestore={(mode) => {
              if (selectedRewindCheckpointId) {
                restoreRewind.mutate({
                  checkpointId: selectedRewindCheckpointId,
                  idempotencyKey: crypto.randomUUID(),
                  mode
                });
              }
            }}
            onClose={() => setRewindOpen(false)}
          />
        ) : null}

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

        <AssistantThread
          runtime={runtime}
          onAnswerDecision={handleAnswerDecision}
          answerPending={answerDecision.isPending}
          highlightedMessageId={highlightedMessageId}
          streamItems={streamItems}
          modelRetry={modelRetry}
          subagentProgress={subagentProgress}
          onCancelJob={(jobId) => cancelJob.mutate(jobId)}
          cancelPendingJobId={cancelJob.isPending ? (cancelJob.variables ?? null) : null}
          rewindCheckpointByItem={rewindCheckpointByItem}
          onOpenRewind={(checkpointId) => {
            setSelectedRewindCheckpointId(checkpointId);
            setRewindOpen(true);
          }}
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
