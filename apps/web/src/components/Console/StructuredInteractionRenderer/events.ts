import type { Decision, DecisionAnswer } from "../../../api/client";
import type {
  DomainSseEvent,
  DomainSsePayload,
  ProgressInteractionItem,
  StructuredInteractionItem
} from "./types";

export function reduceStructuredInteractionItems(
  current: StructuredInteractionItem[],
  payload: DomainSsePayload
): StructuredInteractionItem[] {
  const item = itemFromEvent(payload);
  if (!item) {
    return current;
  }

  if (item.kind === "progress") {
    return upsertById(current, item);
  }

  if (payload.event.event === "JobFailed" && item.kind === "error") {
    const failedProgress = progressEventItem(payload.event, "failed");
    return upsertById(failedProgress ? upsertById(current, failedProgress) : current, item);
  }

  if (item.kind === "decision" && item.status !== "pending") {
    return current.map((existing) =>
      existing.kind === "decision" && existing.decision_id === item.decision_id
        ? { ...existing, status: item.status, answer: item.answer ?? existing.answer }
        : existing
    );
  }

  return upsertById(current, item);
}

export function mergeCurrentDecisionItem(
  items: StructuredInteractionItem[],
  decision: Decision | null
): StructuredInteractionItem[] {
  if (!decision) {
    return items;
  }

  const item: StructuredInteractionItem = {
    kind: "decision",
    id: decisionItemId(decision.decision_id),
    decision_id: decision.decision_id,
    decision,
    status: decision.status,
    answer: decision.answer ?? null
  };

  return upsertById(items, item);
}

export function markDecisionAnswered(
  items: StructuredInteractionItem[],
  decisionId: string,
  answer: DecisionAnswer
): StructuredInteractionItem[] {
  return items.map((item) =>
    item.kind === "decision" && item.decision_id === decisionId
      ? { ...item, status: "answered", answer }
      : item
  );
}

export function itemFromEvent(payload: DomainSsePayload): StructuredInteractionItem | null {
  const event = payload.event;
  switch (event.event) {
    case "DecisionCreated":
      return decisionEventItem(event, "pending");
    case "DecisionAnswered":
      return decisionEventItem(event, "answered");
    case "DecisionCancelled":
      return decisionEventItem(event, "cancelled");
    case "JobEnqueued":
      return progressEventItem(event, "queued");
    case "JobProgress":
      return progressEventItem(event, "running");
    case "JobSucceeded":
      return progressEventItem(event, "succeeded");
    case "JobFailed":
      return errorEventItem(event);
    case "JobCancelled":
      return progressEventItem(event, "cancelled");
    case "PreviewRendered":
      return {
        kind: "preview",
        id: `preview:${payload.event_id}`,
        title: "预览已生成",
        description: stringValue(event.preview_id) ?? "可在右侧查看预览。"
      };
    case "TimelineVersionCreated":
    case "TimelineVersionRestored":
    case "TimelineValidated":
      return {
        kind: "timeline",
        id: `timeline:${payload.event_id}`,
        title: timelineTitle(event.event),
        description: timelineDescription(event)
      };
    default:
      return {
        kind: "unknown",
        id: `unknown:${payload.event_id}`,
        eventName: event.event,
        raw: event
      };
  }
}

function decisionEventItem(
  event: DomainSseEvent,
  status: Decision["status"]
): StructuredInteractionItem | null {
  const decisionId = stringValue(event.decision_id);
  if (!decisionId) {
    return null;
  }
  const rawAnswer = event.answer;
  return {
    kind: "decision",
    id: decisionItemId(decisionId),
    decision_id: decisionId,
    decision: null,
    status,
    answer: isDecisionAnswer(rawAnswer) ? rawAnswer : null
  };
}

function progressEventItem(
  event: DomainSseEvent,
  status: ProgressInteractionItem["status"]
): StructuredInteractionItem | null {
  const jobId = stringValue(event.job_id);
  if (!jobId) {
    return null;
  }
  return {
    kind: "progress",
    id: progressItemId(jobId),
    job_id: jobId,
    job_kind: stringValue(event.kind) ?? stringValue(event.job_kind) ?? "后台任务",
    progress: normalizeProgress(event.progress),
    status
  };
}

function errorEventItem(event: DomainSseEvent): StructuredInteractionItem | null {
  const jobId = stringValue(event.job_id);
  const details = objectValue(event.failure) ?? objectValue(event.error) ?? objectValue(event.error_json);
  const errorCode = stringValue(event.error_code) ?? stringValue(details?.error_code) ?? "JOB_FAILED";
  const message = stringValue(event.message) ?? stringValue(details?.message) ?? "任务执行失败";
  return {
    kind: "error",
    id: jobId ? `error:${jobId}` : `error:${errorCode}`,
    error_code: errorCode,
    message,
    retryable: booleanValue(event.retryable) ?? booleanValue(details?.retryable) ?? false,
    details: details ?? event
  };
}

function upsertById(
  items: StructuredInteractionItem[],
  item: StructuredInteractionItem
): StructuredInteractionItem[] {
  const index = items.findIndex((existing) => existing.id === item.id);
  if (index < 0) {
    return [...items, item];
  }
  return items.map((existing, currentIndex) =>
    currentIndex === index ? mergeItem(existing, item) : existing
  );
}

function mergeItem(
  previous: StructuredInteractionItem,
  next: StructuredInteractionItem
): StructuredInteractionItem {
  if (previous.kind === "decision" && next.kind === "decision") {
    return {
      ...previous,
      ...next,
      decision: next.decision ?? previous.decision,
      answer: next.answer ?? previous.answer
    };
  }
  if (previous.kind === "progress" && next.kind === "progress") {
    return {
      ...previous,
      ...next,
      progress: next.progress ?? previous.progress
    };
  }
  return next;
}

function decisionItemId(decisionId: string): string {
  return `decision:${decisionId}`;
}

function progressItemId(jobId: string): string {
  return `progress:${jobId}`;
}

function normalizeProgress(value: unknown): number | null {
  if (typeof value !== "number" || !Number.isFinite(value)) {
    return null;
  }
  const percent = value <= 1 ? value * 100 : value;
  return Math.max(0, Math.min(100, Math.round(percent)));
}

function timelineTitle(eventName: string): string {
  if (eventName === "TimelineVersionRestored") {
    return "时间线已恢复";
  }
  if (eventName === "TimelineValidated") {
    return "时间线校验通过";
  }
  return "时间线已更新";
}

function timelineDescription(event: DomainSseEvent): string {
  const version = stringValue(event.timeline_version) ?? stringValue(event.version);
  return version ? `版本 ${version}，可在右侧查看时间线。` : "可在右侧查看时间线。";
}

function stringValue(value: unknown): string | null {
  return typeof value === "string" && value.length > 0 ? value : null;
}

function booleanValue(value: unknown): boolean | null {
  return typeof value === "boolean" ? value : null;
}

function objectValue(value: unknown): Record<string, unknown> | null {
  return value !== null && typeof value === "object" && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : null;
}

function isDecisionAnswer(value: unknown): value is DecisionAnswer {
  const record = objectValue(value);
  return (
    record !== null &&
    (record.answered_via === "button" || record.answered_via === "natural_language")
  );
}
