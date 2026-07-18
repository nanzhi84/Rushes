import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useParams } from "@tanstack/react-router";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { ReactElement } from "react";
import {
  ArrowUp,
  Captions,
  Crop,
  History,
  Home,
  Link2,
  ListPlus,
  Magnet,
  MessageSquareX,
  MousePointer2,
  Pencil,
  Replace,
  Scissors,
  Square,
  Trash2,
  Unlink2,
  X,
  ZoomIn,
  ZoomOut
} from "lucide-react";
import type { LucideIcon } from "lucide-react";
import {
  api,
  type DecisionAnswer,
  type MaterialAsset,
  type MessageRecord,
  type RewindRestoreRequest,
  type TimelineClipJson,
  type TimelineJson
} from "../api/client";
import { DRAFT_EVENT_TYPES } from "../api/event_types";
import { queryKeys } from "../app/query_client";
import { useDocumentVisibility } from "../app/use_document_visibility";
import { acquireApiEventSource, ApiError } from "../auth";
import { AssistantThread } from "../components/Console/AssistantThread";
import { RewindPanel } from "../components/Console/RewindPanel";
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
import { AssetMediaPreview } from "../components/Materials/AssetMediaPreview";
import { AssetsPanel } from "../components/Materials/AssetsPanel";
import { DiffusionPreviewPlayer } from "../components/PreviewPlayer";
import { EntityActionDialog } from "../components/Shell/EntityActionDialog";
import { ResizeHandle } from "../components/Shell/ResizeHandle";
import { TopBar } from "../components/Shell/TopBar";
import { TimelineViewer } from "../components/TimelineViewer";
import type {
  TimelineDropMode,
  TimelineEditMode,
  TimelineViewerHandle,
  TimelineTrackStatePatch
} from "../components/TimelineViewer";
import { useUiStore } from "../state/ui_store";
import {
  EditorSession,
  type EditorSessionSnapshot,
  type TimelineOperation
} from "../editor/editor_session";

export function DraftEditorPage(): ReactElement {
  const { draftId } = useParams({ from: "/drafts/$draftId" });
  return <DraftEditorView draftId={draftId} />;
}

type ConversationHistory = {
  messages: ConsoleMessage[];
  rewoundMessageCount: number;
};

