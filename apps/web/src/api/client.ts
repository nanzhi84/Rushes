import { apiFetch, createApiEventSource, getAuthToken } from "../auth";
import type { components } from "./generated/schema";

type Schemas = components["schemas"];

// ---- 决策 / 文件系统 / 存储：直接引 generated（单一来源） ----
export type DecisionOption = Schemas["DecisionOption"];
export type Decision = Schemas["Decision"];
export type DecisionAnswer = Schemas["DecisionAnswer"];
export type FsRoot = Schemas["FsRoot"];
export type FsListEntry = Schemas["FsListEntry"];
export type FsListResponse = Schemas["FsListResponse"];
export type FsRootsResponse = Schemas["FsRootsResponse"];
export type FsPickResponse = Schemas["FsPickResponse"];
export type StorageMode = Schemas["StorageMode"];

// ---- 素材：结构化字段引 generated；kind/understanding_status 在 schema 里是裸 string，
//      前端保留窄化 helper union 供 UI 判定。 ----
export type MaterialKind = "video" | "audio" | "image" | "font";
export type UnderstandingStatus = "none" | "running" | "ready" | "failed";

export type AssetJobSummary = Schemas["AssetJobSummary"];
export type MaterialAsset = Schemas["MaterialAsset"];
export type MaterialsResponse = Schemas["MaterialsResponse"];
export type MaterialMutationResponse = Schemas["MaterialMutationResponse"];

// summary 明细 shape 在 generated 里是 opaque（summary: {[key]: unknown}），
// 保留手写窄化类型，对齐 Go understanding.MaterialSummary。
export type MaterialSummarySegment = {
  start_s: number;
  end_s: number;
  description: string;
  transcript?: string | null;
  tags?: string[];
  quality: string;
  notes?: string | null;
};

export type MaterialSummaryDetail = {
  asset_id?: string;
  version?: number;
  focus?: string | null;
  semantic_role?: string;
  overall?: string;
  language?: string | null;
  segments?: MaterialSummarySegment[];
  generated_at?: string;
  model?: string;
  [key: string]: unknown;
};

export type MaterialSummaryResponse = {
  asset_id: string;
  summary: MaterialSummaryDetail;
};

// timeline JSON shape 在 generated 里是 opaque；保留手写窄化类型供 TimelineViewer/Console 渲染。
export type TimelineClipJson = {
  timeline_clip_id?: string;
  track_id?: string;
  timeline_start_frame?: number;
  timeline_end_frame?: number;
  asset_id?: string;
  clip_id?: string | null;
  role?: string;
  text?: string;
  source_start_frame?: number;
  source_end_frame?: number;
  playback_rate?: number;
  gain_db?: number;
  lock_policy?: string;
  asset_kind?: string;
  parent_block_id?: string;
  linked?: boolean;
  [key: string]: unknown;
};

export type TimelineTrackJson = {
  track_id: string;
  track_type?: string;
  clips?: TimelineClipJson[];
  muted?: boolean;
  solo?: boolean;
  locked?: boolean;
  gain_db?: number;
  [key: string]: unknown;
};

export type TimelineJson = {
  fps: number;
  duration_frames: number;
  tracks: TimelineTrackJson[];
  [key: string]: unknown;
};

export type DraftTimelineResponse = {
  draft_id: string;
  timeline_version: number;
  parent_version: number | null;
  redo_version: number | null;
  latest_version: number;
  timeline: TimelineJson;
  summary: string;
  preview_id: string | null;
};

// ---- 草稿 / 消息 / 成本：引 generated ----
export type DraftListItem = Schemas["DraftListItem"];
export type DraftListResponse = Schemas["DraftListResponse"];
export type DraftRecord = Schemas["DraftRecord"];
export type DraftResponse = Schemas["DraftResponse"];
export type DraftMutationResponse = Schemas["DraftMutationResponse"];
export type DraftCostsResponse = Schemas["DraftCostsResponse"];
export type CostSummary = Schemas["CostSummary"];
export type MessageRecord = Schemas["MessageRecord"];
export type MessagesResponse = Schemas["MessagesResponse"];
export type MessageQueuedResponse = Schemas["MessageQueuedResponse"];
export type TurnCancelResponse = Schemas["TurnCancelResponse"];
export type CurrentDecisionResponse = Schemas["CurrentDecisionResponse"];
export type PendingDecisionsResponse = Schemas["PendingDecisionsResponse"];
export type DecisionAnswerResponse = Schemas["DecisionAnswerResponse"];

