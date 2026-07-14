import { useEffect, useMemo, useRef, useState } from "react";
import type { ReactElement, UIEvent } from "react";
import {
  ChevronRight,
  CornerDownRight,
  LoaderCircle,
  TerminalSquare
} from "lucide-react";
import { Markdown } from "./Markdown";
import {
  DecisionInteractionGroup,
  StructuredInteractionRenderer
} from "./StructuredInteractionRenderer";
import type {
  AnswerDecisionHandler,
  DecisionInteractionItem
} from "./StructuredInteractionRenderer";
import type {
  ConsoleAssistantMessage,
  ConsoleDataMessagePart,
  ConsoleExternalStoreRuntime
} from "./runtime";
import type {
  ModelRetryState,
  StreamMessageItem,
  StreamToolItem,
  SubagentProgressEntry,
  TurnStreamItem
} from "./useTurnStream";
import { formatElapsedTime, useElapsedSeconds } from "./useElapsedTime";

const STRUCTURED_MESSAGE_ID = "structured-interactions";
const FOLLOW_THRESHOLD_PX = 48;

type HistoryBlock =
  | { type: "message"; message: ConsoleAssistantMessage }
  | { type: "activity"; id: string; messages: ConsoleAssistantMessage[] }
  | { type: "tools"; id: string; steps: StreamToolItem[] };

type StreamBlock =
  | { type: "message"; message: StreamMessageItem }
  | { type: "tools"; id: string; steps: StreamToolItem[] };

