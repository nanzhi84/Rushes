import { useQueryClient } from "@tanstack/react-query";
import { useEffect } from "react";
import { queryKeys } from "../../app/query_client";
import { createApiEventSource } from "../../auth";

type MaterialsSsePayload = {
  event_id: number;
  event: {
    event: string;
    project_id?: string | null;
  };
};

/** 订阅 workspace SSE 中素材相关事件，失效当前项目的素材列表查询。 */
export function useMaterialsEvents(projectId: string, enabled: boolean): void {
  const queryClient = useQueryClient();

  useEffect(() => {
    if (!enabled) {
      return;
    }
    const source = createApiEventSource("/api/events");
    const handleEvent = (event: Event) => {
      const message = event as MessageEvent<string>;
      const payload = JSON.parse(message.data) as MaterialsSsePayload;
      const eventProjectId = payload.event.project_id;
      if (!eventProjectId || eventProjectId === projectId) {
        void queryClient.invalidateQueries({ queryKey: queryKeys.materials(projectId) });
      }
    };
    for (const eventName of MATERIAL_EVENT_TYPES) {
      source.addEventListener(eventName, handleEvent);
    }
    return () => {
      source.close();
    };
  }, [enabled, projectId, queryClient]);
}

const MATERIAL_EVENT_TYPES = [
  "AssetImported",
  "AssetProbed",
  "ProxyGenerated",
  "AssetInvalidated",
  "AssetLinked",
  "AssetUnlinked",
  "AssetIndexReady",
  "AssetIndexFailed",
  "MaterialUnderstandingStarted",
  "MaterialUnderstandingCompleted",
  "MaterialUnderstandingFailed",
  "JobEnqueued",
  "JobProgress",
  "JobSucceeded",
  "JobFailed",
  "JobCancelled",
  "DecisionAnswered"
];