// ---- 请求体（引 generated） ----
type DraftCreateRequest = Schemas["DraftCreateRequest"];
type DraftUpdateRequest = Schemas["DraftUpdateRequest"];
type DraftCopyRequest = Schemas["DraftCopyRequest"];
type MaterialImportLocalRequest = Schemas["MaterialImportLocalRequest"];
type MessageCreateRequest = Schemas["MessageCreateRequest"];
type DecisionAnswerRequest = Schemas["DecisionAnswerRequest"];
export type TimelinePatchRequest = Schemas["TimelinePatchRequest"];

// 安全中间件对所有 mutation（含无 body 的 POST/DELETE）强制 Content-Type: application/json。
// 无 body 的 mutation 显式带该头；带 body 的由 apiFetch 自动补。
const JSON_MUTATION_HEADERS = { "Content-Type": "application/json" } as const;

export function fetchDraftTimeline(
  draftId: string,
  version?: number | null
): Promise<DraftTimelineResponse> {
  const params = new URLSearchParams();
  if (version !== undefined && version !== null) {
    params.set("version", String(version));
  }
  const query = params.toString();
  return apiFetch<DraftTimelineResponse>(
    `${draftPath(draftId)}/timeline${query ? `?${query}` : ""}`
  );
}

export function postPreviewViewed(
  draftId: string,
  previewId: string
): Promise<DraftMutationResponse> {
  return apiFetch<DraftMutationResponse>(
    `${draftPath(draftId)}/previews/${encodeURIComponent(previewId)}/viewed`,
    { method: "POST", headers: JSON_MUTATION_HEADERS }
  );
}

export function applyTimelinePatch(
  draftId: string,
  payload: TimelinePatchRequest
): Promise<DraftTimelineResponse> {
  return apiFetch<DraftTimelineResponse>(`${draftPath(draftId)}/timeline/patch`, {
    method: "POST",
    body: payload
  });
}

export function restoreTimelineVersion(
  draftId: string,
  version: number
): Promise<DraftTimelineResponse> {
  return apiFetch<DraftTimelineResponse>(`${draftPath(draftId)}/timeline/restore`, {
    method: "POST",
    body: { version }
  });
}

// limit=最老的前 N 条，升序返回；当前规模够用。
export function getDraftMessages(draftId: string): Promise<MessagesResponse> {
  return apiFetch<MessagesResponse>(`${draftPath(draftId)}/messages?limit=200`);
}

// turn-stream SSE 工厂：鉴权同 createApiEventSource（query token）。断线由浏览器自动重连。
export function createDraftTurnStreamSource(draftId: string): EventSource {
  return createApiEventSource(`${draftPath(draftId)}/turn-stream`);
}

