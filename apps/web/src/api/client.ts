import { ApiError, apiFetch, createApiEventSource, getAuthToken, handleUnauthorized } from "../auth";
import type { components, paths } from "./generated/schema";

export type ProjectTreeCase = components["schemas"]["ProjectTreeCase"];
export type ProjectTreeProject = components["schemas"]["ProjectTreeProject"];
export type ProjectRecord = components["schemas"]["ProjectRecord"];
export type CaseRecord = components["schemas"]["CaseRecord"];
export type DecisionOption = components["schemas"]["DecisionOption"];
export type Decision = components["schemas"]["Decision"];
export type DecisionAnswer = components["schemas"]["DecisionAnswerRequest"]["answer"];
export type FsRoot = components["schemas"]["FsRoot"];
export type FsListEntry = components["schemas"]["FsListEntry"];
export type FsListResponse = components["schemas"]["FsListResponse"];
export type FsPickResponse = components["schemas"]["FsPickResponse"];

// apps/api/schemas.py 已有 M2 materials/upload response models；当前生成 schema 尚未包含这些路径。
export type MaterialKind = "video" | "audio" | "image" | "font";
export type StorageMode = "copy" | "reference";

export type AssetJobSummary = {
  job_id: string;
  kind: string;
  status: string;
  progress: number | null;
  error_json: Record<string, unknown> | null;
};

export type UnderstandingStatus = "none" | "running" | "ready" | "failed";

export type MaterialAsset = {
  asset_id: string;
  storage_mode: StorageMode | string;
  kind: MaterialKind | string;
  source: string;
  filename: string;
  hash: string;
  size: number;
  mtime: number | null;
  ingest_status: string;
  understanding_status: UnderstandingStatus | string;
  usable: boolean;
  enabled: boolean;
  rel_dir?: string | null;
  probe: Record<string, unknown> | null;
  duration_sec: number | null;
  proxy_object_hash: string | null;
  proxy_ready: boolean;
  thumbnail_ready: boolean;
  invalid: boolean;
  failure: Record<string, unknown> | null;
  jobs: AssetJobSummary[];
};

export type MaterialsResponse = {
  project_id: string;
  assets: MaterialAsset[];
  invalidated_asset_ids: string[];
};

// GET .../materials/{aid}/summary 尚未进入 generated/schema.d.ts，先按 apps/api/schemas.py 手写。
// summary 字段对齐 packages/contracts/understanding.py MaterialSummary。
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

export type MaterialMutationResponse = {
  project_id: string;
  asset_id?: string | null;
  asset_ids?: string[];
  skipped?: string[];
  failed?: string[];
  duplicates?: string[];
  job_id?: string | null;
  decision_id?: string | null;
  event_ids: number[];
};

// M6 timeline/preview paths 尚未进入 generated/schema.d.ts，先按 apps/api/schemas.py 手写。
export type TimelineClipJson = {
  timeline_clip_id?: string;
  track_id?: string;
  timeline_start_frame?: number;
  timeline_end_frame?: number;
  asset_id?: string;
  clip_id?: string | null;
  role?: string;
  text?: string;
  [key: string]: unknown;
};

export type TimelineTrackJson = {
  track_id: string;
  clips?: TimelineClipJson[];
  [key: string]: unknown;
};

export type TimelineJson = {
  fps: number;
  duration_frames: number;
  tracks: TimelineTrackJson[];
  [key: string]: unknown;
};

export type CaseTimelineResponse = {
  case_id: string;
  timeline_version: number;
  timeline: TimelineJson;
  summary: string;
  preview_id: string | null;
};

// GET messages 尚未进入 generated/schema.d.ts，先按 apps/api/main.py list_case_messages 手写。
export type CaseMessage = {
  message_id: string;
  role: string;
  kind: string;
  content: string;
  created_at: string;
};

export type CaseMessagesResponse = {
  case_id: string;
  messages: CaseMessage[];
};

type MaterialImportLocalRequest = {
  path?: string;
  paths?: string[];
  storage_mode?: StorageMode | null;
  asset_id?: string | null;
};

type MaterialImportUrlRequest = {
  url: string;
  filename?: string | null;
  max_bytes?: number | null;
  asset_id?: string | null;
};

