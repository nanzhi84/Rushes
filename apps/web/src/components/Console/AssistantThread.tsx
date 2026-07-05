import type { ReactElement } from "react";
import { StructuredInteractionRenderer } from "./StructuredInteractionRenderer";
import type { AnswerDecisionHandler } from "./StructuredInteractionRenderer";
import type {
  ConsoleAssistantMessage,
  ConsoleDataMessagePart,
  ConsoleExternalStoreRuntime
} from "./runtime";

export function AssistantThread({
  runtime,
  onAnswerDecision,
  answerPending
}: {
  runtime: ConsoleExternalStoreRuntime;
  onAnswerDecision: AnswerDecisionHandler;
  answerPending: boolean;
}): ReactElement {
  return (
    <div className="min-h-0 flex-1 space-y-3 overflow-y-auto p-4" aria-label="消息列表">
      {runtime.messages.length === 0 ? (
        <p className="rounded-md border border-dashed border-[#cbd5e1] px-4 py-6 text-sm text-[#64748b]">
          这里会显示当前剪辑任务的消息流。
        </p>
      ) : (
        runtime.messages.map((message) => (
          <MessageRow
            key={message.id}
            message={message}
            onAnswerDecision={onAnswerDecision}
            answerPending={answerPending}
          />
        ))
      )}
    </div>
  );
}

function MessageRow({
  message,
  onAnswerDecision,
  answerPending
}: {
  message: ConsoleAssistantMessage;
  onAnswerDecision: AnswerDecisionHandler;
  answerPending: boolean;
}): ReactElement {
  if (message.metadata.consoleRole === "system_observation") {
    return (
      <details className="mx-auto max-w-[84%] rounded-md bg-[#f8fafc] px-4 py-3 text-sm text-[#475569]">
        <summary className="cursor-pointer text-xs font-medium text-[#64748b]">系统观察</summary>
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
      <div className="mr-auto max-w-[88%] space-y-3">
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
    <article className={messageClass(message)}>
      <span className="text-xs font-medium uppercase text-[#64748b]">{roleLabel(message)}</span>
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

function messageClass(message: ConsoleAssistantMessage): string {
  if (message.role === "user") {
    return "ml-auto max-w-[80%] rounded-lg bg-[#17202a] px-4 py-3 text-white";
  }
  if (message.role === "assistant") {
    return "mr-auto max-w-[80%] rounded-lg bg-[#eef2f7] px-4 py-3 text-[#17202a]";
  }
  return "mx-auto max-w-[80%] rounded-md bg-[#f8fafc] px-4 py-3 text-[#475569]";
}

function roleLabel(message: ConsoleAssistantMessage): string {
  if (message.role === "user") {
    return "用户";
  }
  if (message.role === "assistant") {
    return "助手";
  }
  return "系统";
}
