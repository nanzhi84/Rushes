import { useQueryClient } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { createApiEventSource } from "../auth";
import { queryKeys } from "./query_client";

export type ConnectionState = "connecting" | "open" | "closed";

/** 订阅 workspace 级 SSE：维护连接状态，并在结构性事件到达时失效项目/树查询。 */
export function useWorkspaceEvents(): ConnectionState {
  const queryClient = useQueryClient();
  const [connectionState, setConnectionState] = useState<ConnectionState>("connecting");

  useEffect(() => {
    const source = createApiEventSource("/api/events");
    source.onopen = () => setConnectionState("open");
    source.onerror = () => setConnectionState("closed");
    const handleWorkspaceEvent = () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.projectTree });
      void queryClient.invalidateQueries({ queryKey: queryKeys.projects });
    };
    for (const eventName of WORKSPACE_EVENT_TYPES) {
      source.addEventListener(eventName, handleWorkspaceEvent);
    }
    return () => {
      source.close();
    };
  }, [queryClient]);

  return connectionState;
}

export function connectionLabel(state: ConnectionState): string {
  if (state === "open") {
    return "本地已连接";
  }
  if (state === "closed") {
    return "连接中断";
  }
  return "连接中";
}

const WORKSPACE_EVENT_TYPES = [
  "ProjectCreated",
  "ProjectRenamed",
  "ProjectTrashed",
  "ProjectCopied",
  "CaseCreated",
  "CaseRenamed",
  "CaseCopied",
  "CaseMoved",
  "CaseClosed",
  "CaseTrashed",
  "AssetLinked",
  "AssetUnlinked",
  "MemorySaved",
  "CapabilityDegraded",
  "SecurityRefusal"
];