export function AssistantThread({
  runtime,
  onAnswerDecision,
  answerPending,
  highlightedMessageId = null,
  streamItems = [],
  modelRetry = null,
  subagentProgress = []
}: {
  runtime: ConsoleExternalStoreRuntime;
  onAnswerDecision: AnswerDecisionHandler;
  answerPending: boolean;
  highlightedMessageId?: string | null;
  streamItems?: TurnStreamItem[];
  modelRetry?: ModelRetryState | null;
  subagentProgress?: SubagentProgressEntry[];
}): ReactElement {
  const scrollerRef = useRef<HTMLDivElement | null>(null);
  const followLatestRef = useRef(true);
  const [hasNewOutput, setHasNewOutput] = useState(false);

  // 结构化交互固定排在当前回合之后，避免确认卡被新的工具输出挤到中间。
  const regularMessages = runtime.messages.filter((message) => message.id !== STRUCTURED_MESSAGE_ID);
  const structuredMessage =
    runtime.messages.find((message) => message.id === STRUCTURED_MESSAGE_ID) ?? null;
  const historyBlocks = useMemo(() => groupHistoryMessages(regularMessages), [regularMessages]);
  const streamBlocks = useMemo(() => groupStreamItems(streamItems), [streamItems]);
  const activeToolStepId = findActiveToolStepId(streamItems);
  const isEmpty =
    historyBlocks.length === 0 &&
    streamBlocks.length === 0 &&
    !structuredMessage &&
    !runtime.isRunning;

  const streamFingerprint = useMemo(
    () =>
      streamItems
        .map((item) =>
          item.type === "message"
            ? `${item.message_id}:${item.kind}:${item.text.length}`
            : `${item.step_id}:${item.status}:${item.observation?.length ?? 0}`
        )
        .join("|"),
    [streamItems]
  );
  const progressFingerprint = subagentProgress
    .map((entry) => `${entry.asset_id}:${entry.note}`)
    .join("|");
  const retryFingerprint = modelRetry
    ? `${modelRetry.attempt}:${modelRetry.maxRetries}:${modelRetry.reason}`
    : "";

  // Claude Code 式 follow mode：用户停留在底部时持续追随增量；手动上滚后不抢滚动位置。
  useEffect(() => {
    const scroller = scrollerRef.current;
    if (!scroller) {
      return;
    }
    if (followLatestRef.current) {
      scroller.scrollTop = scroller.scrollHeight;
      setHasNewOutput(false);
    } else if (runtime.isRunning) {
      setHasNewOutput(true);
    }
  }, [
    historyBlocks.length,
    progressFingerprint,
    retryFingerprint,
    runtime.isRunning,
    streamFingerprint,
    structuredMessage
  ]);

  const handleScroll = (event: UIEvent<HTMLDivElement>) => {
    const scroller = event.currentTarget;
    const distance = scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight;
    const followsLatest = distance <= FOLLOW_THRESHOLD_PX;
    followLatestRef.current = followsLatest;
    if (followsLatest) {
      setHasNewOutput(false);
    }
  };

  const jumpToLatest = () => {
    const scroller = scrollerRef.current;
    if (!scroller) {
      return;
    }
    followLatestRef.current = true;
    scroller.scrollTop = scroller.scrollHeight;
    setHasNewOutput(false);
  };

  return (
    <div className="relative min-h-0 flex-1">
      <div
        ref={scrollerRef}
        className="absolute inset-0 overflow-y-auto px-4 py-4"
        aria-label="消息列表"
        onScroll={handleScroll}
      >
        {isEmpty ? (
          <div className="flex min-h-40 items-start gap-2.5 px-1 py-6 text-fg-faint">
            <TerminalSquare size={15} strokeWidth={1.7} aria-hidden className="mt-0.5 shrink-0" />
            <p className="max-w-64 text-xs leading-5">
              描述成片目标、节奏或要删除的内容。剪辑过程和工具调用会持续显示在这里。
            </p>
          </div>
        ) : (
          <div className="space-y-2.5">
            {historyBlocks.map((block) =>
              block.type === "activity" ? (
                <BackgroundActivityGroup key={block.id} messages={block.messages} />
              ) : block.type === "tools" ? (
                <ToolActivityGroup
                  key={block.id}
                  steps={block.steps}
                  activeToolStepId={null}
                  progress={[]}
                />
              ) : (
                <MessageRow
                  key={block.message.id}
                  message={block.message}
                  onAnswerDecision={onAnswerDecision}
                  answerPending={answerPending}
                  highlighted={highlightedMessageId === block.message.id}
                />
              )
            )}

            {streamBlocks.map((block) =>
              block.type === "message" ? (
                <MessageRow
                  key={block.message.message_id}
                  message={toStreamMessage(block.message)}
                  onAnswerDecision={onAnswerDecision}
                  answerPending={answerPending}
                  highlighted={highlightedMessageId === block.message.message_id}
                  streaming={block.message.kind === "assistant"}
                />
              ) : (
                <ToolActivityGroup
                  key={block.id}
                  steps={block.steps}
                  activeToolStepId={activeToolStepId}
                  progress={subagentProgress}
                />
              )
            )}

            {runtime.isRunning ? (
              <TurnActivityIndicator items={streamItems} modelRetry={modelRetry} />
            ) : null}

            {structuredMessage ? (
              <MessageRow
                message={structuredMessage}
                onAnswerDecision={onAnswerDecision}
                answerPending={answerPending}
                highlighted={highlightedMessageId === structuredMessage.id}
              />
            ) : null}
          </div>
        )}
      </div>

      {hasNewOutput ? (
        <button
          className="absolute bottom-3 left-1/2 flex -translate-x-1/2 items-center gap-1.5 rounded-md border border-line-strong bg-raised px-2.5 py-1.5 text-xs font-medium text-fg shadow-overlay transition-[transform,background-color] duration-base ease-out-snappy hover:bg-hover active:translate-y-px"
          type="button"
          onClick={jumpToLatest}
        >
          <CornerDownRight size={13} strokeWidth={1.8} aria-hidden />
          查看最新输出
        </button>
      ) : null}
    </div>
  );
}

function TurnActivityIndicator({
  items,
  modelRetry
}: {
  items: TurnStreamItem[];
  modelRetry: ModelRetryState | null;
}): ReactElement {
  const elapsedSeconds = useElapsedSeconds(true);
  const label = turnActivityLabel(items, modelRetry);
  const elapsed = formatElapsedTime(elapsedSeconds);
  return (
    <div
      className="flex min-w-0 items-center gap-1.5 py-1 text-xs text-fg-muted"
      data-testid="turn-activity-indicator"
      data-turn-activity={label}
    >
      <LoaderCircle
        size={13}
        strokeWidth={1.8}
        aria-hidden
        className="shrink-0 animate-spin text-accent"
      />
      <span className="min-w-0 flex-1 truncate font-medium">{label}</span>
      <time
        className="shrink-0 font-mono text-2xs tabular-nums text-fg-faint"
        aria-label={`已用时 ${elapsedSeconds} 秒`}
        title={`当前任务已运行 ${elapsedSeconds} 秒`}
      >
        已用 {elapsed}
      </time>
      <span className="sr-only" role="status">
        当前任务进行中：{label}
      </span>
    </div>
  );
}

// 流式消息复用持久化消息形状；落库后由 message_id 去重并交给历史列表接管。
function toStreamMessage(item: StreamMessageItem): ConsoleAssistantMessage {
  return {
    id: item.message_id,
    role: "assistant",
    createdAt: "",
    metadata: { consoleRole: "assistant", messageKind: item.kind },
    content: [{ type: "text", text: item.text }]
  };
}

