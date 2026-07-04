"""Tool execution result contracts."""

from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field


class ToolArtifact(BaseModel):
    model_config = ConfigDict(extra="forbid")

    artifact_id: str
    kind: str


class ToolError(BaseModel):
    model_config = ConfigDict(extra="forbid")

    error_code: str
    message: str
    retryable: bool = False
    details: dict[str, Any] = Field(default_factory=dict)


class ToolResult(BaseModel):
    model_config = ConfigDict(extra="forbid")

    tool_call_id: str
    tool_name: str
    status: Literal["succeeded", "failed", "running", "requires_user"]
    observation: str
    artifacts: list[ToolArtifact] = Field(default_factory=list)
    events: list[dict[str, Any]] = Field(default_factory=list)
    error: ToolError | None = None
