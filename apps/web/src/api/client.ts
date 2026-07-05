import { apiFetch } from "../auth";
import type { components, paths } from "./generated/schema";

// 后端这些响应暂未声明 pydantic response model（openapi 无对应 component），
// 先以本地接口描述实际返回形状；后端补 model 后应改回 components["schemas"][...]。
export type ProjectTreeCase = {
  case_id: string;
  project_id: string;
  name: string;
  status: string;
};
export type ProjectTreeProject = {
  project_id: string;
  name: string;
  status: string;
  cases: ProjectTreeCase[];
};
export type ProjectRecord = {
  project_id: string;
  name: string;
  status: string;
  [key: string]: unknown;
};
export type CaseRecord = {
  case_id: string;
  project_id: string;
  name: string;
  status: string;
  [key: string]: unknown;
};
export type DecisionOption = { option_id: string; label: string; [key: string]: unknown };
export type Decision = {
  decision_id: string;
  type: string;
  question: string;
  options: DecisionOption[];
  status: string;
  [key: string]: unknown;
};
export type DecisionAnswer = {
  option_id?: string | null;
  free_text?: string | null;
  answered_via: "button" | "natural_language";
  payload?: Record<string, unknown>;
};

type ProjectTreeResponse = { projects: ProjectTreeProject[] };
type ProjectListResponse = { projects: ProjectRecord[] };
type ProjectCreateRequest =
  paths["/api/projects"]["post"]["requestBody"]["content"]["application/json"];
type ProjectUpdateRequest =
  paths["/api/projects/{project_id}"]["patch"]["requestBody"]["content"]["application/json"];
type ProjectCopyRequest =
  paths["/api/projects/{project_id}/copy"]["post"]["requestBody"]["content"]["application/json"];
type ProjectMutationResponse = { project: ProjectRecord };
type CaseCreateRequest =
  paths["/api/projects/{project_id}/cases"]["post"]["requestBody"]["content"]["application/json"];
type CaseUpdateRequest =
  paths["/api/projects/{project_id}/cases/{case_id}"]["patch"]["requestBody"]["content"]["application/json"];
type CaseCopyRequest =
  paths["/api/projects/{project_id}/cases/{case_id}/copy"]["post"]["requestBody"]["content"]["application/json"];
type CaseMoveRequest =
  paths["/api/projects/{project_id}/cases/{case_id}/move"]["post"]["requestBody"]["content"]["application/json"];
type CaseMutationResponse = { case: CaseRecord };
type MessageCreateRequest =
  paths["/api/projects/{project_id}/cases/{case_id}/messages"]["post"]["requestBody"]["content"]["application/json"];
type MessageQueuedResponse = { message_id: string; queued: boolean; [key: string]: unknown };
type CurrentDecisionResponse = { decision: Decision | null };
type DecisionAnswerRequest =
  paths["/api/decisions/{decision_id}/answer"]["post"]["requestBody"]["content"]["application/json"];
type DecisionAnswerResponse = { decision_id: string; status: string; [key: string]: unknown };

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

  answerDecision(decisionId: string, payload: DecisionAnswerRequest): Promise<DecisionAnswerResponse> {
    return apiFetch<DecisionAnswerResponse>(`/api/decisions/${encodeURIComponent(decisionId)}/answer`, {
      method: "POST",
      body: payload
    });
  }
};

function casePath(projectId: string, caseId: string): string {
  return `/api/projects/${encodeURIComponent(projectId)}/cases/${encodeURIComponent(caseId)}`;
}
