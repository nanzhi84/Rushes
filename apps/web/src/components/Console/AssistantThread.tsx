import type { ReactElement } from "react";
import { Markdown } from "./Markdown";
import { StructuredInteractionRenderer } from "./StructuredInteractionRenderer";
import type { AnswerDecisionHandler } from "./StructuredInteractionRenderer";
import type {
  ConsoleAssistantMessage,
  ConsoleDataMessagePart,
  ConsoleExternalStoreRuntime
} from "./runtime";
import type {
  StreamMessageItem,
  StreamToolItem,
  SubagentProgressEntry,
  TurnStreamItem
} from "./useTurnStream";

const STRUCTURED_MESSAGE_ID = "structured-interactions";

export function AssistantThread({
  runtime,
  onAnswerDecision,
  answerPending,
  highlightedMessageId = null,
  streamItems = [],
  subagentProgress = []
}: {
  runtime: ConsoleExternalStoreRuntime;
  onAnswerDecision: AnswerDecisionHandler;
  answerPending: boolean;
  highlightedMessageId?: string | null;
  streamItems?: TurnStreamItem[];
  subagentProgress?: SubagentProgressEntry[];
}): ReactElement {
  // 结构化交互卡（决策/进度）固定排在最后：本回合流式内容之后才是待回答的卡片。
  const regularMessages = runtime.messages.filter((message) => message.id !== STRUCTURED_MESSAGE_ID);
  const structuredMessage =
    runtime.messages.find((message) => message.id === STRUCTURED_MESSAGE_ID) ?? null;
  // 子代理进度挂在「当前进行中工具行」下方——不特判工具名，取最后一个 running 工具步即可。
  const activeToolStepId = findActiveToolStepId(streamItems);
  const isEmpty = regularMessages.length === 0 && streamItems.length === 0 && !structuredMessage;
  return (
    <div className="min-h-0 flex-1 space-y-3 overflow-y-auto p-4" aria-label="消息列表">
      {isEmpty ? (
        <p className="rounded-md border border-dashed border-line-strong px-4 py-6 text-sm text-fg-muted">
          这里会显示当前剪辑任务的消息流。
        </p>
      ) : (
        regularMessages.map((message) => (
          <MessageRow
            key={message.id}
            message={message}
            onAnswerDecision={onAnswerDecision}
            answerPending={answerPending}
            highlighted={highlightedMessageId === message.id}
          />
        ))
      )}
      {streamItems.map((item) =>
        item.type === "message" ? (
          <MessageRow
            key={item.message_id}
            message={toStreamMessage(item)}
            onAnswerDecision={onAnswerDecision}
            answerPending={answerPending}
            highlighted={highlightedMessageId === item.message_id}
          />
        ) : (
          <ToolStepRow
            key={item.step_id}
            step={item}
            progress={item.step_id === activeToolStepId ? subagentProgress : []}
          />
        )
      )}
      {structuredMessage ? (
        <MessageRow
          message={structuredMessage}
          onAnswerDecision={onAnswerDecision}
          answerPending={answerPending}
          highlighted={highlightedMessageId === structuredMessage.id}
        />
      ) : null}
    </div>
  );
}

