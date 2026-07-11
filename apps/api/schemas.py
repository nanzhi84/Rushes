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


class CostSummary(ApiResponseModel):
    provider_call_count: int
    total_cost_estimate: float
    by_capability: dict[str, float]
    by_provider: dict[str, float]


class DraftListItem(ApiResponseModel):
    # 草稿墙列表项：聚合封面消 N+1（cover_asset_ids ≤4，thumbnail_ready，导入时间倒序）。
    draft_id: str
    name: str
    status: str
    updated_at: str
    material_count: int
    cover_asset_ids: list[str]


class DraftListResponse(ApiResponseModel):
    drafts: list[DraftListItem]


class DraftRecord(ApiResponseModel):
    draft_id: str
    name: str
    state_version: int
    status: str
    defaults: dict[str, Any]
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
    scratch_memory: dict[str, Any]
    messages_tail_ref: str | None
    created_at: str
    updated_at: str


class DraftResponse(ApiResponseModel):
    draft: DraftRecord


class DraftMutationResponse(ApiResponseModel):
    draft: DraftRecord
    event_ids: list[int]


class DraftTimelineResponse(ApiResponseModel):
    draft_id: str
    timeline_version: int
    timeline: dict[str, Any]
    summary: str
    preview_id: str | None


class MessageQueuedResponse(ApiResponseModel):
    status: Literal["queued"]
    kind: Literal["user_message"]
    draft_id: str
    message_id: str


class TurnCancelResponse(ApiResponseModel):
    draft_id: str
    status: Literal["requested", "idle"]
    requested: bool


class MessageRecord(ApiResponseModel):
    message_id: str
    role: str
    kind: str
    content: str
    created_at: str


class MessagesResponse(ApiResponseModel):
    draft_id: str
    messages: list[MessageRecord]


class CurrentDecisionResponse(ApiResponseModel):
    decision: Decision | None


class PendingDecisionsResponse(ApiResponseModel):
    draft_id: str
    decisions: list[Decision]


class DecisionAnswerResponse(ApiResponseModel):
    decision_id: str
    status: Literal["answered"]
    event_ids: list[int]
    replays_enqueued: int


class DraftCostsResponse(ApiResponseModel):
    draft_id: str
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
    rel_dir: str | None = None
    probe: dict[str, Any] | None
    duration_sec: float | None
    proxy_object_hash: str | None
    proxy_ready: bool
    thumbnail_ready: bool
    invalid: bool
    failure: dict[str, Any] | None
    jobs: list[AssetJobSummary]


class MaterialsResponse(ApiResponseModel):
    draft_id: str
    assets: list[MaterialAsset]
    invalidated_asset_ids: list[str] = []


class MaterialSummaryResponse(ApiResponseModel):
    asset_id: str
    summary: dict[str, Any]


class MaterialMutationResponse(ApiResponseModel):
    draft_id: str
    asset_id: str | None = None
    # 批量/文件夹导入：新建 asset_id、跳过的非媒体/越界文件、读取失败文件、草稿内已链接的重复文件。
    asset_ids: list[str] = []
    skipped: list[str] = []
    failed: list[str] = []
    duplicates: list[str] = []
    job_id: str | None = None
    decision_id: str | None = None
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


class FsPickResponse(ApiResponseModel):
    # 宿主机原生选择对话框：非 macOS/无 GUI 时 available=false；用户取消时 paths 为空。
    available: bool
    paths: list[str] = []


class FsListResponse(ApiResponseModel):
    path: str
    entries: list[FsListEntry]


class ReducerConflictDetail(ApiResponseModel):
    draft_id: str
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
