import type { ReactElement } from "react";
import { StructuredInteractionRenderer } from "./StructuredInteractionRenderer";
import type { AnswerDecisionHandler } from "./StructuredInteractionRenderer";
import type {
  ConsoleAssistantMessage,
  ConsoleDataMessagePart,
  ConsoleExternalStoreRuntime
} from "./runtime";
import type { ToolStep } from "./useTurnStream";

export function AssistantThread({
  runtime,
  onAnswerDecision,
  answerPending,
  highlightedMessageId = null,
  toolSteps = []
}: {
  runtime: ConsoleExternalStoreRuntime;
  onAnswerDecision: AnswerDecisionHandler;
  answerPending: boolean;
  highlightedMessageId?: string | null;
  toolSteps?: ToolStep[];
}): ReactElement {
  const isEmpty = runtime.messages.length === 0 && toolSteps.length === 0;
  return (
    <div className="min-h-0 flex-1 space-y-3 overflow-y-auto p-4" aria-label="消息列表">
      {isEmpty ? (
        <p className="rounded-md border border-dashed border-line-strong px-4 py-6 text-sm text-fg-muted">
          这里会显示当前剪辑任务的消息流。
        </p>
      ) : (
        runtime.messages.map((message) => (
          <MessageRow
            key={message.id}
            message={message}
            onAnswerDecision={onAnswerDecision}
            answerPending={answerPending}
            highlighted={highlightedMessageId === message.id}
          />
        ))
      )}
      {toolSteps.length > 0 ? (
        <ul className="space-y-2" aria-label="工具执行过程">
          {toolSteps.map((step) => (
            <ToolStepRow key={step.step_id} step={step} />
          ))}
        </ul>
      ) : null}
    </div>
  );
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

  return (
    <article
      data-console-message-id={message.id}
      data-message-kind={message.metadata.messageKind ?? undefined}
      className={messageClass(message, highlighted)}
    >
      <span className="text-xs font-medium uppercase text-fg-muted">{roleLabel(message)}</span>
      {message.content.map((part, index) =>
        part.type === "text" ? (
          <p key={`${message.id}:${index}`} className="mt-1 whitespace-pre-wrap leading-7">
            {part.text}
          </p>
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
      return `${highlight} mr-auto max-w-[80%] rounded-lg border border-line bg-raised px-4 py-2 text-sm text-fg-muted shadow-raised`;
    }
    return `${highlight} mr-auto max-w-[80%] rounded-lg bg-raised px-4 py-3 text-fg shadow-raised`;
  }
  return `${highlight} mx-auto max-w-[80%] rounded-md bg-raised px-4 py-3 text-fg-muted shadow-raised`;
}

function highlightClass(highlighted: boolean): string {
  return highlighted ? "ring-2 ring-accent ring-offset-2 ring-offset-ink" : "";
}

function roleLabel(message: ConsoleAssistantMessage): string {
  if (message.role === "user") {
    return "用户";
  }
  if (message.role === "assistant") {
    return isNarration(message) ? "助手叙述" : "助手";
  }
  return "系统";
}

function ToolStepRow({ step }: { step: ToolStep }): ReactElement {
  const label = TOOL_STEP_LABELS[step.tool] ?? step.tool;
  return (
    <li
      data-tool-step-id={step.step_id}
      data-tool-status={step.status}
      className="mr-auto flex max-w-[80%] items-center gap-2 rounded-md border border-line bg-raised px-3 py-2 text-sm text-fg-muted shadow-raised"
    >
      <span aria-hidden className={toolStatusToneClass(step.status)}>
        {toolStatusIcon(step.status)}
      </span>
      <span className="flex-1">{label}</span>
      <span className="text-xs text-fg-faint">{toolStatusLabel(step.status)}</span>
    </li>
  );
}

function toolStatusIcon(status: string): string {
  switch (status) {
    case "succeeded":
      return "✓";
    case "failed":
    case "deny":
      return "✗";
    case "running":
      return "…";
    default:
      return "•";
  }
}

function toolStatusToneClass(status: string): string {
  switch (status) {
    case "succeeded":
      return "text-ok";
    case "failed":
    case "deny":
      return "text-danger";
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
  ask: "待确认"
};

// 工具名 → 中文文案集中映射；未映射时回退到原始工具名。
const TOOL_STEP_LABELS: Record<string, string> = {
  "timeline.apply_patch": "修改时间线",
  "timeline.restore_version": "恢复时间线版本",
  "timeline.validate": "校验时间线",
  "timeline.inspect": "查看时间线",
  "render.preview": "渲染预览",
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
