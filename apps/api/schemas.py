"""Pydantic response schemas for the Rushes API."""

from __future__ import annotations

from typing import Any, Literal

from pydantic import BaseModel, ConfigDict

from contracts.decision import Decision

type SecurityReason = Literal[
    "missing_token",
    "bad_token",
    "host_mismatch",
    "origin_mismatch",
    "path_escape",
    "bad_content_type",
]


class ApiResponseModel(BaseModel):
    model_config = ConfigDict(extra="forbid")


class ProjectTreeCase(ApiResponseModel):
    case_id: str
    project_id: str
    name: str
    status: str


class ProjectTreeProject(ApiResponseModel):
    project_id: str
    name: str
    status: str
    cases: list[ProjectTreeCase]


class ProjectTreeResponse(ApiResponseModel):
    projects: list[ProjectTreeProject]


class ProjectRecord(ApiResponseModel):
    project_id: str
    name: str
    status: str
    defaults: dict[str, Any]
    created_at: str
    updated_at: str


class ProjectListResponse(ApiResponseModel):
    projects: list[ProjectRecord]


class ProjectMutationResponse(ApiResponseModel):
    project: ProjectRecord
    event_ids: list[int]


class ProjectPageCase(ApiResponseModel):
    case_id: str
    project_id: str
    name: str
    status: str
    brief: dict[str, Any]


class ProjectPageActions(ApiResponseModel):
    create_case: str
    materials: str


class CostSummary(ApiResponseModel):
    provider_call_count: int
    total_cost_estimate: float
    by_capability: dict[str, float]
    by_provider: dict[str, float]


class ProjectPageResponse(ApiResponseModel):
    project: ProjectRecord
    cases: list[ProjectPageCase]
    case_count: int
    asset_count: int
    memory_count: int
    costs: CostSummary
    actions: ProjectPageActions


class CaseRecord(ApiResponseModel):
    case_id: str
    project_id: str
    name: str
    state_version: int
    status: str
    pending_decision_id: str | None
    running_jobs: list[dict[str, Any]]
    last_error: dict[str, Any] | None
    brief: dict[str, Any]
    content_plan: dict[str, Any] | None
    audio_plan: dict[str, Any] | None
    cut_plan: dict[str, Any] | None
    timeline_current_version: int | None
    timeline_validated: bool
    preview_current_id: str | None
    last_viewed_preview_id: str | None
    rough_cut_approved: bool
    rough_cut_approved_version: int | None
    postprocess_plan: dict[str, Any] | None
    export_current_id: str | None
    selected_asset_ids: list[str]
    disabled_asset_ids: list[str]
    scratch_memory: dict[str, Any]


class CaseResponse(ApiResponseModel):
    case: CaseRecord


class CaseMutationResponse(ApiResponseModel):
    case: CaseRecord
    event_ids: list[int]


class CaseTimelineResponse(ApiResponseModel):
    case_id: str
    timeline_version: int
    timeline: dict[str, Any]
    summary: str
    preview_id: str | None


class MessageQueuedResponse(ApiResponseModel):
    status: Literal["queued"]
    kind: Literal["user_message"]
    project_id: str
    case_id: str
    message_id: str


class MessageRecord(ApiResponseModel):
    message_id: str
    role: str
    kind: str
    content: str
    created_at: str


class MessagesResponse(ApiResponseModel):
    case_id: str
    messages: list[MessageRecord]


class CurrentDecisionResponse(ApiResponseModel):
    decision: Decision | None


class PendingDecisionsResponse(ApiResponseModel):
    project_id: str
    decisions: list[Decision]


class DecisionAnswerResponse(ApiResponseModel):
    decision_id: str
    status: Literal["answered"]
    event_ids: list[int]
    replays_enqueued: int


class CaseCostsResponse(ApiResponseModel):
    project_id: str
    case_id: str
    costs: CostSummary


class JobCancelResponse(ApiResponseModel):
    job_id: str
    status: Literal["cancelled"]
    event_ids: list[int]


class AssetJobSummary(ApiResponseModel):
    job_id: str
    kind: str
    status: str
    progress: float | None = None
    error_json: dict[str, Any] | None = None


class MaterialAsset(ApiResponseModel):
    asset_id: str
    storage_mode: str
    kind: str
    source: str
    filename: str
    hash: str
    size: int
    mtime: int | None
    ingest_status: str
    understanding_status: str
    usable: bool
    enabled: bool
    probe: dict[str, Any] | None
    duration_sec: float | None
    proxy_object_hash: str | None
    proxy_ready: bool
    thumbnail_ready: bool
    invalid: bool
    failure: dict[str, Any] | None
    jobs: list[AssetJobSummary]


class MaterialsResponse(ApiResponseModel):
    project_id: str
    assets: list[MaterialAsset]
    invalidated_asset_ids: list[str] = []


class MaterialMutationResponse(ApiResponseModel):
    project_id: str
    asset_id: str | None = None
    job_id: str | None = None
    decision_id: str | None = None
    event_ids: list[int]


class UploadInitResponse(ApiResponseModel):
    upload_id: str
    part_url_template: str
    complete_url: str


class UploadPartResponse(ApiResponseModel):
    upload_id: str
    part_number: int
    size: int


class UploadCompleteResponse(ApiResponseModel):
    upload_id: str
    project_id: str
    asset_id: str
    event_ids: list[int]


class FsRoot(ApiResponseModel):
    path: str
    name: str
    exists: bool


class FsRootsResponse(ApiResponseModel):
    roots: list[FsRoot]


class FsListEntry(ApiResponseModel):
    name: str
    path: str
    type: Literal["directory", "file"]
    size: int | None = None
    extension: str | None = None


class FsListResponse(ApiResponseModel):
    path: str
    entries: list[FsListEntry]


class ReducerConflictDetail(ApiResponseModel):
    case_id: str
    expected_base_version: int | None
    actual_state_version: int
    event_type: str


class ErrorDetail(ApiResponseModel):
    reason: str
    conflict: ReducerConflictDetail | None = None


class ErrorResponse(ApiResponseModel):
    detail: ErrorDetail


class SecurityRefusalResponse(ApiResponseModel):
    error: Literal["SecurityRefusal"]
    reason: SecurityReason
