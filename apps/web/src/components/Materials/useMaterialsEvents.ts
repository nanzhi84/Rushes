import { useQueryClient } from "@tanstack/react-query";
import { useEffect } from "react";
import { MATERIAL_EVENT_TYPES } from "../../api/event_types";
import { queryKeys } from "../../app/query_client";
import { acquireApiEventSource } from "../../auth";

type MaterialsSsePayload = {
  event_id: number;
  event: {
    event: string;
    draft_id?: string | null;
  };
};

/** 订阅 workspace SSE（共享连接）中素材相关事件，失效当前草稿的素材列表查询。 */
export function useMaterialsEvents(draftId: string, enabled: boolean): void {
  const queryClient = useQueryClient();

  useEffect(() => {
    if (!enabled) {
      return;
    }
    const { source, release } = acquireApiEventSource("/api/events");
    const handleEvent = (event: Event) => {
      const message = event as MessageEvent<string>;
      const payload = JSON.parse(message.data) as MaterialsSsePayload;
      const eventDraftId = payload.event.draft_id;
      if (!eventDraftId || eventDraftId === draftId) {
        void queryClient.invalidateQueries({ queryKey: queryKeys.materials(draftId) });
      }
    };
    for (const eventName of MATERIAL_EVENT_TYPES) {
      source.addEventListener(eventName, handleEvent);
    }
    return () => {
      for (const eventName of MATERIAL_EVENT_TYPES) {
        source.removeEventListener(eventName, handleEvent);
      }
      release();
    };
  }, [enabled, draftId, queryClient]);
}