export function DraftEditorView({ draftId }: { draftId: string }): ReactElement {
  const queryClient = useQueryClient();
  const {
    entityDialog,
    openEntityDialog,
    closeEntityDialog,
    chatPanelWidth,
    setChatPanelWidth,
    materialsPanelWidth,
    setMaterialsPanelWidth,
    timelinePanelHeight,
    setTimelinePanelHeight
  } = useUiStore();
  const [draft, setDraft] = useState("");
  const [awaitingTurnEnd, setAwaitingTurnEnd] = useState(false);
  const [streamState, setStreamState] = useState<"connecting" | "open" | "closed">("connecting");
  const documentVisible = useDocumentVisibility();
  const [structuredItems, setStructuredItems] = useState<StructuredInteractionItem[]>([]);
  const [previewingAssetId, setPreviewingAssetId] = useState<string | null>(null);
  const [selectedClipId, setSelectedClipId] = useState<string | null>(null);
  const [highlightedMessageId, setHighlightedMessageId] = useState<string | null>(null);
  const [playheadSec, setPlayheadSec] = useState<number | null>(null);
  const [seekSec, setSeekSec] = useState<number | null>(null);
  const [pxPerSec, setPxPerSec] = useState(DEFAULT_TIMELINE_PX_PER_SEC);
  const [editMode, setEditMode] = useState<TimelineEditMode>("select");
  const [dropMode, setDropMode] = useState<TimelineDropMode>("insert");
  const [snapEnabled, setSnapEnabled] = useState(true);
  const [timelineEditError, setTimelineEditError] = useState<string | null>(null);
  const [conversationError, setConversationError] = useState<string | null>(null);
  const [rewindOpen, setRewindOpen] = useState(false);
  const [selectedRewindCheckpointId, setSelectedRewindCheckpointId] = useState<string | null>(null);
  const optimisticMessageSequenceRef = useRef(0);
  const timelineBodyRef = useRef<HTMLDivElement | null>(null);
  const timelineViewerRef = useRef<TimelineViewerHandle | null>(null);
  const playheadSecRef = useRef<number | null>(null);
  const playheadTimecodeRef = useRef<HTMLSpanElement | null>(null);
  const lastPlayheadCommitRef = useRef({ at: 0, sec: 0 });
  const draftInvalidationTimerRef = useRef<number | null>(null);
  const editorSessionRef = useRef<EditorSession | null>(null);
  const editorSessionDraftRef = useRef<string | null>(null);
  const editorSessionUnsubscribeRef = useRef<(() => void) | null>(null);
  const [editorSnapshot, setEditorSnapshot] = useState<EditorSessionSnapshot | null>(null);

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

  const draftQuery = useQuery({
    queryKey: queryKeys.draft(draftId),
    queryFn: () => api.getDraft(draftId)
  });

  const costsQuery = useQuery({
    queryKey: queryKeys.costs(draftId),
    queryFn: () => api.draftCosts(draftId)
  });
  const totalCost = costsQuery.data?.costs.total_cost_estimate ?? null;

  // 与 AssetsPanel 共享同一条 materials 查询缓存（同 queryKey，react-query 去重），
  // 从最新列表按 id 反查试看素材，保证 proxy_ready 等字段跟随后台任务刷新。
  const materialsQuery = useQuery({
    queryKey: queryKeys.materials(draftId),
    queryFn: () => api.listMaterials(draftId)
  });
  const previewingAsset = previewingAssetId
    ? (materialsQuery.data?.assets.find((asset) => asset.asset_id === previewingAssetId) ?? null)
    : null;

  const currentDraft = draftQuery.data?.draft ?? null;
  const draftName = currentDraft?.name ?? draftId;
  const timelineVersion = currentDraft?.timeline_current_version ?? null;

  const timelineQuery = useQuery({
    queryKey: queryKeys.timeline(draftId),
    queryFn: () => api.fetchDraftTimeline(draftId),
    enabled: timelineVersion !== null
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
  const timelinePayload = timelineQuery.data ?? null;
  const editorTimeline = editorSnapshot?.timeline ?? timelinePayload?.timeline ?? null;
  const previewSrc = timelinePayload?.preview_id
    ? api.mediaPreviewUrl(timelinePayload.preview_id)
    : null;
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
      setHighlightedMessageId(null);
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

  useEffect(() => {
    if (!timelinePayload) {
      return;
    }
    if (editorSessionDraftRef.current !== draftId || !editorSessionRef.current) {
      editorSessionUnsubscribeRef.current?.();
      const session = new EditorSession(timelinePayload.timeline);
      editorSessionRef.current = session;
      editorSessionDraftRef.current = draftId;
      editorSessionUnsubscribeRef.current = session.subscribe(setEditorSnapshot);
    } else {
      editorSessionRef.current.replaceFromServer(timelinePayload.timeline);
    }
  }, [draftId, timelinePayload]);

  useEffect(
    () => () => {
      editorSessionUnsubscribeRef.current?.();
      editorSessionUnsubscribeRef.current = null;
      editorSessionRef.current = null;
      editorSessionDraftRef.current = null;
    },
    []
  );

  const flushEditorSession = useCallback(async (): Promise<void> => {
    const session = editorSessionRef.current;
    if (!session || editorSessionDraftRef.current !== draftId) {
      return;
    }
    const operations = session.beginSave();
    if (operations.length === 0) {
      return;
    }
    try {
      const response = await api.applyTimelinePatch(draftId, {
        op: { kind: "batch", ops: operations }
      });
      session.acceptSaved(response.timeline);
      queryClient.setQueryData(queryKeys.timeline(draftId), response);
      setTimelineEditError(null);
      await queryClient.invalidateQueries({ queryKey: queryKeys.draft(draftId) });
    } catch (error) {
      session.rejectSave(error);
      setTimelineEditError(timelinePatchErrorMessage(error));
    }
  }, [draftId, queryClient]);

  useEffect(() => {
    if (editorSnapshot?.saveState !== "dirty") {
      return;
    }
    const timer = window.setTimeout(() => void flushEditorSession(), 500);
    return () => window.clearTimeout(timer);
  }, [editorSnapshot?.pendingCount, editorSnapshot?.saveState, flushEditorSession]);

  // turnActive 覆盖刷新/断线重连后重放中的回合；不能只依赖本页发消息时设置的 awaitingTurnEnd。
  // 运行状态只限制会破坏当前回合的操作，不再禁用输入。消息接口和 TurnQueue
  // 已经按草稿 FIFO 串行化，因此运行中提交会自然排到当前回合之后。
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
  const handlePreviewFirstPlay = useCallback(() => {
    const previewId = timelinePayload?.preview_id;
    if (!previewId) {
      return;
    }
    void api
      .postPreviewViewed(draftId, previewId)
      .then(() => queryClient.invalidateQueries({ queryKey: queryKeys.draft(draftId) }))
      .catch(() => undefined);
  }, [draftId, queryClient, timelinePayload?.preview_id]);
  const handlePreviewTimeUpdate = useCallback((sec: number) => {
    playheadSecRef.current = sec;
    timelineViewerRef.current?.setPlayheadSec(sec, true);
    if (playheadTimecodeRef.current) {
      playheadTimecodeRef.current.textContent = formatTimecode(sec);
    }
    const now = performance.now();
    const previous = lastPlayheadCommitRef.current;
    // 播放头每帧只走命令式 transform。React 状态仅低频同步给吸附候选等
    // 编辑逻辑，避免整棵工作台以 10fps 重渲染并反过来阻塞解码/绘制。
    if (now - previous.at >= 500 || Math.abs(sec - previous.sec) >= 5) {
      lastPlayheadCommitRef.current = { at: now, sec };
      setPlayheadSec(sec);
    }
  }, []);
  // 单击瓦片试看；再点已选中瓦片取消，回到成片/占位。
  const handlePreviewAsset = useCallback((asset: MaterialAsset) => {
    setPreviewingAssetId((current) => (current === asset.asset_id ? null : asset.asset_id));
  }, []);
  const closeAssetPreview = useCallback(() => setPreviewingAssetId(null), []);
  const handleTimelineSeek = useCallback((sec: number) => {
    playheadSecRef.current = sec;
    setSeekSec(sec);
    setPlayheadSec(sec);
    timelineViewerRef.current?.setPlayheadSec(sec, false);
    if (playheadTimecodeRef.current) {
      playheadTimecodeRef.current.textContent = formatTimecode(sec);
    }
  }, []);
  const zoomOutTimeline = useCallback(() => {
    setPxPerSec((current) => {
      const lower = [...TIMELINE_ZOOM_LEVELS].reverse().find((level) => level < current);
      return lower ?? current;
    });
  }, []);
  const zoomInTimeline = useCallback(() => {
    setPxPerSec((current) => {
      const higher = TIMELINE_ZOOM_LEVELS.find((level) => level > current);
      return higher ?? current;
    });
  }, []);
  const fitTimeline = useCallback(() => {
    const body = timelineBodyRef.current;
    const timeline = timelineQuery.data?.timeline;
    if (!body || !timeline) {
      return;
    }
    const fps = timeline.fps > 0 ? timeline.fps : 30;
    const durationSec = timeline.duration_frames / fps;
    if (durationSec <= 0) {
      return;
    }
    const available = body.clientWidth - TIMELINE_LABEL_WIDTH - 16;
    setPxPerSec(
      Math.min(
        TIMELINE_ZOOM_LEVELS[TIMELINE_ZOOM_LEVELS.length - 1],
        Math.max(TIMELINE_ZOOM_LEVELS[0], Math.floor(available / durationSec))
      )
    );
  }, [timelineQuery.data?.timeline]);
  const handleClipClick = useCallback(
    (clipId: string) => {
      setSelectedClipId(clipId);
      // 人工编辑保持静默：选中时间线片段不滚动、不高亮，也不向 LLM 面板写提示。
      setHighlightedMessageId(null);
    },
    []
  );
  const handleTimelineDeselect = useCallback(() => setSelectedClipId(null), []);
  const applyTimelinePatch = useCallback(
    (op: TimelineOperation) => {
      try {
        editorSessionRef.current?.apply(op);
        setTimelineEditError(null);
      } catch (error) {
        setTimelineEditError(timelinePatchErrorMessage(error));
      }
    },
    []
  );
  const handleSplitClip = useCallback(
    (clipId: string, splitFrame: number) => {
      setSelectedClipId(clipId);
      applyTimelinePatch({ kind: "split_clip", timeline_clip_id: clipId, split_frame: splitFrame });
    },
    [applyTimelinePatch]
  );
  const handleMoveClip = useCallback(
    (
      clipId: string,
      targetTrackId: string,
      targetFrame: number,
      mode: TimelineDropMode
    ) => {
      setSelectedClipId(clipId);
      applyTimelinePatch({
        kind: "move_clip",
        timeline_clip_id: clipId,
        target_track_id: targetTrackId,
        target_frame: targetFrame,
        mode
      });
    },
    [applyTimelinePatch]
  );
  const handleTrimClip = useCallback(
    (clipId: string, edge: "start" | "end", frame: number) => {
      setSelectedClipId(clipId);
      applyTimelinePatch({
        kind: "trim_clip_edge",
        timeline_clip_id: clipId,
        edge,
        timeline_frame: frame
      });
    },
    [applyTimelinePatch]
  );
  const handleTrackStateChange = useCallback(
    (trackId: string, patch: TimelineTrackStatePatch) => {
      applyTimelinePatch({ kind: "set_track_state", track_id: trackId, ...patch });
    },
    [applyTimelinePatch]
  );
  const handleSplitSelected = useCallback(() => {
    if (!selectedClipId || !editorTimeline) {
      return;
    }
    const detail = findTimelineClip(editorTimeline, selectedClipId);
    if (!detail?.assetId) {
      setTimelineEditError("请选择视频或图片片段后再分割。");
      return;
    }
    const fps = editorTimeline.fps > 0 ? editorTimeline.fps : 30;
    const playheadFrame = Math.round((playheadSecRef.current ?? playheadSec ?? -1) * fps);
    const splitAt =
      playheadFrame > detail.startFrame && playheadFrame < detail.endFrame
        ? playheadFrame
        : Math.round((detail.startFrame + detail.endFrame) / 2);
    handleSplitClip(selectedClipId, splitAt);
  }, [editorTimeline, handleSplitClip, playheadSec, selectedClipId]);
  const handleDeleteSelected = useCallback(() => {
    if (!selectedClipId || !editorTimeline) {
      return;
    }
    const detail = findTimelineClip(editorTimeline, selectedClipId);
    if (!detail) {
      setTimelineEditError("找不到当前选中的片段。");
      return;
    }
    applyTimelinePatch({ kind: "delete_clip", timeline_clip_id: selectedClipId });
    setSelectedClipId(null);
  }, [
    applyTimelinePatch,
    selectedClipId,
    editorTimeline
  ]);
  const handleToggleLinked = useCallback(
    (clipId: string, linked: boolean) => {
      applyTimelinePatch({ kind: "set_clip_linked", timeline_clip_id: clipId, linked });
    },
    [applyTimelinePatch]
  );
  const handleClipGainChange = useCallback(
    (clipId: string, gainDb: number) => {
      applyTimelinePatch({ kind: "adjust_gain", timeline_clip_id: clipId, gain_db: gainDb });
    },
    [applyTimelinePatch]
  );
  const handleClipFadeChange = useCallback(
    (clipId: string, fadeInFrames: number, fadeOutFrames: number) => {
      applyTimelinePatch({
        kind: "set_clip_fades",
        timeline_clip_id: clipId,
        fade_in_frames: fadeInFrames,
        fade_out_frames: fadeOutFrames
      });
    },
    [applyTimelinePatch]
  );
  const handleTrackGainChange = useCallback(
    (trackId: string, gainDb: number) => {
      handleTrackStateChange(trackId, { gain_db: gainDb });
    },
    [handleTrackStateChange]
  );
  const handleSubtitleChange = useCallback(
    (clipId: string, text: string) => {
      applyTimelinePatch({ kind: "edit_subtitle_text", timeline_clip_id: clipId, text });
    },
    [applyTimelinePatch]
  );
  const handleAddSubtitle = useCallback(() => {
    const timeline = editorTimeline;
    if (!timeline || timeline.duration_frames < 1) {
      return;
    }
    const fps = timeline.fps > 0 ? timeline.fps : 30;
    const length = Math.min(timeline.duration_frames, fps * 2);
    let startFrame = clampNumber(
      Math.round((playheadSecRef.current ?? playheadSec ?? 0) * fps),
      0,
      Math.max(0, timeline.duration_frames - 1)
    );
    let endFrame = Math.min(timeline.duration_frames, startFrame + length);
    if (endFrame <= startFrame) {
      startFrame = Math.max(0, timeline.duration_frames - length);
      endFrame = timeline.duration_frames;
    }
    const clipId = `subtitle_manual_${Date.now()}`;
    setSelectedClipId(clipId);
    setEditMode("select");
    applyTimelinePatch({
      kind: "insert_subtitle",
      timeline_clip_id: clipId,
      start_frame: startFrame,
      end_frame: endFrame,
      text: "在这里输入字幕"
    });
  }, [applyTimelinePatch, editorTimeline, playheadSec]);
  const handleExport = useCallback(() => {
    postMessage.mutate("请把当前时间线导出为最终 MP4。");
  }, [postMessage]);

  useEffect(() => {
    playheadSecRef.current = null;
    setPlayheadSec(null);
    setSeekSec(null);
    timelineViewerRef.current?.setPlayheadSec(null, false);
    if (playheadTimecodeRef.current) {
      playheadTimecodeRef.current.textContent = formatTimecode(0);
    }
  }, [timelinePayload?.preview_id]);

  useEffect(() => {
    const handleShortcut = (event: KeyboardEvent): void => {
      const target = event.target as HTMLElement | null;
      if (
        target?.isContentEditable ||
        target?.tagName === "INPUT" ||
        target?.tagName === "TEXTAREA" ||
        target?.tagName === "SELECT"
      ) {
        return;
      }
      const key = event.key.toLowerCase();
      if (event.metaKey || event.ctrlKey || event.altKey) {
        return;
      } else if (key === "v") {
        setEditMode("select");
      } else if (key === "n") {
        setEditMode("trim");
      } else if (key === "b") {
        setEditMode("blade");
      } else if ((event.key === "Delete" || event.key === "Backspace") && selectedClipId) {
        event.preventDefault();
        handleDeleteSelected();
      }
    };
    window.addEventListener("keydown", handleShortcut);
    return () => window.removeEventListener("keydown", handleShortcut);
  }, [handleDeleteSelected, selectedClipId]);

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

  const timelineDurationSec = useMemo(() => {
    const timeline = editorTimeline;
    if (!timeline) {
      return 0;
    }
    const fps = timeline.fps > 0 ? timeline.fps : 30;
    return timeline.duration_frames / fps;
  }, [editorTimeline]);
  const selectedClipDetail = useMemo(
    () =>
      selectedClipId && editorTimeline
        ? findTimelineClip(editorTimeline, selectedClipId)
        : null,
    [editorTimeline, selectedClipId]
  );
  const timelineEditingDisabled = editorTimeline === null;

  return (
    <div className="flex h-[100dvh] min-h-0 flex-col bg-ink text-fg">
      <TopBar
        connectionState={streamState}
        showSettings={false}
        leading={
          <>
            <Link
              aria-label="返回草稿"
              className="grid size-7 shrink-0 place-items-center rounded-sm text-fg-muted hover:bg-hover hover:text-fg"
              to="/"
            >
              <Home size={15} strokeWidth={1.75} aria-hidden />
            </Link>
            <span className="truncate text-[13px] font-semibold">{draftName}</span>
            <button
              className="grid size-6 shrink-0 place-items-center rounded-sm text-fg-faint hover:bg-hover hover:text-fg"
              type="button"
              aria-label="重命名草稿"
              onClick={() => openEntityDialog({ kind: "renameDraft", draftId })}
            >
              <Pencil size={13} strokeWidth={1.75} aria-hidden />
            </button>
          </>
        }
        trailing={
          <div className="flex items-center gap-2">
            <span
              className="px-1.5 text-xs tabular-nums text-fg-muted"
              aria-label="本草稿成本小计"
              title="本草稿累计成本估算（人民币）"
            >
              {formatCost(totalCost)}
            </span>
            <button
              className="rounded-sm bg-accent px-3 py-1.5 text-xs font-semibold text-white hover:bg-accent-strong disabled:opacity-40"
              type="button"
              disabled={turnBusy || timelineVersion === null}
              onClick={handleExport}
            >
              导出
            </button>
          </div>
        }
      />

      {/* ChatCut 式工作台：左侧 AI 贯穿全高；右侧素材/预览在上，时间线固定在下。 */}
      <div className="flex min-h-0 flex-1">
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

        <ResizeHandle
          orientation="vertical"
          value={chatPanelWidth}
          onChange={setChatPanelWidth}
          ariaLabel="调整对话面板宽度"
        />

        <main
          data-testid="editor-workspace"
          className="flex min-h-0 min-w-0 flex-1 flex-col"
        >
          <div className="flex min-h-0 min-w-0 flex-1">
            <div
              data-testid="materials-panel"
              className="min-h-0 shrink-0 bg-panel"
              style={{ width: materialsPanelWidth }}
            >
              <AssetsPanel
                draftId={draftId}
                enableEvents={false}
                management
                onPreviewAsset={handlePreviewAsset}
                previewingAssetId={previewingAssetId}
              />
            </div>

            <ResizeHandle
              orientation="vertical"
              value={materialsPanelWidth}
              onChange={setMaterialsPanelWidth}
              ariaLabel="调整素材面板宽度"
            />

            <section className="flex min-h-0 min-w-0 flex-1 flex-col bg-panel" aria-label="预览区">
              <div className="flex h-8 shrink-0 items-center justify-between border-b border-line px-3">
                <span className="text-xs font-semibold tracking-wide">预览</span>
                {timelinePayload?.summary ? (
                  <span className="max-w-[45%] truncate text-2xs text-fg-faint">
                    {timelinePayload.summary}
                  </span>
                ) : null}
              </div>
              <div className="min-h-0 flex-1 bg-panel p-2">
                {previewingAsset ? (
                  <AssetPreviewPane asset={previewingAsset} onClose={closeAssetPreview} />
                ) : timelineVersion === null ? (
                  <PreviewPlaceholder text="暂无时间线。告诉 AI 如何剪辑，或先导入素材。" />
                ) : timelineQuery.isPending ? (
                  <PreviewPlaceholder text="时间线加载中…" />
                ) : editorTimeline ? (
                  <DiffusionPreviewPlayer
                    key={draftId}
                    timeline={editorTimeline}
                    fallbackSrc={previewSrc}
                    onFirstPlay={handlePreviewFirstPlay}
                    onTimeUpdate={handlePreviewTimeUpdate}
                    seekSec={seekSec}
                  />
                ) : (
                  <PreviewPlaceholder text="时间线暂不可用。" />
                )}
              </div>
            </section>
          </div>

          <ResizeHandle
            orientation="horizontal"
            invert
            value={timelinePanelHeight}
            onChange={setTimelinePanelHeight}
            ariaLabel="调整时间线高度"
          />

          <section
            className="flex min-h-0 shrink-0 flex-col bg-panel"
            style={{ height: timelinePanelHeight }}
            aria-label="时间线"
          >
            <div className="flex h-9 shrink-0 items-center gap-1 overflow-x-auto border-b border-line px-2">
              <TimelineToolButton
                icon={MousePointer2}
                label="选择"
                shortcut="V"
                active={editMode === "select"}
                onClick={() => setEditMode("select")}
              />
              <TimelineToolButton
                icon={Crop}
                label="裁剪"
                shortcut="N"
                active={editMode === "trim"}
                onClick={() => setEditMode("trim")}
              />
              <TimelineToolButton
                icon={Scissors}
                label="刀片"
                shortcut="B"
                active={editMode === "blade"}
                onClick={() => setEditMode("blade")}
              />
              <span aria-hidden className="mx-1 h-4 w-px bg-line-strong" />
              <TimelineToolButton
                icon={Scissors}
                label="分割"
                disabled={timelineEditingDisabled || !selectedClipId}
                onClick={handleSplitSelected}
              />
              <TimelineToolButton
                icon={Trash2}
                label="删除"
                disabled={timelineEditingDisabled || !selectedClipId}
                onClick={handleDeleteSelected}
              />
              <TimelineToolButton
                icon={Captions}
                label="添加字幕"
                disabled={timelineEditingDisabled}
                onClick={handleAddSubtitle}
              />
              <TimelineToolButton
                icon={selectedClipDetail?.linked ? Unlink2 : Link2}
                label={selectedClipDetail?.linked ? "取消联动" : "音画联动"}
                active={selectedClipDetail?.linked === true}
                disabled={
                  timelineEditingDisabled ||
                  !selectedClipDetail ||
                  !isLinkableTrack(selectedClipDetail.trackId)
                }
                onClick={() => {
                  if (selectedClipDetail) {
                    handleToggleLinked(selectedClipDetail.clipId, !selectedClipDetail.linked);
                  }
                }}
              />
              <TimelineToolButton
                icon={Magnet}
                label="吸附"
                active={snapEnabled}
                onClick={() => setSnapEnabled((current) => !current)}
              />
              <span aria-hidden className="mx-1 h-4 w-px shrink-0 bg-line-strong" />
              <TimelineToolButton
                icon={ListPlus}
                label="插入"
                active={dropMode === "insert"}
                onClick={() => setDropMode("insert")}
              />
              <TimelineToolButton
                icon={Replace}
                label="覆盖"
                active={dropMode === "overwrite"}
                onClick={() => setDropMode("overwrite")}
              />
              <span className="mx-auto font-mono text-2xs tabular-nums text-fg-muted">
                <span ref={playheadTimecodeRef}>{formatTimecode(playheadSec ?? 0)}</span> / {formatTimecode(timelineDurationSec)}
              </span>

              <span
                className={`shrink-0 text-2xs ${
                  editorSnapshot?.saveState === "error" ? "text-danger" : "text-fg-faint"
                }`}
                role="status"
                data-testid="editor-save-state"
              >
                {editorSaveLabel(editorSnapshot?.saveState ?? "saved")}
              </span>

              <div className="flex items-center gap-0.5">
                <button
                  type="button"
                  className="grid size-7 place-items-center rounded-sm text-fg-muted hover:bg-hover disabled:opacity-35"
                  aria-label="缩小时间线"
                  title="缩小时间线"
                  onClick={zoomOutTimeline}
                  disabled={pxPerSec <= TIMELINE_ZOOM_LEVELS[0]}
                >
                  <ZoomOut size={14} strokeWidth={1.75} aria-hidden />
                </button>
                <input
                  type="range"
                  aria-label="时间线缩放"
                  title="拖动缩放；也可在时间线上按住 ⌘/Ctrl 滚轮"
                  className="h-1 w-20 accent-accent"
                  min={TIMELINE_ZOOM_LEVELS[0]}
                  max={TIMELINE_ZOOM_LEVELS[TIMELINE_ZOOM_LEVELS.length - 1]}
                  step={1}
                  value={pxPerSec}
                  onChange={(event) => setPxPerSec(Number(event.target.value))}
                />
                <span className="w-12 text-center text-2xs tabular-nums text-fg-faint">
                  {pxPerSec} px/s
                </span>
                <button
                  type="button"
                  className="grid size-7 place-items-center rounded-sm text-fg-muted hover:bg-hover disabled:opacity-35"
                  aria-label="放大时间线"
                  title="放大时间线"
                  onClick={zoomInTimeline}
                  disabled={pxPerSec >= TIMELINE_ZOOM_LEVELS[TIMELINE_ZOOM_LEVELS.length - 1]}
                >
                  <ZoomIn size={14} strokeWidth={1.75} aria-hidden />
                </button>
                <button
                  type="button"
                  className="rounded-sm px-2 py-1 text-2xs text-fg-muted hover:bg-hover"
                  onClick={fitTimeline}
                >
                  适应
                </button>
              </div>
            </div>

            {timelineEditError ? (
              <div
                className="flex shrink-0 items-center justify-between gap-3 border-b border-danger/30 bg-danger/8 px-3 py-1 text-2xs text-danger"
                role="status"
              >
                <span>{timelineEditError}</span>
              </div>
            ) : null}

            <div ref={timelineBodyRef} className="min-h-0 flex-1 overflow-hidden bg-ink">
              {timelineVersion === null ? (
                <p className="px-4 py-5 text-xs text-fg-muted">暂无时间线</p>
              ) : timelineQuery.isPending ? (
                <p className="px-4 py-5 text-xs text-fg-muted">时间线加载中…</p>
              ) : editorTimeline ? (
                <TimelineViewer
                  ref={timelineViewerRef}
                  timeline={editorTimeline}
                  pxPerSec={pxPerSec}
                  playheadSec={playheadSec}
                  selectedClipId={selectedClipId}
                  onClipClick={handleClipClick}
                  onDeselect={handleTimelineDeselect}
                  onSeek={handleTimelineSeek}
                  onZoomChange={setPxPerSec}
                  editMode={editMode}
                  dropMode={dropMode}
                  snapEnabled={snapEnabled}
                  editing={false}
                  onSplitClip={handleSplitClip}
                  onMoveClip={handleMoveClip}
                  onTrimClip={handleTrimClip}
                  onClipFadeChange={handleClipFadeChange}
                  onTrackStateChange={handleTrackStateChange}
                />
              ) : (
                <p className="px-4 py-5 text-xs text-fg-muted">时间线暂不可用。</p>
              )}
            </div>

            {selectedClipDetail ? (
              <ClipDetailBar
                detail={selectedClipDetail}
                editing={timelineEditingDisabled}
                onToggleLinked={handleToggleLinked}
                onClipGainChange={handleClipGainChange}
                onClipFadeChange={handleClipFadeChange}
                onTrackGainChange={handleTrackGainChange}
                onSubtitleChange={handleSubtitleChange}
              />
            ) : null}
          </section>
        </main>
      </div>

      <EntityActionDialog
        dialog={entityDialog}
        drafts={currentDraft ? [currentDraft] : []}
        onClose={closeEntityDialog}
      />
    </div>
  );
}

function PreviewPlaceholder({ text }: { text: string }): ReactElement {
  return (
    <div className="grid h-full place-items-center">
      <div className="grid aspect-video max-h-full w-full place-items-center bg-preview">
        <p className="max-w-[280px] text-center text-xs leading-5 text-preview-fg">{text}</p>
      </div>
    </div>
  );
}

function TimelineToolButton({
  icon: Icon,
  label,
  shortcut,
  active,
  disabled = false,
  onClick
}: {
  icon: LucideIcon;
  label: string;
  shortcut?: string;
  active?: boolean;
  disabled?: boolean;
  onClick: () => void;
}): ReactElement {
  const title = shortcut ? `${label} (${shortcut})` : label;
  return (
    <button
      type="button"
      className={`inline-flex h-7 items-center gap-1 rounded-sm px-1.5 text-2xs transition-colors ease-standard disabled:opacity-35 ${
        active === true ? "bg-active text-fg" : "text-fg-muted hover:bg-hover hover:text-fg"
      }`}
      aria-label={title}
      aria-pressed={active === undefined ? undefined : active}
      title={title}
      disabled={disabled}
      onClick={onClick}
    >
      <Icon size={14} strokeWidth={1.75} aria-hidden />
      <span className="hidden 2xl:inline">{label}</span>
    </button>
  );
}

/** 素材试看面板：顶部工具条（素材名 + 关闭）+ 原片优先播放器；Esc 关闭回成片。 */
function AssetPreviewPane({
  asset,
  onClose
}: {
  asset: MaterialAsset;
  onClose: () => void;
}): ReactElement {
  useEffect(() => {
    const onKey = (event: KeyboardEvent): void => {
      if (event.key === "Escape") {
        onClose();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  return (
    <div className="flex h-full min-h-0 flex-col overflow-hidden border border-line bg-panel">
      <div className="flex h-8 shrink-0 items-center justify-between gap-2 border-b border-line px-2">
        <span className="truncate text-xs text-fg" title={`试看 · ${asset.filename || asset.asset_id}`}>
          试看 · {asset.filename || asset.asset_id}
        </span>
        <button
          type="button"
          className="grid size-6 shrink-0 place-items-center rounded-sm text-fg-muted transition-colors ease-standard hover:bg-hover hover:text-fg"
          aria-label="关闭试看"
          title="关闭试看（Esc）"
          onClick={onClose}
        >
          <X size={16} strokeWidth={1.75} aria-hidden />
        </button>
      </div>
      <div className="grid min-h-0 flex-1 place-items-center overflow-hidden bg-black p-2">
        <AssetMediaPreview asset={asset} />
      </div>
    </div>
  );
}

type ClipDetail = {
  clipId: string;
  trackId: string;
  startFrame: number;
  endFrame: number;
  startSec: number;
  endSec: number;
  label: string | null;
  assetId: string | null;
  assetKind: string | null;
  text: string;
  gainDb: number;
  fadeInFrames: number;
  fadeOutFrames: number;
  fps: number;
  trackGainDb: number;
  linked: boolean;
  trackMuted: boolean;
  trackSolo: boolean;
  trackLocked: boolean;
};

function ClipDetailBar({
  detail,
  editing,
  onToggleLinked,
  onClipGainChange,
  onClipFadeChange,
  onTrackGainChange,
  onSubtitleChange
}: {
  detail: ClipDetail;
  editing: boolean;
  onToggleLinked: (clipId: string, linked: boolean) => void;
  onClipGainChange: (clipId: string, gainDb: number) => void;
  onClipFadeChange: (clipId: string, fadeInFrames: number, fadeOutFrames: number) => void;
  onTrackGainChange: (trackId: string, gainDb: number) => void;
  onSubtitleChange: (clipId: string, text: string) => void;
}): ReactElement {
  const [clipGain, setClipGain] = useState(detail.gainDb);
  const [trackGain, setTrackGain] = useState(detail.trackGainDb);
  const [fadeInFrames, setFadeInFrames] = useState(detail.fadeInFrames);
  const [fadeOutFrames, setFadeOutFrames] = useState(detail.fadeOutFrames);
  const [subtitleText, setSubtitleText] = useState(detail.text);
  const committedClipGain = useRef(detail.gainDb);
  const committedTrackGain = useRef(detail.trackGainDb);
  const committedFades = useRef({
    fadeInFrames: detail.fadeInFrames,
    fadeOutFrames: detail.fadeOutFrames
  });
  useEffect(() => {
    setClipGain(detail.gainDb);
    committedClipGain.current = detail.gainDb;
  }, [detail.gainDb]);
  useEffect(() => {
    setTrackGain(detail.trackGainDb);
    committedTrackGain.current = detail.trackGainDb;
  }, [detail.trackGainDb]);
  useEffect(() => {
    setFadeInFrames(detail.fadeInFrames);
    setFadeOutFrames(detail.fadeOutFrames);
    committedFades.current = {
      fadeInFrames: detail.fadeInFrames,
      fadeOutFrames: detail.fadeOutFrames
    };
  }, [detail.clipId, detail.fadeInFrames, detail.fadeOutFrames]);
  useEffect(() => setSubtitleText(detail.text), [detail.text]);
  const audio = isAudioTrack(detail.trackId);
  const subtitle = detail.trackId === "subtitles";
  const commitClipGain = (): void => {
    if (clipGain !== committedClipGain.current) {
      committedClipGain.current = clipGain;
      onClipGainChange(detail.clipId, clipGain);
    }
  };
  const commitTrackGain = (): void => {
    if (trackGain !== committedTrackGain.current) {
      committedTrackGain.current = trackGain;
      onTrackGainChange(detail.trackId, trackGain);
    }
  };
  const commitFades = (): void => {
    const committed = committedFades.current;
    if (
      fadeInFrames !== committed.fadeInFrames ||
      fadeOutFrames !== committed.fadeOutFrames
    ) {
      committedFades.current = { fadeInFrames, fadeOutFrames };
      onClipFadeChange(detail.clipId, fadeInFrames, fadeOutFrames);
    }
  };
  const commitSubtitle = (): void => {
    const text = subtitleText.trim();
    if (text && text !== detail.text) {
      onSubtitleChange(detail.clipId, text);
    }
  };

  return (
    <div className="flex min-h-10 shrink-0 items-center gap-3 overflow-x-auto border-t border-line bg-raised px-3 py-1 text-2xs text-fg-muted">
      <div className="min-w-0 shrink overflow-hidden text-ellipsis whitespace-nowrap">
        <span className="font-semibold text-fg">已选：</span>
        <span className="font-mono">{detail.clipId}</span>
        <span className="mx-2 text-fg-faint">|</span>
        <span>{trackDisplayLabel(detail.trackId)}</span>
        <span className="mx-2 text-fg-faint">|</span>
        <span>{formatSeconds(detail.startSec)}-{formatSeconds(detail.endSec)}</span>
      </div>

      {isLinkableTrack(detail.trackId) ? (
        <button
          type="button"
          className={`inline-flex h-7 shrink-0 items-center gap-1 rounded-sm border px-2 font-semibold ${
            detail.linked
              ? "border-accent/50 bg-accent/10 text-accent-strong"
              : "border-line-strong text-fg-muted hover:bg-hover"
          }`}
          disabled={editing || detail.trackLocked}
          aria-pressed={detail.linked}
          onClick={() => onToggleLinked(detail.clipId, !detail.linked)}
        >
          {detail.linked ? <Unlink2 size={12} aria-hidden /> : <Link2 size={12} aria-hidden />}
          {detail.linked ? "取消音画联动" : "建立音画联动"}
        </button>
      ) : null}

      {audio ? (
        <>
          <label className="flex shrink-0 items-center gap-1.5">
            <span>片段</span>
            <input
              aria-label="片段音量"
              className="w-24 accent-accent"
              type="range"
              min={-60}
              max={12}
              step={1}
              value={clipGain}
              disabled={editing || detail.trackLocked}
              onChange={(event) => setClipGain(Number(event.target.value))}
              onPointerUp={commitClipGain}
              onBlur={commitClipGain}
            />
            <span className="w-10 font-mono tabular-nums">{clipGain.toFixed(0)} dB</span>
          </label>
          <label className="flex shrink-0 items-center gap-1.5">
            <span>轨道</span>
            <input
              aria-label="所选轨道音量"
              className="w-24 accent-accent"
              type="range"
              min={-60}
              max={12}
              step={1}
              value={trackGain}
              disabled={editing}
              onChange={(event) => setTrackGain(Number(event.target.value))}
              onPointerUp={commitTrackGain}
              onBlur={commitTrackGain}
            />
            <span className="w-10 font-mono tabular-nums">{trackGain.toFixed(0)} dB</span>
          </label>
          <label className="flex shrink-0 items-center gap-1.5">
            <span>淡入</span>
            <input
              aria-label="片段淡入"
              className="w-20 accent-accent"
              type="range"
              min={0}
              max={Math.max(0, detail.endFrame - detail.startFrame - fadeOutFrames)}
              step={1}
              value={fadeInFrames}
              disabled={editing || detail.trackLocked}
              onChange={(event) => setFadeInFrames(Number(event.target.value))}
              onPointerUp={commitFades}
              onBlur={commitFades}
            />
            <span className="w-10 font-mono tabular-nums">
              {(fadeInFrames / detail.fps).toFixed(1)}s
            </span>
          </label>
          <label className="flex shrink-0 items-center gap-1.5">
            <span>淡出</span>
            <input
              aria-label="片段淡出"
              className="w-20 accent-accent"
              type="range"
              min={0}
              max={Math.max(0, detail.endFrame - detail.startFrame - fadeInFrames)}
              step={1}
              value={fadeOutFrames}
              disabled={editing || detail.trackLocked}
              onChange={(event) => setFadeOutFrames(Number(event.target.value))}
              onPointerUp={commitFades}
              onBlur={commitFades}
            />
            <span className="w-10 font-mono tabular-nums">
              {(fadeOutFrames / detail.fps).toFixed(1)}s
            </span>
          </label>
        </>
      ) : null}

      {subtitle ? (
        <div className="flex min-w-[280px] flex-1 items-center gap-1.5">
          <label className="sr-only" htmlFor={`subtitle-${detail.clipId}`}>编辑字幕</label>
          <input
            id={`subtitle-${detail.clipId}`}
            aria-label="编辑字幕"
            className="h-7 min-w-0 flex-1 rounded-sm border border-line-strong bg-panel px-2 text-xs text-fg outline-none focus:border-accent"
            value={subtitleText}
            disabled={editing || detail.trackLocked}
            onChange={(event) => setSubtitleText(event.target.value)}
            onKeyDown={(event) => {
              if (event.key === "Enter") {
                event.preventDefault();
                commitSubtitle();
              }
            }}
          />
          <button
            type="button"
            className="h-7 shrink-0 rounded-sm bg-accent px-2 font-semibold text-white disabled:opacity-35"
            disabled={
              editing ||
              detail.trackLocked ||
              !subtitleText.trim() ||
              subtitleText.trim() === detail.text
            }
            onClick={commitSubtitle}
          >
            保存字幕
          </button>
        </div>
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
        startFrame: clip.timeline_start_frame,
        endFrame: clip.timeline_end_frame,
        startSec: clip.timeline_start_frame / fps,
        endSec: clip.timeline_end_frame / fps,
        label: clipLabel(clip),
        assetId: typeof clip.asset_id === "string" ? clip.asset_id : null,
        assetKind: typeof clip.asset_kind === "string" ? clip.asset_kind : null,
        text: typeof clip.text === "string" ? clip.text : "",
        gainDb: typeof clip.gain_db === "number" ? clip.gain_db : 0,
        fadeInFrames:
          typeof clip.fade_in_frames === "number" ? Math.max(0, Math.round(clip.fade_in_frames)) : 0,
        fadeOutFrames:
          typeof clip.fade_out_frames === "number" ? Math.max(0, Math.round(clip.fade_out_frames)) : 0,
        fps,
        trackGainDb: typeof track.gain_db === "number" ? track.gain_db : 0,
        linked: clip.linked === true,
        trackMuted: track.muted === true,
        trackSolo: track.solo === true,
        trackLocked: track.locked === true
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

function isAudioTrack(trackId: string): boolean {
  return ["original_audio", "voiceover", "bgm", "sfx"].includes(trackId);
}

function isLinkableTrack(trackId: string): boolean {
  return trackId === "visual_base" || trackId === "original_audio";
}

function trackDisplayLabel(trackId: string): string {
  const labels: Record<string, string> = {
    visual_base: "主视频",
    visual_overlay: "叠加",
    original_audio: "原声",
    voiceover: "配音",
    bgm: "音乐",
    sfx: "音效",
    subtitles: "字幕"
  };
  return labels[trackId] ?? trackId;
}

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

function formatSeconds(value: number): string {
  return `${value.toFixed(2)}s`;
}

function clampNumber(value: number, minimum: number, maximum: number): number {
  return Math.min(Math.max(value, minimum), maximum);
}

function timelinePatchErrorMessage(error: unknown): string {
  if (error instanceof ApiError && error.payload && typeof error.payload === "object") {
    const detail = Reflect.get(error.payload, "detail");
    if (detail && typeof detail === "object") {
      const reason = Reflect.get(detail, "reason");
      if (typeof reason === "string" && reason.trim()) {
        return reason;
      }
    }
  }
  return error instanceof Error ? error.message : "时间线修改失败";
}

function conversationClearErrorMessage(error: unknown): string {
  const reason = timelinePatchErrorMessage(error);
  if (reason === "turn_active") {
    return "当前任务仍在运行，请先停止或等待本轮结束后再清空对话。";
  }
  return reason === "API 请求失败：409" ? "当前任务仍在运行，暂时不能清空对话。" : reason;
}

function jobCancelErrorMessage(error: unknown): string {
  const reason = timelinePatchErrorMessage(error);
  if (reason === "job_not_cancellable" || reason === "API 请求失败：409") {
    return "任务状态已变化，无法取消；已刷新当前状态。";
  }
  return `取消任务失败：${reason}`;
}

function rewindErrorMessage(error: unknown): string {
  const reason = timelinePatchErrorMessage(error);
  if (reason === "rewind_checkpoint_not_found") {
    return "检查点已被清理，请刷新后选择新的检查点。";
  }
  if (reason === "rewind_checkpoint_has_no_timeline") {
    return "这个检查点没有可恢复的时间线，请改用仅对话。";
  }
  if (reason === "rewind_cancellation_timeout") {
    return "当前任务尚未安全停止，请稍后重试恢复。";
  }
  if (reason === "turn_queue_closed") {
    return "剪辑任务队列已停止，请重启本地服务后再恢复。";
  }
  if (reason === "rewind_idempotency_key_reused") {
    return "这次恢复请求已用于另一个检查点，请重新操作。";
  }
  if (reason === "version_conflict" || reason === "API 请求失败：409") {
    return "草稿刚刚发生了变化，请刷新检查点后重试。";
  }
  return `恢复检查点失败：${reason}`;
}

function formatTimecode(sec: number): string {
  const safe = Math.max(0, sec);
  const minutes = Math.floor(safe / 60);
  const seconds = Math.floor(safe % 60);
  const tenths = Math.floor((safe % 1) * 10);
  return `${String(minutes).padStart(2, "0")}:${String(seconds).padStart(2, "0")}.${tenths}`;
}

function editorSaveLabel(state: EditorSessionSnapshot["saveState"]): string {
  switch (state) {
    case "dirty":
      return "本地已更新";
    case "saving":
      return "保存中…";
    case "error":
      return "保存失败";
    default:
      return "已保存";
  }
}

/** 成本小计：估算金额以人民币四位小数显示；未加载时占位。 */
function formatCost(total: number | null): string {
  if (total === null) {
    return "¥--";
  }
  return `¥${total.toFixed(4)}`;
}

const TIMELINE_ZOOM_LEVELS = [8, 12, 24, 48, 96, 192, 320];
const DEFAULT_TIMELINE_PX_PER_SEC = 96;
const TIMELINE_LABEL_WIDTH = 184;
