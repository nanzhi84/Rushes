import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useParams } from "@tanstack/react-router";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { ReactElement } from "react";
import {
  ArrowUp,
  Captions,
  Crop,
  Home,
  Link2,
  ListPlus,
  Magnet,
  MousePointer2,
  Pencil,
  Redo2,
  Replace,
  Scissors,
  Square,
  Trash2,
  Undo2,
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
  type TimelineClipJson,
  type TimelineJson
} from "../api/client";
import { DRAFT_EVENT_TYPES } from "../api/event_types";
import { queryKeys } from "../app/query_client";
import { ApiError, createApiEventSource } from "../auth";
import { useWorkspaceEvents } from "../app/use_workspace_events";
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
import { AssetMediaPreview } from "../components/Materials/AssetMediaPreview";
import { AssetsPanel } from "../components/Materials/AssetsPanel";
import { PreviewPlayer } from "../components/PreviewPlayer";
import { EntityActionDialog } from "../components/Shell/EntityActionDialog";
import { ResizeHandle } from "../components/Shell/ResizeHandle";
import { TopBar } from "../components/Shell/TopBar";
import { TimelineViewer } from "../components/TimelineViewer";
import type {
  TimelineDropMode,
  TimelineEditMode,
  TimelineTrackStatePatch
} from "../components/TimelineViewer";
import { useUiStore } from "../state/ui_store";

export function DraftEditorPage(): ReactElement {
  const { draftId } = useParams({ from: "/drafts/$draftId" });
  return <DraftEditorView draftId={draftId} />;
}