export const api = {
  // ---- 草稿生命周期 ----
  listDrafts(): Promise<DraftListResponse> {
    return apiFetch<DraftListResponse>("/api/drafts");
  },

  // 响应为完整草稿详情（draft_id/name/status/defaults/created_at/updated_at）。
  createDraft(payload: DraftCreateRequest = {}): Promise<DraftMutationResponse> {
    return apiFetch<DraftMutationResponse>("/api/drafts", {
      method: "POST",
      body: payload
    });
  },

  getDraft(draftId: string): Promise<DraftResponse> {
    return apiFetch<DraftResponse>(draftPath(draftId));
  },

  renameDraft(draftId: string, payload: DraftUpdateRequest): Promise<DraftMutationResponse> {
    return apiFetch<DraftMutationResponse>(draftPath(draftId), {
      method: "PATCH",
      body: payload
    });
  },

  copyDraft(draftId: string, payload: DraftCopyRequest = {}): Promise<DraftMutationResponse> {
    return apiFetch<DraftMutationResponse>(`${draftPath(draftId)}/copy`, {
      method: "POST",
      body: payload
    });
  },

  trashDraft(draftId: string, confirm = true): Promise<DraftMutationResponse> {
    return apiFetch<DraftMutationResponse>(draftPath(draftId), {
      method: "DELETE",
      body: { confirm }
    });
  },

  // ---- 对话 / 决策 / 时间线 / 成本 ----
  postMessage(draftId: string, payload: MessageCreateRequest): Promise<MessageQueuedResponse> {
    return apiFetch<MessageQueuedResponse>(`${draftPath(draftId)}/messages`, {
      method: "POST",
      body: payload
    });
  },

  cancelTurn(draftId: string): Promise<TurnCancelResponse> {
    return apiFetch<TurnCancelResponse>(`${draftPath(draftId)}/turn/cancel`, {
      method: "POST",
      headers: JSON_MUTATION_HEADERS
    });
  },

  getDraftMessages,

  currentDecision(draftId: string): Promise<CurrentDecisionResponse> {
    return apiFetch<CurrentDecisionResponse>(`${draftPath(draftId)}/decisions/current`);
  },

  pendingDecisions(draftId: string): Promise<PendingDecisionsResponse> {
    return apiFetch<PendingDecisionsResponse>(`${draftPath(draftId)}/decisions/pending`);
  },

  answerDecision(decisionId: string, payload: DecisionAnswerRequest): Promise<DecisionAnswerResponse> {
    return apiFetch<DecisionAnswerResponse>(`/api/decisions/${encodeURIComponent(decisionId)}/answer`, {
      method: "POST",
      body: payload
    });
  },

  fetchDraftTimeline,

  applyTimelinePatch,

  restoreTimelineVersion,

  postPreviewViewed,

  draftCosts(draftId: string): Promise<DraftCostsResponse> {
    return apiFetch<DraftCostsResponse>(`${draftPath(draftId)}/costs`);
  },

  // ---- 素材（挂当前草稿） ----
  listMaterials(draftId: string): Promise<MaterialsResponse> {
    return apiFetch<MaterialsResponse>(`${draftPath(draftId)}/materials`);
  },

  revalidateMaterials(draftId: string): Promise<MaterialsResponse> {
    return apiFetch<MaterialsResponse>(`${draftPath(draftId)}/materials/revalidate`, {
      method: "POST",
      headers: JSON_MUTATION_HEADERS
    });
  },

  importLocalMaterial(
    draftId: string,
    payload: MaterialImportLocalRequest
  ): Promise<MaterialMutationResponse> {
    return apiFetch<MaterialMutationResponse>(`${draftPath(draftId)}/materials/import-local`, {
      method: "POST",
      body: payload
    });
  },

  deleteMaterial(draftId: string, assetId: string): Promise<MaterialMutationResponse> {
    return apiFetch<MaterialMutationResponse>(
      `${draftPath(draftId)}/materials/${encodeURIComponent(assetId)}`,
      {
        method: "DELETE",
        headers: JSON_MUTATION_HEADERS
      }
    );
  },

  getAssetSummary(draftId: string, assetId: string): Promise<MaterialSummaryResponse> {
    return apiFetch<MaterialSummaryResponse>(
      `${draftPath(draftId)}/materials/${encodeURIComponent(assetId)}/summary`
    );
  },

  // ---- 文件系统原生选择 ----
  fsRoots(): Promise<FsRootsResponse> {
    return apiFetch<FsRootsResponse>("/api/fs/roots");
  },

  /** 弹出宿主机原生选择对话框（macOS）；available=false 时前端提示不可用。 */
  pickLocalPaths(mode: "files" | "folder"): Promise<FsPickResponse> {
    return apiFetch<FsPickResponse>("/api/fs/pick", {
      method: "POST",
      body: { mode }
    });
  },

  fsList(path: string): Promise<FsListResponse> {
    const params = new URLSearchParams({ path });
    return apiFetch<FsListResponse>(`/api/fs/list?${params.toString()}`);
  },

  // media 族 URL 由浏览器原生 <img>/<video>/wavesurfer 直连，设不了 Authorization header，
  // 统一带 query token（Go 鉴权中间件对白名单媒体 GET/HEAD 放行，语义同 SSE）。
  // 素材试看优先直连原片（导入即刻可播，浏览器硬解 H.264/HEVC）；原片播不动时前端回落 proxy。
  mediaSourceUrl(assetId: string): string {
    return withQueryToken(`/api/media/${encodeURIComponent(assetId)}/source`);
  },

  mediaProxyUrl(assetId: string): string {
    return withQueryToken(`/api/media/${encodeURIComponent(assetId)}/proxy`);
  },

  mediaThumbnailUrl(assetId: string): string {
    return withQueryToken(`/api/media/${encodeURIComponent(assetId)}/thumbnail`);
  },

  mediaPreviewUrl(previewId: string): string {
    return withQueryToken(`/api/media/preview/${encodeURIComponent(previewId)}`);
  }
};

function withQueryToken(path: string): string {
  const token = getAuthToken();
  return token ? `${path}?token=${encodeURIComponent(token)}` : path;
}

function draftPath(draftId: string): string {
  return `/api/drafts/${encodeURIComponent(draftId)}`;
}
