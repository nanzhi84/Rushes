"""Project state contracts."""

from typing import Literal

from pydantic import BaseModel, ConfigDict, Field


class ProjectDefaults(BaseModel):
    model_config = ConfigDict(extra="forbid")

    aspect_ratio: str = "9:16"
    fps: int = 30
    preview_quality: str = "low"
    export_quality: str = "high"


class ProjectAssetLink(BaseModel):
    model_config = ConfigDict(extra="forbid")

    asset_id: str
    enabled: bool = True
    linked_at: str
    note: str = ""


class ProjectState(BaseModel):
    model_config = ConfigDict(extra="forbid")

    project_id: str
    name: str
    status: Literal["active", "archived", "trashed"] = "active"
    asset_links: list[ProjectAssetLink] = Field(default_factory=list)
    case_ids: list[str] = Field(default_factory=list)
    memory_ids: list[str] = Field(default_factory=list)
    defaults: ProjectDefaults = Field(default_factory=ProjectDefaults)
    created_at: str
    updated_at: str
