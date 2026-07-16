import type { Decision, DecisionAnswer, DecisionOption } from "../../../api/client";
import type {
  DecisionInteractionItem,
  DomainSseEvent,
  DomainSsePayload,
  ErrorInteractionItem,
  ProgressInteractionItem,
  StructuredInteractionItem
} from "./types";

export function reduceStructuredInteractionItems(
  current: StructuredInteractionItem[],
  payload: DomainSsePayload
): StructuredInteractionItem[] {
  if (payload.event.event === "ConversationContextCleared") {
    return [];
  }
  // 时间线的保存与校验状态已经在右侧编辑器和工具步骤中体现；无论来自
  // 人工还是 Agent，都不要再把同一领域事件堆进对话区。
  if (SILENT_TIMELINE_EVENTS.has(payload.event.event)) {
    return current;
  }
  const item = itemFromEvent(payload);
  if (!item) {
    return current;
  }

  if (item.kind === "progress") {
    return upsertProgress(current, item);
  }

  // 预览与时间线是编辑器状态提示，不是聊天正文。每类只保留最新一行，
  // 同类重复事件累加次数，避免渲染/校验轮询把消息区铺成卡片墙。
  if (item.kind === "preview" || item.kind === "timeline") {
    return upsertActivity(current, item);
  }

  if (payload.event.event === "JobFailed" && item.kind === "error") {
    const failedProgress = progressEventItem(payload.event, "failed");
    return upsertById(failedProgress ? upsertProgress(current, failedProgress) : current, item);
  }

  if (item.kind === "decision" && item.status !== "pending") {
    const matched = current.some(
      (existing) => existing.kind === "decision" && existing.decision_id === item.decision_id
    );
    if (!matched) {
      return upsertById(current, item);
    }
    return current.map((existing) => {
      if (existing.kind !== "decision" || existing.decision_id !== item.decision_id) {
        return existing;
      }
      const answer = item.answer ?? existing.answer ?? null;
      return {
        ...existing,
        status: item.status,
        answer,
        decision: existing.decision
          ? { ...existing.decision, status: item.status, answer }
          : item.decision
      };
    });
  }

  return upsertById(current, item);
}

const SILENT_TIMELINE_EVENTS = new Set([
  "TimelineVersionCreated",
  "TimelineVersionRestored",
  "TimelineValidated",
  "TimelineValidationFailed"
]);

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
      ? {
          ...item,
          status: "answered",
          answer,
          decision: item.decision ? { ...item.decision, status: "answered", answer } : null
        }
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
        id: "preview:latest",
        title: "预览已生成",
        description: "可在右侧查看预览。",
        occurrences: 1
      };
    case "ExportCompleted":
      return {
        kind: "preview",
        id: "export:latest",
        title: "导出完成",
        description: "最终 MP4 已生成。",
        occurrences: 1
      };
    default:
      // 常规生命周期事件不进对话流；真正未知的事件保留 JSON 兜底防止信息丢失。
      if (SILENT_EVENTS.has(event.event)) {
        return null;
      }
      return {
        kind: "unknown",
        id: `unknown:${payload.event_id}`,
        eventName: event.event,
        raw: event
      };
  }
}

const SILENT_EVENTS = new Set([
  "DraftCreated",
  "AssetImported",
  "AssetProbed",
  "AssetLinked",
  "AssetUnlinked",
  "ProxyGenerated",
  "MaterialUnderstandingStarted",
  "MaterialUnderstandingCompleted",
  "MaterialUnderstandingFailed",
  "ConversationContextCleared",
  "PreviewViewed"
]);

// 只有 Agent 会等待的 job 才进入对话进度行；ingest 进度由素材面板呈现。
// 此集合与 go/internal/agent.agentWaitedJobKinds 对齐。
const PROGRESS_JOB_KINDS = new Set([
  "understand",
  "render_preview",
  "render_final"
]);

// 进度行标题按 kind 给中文名，比笼统的「后台任务」可读。
const JOB_KIND_LABELS: Record<string, string> = {
  understand: "理解素材",
  render_preview: "渲染预览",
  render_final: "渲染成片"
};

// 领域事件是 envelope；job 字段位于 event.payload。
function progressJobKind(event: DomainSseEvent): string | null {
  const payload = objectValue(event.payload);
  const kind = payload ? stringValue(payload.kind) : null;
  if (kind === null || !PROGRESS_JOB_KINDS.has(kind)) {
    return null;
  }
  return kind;
}

function decisionEventItem(
  event: DomainSseEvent,
  status: Decision["status"]
): DecisionInteractionItem | null {
  const decisionId = stringValue(eventField(event, "decision_id"));
  if (!decisionId) {
    return null;
  }
  const rawAnswer = eventField(event, "answer");
  const answer = isDecisionAnswer(rawAnswer) ? rawAnswer : null;
  return {
    kind: "decision",
    id: decisionItemId(decisionId),
    decision_id: decisionId,
    decision: decisionFromEvent(event, decisionId, status, answer),
    status,
    answer
  };
}

// DecisionCreated 已经把问题、选项和交互约束写进领域事件。重放时直接恢复
// 完整确认项，避免 answered 事件只剩 decision_id 后退化成无法辨认的空壳卡片。
function decisionFromEvent(
  event: DomainSseEvent,
  decisionId: string,
  status: Decision["status"],
  answer: DecisionAnswer | null
): Decision | null {
  const question = stringValue(eventField(event, "question"));
  if (!question) {
    return null;
  }
  const rawScope = stringValue(eventField(event, "scope_type"));
  const draftId = stringValue(event.draft_id);
  return {
    decision_id: decisionId,
    scope_type: rawScope === "workspace" ? "workspace" : "draft",
    draft_id: draftId,
    type: decisionType(eventField(event, "type")),
    question,
    options: decisionOptions(eventField(event, "options")),
    allow_free_text: booleanValue(eventField(event, "allow_free_text")) ?? false,
    blocking: booleanValue(eventField(event, "blocking")) ?? false,
    status,
    answer
  };
}

