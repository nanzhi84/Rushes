import { apiFetch } from "../auth";
import type { components, paths } from "./generated/schema";

export type ProjectTreeCase = components["schemas"]["ProjectTreeCase"];
export type ProjectTreeProject = components["schemas"]["ProjectTreeProject"];
export type ProjectRecord = components["schemas"]["ProjectRecord"];
export type CaseRecord = components["schemas"]["CaseRecord"];
export type DecisionOption = components["schemas"]["DecisionOption"];
export type Decision = components["schemas"]["Decision"];
export type DecisionAnswer = components["schemas"]["DecisionAnswerRequest"]["answer"];

type ProjectTreeResponse = components["schemas"]["ProjectTreeResponse"];
type ProjectListResponse = components["schemas"]["ProjectListResponse"];
type ProjectCreateRequest =
  paths["/api/projects"]["post"]["requestBody"]["content"]["application/json"];
type ProjectUpdateRequest =
  paths["/api/projects/{project_id}"]["patch"]["requestBody"]["content"]["application/json"];
type ProjectCopyRequest =
  paths["/api/projects/{project_id}/copy"]["post"]["requestBody"]["content"]["application/json"];
type ProjectMutationResponse = components["schemas"]["ProjectMutationResponse"];
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
