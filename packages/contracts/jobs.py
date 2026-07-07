"""Job table contracts."""

from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field

JobKind = Literal[
    "annotation",
    "asr",
    "tts",
    "render_preview",
    "render_final",
    "proxy",
    "index",
    "align",
    "import_url",
    "noop",
]
JobStatus = Literal["pending", "running", "succeeded", "failed", "cancelled"]


class JobError(BaseModel):
    model_config = ConfigDict(extra="forbid")

    error_code: str
    message: str
    retryable: bool = False
    stderr_summary: str | None = None
    details: dict[str, Any] = Field(default_factory=dict)


class Job(BaseModel):
    model_config = ConfigDict(extra="forbid")

    job_id: str
    kind: JobKind
    status: JobStatus = "pending"
    draft_id: str | None = None
    requested_by_draft_id: str | None = None
    asset_id: str | None = None
    idempotency_key: str
    payload_json: dict[str, Any] = Field(default_factory=dict)
    result_json: dict[str, Any] | None = None
    error_json: JobError | None = None
    attempts: int = 0
    max_retries: int = 2
    next_run_at: str | None = None
    progress: float | None = None
    worker_id: str | None = None
    heartbeat_at: str | None = None
    created_at: str
    started_at: str | None = None
    finished_at: str | None = None