function MessageRow({
  message,
  onAnswerDecision,
  answerPending,
  highlighted,
  streaming = false
}: {
  message: ConsoleAssistantMessage;
  onAnswerDecision: AnswerDecisionHandler;
  answerPending: boolean;
  highlighted: boolean;
  streaming?: boolean;
}): ReactElement {
  if (message.metadata.consoleRole === "system_observation") {
    return <BackgroundActivityGroup messages={[message]} />;
  }

  if (isBackgroundActivity(message)) {
    return <BackgroundActivityGroup messages={[message]} />;
  }

  const dataParts = message.content.filter(
    (part): part is ConsoleDataMessagePart => part.type === "data"
  );
  if (dataParts.length > 0) {
    const decisionItems = dataParts.flatMap((part): DecisionInteractionItem[] =>
      part.data.kind === "decision" ? [part.data] : []
    );
    const otherParts = dataParts.filter((part) => part.data.kind !== "decision");
    return (
      <div
        className={`${highlightClass(highlighted)} w-full space-y-0.5`}
      >
        {otherParts.map((part) => (
          <StructuredInteractionRenderer
            key={part.data.id}
            item={part.data}
            onAnswerDecision={onAnswerDecision}
            answerPending={answerPending}
          />
        ))}
        {decisionItems.length > 0 ? (
          <DecisionInteractionGroup
            items={decisionItems}
            onAnswerDecision={onAnswerDecision}
            answerPending={answerPending}
          />
        ) : null}
      </div>
    );
  }

  if (message.role === "user") {
    return (
      <article
        data-message-kind={message.metadata.messageKind ?? undefined}
        className="flex w-full justify-end"
      >
        <div
          data-user-message=""
          className={`${highlightClass(highlighted)} w-fit max-w-[85%] rounded-sm bg-user-bubble px-3 py-1.5 text-[13px] leading-5 text-fg`}
        >
          {message.content.map((part, index) =>
            part.type === "text" ? (
              <p key={`${message.id}:${index}`} className="break-words whitespace-pre-wrap">
                {part.text}
              </p>
            ) : null
          )}
        </div>
      </article>
    );
  }

  const narration = isNarration(message);
  return (
    <article
      data-message-kind={message.metadata.messageKind ?? undefined}
      data-streaming={streaming ? "true" : undefined}
      className={`${highlightClass(highlighted)} ${
        narration
          ? "w-full py-0.5 text-xs text-fg-muted"
          : "w-full py-1 text-[13px] text-fg"
      }`}
    >
      <div className="min-w-0 flex-1">
        {message.content.map((part, index) =>
          part.type === "text" ? (
            <div key={`${message.id}:${index}`} className={narration ? "leading-5" : "leading-[1.55]"}>
              {narration ? <p className="whitespace-pre-wrap">{part.text}</p> : <Markdown text={part.text} />}
            </div>
          ) : null
        )}
        {streaming ? (
          <span className="sr-only" role="status">
            正在生成
          </span>
        ) : null}
      </div>
    </article>
  );
}

function BackgroundActivityGroup({
  messages
}: {
  messages: ConsoleAssistantMessage[];
}): ReactElement {
  const entries = compactActivityMessages(messages);
  const total = messages.length;
  return (
    <details
      data-testid="background-activity-group"
      data-background-count={total}
      data-layout="inline"
      className="group w-full text-xs text-fg-muted"
    >
      <summary className="-ml-1 inline-flex h-5 cursor-pointer list-none items-center gap-1.5 rounded-sm px-1 text-fg-muted transition-colors duration-fast hover:bg-hover hover:text-fg [&::-webkit-details-marker]:hidden">
        <span aria-hidden className="size-1.5 shrink-0 rounded-full bg-ok" />
        <span className="font-medium text-fg-muted">后台活动</span>
        <span className="font-mono text-2xs text-fg-faint">{total} 条</span>
        <ChevronRight
          size={12}
          strokeWidth={1.8}
          aria-hidden
          className="shrink-0 text-fg-faint transition-transform duration-base group-open:rotate-90"
        />
      </summary>
      <div className="mt-1 max-h-40 overflow-y-auto border-l-2 border-line-strong/50 pl-3">
        <ul className="space-y-1" aria-label="后台活动详情">
          {entries.map((entry) => (
            <li key={entry.text} className="flex items-start gap-2 text-xs leading-5 text-fg-muted">
              <span className="min-w-0 flex-1 whitespace-pre-wrap">{entry.text}</span>
              {entry.count > 1 ? (
                <span className="shrink-0 font-mono text-2xs text-fg-faint">×{entry.count}</span>
              ) : null}
            </li>
          ))}
        </ul>
      </div>
    </details>
  );
}

