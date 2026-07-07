"""Provider cost record contracts."""

from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field


class ProviderCall(BaseModel):
    model_config = ConfigDict(extra="forbid")

    call_id: str
    provider_id: str
    capability: str
    model: str
    draft_id: str | None = None
    job_id: str | None = None
    latency_ms: int
    usage_json: dict[str, Any] = Field(default_factory=dict)
    cost_estimate: float | None = None
    status: Literal["succeeded", "failed", "running"]