// 流式消息复用 MessageRow 的持久化消息形状：id 就是 message_id，落库后由历史接管同一行。
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
  highlighted
}: {
  message: ConsoleAssistantMessage;
  onAnswerDecision: AnswerDecisionHandler;
  answerPending: boolean;
  highlighted: boolean;
}): ReactElement {
  if (message.metadata.consoleRole === "system_observation") {
    return (
      <details
        data-console-message-id={message.id}
        className={`${highlightClass(highlighted)} mx-auto max-w-[84%] rounded-md bg-raised px-4 py-3 text-sm text-fg-muted shadow-raised`}
      >
        <summary className="cursor-pointer text-xs font-medium text-fg-muted">系统观察</summary>
        {message.content.map((part, index) =>
          part.type === "text" ? (
            <p key={`${message.id}:${index}`} className="mt-2 whitespace-pre-wrap leading-6">
              {part.text}
            </p>
          ) : null
        )}
      </details>
    );
  }

  const dataParts = message.content.filter(
    (part): part is ConsoleDataMessagePart => part.type === "data"
  );
  if (dataParts.length > 0) {
    return (
      <div
        data-console-message-id={message.id}
        className={`${highlightClass(highlighted)} mr-auto max-w-[88%] space-y-3 rounded-md`}
      >
        {dataParts.map((part) => (
          <StructuredInteractionRenderer
            key={part.data.id}
            item={part.data}
            onAnswerDecision={onAnswerDecision}
            answerPending={answerPending}
          />
        ))}
      </div>
    );
  }

  const renderAsMarkdown = message.role === "assistant";
  return (
    <article
      data-console-message-id={message.id}
      data-message-kind={message.metadata.messageKind ?? undefined}
      className={messageClass(message, highlighted)}
    >
      {message.content.map((part, index) =>
        part.type === "text" ? (
          renderAsMarkdown ? (
            <div key={`${message.id}:${index}`} className="text-[0.95rem]">
              <Markdown text={part.text} />
            </div>
          ) : (
            <p key={`${message.id}:${index}`} className="whitespace-pre-wrap leading-7">
              {part.text}
            </p>
          )
        ) : null
      )}
    </article>
  );
}

function isNarration(message: ConsoleAssistantMessage): boolean {
  return message.role === "assistant" && message.metadata.messageKind === "narration";
}

function messageClass(message: ConsoleAssistantMessage, highlighted: boolean): string {
  const highlight = highlightClass(highlighted);
  if (message.role === "user") {
    // accent 减负：用户气泡改低饱和深底 + accent 描边，不再整块铺纯橙。
    return `${highlight} ml-auto max-w-[80%] rounded-lg border border-user-bubble-border bg-user-bubble px-4 py-3 text-fg shadow-raised`;
  }
  if (message.role === "assistant") {
    // narration 叙述用弱化样式：更浅的底色与文字，和正式 reply 拉开层级。
    if (isNarration(message)) {
      return `${highlight} mr-auto max-w-[88%] rounded-lg px-1 py-0.5 text-sm text-fg-muted`;
    }
    return `${highlight} mr-auto max-w-[88%] rounded-lg bg-raised px-4 py-3 text-fg shadow-raised`;
  }
  return `${highlight} mx-auto max-w-[80%] rounded-md bg-raised px-4 py-3 text-fg-muted shadow-raised`;
}

function highlightClass(highlighted: boolean): string {
  return highlighted ? "ring-2 ring-accent ring-offset-2 ring-offset-ink" : "";
}

/** Claude Code 式工具行：状态圆点 + 中文名 + 参数摘要一行带过，结果可展开。 */
function ToolStepRow({
  step,
  progress = []
}: {
  step: StreamToolItem;
  progress?: SubagentProgressEntry[];
}): ReactElement {
  const label = TOOL_STEP_LABELS[step.tool] ?? step.tool;
  const summaryRow = (
    <span className="flex min-w-0 items-center gap-2">
      <span
        aria-hidden
        className={`${toolStatusToneClass(step.status)} ${step.status === "running" ? "tile-pulse" : ""} text-[0.6rem] leading-none`}
      >
        ●
      </span>
      <span className="shrink-0 text-sm text-fg">{label}</span>
      {step.argsSummary ? (
        <span className="min-w-0 truncate font-mono text-xs text-fg-faint">{step.argsSummary}</span>
      ) : null}
      <span className="ml-auto shrink-0 pl-2 text-xs text-fg-faint">
        {toolStatusLabel(step.status)}
      </span>
    </span>
  );

  const toolRow = !step.observation ? (
    <div data-tool-step-id={step.step_id} data-tool-status={step.status} className="px-1 py-0.5">
      {summaryRow}
    </div>
  ) : (
    <details data-tool-step-id={step.step_id} data-tool-status={step.status} className="px-1 py-0.5">
      <summary className="cursor-pointer list-none [&::-webkit-details-marker]:hidden">
        {summaryRow}
      </summary>
      <div className="mt-1 border-l-2 border-line pl-3 text-xs leading-5 text-fg-muted whitespace-pre-wrap">
        {step.observation}
      </div>
    </details>
  );

  return (
    <div className="mr-auto w-full max-w-[88%]">
      {toolRow}
      {progress.length > 0 ? (
        <ul className="mt-0.5 space-y-0.5 pl-5" aria-label="子代理进度">
          {progress.map((entry) => (
            <SubagentProgressRow key={entry.asset_id} entry={entry} />
          ))}
        </ul>
      ) : null}
    </div>
  );
}

