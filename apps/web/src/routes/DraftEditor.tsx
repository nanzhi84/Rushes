import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useParams } from "@tanstack/react-router";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { ReactElement } from "react";
import { X } from "lucide-react";
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
import { createApiEventSource } from "../auth";
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
      const event = payload.event;
      if (event.event === "TurnEnded") {
        setAwaitingTurnEnd(false);
      }
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
  const refreshMessages = useCallback(() => {
    void queryClient.invalidateQueries({ queryKey: queryKeys.messages(draftId) });
  }, [draftId, queryClient]);
  const { items: streamItems, subagentProgress } = useTurnStream(draftId, {
    onTurnEnded: refreshMessages
  });

  // 当前回合的消息以流式列表为准（与工具行按到达顺序交错展示），历史里同
  // message_id 的落库副本让位，避免同一条消息渲染两遍且丢失工具行的位置。
  const messages = useMemo<ConsoleMessage[]>(() => {
    const streamMessageIds = new Set(
      streamItems
        .filter((item) => item.type === "message")
        .map((item) => item.message_id)
    );
    return historyMessages.filter((message) => !streamMessageIds.has(message.id));
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

  const timelineDurationSec = useMemo(() => {
    const timeline = timelinePayload?.timeline;
    if (!timeline) {
      return 0;
    }
    const fps = timeline.fps > 0 ? timeline.fps : 30;
    return timeline.duration_frames / fps;
  }, [timelinePayload?.timeline]);

  return (
    <div className="flex h-screen min-h-0 flex-col bg-ink text-fg">
      <TopBar
        connectionState={connectionState}
        showSettings={false}
        leading={
          <>
            <Link
              aria-label="返回草稿"
              className="grid h-8 w-8 shrink-0 place-items-center rounded-md text-fg-muted hover:bg-hover hover:text-fg"
              to="/"
            >
              <HomeGlyph />
            </Link>
            <span className="truncate text-sm font-semibold">{draftName}</span>
            <button
              className="grid h-7 w-7 shrink-0 place-items-center rounded-md text-fg-muted hover:bg-hover hover:text-fg"
              type="button"
              aria-label="重命名草稿"
              onClick={() => openEntityDialog({ kind: "renameDraft", draftId })}
            >
              <PencilGlyph />
            </button>
          </>
        }
        trailing={
          <div className="flex items-center gap-3">
            <span
              className="rounded-md bg-raised px-2 py-1 text-xs tabular-nums text-fg-muted"
              aria-label="本草稿成本小计"
              title="本草稿累计成本估算（人民币）"
            >
              {formatCost(totalCost)}
            </span>
            <button
              className="rounded-md bg-accent px-3 py-1.5 text-sm font-medium text-white hover:bg-accent-strong disabled:opacity-40"
              type="button"
              disabled={disabled || timelineVersion === null}
              onClick={handleExport}
            >
              导出
            </button>
          </div>
        }
      />

      {/* 三栏行：左对话（拖宽）| 中素材（拖宽）| 右预览（弹性）；时间线在其下全宽通栏。 */}
      <div className="flex min-h-0 flex-1">
        {/* 左：剪辑对话 */}
        <aside
          className="flex min-h-0 shrink-0 flex-col bg-panel"
          style={{ width: chatPanelWidth }}
          aria-label="剪辑对话"
        >
          <div className="flex shrink-0 items-center justify-between border-b border-line px-3 py-2">
            <span className="text-sm font-semibold">剪辑对话</span>
            <span className="text-xs text-fg-faint">{statusLabel}</span>
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
            <div className="shrink-0 border-t border-line p-3" aria-label="当前确认项">
              <StructuredInteractionRenderer
                item={sideDecisionItem}
                onAnswerDecision={handleAnswerDecision}
                answerPending={answerDecision.isPending}
              />
            </div>
          ) : null}

          <form
            className="shrink-0 border-t border-line p-3"
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
            <textarea
              aria-label="消息输入"
              className="h-20 w-full resize-none rounded-md border border-line bg-ink px-3 py-2 text-sm text-fg outline-none placeholder:text-fg-faint focus:border-accent disabled:bg-raised"
              value={draft}
              onChange={(event) => setDraft(event.target.value)}
              disabled={!runtime.canSubmit}
              placeholder={runtime.isRunning ? "等待本轮结束…" : "告诉代理要怎么剪"}
            />
            <div className="mt-2 flex items-center justify-between gap-3">
              <p className="text-xs text-fg-faint">
                {runtime.isRunning ? "输入框会在本轮结束后恢复。" : "消息会进入后端任务队列。"}
              </p>
              <button
                className="rounded-md bg-accent px-4 py-1.5 text-sm font-medium text-white hover:bg-accent-strong disabled:opacity-40"
                type="submit"
                disabled={!runtime.canSubmit || draft.trim().length === 0}
              >
                发送
              </button>
            </div>
          </form>
        </aside>

        <ResizeHandle
          orientation="vertical"
          value={chatPanelWidth}
          onChange={setChatPanelWidth}
          ariaLabel="调整对话面板宽度"
        />

        {/* 中：素材面板（可拖宽） */}
        <div
          data-testid="materials-panel"
          className="min-h-0 shrink-0 border-r border-line bg-panel"
          style={{ width: materialsPanelWidth }}
        >
          <AssetsPanel
            draftId={draftId}
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

        {/* 右：预览（弹性）——选中素材时试看原片，否则回到成片/占位 */}
        <section className="min-h-0 min-w-0 flex-1 p-3" aria-label="预览区">
          {previewingAsset ? (
            <AssetPreviewPane asset={previewingAsset} onClose={closeAssetPreview} />
          ) : timelineVersion === null ? (
            <PreviewPlaceholder text="暂无时间线。让代理开始剪辑后，这里会出现成片预览。" />
          ) : timelineQuery.isPending ? (
            <PreviewPlaceholder text="时间线加载中…" />
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
        </section>
      </div>

      {/* 全宽拖高手柄 + 全宽通栏时间线（三栏行下方，通栏跨越对话/素材/预览）。 */}
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
        <div className="flex shrink-0 flex-wrap items-center gap-3 border-b border-line px-3 py-1.5">
          <h2 className="text-sm font-semibold">时间线</h2>
          {timelineVersion !== null ? (
            <select
              aria-label="时间线版本"
              className="rounded-md border border-line bg-ink px-2 py-1 text-xs text-fg outline-none focus:border-accent"
              value={effectiveVersion ?? ""}
              onChange={(event) => {
                const next = Number(event.target.value);
                setViewedVersion(next === timelineVersion ? null : next);
              }}
            >
              {versionOptions(timelineVersion).map((version) => (
                <option key={version} value={version}>
                  v{version}
                  {version === timelineVersion ? "（当前）" : ""}
                </option>
              ))}
            </select>
          ) : null}
          {viewingHistory ? (
            <span className="rounded bg-warn/15 px-2 py-0.5 text-xs text-warn">
              正在查看历史版本，恢复请在对话中告诉代理
            </span>
          ) : null}
          {timelinePayload?.summary ? (
            <span className="hidden max-w-[320px] truncate text-xs text-fg-faint lg:inline">
              {timelinePayload.summary}
            </span>
          ) : null}
          <div className="ml-auto flex items-center gap-2">
            <span className="text-xs tabular-nums text-fg-muted">
              {formatTimecode(playheadSec ?? 0)} / {formatTimecode(timelineDurationSec)}
            </span>
            <button
              type="button"
              className="grid h-6 w-6 place-items-center rounded-md border border-line text-sm text-fg-muted hover:bg-hover disabled:opacity-40"
              aria-label="缩小时间线"
              onClick={zoomOutTimeline}
              disabled={pxPerSec <= TIMELINE_ZOOM_LEVELS[0]}
            >
              −
            </button>
            <span className="text-xs tabular-nums text-fg-faint">{pxPerSec}px/s</span>
            <button
              type="button"
              className="grid h-6 w-6 place-items-center rounded-md border border-line text-sm text-fg-muted hover:bg-hover disabled:opacity-40"
              aria-label="放大时间线"
              onClick={zoomInTimeline}
              disabled={pxPerSec >= TIMELINE_ZOOM_LEVELS[TIMELINE_ZOOM_LEVELS.length - 1]}
            >
              ＋
            </button>
            <button
              type="button"
              className="rounded-md border border-line px-2 py-1 text-xs text-fg-muted hover:bg-hover"
              onClick={fitTimeline}
            >
              适应
            </button>
          </div>
        </div>

        <div ref={timelineBodyRef} className="min-h-0 flex-1 overflow-auto">
          {timelineVersion === null ? (
            <p className="p-4 text-sm text-fg-muted">暂无时间线。</p>
          ) : timelineQuery.isPending ? (
            <p className="p-4 text-sm text-fg-muted">时间线加载中…</p>
          ) : timelinePayload ? (
            <TimelineViewer
              timeline={timelinePayload.timeline}
              pxPerSec={pxPerSec}
              playheadSec={playheadSec}
              selectedClipId={selectedClipId}
              onClipClick={handleClipClick}
              onSeek={handleTimelineSeek}
              waveformSrc={previewSrc}
            />
          ) : (
            <p className="p-4 text-sm text-fg-muted">时间线暂不可用。</p>
          )}
        </div>

        {unmatchedClipDetail ? <ClipDetailBar detail={unmatchedClipDetail} /> : null}
      </section>

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
    <div className="grid h-full place-items-center rounded-lg border border-dashed border-line-strong">
      <p className="max-w-[260px] text-center text-sm leading-6 text-fg-muted">{text}</p>
    </div>
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
    <div className="flex h-full min-h-0 flex-col overflow-hidden rounded-lg border border-line bg-panel">
      <div className="flex shrink-0 items-center justify-between gap-2 border-b border-line px-3 py-2">
        <span className="truncate text-sm text-fg" title={`试看 · ${asset.filename || asset.asset_id}`}>
          试看 · {asset.filename || asset.asset_id}
        </span>
        <button
          type="button"
          className="grid size-7 shrink-0 place-items-center rounded-md text-fg-muted transition-colors ease-standard hover:bg-hover hover:text-fg"
          aria-label="关闭试看"
          title="关闭试看（Esc）"
          onClick={onClose}
        >
          <X size={16} strokeWidth={1.75} aria-hidden />
        </button>
      </div>
      <div className="grid min-h-0 flex-1 place-items-center overflow-hidden bg-black p-3">
        <AssetMediaPreview asset={asset} />
      </div>
    </div>
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
    <div className="shrink-0 border-t border-line bg-raised px-3 py-2 text-xs leading-5 text-fg-muted">
      <span className="font-semibold text-fg">已选片段：</span>
      <span className="font-mono">{detail.clipId}</span>
      <span className="mx-2 text-fg-faint">|</span>
      <span>轨道 {detail.trackId}</span>
      <span className="mx-2 text-fg-faint">|</span>
      <span>
        {formatSeconds(detail.startSec)} - {formatSeconds(detail.endSec)}
      </span>
      {detail.label ? (
        <>
          <span className="mx-2 text-fg-faint">|</span>
          <span>{detail.label}</span>
        </>
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
    return "¥—";
  }
  return `¥${total.toFixed(4)}`;
}

function HomeGlyph(): ReactElement {
  return (
    <svg aria-hidden width="16" height="16" viewBox="0 0 24 24" fill="none">
      <path
        d="M4 11.5 12 4l8 7.5M6 10v9h12v-9"
        stroke="currentColor"
        strokeWidth="1.8"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </svg>
  );
}

function PencilGlyph(): ReactElement {
  return (
    <svg aria-hidden width="13" height="13" viewBox="0 0 24 24" fill="none">
      <path
        d="m4 20 .8-4L16 4.8a2 2 0 0 1 2.8 0l.4.4a2 2 0 0 1 0 2.8L8 19.2 4 20Z"
        stroke="currentColor"
        strokeWidth="1.8"
        strokeLinejoin="round"
      />
    </svg>
  );
}

const TIMELINE_ZOOM_LEVELS = [12, 24, 48, 96, 192];
const DEFAULT_TIMELINE_PX_PER_SEC = 96;
const TIMELINE_LABEL_WIDTH = 112;
const MAX_VERSION_OPTIONS = 50;
