import { useMemo } from "react";
import type { StructuredInteractionItem } from "./StructuredInteractionRenderer";

export type ConsoleMessageRole = "user" | "assistant" | "system" | "system_observation";

export type ConsoleMessage = {
  id: string;
  role: ConsoleMessageRole;
  content: string;
  createdAt: string;
  // 持久化消息的 kind（narration/reply/user...），用于叙述弱化样式；乐观消息可省略。
  kind?: string | null;
};

export type ConsoleTextPart = {
  type: "text";
  text: string;
};

export type ConsoleDataMessagePart = {
  type: "data";
  data: StructuredInteractionItem;
};

export type ConsoleAssistantMessage = {
  id: string;
  role: "user" | "assistant" | "system";
  content: Array<ConsoleTextPart | ConsoleDataMessagePart>;
  createdAt: string;
  metadata: {
    consoleRole: ConsoleMessageRole;
    messageKind?: string | null;
  };
};

export type ConsoleExternalStoreRuntime = {
  messages: ConsoleAssistantMessage[];
  isRunning: boolean;
  canSubmit: boolean;
  submit: (content: string) => void;
};

// assistant-ui ExternalStoreRuntime 的唯一适配边界；Console 其余组件只消费这个自有形状。
export function useConsoleExternalStoreRuntime({
  messages,
  structuredItems,
  isRunning,
  canSubmit,
  submit
}: {
  messages: ConsoleMessage[];
  structuredItems: StructuredInteractionItem[];
  isRunning: boolean;
  canSubmit: boolean;
  submit: (content: string) => void;
}): ConsoleExternalStoreRuntime {
  return useMemo(
    () => ({
      messages: toAssistantUiMessages(messages, structuredItems),
      isRunning,
      canSubmit,
      submit
    }),
    [canSubmit, isRunning, messages, structuredItems, submit]
  );
}

export function toAssistantUiMessages(
  messages: ConsoleMessage[],
  structuredItems: StructuredInteractionItem[]
): ConsoleAssistantMessage[] {
  const textMessages = messages.map(toAssistantUiMessage);
  if (structuredItems.length === 0) {
    return textMessages;
  }
  return [
    ...textMessages,
    {
      id: "structured-interactions",
      role: "assistant",
      createdAt: new Date(0).toISOString(),
      metadata: { consoleRole: "assistant" },
      content: structuredItems.map((item) => ({
        type: "data",
        data: item
      }))
    }
  ];
}

function toAssistantUiMessage(message: ConsoleMessage): ConsoleAssistantMessage {
  return {
    id: message.id,
    role: assistantRole(message.role),
    createdAt: message.createdAt,
    metadata: { consoleRole: message.role, messageKind: message.kind ?? null },
    content: [{ type: "text", text: message.content }]
  };
}

function assistantRole(role: ConsoleMessageRole): ConsoleAssistantMessage["role"] {
  if (role === "system_observation" || role === "system") {
    return "system";
  }
  return role;
}