function ToolActivityGroup({
  steps,
  activeToolStepId,
  progress
}: {
  steps: StreamToolItem[];
  activeToolStepId: string | null;
  progress: SubagentProgressEntry[];
}): ReactElement {
  const isActive = steps.some((step) => step.status === "running");
  const hasIssue = steps.some((step) => ["failed", "deny", "ask", "requires_user"].includes(step.status));
  const wasActiveRef = useRef(isActive);
  const [expanded, setExpanded] = useState(isActive || hasIssue);

  useEffect(() => {
    if (isActive || hasIssue) {
      setExpanded(true);
    } else if (wasActiveRef.current) {
      setExpanded(false);
    }
    wasActiveRef.current = isActive;
  }, [hasIssue, isActive]);

  return (
    <details
      open={expanded}
      onToggle={(event) => setExpanded(event.currentTarget.open)}
      data-testid="tool-activity-group"
      data-layout="inline"
      className="group w-full text-xs text-fg-muted"
    >
      <summary className="-ml-1 flex min-h-5 cursor-pointer list-none items-start gap-1.5 rounded-sm px-1 py-0.5 transition-colors duration-fast hover:bg-hover [&::-webkit-details-marker]:hidden">
        <StatusDot status={hasIssue ? "failed" : isActive ? "running" : "succeeded"} />
        <span className="shrink-0 font-medium text-fg-muted">
          {isActive ? "正在使用工具" : hasIssue ? "工具调用需要处理" : "已使用工具"}
        </span>
        <span className="min-w-0 flex-1 truncate text-fg-faint" title={toolGroupSummary(steps)}>
          {toolGroupSummary(steps)}
        </span>
        {isActive ? <span className="shrink-0 text-2xs text-fg-faint">持续更新</span> : null}
        <ChevronRight
          size={12}
          strokeWidth={1.8}
          aria-hidden
          className="mt-0.5 shrink-0 text-fg-faint transition-transform duration-base group-open:rotate-90"
        />
      </summary>
      <div className="mt-1 border-l border-line-strong/50 pl-3">
        {steps.map((step) => (
          <ToolStepRow
            key={step.step_id}
            step={step}
            progress={step.step_id === activeToolStepId ? progress : []}
          />
        ))}
      </div>
    </details>
  );
}

function ToolStepRow({
  step,
  progress = []
}: {
  step: StreamToolItem;
  progress?: SubagentProgressEntry[];
}): ReactElement {
  const label = TOOL_STEP_LABELS[step.tool] ?? step.tool;
  const hasDetails = Boolean(step.argsSummary || step.observation);
  const summaryRow = (
    <span className="flex min-w-0 flex-1 items-start gap-1.5">
      <StatusDot status={step.status} />
      <span className="min-w-0 flex-1 break-words text-xs text-fg-muted">{label}</span>
      <span className={`shrink-0 text-2xs ${toolStatusToneClass(step.status)}`}>
        {toolStatusLabel(step.status)}
      </span>
      {hasDetails ? (
        <ChevronRight size={12} strokeWidth={1.7} aria-hidden className="shrink-0 text-fg-faint transition-transform duration-fast group-open/tool:rotate-90" />
      ) : null}
    </span>
  );

  return (
    <div className="py-0.5" data-tool-step-id={step.step_id} data-tool-status={step.status}>
      {hasDetails ? (
        <details className="group/tool">
          <summary className="flex cursor-pointer list-none items-center [&::-webkit-details-marker]:hidden">
            {summaryRow}
          </summary>
          <div className="ml-2.5 mt-1 border-l border-line pl-3">
            {step.argsSummary ? (
              <div className="mb-1.5">
                <p className="text-2xs font-medium text-fg-faint">输入</p>
                <pre className="mt-1 max-h-32 overflow-auto whitespace-pre-wrap break-all font-mono text-[0.65rem] leading-4 text-fg-muted">
                  {formatToolPayload(step.argsSummary)}
                </pre>
              </div>
            ) : null}
            {step.observation ? (
              <div>
                <p className="text-2xs font-medium text-fg-faint">结果</p>
                <pre className="mt-1 max-h-40 overflow-auto whitespace-pre-wrap break-all font-mono text-[0.65rem] leading-4 text-fg-muted">
                  {formatToolPayload(step.observation)}
                </pre>
              </div>
            ) : null}
          </div>
        </details>
      ) : (
        summaryRow
      )}

      {progress.length > 0 ? (
        <ul className="mt-1 space-y-1 pl-5" aria-label="子代理进度">
          {progress.map((entry) => (
            <SubagentProgressRow key={entry.asset_id} entry={entry} />
          ))}
        </ul>
      ) : null}
    </div>
  );
}