type MaterialAssetLinkRequest = {
  asset_id: string;
  enabled?: boolean;
  note?: string;
};

type MaterialPatchRequest = {
  enabled?: boolean | null;
  reference_path?: string | null;
};

type UploadInitRequest = {
  project_id: string;
  filename: string;
  size?: number | null;
  asset_id?: string | null;
};

type UploadInitResponse = {
  upload_id: string;
  part_url_template: string;
  complete_url: string;
};

type UploadPartResponse = {
  upload_id: string;
  part_number: number;
  size: number;
};

type UploadCompleteRequest = {
  project_id?: string | null;
  asset_id?: string | null;
  rel_dir?: string | null;
};

type UploadCompleteResponse = {
  upload_id: string;
  project_id: string;
  asset_id: string;
  event_ids: number[];
};

type ProjectTreeResponse = components["schemas"]["ProjectTreeResponse"];
type ProjectListResponse = components["schemas"]["ProjectListResponse"];
type ProjectCreateRequest =
  paths["/api/projects"]["post"]["requestBody"]["content"]["application/json"];
type ProjectUpdateRequest =
  paths["/api/projects/{project_id}"]["patch"]["requestBody"]["content"]["application/json"];
type ProjectCopyRequest =
  paths["/api/projects/{project_id}/copy"]["post"]["requestBody"]["content"]["application/json"];
type ProjectMutationResponse = components["schemas"]["ProjectMutationResponse"];
type CaseResponse = components["schemas"]["CaseResponse"];
type CaseCreateRequest =
  paths["/api/projects/{project_id}/cases"]["post"]["requestBody"]["content"]["application/json"];
type CaseUpdateRequest =
  paths["/api/projects/{project_id}/cases/{case_id}"]["patch"]["requestBody"]["content"]["application/json"];
type CaseCopyRequest =
  paths["/api/projects/{project_id}/cases/{case_id}/copy"]["post"]["requestBody"]["content"]["application/json"];
type CaseMoveRequest =
  paths["/api/projects/{project_id}/cases/{case_id}/move"]["post"]["requestBody"]["content"]["application/json"];
type CaseMutationResponse = components["schemas"]["CaseMutationResponse"];
type MessageCreateRequest =
  paths["/api/projects/{project_id}/cases/{case_id}/messages"]["post"]["requestBody"]["content"]["application/json"];
type MessageQueuedResponse = components["schemas"]["MessageQueuedResponse"];
type CurrentDecisionResponse = components["schemas"]["CurrentDecisionResponse"];
type DecisionAnswerRequest =
  paths["/api/decisions/{decision_id}/answer"]["post"]["requestBody"]["content"]["application/json"];
type DecisionAnswerResponse = components["schemas"]["DecisionAnswerResponse"];

export function fetchCaseTimeline(
  projectId: string,
  caseId: string,
  version?: number | null
): Promise<CaseTimelineResponse> {
  const params = new URLSearchParams();
  if (version !== undefined && version !== null) {
    params.set("version", String(version));
  }
  const query = params.toString();
  return apiFetch<CaseTimelineResponse>(
    `${casePath(projectId, caseId)}/timeline${query ? `?${query}` : ""}`
  );
}

export function postPreviewViewed(
  projectId: string,
  caseId: string,
  previewId: string
): Promise<CaseMutationResponse> {
  return apiFetch<CaseMutationResponse>(
    `${casePath(projectId, caseId)}/previews/${encodeURIComponent(previewId)}/viewed`,
    { method: "POST", body: {} }
  );
}

// limit=最老的前 N 条，升序返回；当前规模够用。
export function getCaseMessages(projectId: string, caseId: string): Promise<CaseMessagesResponse> {
  return apiFetch<CaseMessagesResponse>(`${casePath(projectId, caseId)}/messages?limit=200`);
}

// turn-stream SSE 工厂：鉴权同 createApiEventSource（query token）。断线由浏览器自动重连。
export function createCaseTurnStreamSource(projectId: string, caseId: string): EventSource {
  return createApiEventSource(`${casePath(projectId, caseId)}/turn-stream`);
}

