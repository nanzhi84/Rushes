import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useParams } from "@tanstack/react-router";
import { startTransition, useCallback, useDeferredValue, useEffect, useMemo, useRef, useState } from "react";
import type { ReactElement } from "react";
import {
  Captions,
  Crop,
  Home,
  Link2,
  ListPlus,
  Magnet,
  MousePointer2,
  Pencil,
  Replace,
  Scissors,
  Trash2,
  Unlink2,
  X,
  ZoomIn,
  ZoomOut
} from "lucide-react";
import type { LucideIcon } from "lucide-react";
import {
  api,
  type MaterialAsset,
  type TimelineClipJson,
  type TimelineJson
} from "../api/client";
import { queryKeys } from "../app/query_client";
import { AssetMediaPreview } from "../components/Materials/AssetMediaPreview";
import { AssetsPanel } from "../components/Materials/AssetsPanel";
import {
  ConsolePanel,
  type ConsoleConnectionState,
  type ConsolePanelHandle
} from "../components/Console/ConsolePanel";
import { timelinePatchErrorMessage } from "../components/Console/error_messages";
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
  // 连接态与回合忙碌态由 ConsolePanel 通过回调低频回传：顶栏连接指示与导出按钮禁用态用它，
  // 但每个 text_delta 不会流经这里，故不会重渲染右侧工作区。
  const [consoleConnection, setConsoleConnection] = useState<ConsoleConnectionState>("connecting");
  const [consoleBusy, setConsoleBusy] = useState(false);
  const consoleRef = useRef<ConsolePanelHandle | null>(null);
  const timelineBodyRef = useRef<HTMLDivElement | null>(null);
  const timelineViewerRef = useRef<TimelineViewerHandle | null>(null);
  const playheadSecRef = useRef<number | null>(null);
  const playheadTimecodeRef = useRef<HTMLSpanElement | null>(null);
  const lastPlayheadCommitRef = useRef({ at: 0, sec: 0 });
  const editorSessionRef = useRef<EditorSession | null>(null);
  const editorSessionDraftRef = useRef<string | null>(null);
  const editorSessionUnsubscribeRef = useRef<(() => void) | null>(null);
  const [editorSnapshot, setEditorSnapshot] = useState<EditorSessionSnapshot | null>(null);

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

  const timelinePayload = timelineQuery.data ?? null;
  const editorTimeline = editorSnapshot?.timeline ?? timelinePayload?.timeline ?? null;
  // 时间线渲染让位于紧急交互：缩放/流式高频提交时，React 可中断这棵较重的子树，
  // 用上一份时间线继续绘制，避免阻塞输入。仅用于喂 TimelineViewer，handler 仍用 editorTimeline。
  const deferredEditorTimeline = useDeferredValue(editorTimeline);
  const previewSrc = timelinePayload?.preview_id
    ? api.mediaPreviewUrl(timelinePayload.preview_id)
    : null;

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

  // ConsolePanel 用稳定回调把低频的连接态 / 回合忙碌态回传给工作区（顶栏与导出按钮）。
  const handleConsoleConnectionChange = useCallback((state: ConsoleConnectionState) => {
    setConsoleConnection(state);
  }, []);
  const handleConsoleTurnBusyChange = useCallback((busy: boolean) => {
    setConsoleBusy(busy);
  }, []);
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
      // 播放头的可见推进走命令式 DOM；这里只是低频把秒数同步给吸附候选等编辑逻辑，
      // 标记为非紧急，播放期间不与解码/绘制争抢主线程。
      startTransition(() => setPlayheadSec(sec));
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
    // 预览 seek（setSeekSec）与命令式播放头保持紧急；React 侧秒数同步为非紧急。
    startTransition(() => setPlayheadSec(sec));
    timelineViewerRef.current?.setPlayheadSec(sec, false);
    if (playheadTimecodeRef.current) {
      playheadTimecodeRef.current.textContent = formatTimecode(sec);
    }
  }, []);
  // 缩放会让整条时间线以新几何重排（clip 多时重）；用 transition 标记为非紧急，
  // 缩放期间输入/滚动不被这次重排阻塞（React19 并发调度）。
  const commitZoom = useCallback((next: number | ((current: number) => number)) => {
    startTransition(() => setPxPerSec(next));
  }, []);
  const zoomOutTimeline = useCallback(() => {
    commitZoom((current) => {
      const lower = [...TIMELINE_ZOOM_LEVELS].reverse().find((level) => level < current);
      return lower ?? current;
    });
  }, [commitZoom]);
  const zoomInTimeline = useCallback(() => {
    commitZoom((current) => {
      const higher = TIMELINE_ZOOM_LEVELS.find((level) => level > current);
      return higher ?? current;
    });
  }, [commitZoom]);
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
    commitZoom(
      Math.min(
        TIMELINE_ZOOM_LEVELS[TIMELINE_ZOOM_LEVELS.length - 1],
        Math.max(TIMELINE_ZOOM_LEVELS[0], Math.floor(available / durationSec))
      )
    );
  }, [commitZoom, timelineQuery.data?.timeline]);
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
        connectionState={consoleConnection}
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
              disabled={consoleBusy || timelineVersion === null}
              onClick={() => consoleRef.current?.submit("请把当前时间线导出为最终 MP4。")}
            >
              导出
            </button>
          </div>
        }
      />

      {/* ChatCut 式工作台：左侧 AI 贯穿全高；右侧素材/预览在上，时间线固定在下。 */}
      <div className="flex min-h-0 flex-1">
        <ConsolePanel
          ref={consoleRef}
          draftId={draftId}
          chatPanelWidth={chatPanelWidth}
          highlightedMessageId={highlightedMessageId}
          onConnectionStateChange={handleConsoleConnectionChange}
          onTurnBusyChange={handleConsoleTurnBusyChange}
        />

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
                  onChange={(event) => commitZoom(Number(event.target.value))}
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
                  timeline={deferredEditorTimeline ?? editorTimeline}
                  pxPerSec={pxPerSec}
                  playheadSec={playheadSec}
                  selectedClipId={selectedClipId}
                  onClipClick={handleClipClick}
                  onDeselect={handleTimelineDeselect}
                  onSeek={handleTimelineSeek}
                  onZoomChange={commitZoom}
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

function formatSeconds(value: number): string {
  return `${value.toFixed(2)}s`;
}

function clampNumber(value: number, minimum: number, maximum: number): number {
  return Math.min(Math.max(value, minimum), maximum);
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