function SubagentProgressRow({ entry }: { entry: SubagentProgressEntry }): ReactElement {
  return (
    <li
      data-subagent-progress-asset={entry.asset_id}
      className="flex min-w-0 items-center gap-1.5 text-2xs leading-4"
    >
      <LoaderCircle size={11} strokeWidth={1.7} aria-hidden className="shrink-0 animate-spin text-accent" />
      {showAssetPrefix(entry.note) ? (
        <span className="max-w-[7rem] shrink-0 truncate font-mono text-[0.65rem] text-fg-faint">
          {entry.asset_id}
        </span>
      ) : null}
      <span className="min-w-0 truncate text-fg-muted">{entry.note}</span>
    </li>
  );
}

function StatusDot({ status }: { status: string }): ReactElement {
  return (
    <span
      aria-hidden
      className={`mt-1 size-1.5 shrink-0 rounded-full ${statusDotClass(status)} ${
        status === "running" ? "animate-pulse" : ""
      }`}
    />
  );
}

function groupHistoryMessages(messages: ConsoleAssistantMessage[]): HistoryBlock[] {
  const blocks: HistoryBlock[] = [];
  let activity: ConsoleAssistantMessage[] = [];
  let tools: StreamToolItem[] = [];
  const flushActivity = () => {
    if (activity.length === 0) {
      return;
    }
    blocks.push({ type: "activity", id: `activity:${activity[0].id}`, messages: activity });
    activity = [];
  };
  const flushTools = () => {
    if (tools.length === 0) {
      return;
    }
    blocks.push({ type: "tools", id: `history-tools:${tools[0].step_id}`, steps: tools });
    tools = [];
  };
  for (const message of messages) {
    if (message.metadata.messageKind === "tool") {
      const parsed = parsePersistedTool(message);
      flushActivity();
      if (parsed) {
        tools.push(parsed);
        continue;
      }
      flushTools();
      blocks.push({ type: "message", message });
      continue;
    }
    if (isBackgroundActivity(message) || message.metadata.consoleRole === "system_observation") {
      flushTools();
      activity.push(message);
      continue;
    }
    flushActivity();
    flushTools();
    blocks.push({ type: "message", message });
  }
  flushActivity();
  flushTools();
  return blocks;
}

function groupStreamItems(items: TurnStreamItem[]): StreamBlock[] {
  const blocks: StreamBlock[] = [];
  let tools: StreamToolItem[] = [];
  const flushTools = () => {
    if (tools.length === 0) {
      return;
    }
    blocks.push({ type: "tools", id: `tools:${tools[0].step_id}`, steps: tools });
    tools = [];
  };
  for (const item of items) {
    if (item.type === "tool") {
      tools.push(item);
      continue;
    }
    flushTools();
    blocks.push({ type: "message", message: item });
  }
  flushTools();
  return blocks;
}

function compactActivityMessages(messages: ConsoleAssistantMessage[]): Array<{ text: string; count: number }> {
  const entries: Array<{ text: string; count: number }> = [];
  for (const message of messages) {
    const text = messageText(message);
    const existing = entries.find((entry) => entry.text === text);
    if (existing) {
      existing.count += 1;
    } else {
      entries.push({ text, count: 1 });
    }
  }
  return entries;
}

function messageText(message: ConsoleAssistantMessage): string {
  return message.content
    .filter((part) => part.type === "text")
    .map((part) => (part.type === "text" ? part.text : ""))
    .join("\n")
    .trim();
}

function isBackgroundActivity(message: ConsoleAssistantMessage): boolean {
  if (message.metadata.messageKind === "observation") {
    return true;
  }
  return message.role === "assistant" && messageText(message).startsWith("后台任务已完成");
}

function isNarration(message: ConsoleAssistantMessage): boolean {
  return message.role === "assistant" && message.metadata.messageKind === "narration";
}

