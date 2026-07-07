import { useQueryClient } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { WORKSPACE_EVENT_TYPES } from "../api/event_types";
import { acquireApiEventSource } from "../auth";
import { queryKeys } from "./query_client";

export type ConnectionState = "connecting" | "open" | "closed";

/** 订阅 workspace 级 SSE（共享连接）：维护连接状态，并在结构性事件到达时失效草稿列表查询。 */
export function useWorkspaceEvents(): ConnectionState {
  const queryClient = useQueryClient();
  const [connectionState, setConnectionState] = useState<ConnectionState>("connecting");

  useEffect(() => {
    const { source, release } = acquireApiEventSource("/api/events");
    // 共享连接：用 addEventListener，不占用 onopen/onerror 独占槽位。
    if (source.readyState === 1 /* OPEN */) {
      setConnectionState("open");
    }
    const handleOpen = () => setConnectionState("open");
    const handleError = () => setConnectionState("closed");
    const handleWorkspaceEvent = () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.drafts });
    };
    source.addEventListener("open", handleOpen);
    source.addEventListener("error", handleError);
    for (const eventName of WORKSPACE_EVENT_TYPES) {
      source.addEventListener(eventName, handleWorkspaceEvent);
    }
    return () => {
      source.removeEventListener("open", handleOpen);
      source.removeEventListener("error", handleError);
      for (const eventName of WORKSPACE_EVENT_TYPES) {
        source.removeEventListener(eventName, handleWorkspaceEvent);
      }
      release();
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