/** 进度行：次级弱化文本 + ↳ 缩进，读作当前工具行的子活动（对齐工具行的次级信息层级）。 */
function SubagentProgressRow({ entry }: { entry: SubagentProgressEntry }): ReactElement {
  return (
    <li
      data-subagent-progress-asset={entry.asset_id}
      className="flex min-w-0 items-baseline gap-1.5 text-xs leading-5"
    >
      <span aria-hidden className="shrink-0 text-fg-faint">
        ↳
      </span>
      {showAssetPrefix(entry.note) ? (
        <span className="max-w-[8rem] shrink-0 truncate font-mono text-[0.7rem] text-fg-faint">
          {entry.asset_id}
        </span>
      ) : null}
      <span className="min-w-0 truncate text-fg-muted">{entry.note}</span>
    </li>
  );
}

// 取最后一个 running 工具步作为「当前进行中工具行」；无进行中工具则不挂进度。
function findActiveToolStepId(items: TurnStreamItem[]): string | null {
  for (let index = items.length - 1; index >= 0; index -= 1) {
    const item = items[index];
    if (item.type === "tool" && item.status === "running") {
      return item.step_id;
    }
  }
  return null;
}

// note 已含文件名（如 IMG_2031.mp4）时，文件名本身即可辨认素材，不再叠加不友好的 asset_id；
// 否则（"转写音频中"等通用文案）用 asset_id 前缀区分并发的多个素材。
function showAssetPrefix(note: string): boolean {
  return !/\.[a-z][a-z0-9]{1,4}\b/i.test(note);
}

function toolStatusToneClass(status: string): string {
  switch (status) {
    case "succeeded":
      return "text-ok";
    case "failed":
    case "deny":
      return "text-danger";
    case "ask":
      return "text-warn";
    default:
      return "text-fg-faint";
  }
}

function toolStatusLabel(status: string): string {
  return TOOL_STATUS_LABELS[status] ?? status;
}

const TOOL_STATUS_LABELS: Record<string, string> = {
  running: "进行中",
  succeeded: "完成",
  failed: "失败",
  deny: "已拒绝",
  ask: "待确认",
  requires_user: "待回答"
};

// 工具名 → 中文文案集中映射；未映射时回退到原始工具名。
const TOOL_STEP_LABELS: Record<string, string> = {
  "asset.list_assets": "清点素材",
  "asset.import_local_file": "导入本地素材",
  "asset.import_url": "URL 导入素材",
  "understand.materials": "理解素材",
  "decision.answer": "记录你的回答",
  "timeline.apply_patch": "修改时间线",
  "timeline.compose_initial": "生成初版时间线",
  "timeline.restore_version": "恢复时间线版本",
  "timeline.validate": "校验时间线",
  "timeline.inspect": "查看时间线",
  "render.preview": "渲染预览",
  "render.final_mp4": "导出成片",
  "render.status": "查询渲染进度",
  "content.create_plan": "生成内容方案",
  "content.revise_plan": "修订内容方案",
  "audio.generate_tts": "生成配音",
  "audio.rough_cut_speech": "粗剪口播",
  "audio.asr_original": "识别原声",
  "audio.align_uploaded_voiceover": "对齐上传配音",
  "audio.inspect_sources": "检查音频素材",
  "media.view_frames": "查看画面",
  "interaction.ask_user": "向你提问",
  "interaction.confirm_action": "请求确认",
  "interaction.show_preview": "展示预览",
  "interaction.show_timeline": "展示时间线",
  "interaction.show_progress": "更新进度",
  "interaction.show_error": "展示错误",
  "memory.search_relevant": "检索记忆",
  "memory.save": "保存记忆",
  "memory.ask_scope": "询问记忆范围",
  "memory.extract_from_draft": "提炼记忆"
};