function findActiveToolStepId(items: TurnStreamItem[]): string | null {
  for (let index = items.length - 1; index >= 0; index -= 1) {
    const item = items[index];
    if (item.type === "tool" && item.status === "running") {
      return item.step_id;
    }
  }
  return null;
}

function turnActivityLabel(items: TurnStreamItem[], modelRetry: ModelRetryState | null): string {
  if (modelRetry) {
    return `${modelRetry.reason}，正在重试 ${modelRetry.attempt}/${modelRetry.maxRetries}`;
  }
  for (let index = items.length - 1; index >= 0; index -= 1) {
    const item = items[index];
    if (item.type === "tool" && item.status === "running") {
      return `正在${TOOL_STEP_LABELS[item.tool] ?? `执行 ${item.tool}`}`;
    }
  }
  const latest = items.at(-1);
  if (latest?.type === "message" && latest.kind === "assistant") {
    return "正在生成回复";
  }
  if (latest?.type === "message") {
    return latest.kind === "narration" ? "正在继续处理" : "正在收尾";
  }
  if (latest?.type === "tool") {
    return "正在整理工具结果";
  }
  return "正在读取上下文";
}

function showAssetPrefix(note: string): boolean {
  return !/\.[a-z][a-z0-9]{1,4}\b/i.test(note);
}

function formatToolPayload(value: string): string {
  try {
    return JSON.stringify(JSON.parse(value), null, 2);
  } catch {
    return value;
  }
}

function parsePersistedTool(message: ConsoleAssistantMessage): StreamToolItem | null {
  try {
    const raw = JSON.parse(messageText(message)) as Record<string, unknown>;
    if (typeof raw.tool !== "string" || typeof raw.status !== "string") {
      return null;
    }
    return {
      type: "tool",
      step_id: typeof raw.step_id === "string" && raw.step_id ? raw.step_id : message.id,
      tool: raw.tool,
      status: raw.status,
      argsSummary: typeof raw.args_summary === "string" && raw.args_summary ? raw.args_summary : null,
      observation: typeof raw.observation === "string" && raw.observation ? raw.observation : null
    };
  } catch {
    return null;
  }
}

function toolGroupSummary(steps: StreamToolItem[]): string {
  const counts = new Map<string, number>();
  for (const step of steps) {
    const label = TOOL_STEP_LABELS[step.tool] ?? step.tool;
    counts.set(label, (counts.get(label) ?? 0) + 1);
  }
  return [...counts.entries()]
    .map(([label, count]) => (count > 1 ? `${label} ×${count}` : label))
    .join("、");
}

function statusDotClass(status: string): string {
  switch (status) {
    case "succeeded":
      return "bg-ok";
    case "failed":
    case "deny":
      return "bg-danger";
    case "ask":
    case "requires_user":
      return "bg-warn";
    default:
      return "bg-fg-faint";
  }
}

function toolStatusToneClass(status: string): string {
  switch (status) {
    case "succeeded":
      return "text-ok";
    case "failed":
    case "deny":
      return "text-danger";
    case "ask":
    case "requires_user":
      return "text-warn";
    default:
      return "text-fg-faint";
  }
}

function toolStatusLabel(status: string): string {
  return TOOL_STATUS_LABELS[status] ?? status;
}

function highlightClass(highlighted: boolean): string {
  return highlighted ? "ring-2 ring-accent ring-offset-2 ring-offset-panel" : "";
}

const TOOL_STATUS_LABELS: Record<string, string> = {
  running: "进行中",
  succeeded: "完成",
  failed: "失败",
  deny: "已拒绝",
  ask: "待确认",
  requires_user: "待回答"
};

const TOOL_STEP_LABELS: Record<string, string> = {
  "asset.list_assets": "清点素材",
  "asset.import_local_file": "导入本地素材",
  "understand.materials": "理解素材",
  "media.search_shots": "检索镜头",
  "audio.analyze_beats": "分析音乐节拍",
  "audio.analyze_speech_pauses": "分析口播气口",
  "decision.answer": "记录你的回答",
  "timeline.apply_patch": "修改时间线",
  "timeline.apply_patches": "批量修改时间线",
  "timeline.recut_to_beats": "按节拍重剪",
  "timeline.compose_initial": "生成初版时间线",
  "timeline.validate": "校验时间线",
  "timeline.inspect": "查看时间线",
  "render.preview": "渲染预览",
  "render.final_mp4": "导出成片",
  "render.status": "查询渲染进度",
  "render.inspect_preview": "检查成片",
  "interaction.ask_user": "向你提问",
  "interaction.confirm_action": "请求确认"
};
