// 领域 SSE 事件名单的前端单一来源；与 go/internal/contracts.EventRegistry 对齐。
// 回合流的 8 种 type 属于独立 turn-stream，不得混入领域事件。

const ALL_EVENT_TYPES = [
  "DraftCreated",
  "DraftRenamed",
  "DraftCopied",
  "DraftTrashed",
  "AssetImported",
  "AssetProbed",
  "AssetLinked",
  "AssetUnlinked",
  "MaterialUnderstandingStarted",
  "MaterialUnderstandingCompleted",
  "MaterialUnderstandingFailed",
  "DecisionCreated",
  "DecisionAnswered",
  "ConversationContextCleared",
  "TimelineVersionCreated",
  "TimelineVersionRestored",
  "TimelineValidated",
  "TimelineValidationFailed",
  "PreviewRendered",
  "ExportCompleted",
  "JobEnqueued",
  "JobSucceeded",
  "JobFailed",
  "JobCancelled",
  "ProxyGenerated",
  "JobProgress",
  "PreviewViewed"
] as const;

type EventType = (typeof ALL_EVENT_TYPES)[number];

const DRAFT_LIFECYCLE_EVENTS = [
  "DraftCreated",
  "DraftRenamed",
  "DraftCopied",
  "DraftTrashed"
] as const satisfies readonly EventType[];

const DECISION_EVENTS = [
  "DecisionCreated",
  "DecisionAnswered"
] as const satisfies readonly EventType[];

const CONVERSATION_EVENTS = ["ConversationContextCleared"] as const satisfies readonly EventType[];

const TIMELINE_EVENTS = [
  "TimelineVersionCreated",
  "TimelineVersionRestored",
  "TimelineValidated",
  "TimelineValidationFailed"
] as const satisfies readonly EventType[];

const PREVIEW_EVENTS = [
  "PreviewRendered",
  "ExportCompleted",
  "PreviewViewed"
] as const satisfies readonly EventType[];

const JOB_EVENT_TYPES = [
  "JobEnqueued",
  "JobProgress",
  "JobSucceeded",
  "JobFailed",
  "JobCancelled"
] as const satisfies readonly EventType[];

const ASSET_EVENTS = [
  "AssetImported",
  "AssetProbed",
  "AssetLinked",
  "AssetUnlinked",
  "ProxyGenerated",
  "MaterialUnderstandingStarted",
  "MaterialUnderstandingCompleted",
  "MaterialUnderstandingFailed"
] as const satisfies readonly EventType[];

/** `/api/drafts/{id}/events`：草稿自身或 requested_by_draft_id 命中的事件。 */
export const DRAFT_EVENT_TYPES = [
  ...DRAFT_LIFECYCLE_EVENTS,
  "AssetLinked",
  "AssetUnlinked",
  ...DECISION_EVENTS,
  ...CONVERSATION_EVENTS,
  ...TIMELINE_EVENTS,
  ...PREVIEW_EVENTS,
  ...JOB_EVENT_TYPES
] as const;

/** `/api/events`：必须与 contracts.RoutesToWorkspace 的 switch 保持一致。 */
export const WORKSPACE_EVENT_TYPES = [
  ...DRAFT_LIFECYCLE_EVENTS,
  ...ASSET_EVENTS,
  ...JOB_EVENT_TYPES
] as const;

/** 素材面板只关心 workspace SSE 中会改变素材或后台任务状态的事件。 */
export const MATERIAL_EVENT_TYPES = [
  ...ASSET_EVENTS,
  ...JOB_EVENT_TYPES
] as const;