export const api = {
  projectTree(): Promise<ProjectTreeResponse> {
    return apiFetch<ProjectTreeResponse>("/api/project-tree");
  },

  listProjects(): Promise<ProjectListResponse> {
    return apiFetch<ProjectListResponse>("/api/projects");
  },

  createProject(payload: ProjectCreateRequest): Promise<ProjectMutationResponse> {
    return apiFetch<ProjectMutationResponse>("/api/projects", {
      method: "POST",
      body: payload
    });
  },

  renameProject(projectId: string, payload: ProjectUpdateRequest): Promise<ProjectMutationResponse> {
    return apiFetch<ProjectMutationResponse>(`/api/projects/${encodeURIComponent(projectId)}`, {
      method: "PATCH",
      body: payload
    });
  },

  deleteProject(projectId: string, confirm = true): Promise<ProjectMutationResponse> {
    return apiFetch<ProjectMutationResponse>(`/api/projects/${encodeURIComponent(projectId)}`, {
      method: "DELETE",
      body: { confirm }
    });
  },

  copyProject(projectId: string, payload: ProjectCopyRequest = {}): Promise<ProjectMutationResponse> {
    return apiFetch<ProjectMutationResponse>(`/api/projects/${encodeURIComponent(projectId)}/copy`, {
      method: "POST",
      body: payload
    });
  },

  createCase(projectId: string, payload: CaseCreateRequest): Promise<CaseMutationResponse> {
    return apiFetch<CaseMutationResponse>(`/api/projects/${encodeURIComponent(projectId)}/cases`, {
      method: "POST",
      body: payload
    });
  },

  getCase(projectId: string, caseId: string): Promise<CaseResponse> {
    return apiFetch<CaseResponse>(casePath(projectId, caseId));
  },

  renameCase(
    projectId: string,
    caseId: string,
    payload: CaseUpdateRequest
  ): Promise<CaseMutationResponse> {
    return apiFetch<CaseMutationResponse>(casePath(projectId, caseId), {
      method: "PATCH",
      body: payload
    });
  },

  deleteCase(projectId: string, caseId: string, confirm = true): Promise<CaseMutationResponse> {
    return apiFetch<CaseMutationResponse>(casePath(projectId, caseId), {
      method: "DELETE",
      body: { confirm }
    });
  },

  copyCase(
    projectId: string,
    caseId: string,
    payload: CaseCopyRequest = {}
  ): Promise<CaseMutationResponse> {
    return apiFetch<CaseMutationResponse>(`${casePath(projectId, caseId)}/copy`, {
      method: "POST",
      body: payload
    });
  },

  moveCase(projectId: string, caseId: string, payload: CaseMoveRequest): Promise<CaseMutationResponse> {
    return apiFetch<CaseMutationResponse>(`${casePath(projectId, caseId)}/move`, {
      method: "POST",
      body: payload
    });
  },

  postMessage(
    projectId: string,
    caseId: string,
    payload: MessageCreateRequest
  ): Promise<MessageQueuedResponse> {
    return apiFetch<MessageQueuedResponse>(`${casePath(projectId, caseId)}/messages`, {
      method: "POST",
      body: payload
    });
  },

  currentDecision(projectId: string, caseId: string): Promise<CurrentDecisionResponse> {
    return apiFetch<CurrentDecisionResponse>(`${casePath(projectId, caseId)}/decisions/current`);
  },

  fetchCaseTimeline,

  postPreviewViewed,

  getCaseMessages,

  answerDecision(decisionId: string, payload: DecisionAnswerRequest): Promise<DecisionAnswerResponse> {
    return apiFetch<DecisionAnswerResponse>(`/api/decisions/${encodeURIComponent(decisionId)}/answer`, {
      method: "POST",
      body: payload
    });
  },

  listMaterials(projectId: string): Promise<MaterialsResponse> {
    return apiFetch<MaterialsResponse>(`${projectPath(projectId)}/materials`);
  },

  revalidateMaterials(projectId: string): Promise<MaterialsResponse> {
    return apiFetch<MaterialsResponse>(`${projectPath(projectId)}/materials/revalidate`, {
      method: "POST"
    });
  },

  importLocalMaterial(
    projectId: string,
    payload: MaterialImportLocalRequest
  ): Promise<MaterialMutationResponse> {
    return apiFetch<MaterialMutationResponse>(`${projectPath(projectId)}/materials/import-local`, {
      method: "POST",
      body: payload
    });
  },

  importUrlMaterial(
    projectId: string,
    payload: MaterialImportUrlRequest
  ): Promise<MaterialMutationResponse> {
    return apiFetch<MaterialMutationResponse>(`${projectPath(projectId)}/materials/import-url`, {
      method: "POST",
      body: payload
    });
  },

  linkMaterial(projectId: string, payload: MaterialAssetLinkRequest): Promise<MaterialMutationResponse> {
    return apiFetch<MaterialMutationResponse>(`${projectPath(projectId)}/materials/link`, {
      method: "POST",
      body: payload
    });
  },

  unlinkMaterial(
    projectId: string,
    payload: MaterialAssetLinkRequest
  ): Promise<MaterialMutationResponse> {
    return apiFetch<MaterialMutationResponse>(`${projectPath(projectId)}/materials/unlink`, {
      method: "POST",
      body: payload
    });
  },

  patchMaterial(
    projectId: string,
    assetId: string,
    payload: MaterialPatchRequest
  ): Promise<MaterialMutationResponse> {
    return apiFetch<MaterialMutationResponse>(
      `${projectPath(projectId)}/materials/${encodeURIComponent(assetId)}`,
      {
        method: "PATCH",
        body: payload
      }
    );
  },

  getAssetSummary(projectId: string, assetId: string): Promise<MaterialSummaryResponse> {
    return apiFetch<MaterialSummaryResponse>(
      `${projectPath(projectId)}/materials/${encodeURIComponent(assetId)}/summary`
    );
  },

  fsRoots(): Promise<components["schemas"]["FsRootsResponse"]> {
    return apiFetch<components["schemas"]["FsRootsResponse"]>("/api/fs/roots");
  },

  /** 弹出宿主机原生选择对话框（macOS）；available=false 时前端回退分片上传。 */
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

  initUpload(payload: UploadInitRequest): Promise<UploadInitResponse> {
    return apiFetch<UploadInitResponse>("/api/uploads/init", {
      method: "POST",
      body: payload
    });
  },

  uploadPart(partUrl: string, body: Blob): Promise<UploadPartResponse> {
    return apiBinaryFetch<UploadPartResponse>(partUrl, {
      method: "PUT",
      body,
      headers: { "Content-Type": "application/octet-stream" }
    });
  },

  completeUpload(
    completeUrl: string,
    payload: UploadCompleteRequest = {}
  ): Promise<UploadCompleteResponse> {
    return apiFetch<UploadCompleteResponse>(completeUrl, {
      method: "POST",
      body: payload
    });
  },

  // media 族 URL 由浏览器原生 <img>/<video>/wavesurfer 直连，设不了 Authorization header，
  // 统一带 query token（后端 apps/api/deps.py 对 GET /api/media/ 前缀放行 query token，语义同 SSE）。
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

function projectPath(projectId: string): string {
  return `/api/projects/${encodeURIComponent(projectId)}`;
}

function casePath(projectId: string, caseId: string): string {
  return `${projectPath(projectId)}/cases/${encodeURIComponent(caseId)}`;
}

async function apiBinaryFetch<T>(path: string, options: RequestInit): Promise<T> {
  const headers = new Headers(options.headers);
  const token = getAuthToken();
  if (token) {
    headers.set("Authorization", `Bearer ${token}`);
  }
  const response = await fetch(path, { ...options, headers });

  if (response.status === 401) {
    handleUnauthorized();
  }

  if (!response.ok) {
    throw new ApiError(response.status, `API 请求失败：${response.status}`, await readPayload(response));
  }

  const text = await response.text();
  return (text ? JSON.parse(text) : undefined) as T;
}

async function readPayload(response: Response): Promise<unknown> {
  const text = await response.text();
  if (!text) {
    return null;
  }
  try {
    return JSON.parse(text);
  } catch {
    return text;
  }
}
