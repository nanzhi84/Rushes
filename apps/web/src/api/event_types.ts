// SSE 事件名单一常量来源。
//
// 权威主清单镜像后端 packages/contracts/events.py 的 EVENT_CLASSES（44 个领域事件）。
// 三个消费方清单（草稿编辑器 / 首页草稿墙 / 素材面板）全部从本文件的语义分组派生，
// 不再在各处硬编码字符串数组。后端 SSE 路由谓词见 packages/events/routing.py。

/** 44 个领域事件的规范名单（顺序对齐 contracts.events.EVENT_CLASSES）。 */
export const ALL_EVENT_TYPES = [
  // 草稿生命周期
  "DraftCreated",
  "DraftRenamed",
  "DraftCopied",
  "DraftTrashed",
  // 素材摄取 / 代理 / 索引 / 理解
  "AssetImported",
  "AssetProbed",
  "ProxyGenerated",
  "AssetInvalidated",
  "AssetIndexReady",
  "AssetIndexFailed",
  "MaterialUnderstandingStarted",
  "MaterialUnderstandingCompleted",
  "MaterialUnderstandingFailed",
  // 素材链接
  "AssetLinked",
  "AssetUnlinked",
  // 决策
  "DecisionCreated",
  "DecisionAnswered",
  "DecisionCancelled",
  // 计划族
  "BriefUpdated",
  "ContentPlanUpdated",
  "AudioPlanUpdated",
  "CutPlanUpdated",
  "PostprocessPlanUpdated",
  // 时间线
  "TimelineVersionCreated",
  "TimelineVersionRestored",
  "TimelineValidated",
  "TimelineValidationFailed",
  // 预览 / 导出
  "PreviewRendered",
  "PreviewViewed",
  "ExportCompleted",
  // 记忆
  "MemoryCandidateExtracted",
  "MemoryCandidateDiscarded",
  "MemorySaved",
  // job
  "JobEnqueued",
  "JobProgress",
  "JobSucceeded",
  "JobFailed",
  "JobCancelled",
  // 系统 / 观测（前三条后端 SSE 抑制，不推送）
  "PolicyRefusal",
  "ProviderCallRecorded",
  "ContextCompacted",
  "TurnEnded",
  "CapabilityDegraded",
  "SecurityRefusal"
] as const;

export type EventType = (typeof ALL_EVENT_TYPES)[number];

// `satisfies readonly EventType[]` 保证下列语义分组的每个成员都是 ALL_EVENT_TYPES 的
// 合法元素——写错名字或引用被删事件会直接编译报错（单一来源约束）。

// ---- 语义分组（从主清单切片） ----

/** 草稿本体生命周期。 */
const DRAFT_LIFECYCLE_EVENTS = [
  "DraftCreated",
  "DraftRenamed",
  "DraftCopied",
  "DraftTrashed"
] as const satisfies readonly EventType[];

/** 决策族。 */
const DECISION_EVENTS = [
  "DecisionCreated",
  "DecisionAnswered",
  "DecisionCancelled"
] as const satisfies readonly EventType[];

/** 计划族。 */
const PLAN_EVENTS = [
  "BriefUpdated",
  "ContentPlanUpdated",
  "AudioPlanUpdated",
  "CutPlanUpdated",
  "PostprocessPlanUpdated"
] as const satisfies readonly EventType[];

/** 时间线族。 */
const TIMELINE_EVENTS = [
  "TimelineVersionCreated",
  "TimelineVersionRestored",
  "TimelineValidated",
  "TimelineValidationFailed"
] as const satisfies readonly EventType[];

/** 预览 / 导出族。 */
const PREVIEW_EVENTS = [
  "PreviewRendered",
  "PreviewViewed",
  "ExportCompleted"
] as const satisfies readonly EventType[];

/** 草稿域记忆候选（提取 / 丢弃）。MemorySaved 属 workspace 域。 */
const MEMORY_CANDIDATE_EVENTS = [
  "MemoryCandidateExtracted",
  "MemoryCandidateDiscarded"
] as const satisfies readonly EventType[];

/** job 族（携带 requested_by_draft_id）。 */
export const JOB_EVENT_TYPES = [
  "JobEnqueued",
  "JobProgress",
  "JobSucceeded",
  "JobFailed",
  "JobCancelled"
] as const satisfies readonly EventType[];

/** 素材族（摄取 / 代理 / 索引 / 理解 / 链接）。 */
const ASSET_EVENTS = [
  "AssetImported",
  "AssetProbed",
  "ProxyGenerated",
  "AssetInvalidated",
  "AssetIndexReady",
  "AssetIndexFailed",
  "MaterialUnderstandingStarted",
  "MaterialUnderstandingCompleted",
  "MaterialUnderstandingFailed",
  "AssetLinked",
  "AssetUnlinked"
] as const satisfies readonly EventType[];

// ---- 消费方清单（从语义分组组合） ----

/**
 * 草稿编辑器：订阅 `/api/drafts/{id}/events`，失效当前草稿的详情 / 时间线 / 消息 / 决策查询。
 * 对齐后端 routes_to_draft（draft_id 或 requested_by_draft_id 命中本草稿）。
 */
export const DRAFT_EVENT_TYPES = [
  ...DRAFT_LIFECYCLE_EVENTS,
  ...DECISION_EVENTS,
  ...PLAN_EVENTS,
  ...TIMELINE_EVENTS,
  ...PREVIEW_EVENTS,
  ...MEMORY_CANDIDATE_EVENTS,
  ...JOB_EVENT_TYPES,
  "TurnEnded",
  "CapabilityDegraded"
] as const;

/**
 * 首页草稿墙：订阅 workspace SSE `/api/events`，失效草稿列表（含封面 / 素材计数聚合）。
 * 草稿生命周期与素材链接变化都会影响列表卡片。
 */
export const WORKSPACE_EVENT_TYPES = [
  ...DRAFT_LIFECYCLE_EVENTS,
  "AssetLinked",
  "AssetUnlinked",
  "MemorySaved",
  "CapabilityDegraded",
  "SecurityRefusal"
] as const;

/**
 * 素材面板：订阅 workspace SSE 中素材 / job / 决策相关事件，失效当前草稿的素材列表查询。
 */
export const MATERIAL_EVENT_TYPES = [
  ...ASSET_EVENTS,
  ...JOB_EVENT_TYPES,
  "DecisionAnswered"
] as const;