export function DraftEditorView({ draftId }: { draftId: string }): ReactElement {
  const queryClient = useQueryClient();
  const connectionState = useWorkspaceEvents();
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
  const [structuredItems, setStructuredItems] = useState<StructuredInteractionItem[]>([]);
  const [previewingAssetId, setPreviewingAssetId] = useState<string | null>(null);
  const [selectedClipId, setSelectedClipId] = useState<string | null>(null);
  const [unmatchedClipId, setUnmatchedClipId] = useState<string | null>(null);
  const [highlightedMessageId, setHighlightedMessageId] = useState<string | null>(null);
  const [playheadSec, setPlayheadSec] = useState<number | null>(null);
  const [seekSec, setSeekSec] = useState<number | null>(null);
  const [pxPerSec, setPxPerSec] = useState(DEFAULT_TIMELINE_PX_PER_SEC);
  const [viewedVersion, setViewedVersion] = useState<number | null>(null);
  const [editMode, setEditMode] = useState<TimelineEditMode>("select");
  const [dropMode, setDropMode] = useState<TimelineDropMode>("insert");
  const [snapEnabled, setSnapEnabled] = useState(true);
  const [timelineEditError, setTimelineEditError] = useState<string | null>(null);
  const [previewRefreshing, setPreviewRefreshing] = useState(false);
  const timelineBodyRef = useRef<HTMLDivElement | null>(null);

  const messagesQuery = useQuery({
    queryKey: queryKeys.messages(draftId),
    queryFn: async () => {
      const response = await api.getDraftMessages(draftId);
      return response.messages.map(toConsoleMessage);
    },
    initialData: [] as ConsoleMessage[]
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
  const effectiveVersion = viewedVersion ?? timelineVersion;
  const viewingHistory =
    viewedVersion !== null && timelineVersion !== null && viewedVersion !== timelineVersion;

  const timelineQuery = useQuery({
    queryKey: queryKeys.timeline(draftId, effectiveVersion),
    queryFn: () => api.fetchDraftTimeline(draftId, effectiveVersion),
    enabled: effectiveVersion !== null
  });

  const currentDecision = decisionQuery.data?.decision ?? null;
  const historyMessages = messagesQuery.data ?? [];
  const timelinePayload = timelineQuery.data ?? null;
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
    () => mergeCurrentDecisionItem([], currentDecision)[0] ?? null,
    [currentDecision]
  );

  const invalidateDraftQueries = useCallback(
    async (payload: DomainSsePayload) => {
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: queryKeys.draft(draftId) }),
        queryClient.invalidateQueries({ queryKey: ["timeline", draftId] }),
        queryClient.invalidateQueries({ queryKey: queryKeys.messages(draftId) }),
        queryClient.invalidateQueries({ queryKey: queryKeys.currentDecision(draftId) }),
        queryClient.invalidateQueries({ queryKey: queryKeys.costs(draftId) })
      ]);
    },
    [draftId, queryClient]
  );

  useEffect(() => {
    const source = createApiEventSource(`/api/drafts/${draftId}/events`);
    source.onopen = () => setStreamState("open");
    source.onerror = () => setStreamState("closed");
    const handleEvent = (event: Event) => {
      const message = event as MessageEvent<string>;
      const payload = JSON.parse(message.data) as DomainSsePayload;
      setStructuredItems((current) => reduceStructuredInteractionItems(current, payload));
      void invalidateDraftQueries(payload);
    };
    for (const eventName of DRAFT_EVENT_TYPES) {
      source.addEventListener(eventName, handleEvent);
    }
    return () => {
      source.close();
    };
  }, [draftId, invalidateDraftQueries]);

  // turn-stream 订阅置于领域 /events 订阅之后，保证 /events 是首个 EventSource。
  const finishTurn = useCallback(() => {
    setAwaitingTurnEnd(false);
    void queryClient.invalidateQueries({ queryKey: queryKeys.messages(draftId) });
  }, [draftId, queryClient]);
  const { items: streamItems, subagentProgress, understandingProgress } = useTurnStream(draftId, {
    onTurnEnded: finishTurn,
    onTurnError: finishTurn
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
      const optimistic: ConsoleMessage = {
        id: `local_${Date.now()}`,
        role: "user",
        content,
        createdAt: new Date().toISOString()
      };
      queryClient.setQueryData<ConsoleMessage[]>(queryKeys.messages(draftId), (current) => [
        ...(current ?? []),
        optimistic
      ]);
    },
    onError: () => setAwaitingTurnEnd(false)
  });

  const cancelTurn = useMutation({
    mutationFn: () => api.cancelTurn(draftId)
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

  const timelinePatch = useMutation({
    mutationFn: (op: Record<string, unknown>) => api.applyTimelinePatch(draftId, { op }),
    onSuccess: async (response) => {
      setTimelineEditError(null);
      setPreviewRefreshing(true);
      setViewedVersion(null);
      queryClient.setQueryData(queryKeys.timeline(draftId, response.timeline_version), response);
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: queryKeys.draft(draftId) }),
        queryClient.invalidateQueries({ queryKey: ["timeline", draftId] })
      ]);
    },
    onError: (error) => {
      setTimelineEditError(timelinePatchErrorMessage(error));
    }
  });

  const timelineRestore = useMutation({
    mutationFn: (version: number) => api.restoreTimelineVersion(draftId, version),
    onSuccess: async (response) => {
      setTimelineEditError(null);
      setPreviewRefreshing(true);
      setViewedVersion(null);
      setSelectedClipId(null);
      queryClient.setQueryData(queryKeys.timeline(draftId, response.timeline_version), response);
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: queryKeys.draft(draftId) }),
        queryClient.invalidateQueries({ queryKey: ["timeline", draftId] })
      ]);
    },
    onError: (error) => {
      setTimelineEditError(timelinePatchErrorMessage(error));
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
      .postPreviewViewed(draftId, previewId)
      .then(() => queryClient.invalidateQueries({ queryKey: queryKeys.draft(draftId) }))
      .catch(() => undefined);
  }, [draftId, queryClient, timelinePayload?.preview_id]);
  const handlePreviewTimeUpdate = useCallback((sec: number) => {
    setPlayheadSec(sec);
  }, []);
  // 单击瓦片试看；再点已选中瓦片取消，回到成片/占位。
  const handlePreviewAsset = useCallback((asset: MaterialAsset) => {
    setPreviewingAssetId((current) => (current === asset.asset_id ? null : asset.asset_id));
  }, []);
  const closeAssetPreview = useCallback(() => setPreviewingAssetId(null), []);
  const handleTimelineSeek = useCallback((sec: number) => {
    setSeekSec(sec);
    setPlayheadSec(sec);
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
    setPxPerSec(Math.max(4, Math.floor(available / durationSec)));
  }, [timelineQuery.data?.timeline]);
  const handleClipClick = useCallback(
    (clipId: string) => {
      setSelectedClipId(clipId);
      const streamMatch = streamItems.find(
        (item) => item.type === "message" && item.text.includes(clipId)
      );
      const messageMatch =
        messages.find((message) => message.content.includes(clipId)) ??
        (streamMatch && streamMatch.type === "message"
          ? { id: streamMatch.message_id }
          : undefined);
      const structuredMatch = renderedStructuredItems.some((item) => JSON.stringify(item).includes(clipId));
      const targetId = messageMatch?.id ?? (structuredMatch ? "structured-interactions" : null);
      setHighlightedMessageId(targetId);
      setUnmatchedClipId(targetId ? null : clipId);
      if (targetId) {
        window.requestAnimationFrame(() => scrollToMessage(targetId));
      }
    },
    [messages, renderedStructuredItems, streamItems]
  );
  const applyTimelinePatch = useCallback(
    (op: Record<string, unknown>) => {
      if (viewingHistory) {
        setTimelineEditError("历史版本仅供查看，请回到当前版本后再编辑。");
        return;
      }
      if (timelinePatch.isPending || timelineRestore.isPending) {
        setTimelineEditError("时间线正在修改，请稍候。");
        return;
      }
      setTimelineEditError(null);
      timelinePatch.mutate(op);
    },
    [timelinePatch, timelineRestore.isPending, viewingHistory]
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
    if (!selectedClipId || !timelinePayload) {
      return;
    }
    const detail = findTimelineClip(timelinePayload.timeline, selectedClipId);
    if (!detail?.assetId) {
      setTimelineEditError("请选择视频或图片片段后再分割。");
      return;
    }
    const fps = timelinePayload.timeline.fps > 0 ? timelinePayload.timeline.fps : 30;
    const playheadFrame = Math.round((playheadSec ?? -1) * fps);
    const splitAt =
      playheadFrame > detail.startFrame && playheadFrame < detail.endFrame
        ? playheadFrame
        : Math.round((detail.startFrame + detail.endFrame) / 2);
    handleSplitClip(selectedClipId, splitAt);
  }, [handleSplitClip, playheadSec, selectedClipId, timelinePayload]);
  const handleDeleteSelected = useCallback(() => {
    if (!selectedClipId || !timelinePayload) {
      return;
    }
    if (viewingHistory || timelinePatch.isPending || timelineRestore.isPending) {
      setTimelineEditError(
        viewingHistory ? "历史版本仅供查看，请回到当前版本后再编辑。" : "时间线正在修改，请稍候。"
      );
      return;
    }
    const detail = findTimelineClip(timelinePayload.timeline, selectedClipId);
    if (!detail) {
      setTimelineEditError("找不到当前选中的片段。");
      return;
    }
    applyTimelinePatch({ kind: "delete_clip", timeline_clip_id: selectedClipId });
    setSelectedClipId(null);
  }, [
    applyTimelinePatch,
    selectedClipId,
    timelinePatch.isPending,
    timelinePayload,
    timelineRestore.isPending,
    viewingHistory
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
    const timeline = timelinePayload?.timeline;
    if (!timeline || timeline.duration_frames < 1) {
      return;
    }
    const fps = timeline.fps > 0 ? timeline.fps : 30;
    const length = Math.min(timeline.duration_frames, fps * 2);
    let startFrame = clampNumber(
      Math.round((playheadSec ?? 0) * fps),
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
  }, [applyTimelinePatch, playheadSec, timelinePayload?.timeline]);
  const restoreVersion = useCallback(
    (version: number | null | undefined) => {
      if (!version || timelinePatch.isPending || timelineRestore.isPending) {
        return;
      }
      setTimelineEditError(null);
      timelineRestore.mutate(version);
    },
    [timelinePatch.isPending, timelineRestore]
  );
  const handleUndo = useCallback(
    () => restoreVersion(viewingHistory ? null : timelinePayload?.parent_version),
    [restoreVersion, timelinePayload?.parent_version, viewingHistory]
  );
  const handleRedo = useCallback(
    () => restoreVersion(viewingHistory ? null : timelinePayload?.redo_version),
    [restoreVersion, timelinePayload?.redo_version, viewingHistory]
  );
  const handleExport = useCallback(() => {
    postMessage.mutate("请把当前时间线导出为最终 MP4。");
  }, [postMessage]);

  useEffect(() => {
    setPlayheadSec(null);
    setSeekSec(null);
  }, [timelinePayload?.preview_id]);

  // 时间线推进到新版本时回到「当前版本」视图。
  useEffect(() => {
    setViewedVersion(null);
  }, [timelineVersion]);

  useEffect(() => {
    if (previewSrc) {
      setPreviewRefreshing(false);
    }
  }, [previewSrc]);

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
      if ((event.metaKey || event.ctrlKey) && key === "z" && !event.altKey) {
        event.preventDefault();
        if (event.shiftKey) {
          handleRedo();
        } else {
          handleUndo();
        }
      } else if (event.metaKey || event.ctrlKey || event.altKey) {
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
  }, [handleDeleteSelected, handleRedo, handleUndo, selectedClipId]);

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
  const submitComposer = useCallback(() => {
    const content = draft.trim();
    if (!content || disabled) {
      return;
    }
    setDraft("");
    runtime.submit(content);
  }, [disabled, draft, runtime]);
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
    const timeline = timelinePayload?.timeline;
    if (!timeline) {
      return 0;
    }
    const fps = timeline.fps > 0 ? timeline.fps : 30;
    return timeline.duration_frames / fps;
  }, [timelinePayload?.timeline]);
  const selectedClipDetail = useMemo(
    () =>
      selectedClipId && timelinePayload
        ? findTimelineClip(timelinePayload.timeline, selectedClipId)
        : null,
    [selectedClipId, timelinePayload]
  );
  const timelineEditingDisabled =
    timelinePatch.isPending || timelineRestore.isPending || viewingHistory || timelineVersion === null;

  return (
    <div className="flex h-[100dvh] min-h-0 flex-col bg-ink text-fg">
      <TopBar
        connectionState={connectionState}
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
              disabled={disabled || timelineVersion === null}
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

          <AssistantThread
            runtime={runtime}
            onAnswerDecision={handleAnswerDecision}
            answerPending={answerDecision.isPending}
            highlightedMessageId={highlightedMessageId}
            streamItems={streamItems}
            subagentProgress={subagentProgress}
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
                disabled={!runtime.canSubmit}
                placeholder={runtime.isRunning ? "等待本轮结束…" : "描述你想怎样剪辑…"}
              />
              <div className="flex items-center justify-between gap-3 border-t border-line px-2 py-1.5">
                <span className="text-2xs text-fg-faint">
                  <kbd className="font-mono">Enter</kbd> 发送　
                  <kbd className="font-mono">Shift+Enter</kbd> 换行
                </span>
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
                ) : (
                  <button
                    className="flex size-7 items-center justify-center rounded-md bg-accent text-white transition-[transform,background-color] duration-fast hover:bg-accent-strong active:translate-y-px disabled:opacity-40"
                    type="submit"
                    aria-label="发送消息"
                    disabled={!runtime.canSubmit || draft.trim().length === 0}
                  >
                    <ArrowUp size={15} strokeWidth={2} aria-hidden />
                    <span className="sr-only">发送</span>
                  </button>
                )}
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
                management
                onPreviewAsset={handlePreviewAsset}
                previewingAssetId={previewingAssetId}
                understandingProgress={understandingProgress}
                onCancelUnderstanding={() => cancelTurn.mutate()}
                cancellingUnderstanding={cancelTurn.isPending}
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
                ) : previewRefreshing ? (
                  <PreviewPlaceholder text="时间线已更新，正在生成新预览…" />
                ) : timelinePayload && previewSrc ? (
                  <PreviewPlayer
                    key={timelinePayload.preview_id}
                    src={previewSrc}
                    fps={timelinePayload.timeline.fps}
                    fit="height"
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
                icon={Undo2}
                label="撤销"
                shortcut="⌘Z"
                disabled={timelineEditingDisabled || timelinePayload?.parent_version == null}
                onClick={handleUndo}
              />
              <TimelineToolButton
                icon={Redo2}
                label="重做"
                shortcut="⇧⌘Z"
                disabled={timelineEditingDisabled || timelinePayload?.redo_version == null}
                onClick={handleRedo}
              />
              <span aria-hidden className="mx-1 h-4 w-px shrink-0 bg-line-strong" />
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
              {timelineVersion !== null ? (
                <select
                  aria-label="时间线版本"
                  className="ml-1 rounded-sm border border-line bg-raised px-1.5 py-1 text-2xs text-fg-muted outline-none focus:border-accent"
                  value={effectiveVersion ?? ""}
                  onChange={(event) => {
                    const next = Number(event.target.value);
                    setViewedVersion(next === timelineVersion ? null : next);
                  }}
                >
                  {versionOptions(timelinePayload?.latest_version ?? timelineVersion).map((version) => (
                    <option key={version} value={version}>
                      v{version}{version === timelineVersion ? " · 当前" : ""}
                    </option>
                  ))}
                </select>
              ) : null}

              <span className="mx-auto font-mono text-2xs tabular-nums text-fg-muted">
                {formatTimecode(playheadSec ?? 0)} / {formatTimecode(timelineDurationSec)}
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
                <span className="w-11 text-center text-2xs tabular-nums text-fg-faint">
                  {pxPerSec}px/s
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

            {viewingHistory || timelineEditError ? (
              <div
                className={`flex shrink-0 items-center justify-between gap-3 border-b px-3 py-1 text-2xs ${
                  timelineEditError
                    ? "border-danger/30 bg-danger/8 text-danger"
                    : "border-warn/30 bg-warn/8 text-warn"
                }`}
                role="status"
              >
                <span>{timelineEditError ?? "正在查看历史版本；恢复后会成为当前编辑版本。"}</span>
                {viewingHistory && !timelineEditError && timelinePayload ? (
                  <button
                    type="button"
                    className="shrink-0 rounded-sm border border-warn/40 px-2 py-0.5 font-semibold hover:bg-warn/10"
                    disabled={timelineRestore.isPending}
                    onClick={() => restoreVersion(timelinePayload.timeline_version)}
                  >
                    恢复 v{timelinePayload.timeline_version}
                  </button>
                ) : null}
              </div>
            ) : null}

            <div ref={timelineBodyRef} className="min-h-0 flex-1 overflow-auto bg-ink">
              {timelineVersion === null ? (
                <p className="px-4 py-5 text-xs text-fg-muted">暂无时间线</p>
              ) : timelineQuery.isPending ? (
                <p className="px-4 py-5 text-xs text-fg-muted">时间线加载中…</p>
              ) : timelinePayload ? (
                <TimelineViewer
                  timeline={timelinePayload.timeline}
                  pxPerSec={pxPerSec}
                  playheadSec={playheadSec}
                  selectedClipId={selectedClipId}
                  onClipClick={handleClipClick}
                  onSeek={handleTimelineSeek}
                  waveformSrc={previewSrc}
                  editMode={editMode}
                  dropMode={dropMode}
                  snapEnabled={snapEnabled}
                  editing={timelinePatch.isPending || timelineRestore.isPending || viewingHistory}
                  onSplitClip={handleSplitClip}
                  onMoveClip={handleMoveClip}
                  onTrimClip={handleTrimClip}
                  onTrackStateChange={handleTrackStateChange}
                />
              ) : (
                <p className="px-4 py-5 text-xs text-fg-muted">时间线暂不可用。</p>
              )}
            </div>

            {selectedClipDetail || unmatchedClipDetail ? (
              <ClipDetailBar
                detail={selectedClipDetail ?? unmatchedClipDetail!}
                editing={timelineEditingDisabled}
                onToggleLinked={handleToggleLinked}
                onClipGainChange={handleClipGainChange}
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
  onTrackGainChange,
  onSubtitleChange
}: {
  detail: ClipDetail;
  editing: boolean;
  onToggleLinked: (clipId: string, linked: boolean) => void;
  onClipGainChange: (clipId: string, gainDb: number) => void;
  onTrackGainChange: (trackId: string, gainDb: number) => void;
  onSubtitleChange: (clipId: string, text: string) => void;
}): ReactElement {
  const [clipGain, setClipGain] = useState(detail.gainDb);
  const [trackGain, setTrackGain] = useState(detail.trackGainDb);
  const [subtitleText, setSubtitleText] = useState(detail.text);
  const committedClipGain = useRef(detail.gainDb);
  const committedTrackGain = useRef(detail.trackGainDb);
  useEffect(() => {
    setClipGain(detail.gainDb);
    committedClipGain.current = detail.gainDb;
  }, [detail.gainDb]);
  useEffect(() => {
    setTrackGain(detail.trackGainDb);
    committedTrackGain.current = detail.trackGainDb;
  }, [detail.trackGainDb]);
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

function versionOptions(currentVersion: number): number[] {
  const count = Math.min(currentVersion, MAX_VERSION_OPTIONS);
  return Array.from({ length: count }, (_item, index) => currentVersion - index);
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

function toConsoleMessage(message: MessageRecord): ConsoleMessage {
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

function formatTimecode(sec: number): string {
  const safe = Math.max(0, sec);
  const minutes = Math.floor(safe / 60);
  const seconds = Math.floor(safe % 60);
  const tenths = Math.floor((safe % 1) * 10);
  return `${String(minutes).padStart(2, "0")}:${String(seconds).padStart(2, "0")}.${tenths}`;
}

/** 成本小计：估算金额以人民币四位小数显示；未加载时占位。 */
function formatCost(total: number | null): string {
  if (total === null) {
    return "¥--";
  }
  return `¥${total.toFixed(4)}`;
}

const TIMELINE_ZOOM_LEVELS = [12, 24, 48, 96, 192];
const DEFAULT_TIMELINE_PX_PER_SEC = 96;
const TIMELINE_LABEL_WIDTH = 184;
const MAX_VERSION_OPTIONS = 50;