const DECISION_TYPES = new Set<Decision["type"]>([
  "audio_mode",
  "approve_content_plan",
  "approve_speech_cut",
  "approve_rough_cut",
  "critical",
  "subtitle",
  "bgm",
  "export",
  "memory_scope",
  "url_import",
  "generic"
]);

function decisionType(value: unknown): Decision["type"] {
  const type = stringValue(value) as Decision["type"] | null;
  return type && DECISION_TYPES.has(type) ? type : "generic";
}

function decisionOptions(value: unknown): DecisionOption[] {
  if (!Array.isArray(value)) {
    return [];
  }
  return value.flatMap((candidate) => {
    const option = objectValue(candidate);
    const optionId = stringValue(option?.option_id);
    const label = stringValue(option?.label);
    if (!option || !optionId || !label) {
      return [];
    }
    const description = stringValue(option.description);
    const payload = objectValue(option.payload);
    return [
      {
        option_id: optionId,
        label,
        ...(description ? { description } : {}),
        ...(payload ? { payload } : {})
      }
    ];
  });
}

function progressEventItem(
  event: DomainSseEvent,
  status: ProgressInteractionItem["status"]
): ProgressInteractionItem | null {
  const jobId = stringValue(eventField(event, "job_id"));
  if (!jobId) {
    return null;
  }
  const kind = progressJobKind(event);
  if (kind === null) {
    return null;
  }
  const currentAssetId = stringValue(eventField(event, "current_asset_id"));
  const done = integerValue(eventField(event, "done"));
  const total = integerValue(eventField(event, "total"));
  const stage = stringValue(eventField(event, "stage"));
  const detail = stringValue(eventField(event, "detail"));
  return {
    kind: "progress",
    id: progressItemId(jobId),
    job_id: jobId,
    job_kind: JOB_KIND_LABELS[kind] ?? kind,
    progress: normalizeProgress(eventField(event, "progress")),
    status,
    ...(currentAssetId ? { current_asset_id: currentAssetId } : {}),
    ...(done !== null ? { done } : {}),
    ...(total !== null ? { total } : {}),
    ...(stage ? { stage } : {}),
    ...(detail ? { detail } : {})
  };
}

function errorEventItem(event: DomainSseEvent): ErrorInteractionItem | null {
  // ingest 失败由素材面板呈现，避免批量导入时在对话区刷屏。
  const payload = objectValue(event.payload);
  const payloadKind = payload ? stringValue(payload.kind) : null;
  if (payloadKind !== null && !PROGRESS_JOB_KINDS.has(payloadKind)) {
    return null;
  }
  const jobId = stringValue(eventField(event, "job_id"));
  const details = objectValue(eventField(event, "failure")) ?? objectValue(eventField(event, "error"));
  const errorCode = stringValue(eventField(event, "error_code")) ?? stringValue(details?.error_code) ?? "JOB_FAILED";
  const message = stringValue(eventField(event, "message")) ?? stringValue(details?.message) ?? "任务执行失败";
  return {
    kind: "error",
    id: jobId ? `error:${jobId}` : `error:${errorCode}`,
    error_code: errorCode,
    message,
    retryable: booleanValue(eventField(event, "retryable")) ?? booleanValue(details?.retryable) ?? false,
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

function upsertProgress(
  items: StructuredInteractionItem[],
  item: ProgressInteractionItem
): StructuredInteractionItem[] {
  const previous = items.find(
    (existing) => existing.kind === "progress" && existing.job_id === item.job_id
  );
  const withoutPrevious = items.filter(
    (existing) => !(existing.kind === "progress" && existing.job_id === item.job_id)
  );
  return [...withoutPrevious, previous ? mergeItem(previous, item) : item];
}

function upsertActivity(
  items: StructuredInteractionItem[],
  item: Extract<StructuredInteractionItem, { kind: "preview" | "timeline" }>
): StructuredInteractionItem[] {
  const previous = items.filter((existing) => existing.kind === item.kind);
  const occurrences = previous.reduce(
    (total, existing) =>
      total +
      (existing.kind === "preview" || existing.kind === "timeline"
        ? (existing.occurrences ?? 1)
        : 0),
    item.occurrences ?? 1
  );
  return [
    ...items.filter((existing) => existing.kind !== item.kind),
    { ...item, occurrences }
  ];
}

function mergeItem(
  previous: StructuredInteractionItem,
  next: StructuredInteractionItem
): StructuredInteractionItem {
  if (previous.kind === "decision" && next.kind === "decision") {
    const answer = next.answer ?? previous.answer ?? null;
    const decision = next.decision ?? previous.decision;
    return {
      ...previous,
      ...next,
      decision: decision ? { ...decision, status: next.status, answer } : null,
      answer
    };
  }
  if (previous.kind === "progress" && next.kind === "progress") {
    if (
      previous.job_id === next.job_id &&
      ["succeeded", "failed", "cancelled"].includes(previous.status) &&
      ["queued", "running"].includes(next.status)
    ) {
      return previous;
    }
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

function eventField(event: DomainSseEvent, key: string): unknown {
  const payload = objectValue(event.payload);
  return payload?.[key] ?? event[key];
}

function stringValue(value: unknown): string | null {
  return typeof value === "string" && value.length > 0 ? value : null;
}

function booleanValue(value: unknown): boolean | null {
  return typeof value === "boolean" ? value : null;
}

function integerValue(value: unknown): number | null {
  return typeof value === "number" && Number.isInteger(value) ? value : null;
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
