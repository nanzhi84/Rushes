"""Workspace-level configuration contracts."""

from pydantic import BaseModel, ConfigDict, Field


class WorkspaceDefaults(BaseModel):
    model_config = ConfigDict(extra="forbid")

    aspect_ratio: str = "9:16"
    fps: int = 30
    preview_quality: str = "low"
    export_quality: str = "high"


class WorkspaceProjectRef(BaseModel):
    model_config = ConfigDict(extra="forbid")

    project_id: str
    name: str | None = None
    status: str | None = None


class WorkspaceConfig(BaseModel):
    model_config = ConfigDict(extra="forbid")

    workspace_id: str = "local"
    project_ids: list[str] = Field(default_factory=list)
    project_refs: list[WorkspaceProjectRef] = Field(default_factory=list)
    defaults: WorkspaceDefaults = Field(default_factory=WorkspaceDefaults)
    created_at: str | None = None
    updated_at: str | None = None
