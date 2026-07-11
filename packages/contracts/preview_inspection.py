"""Rendered preview inspection contracts."""

from __future__ import annotations

from typing import Literal

from pydantic import BaseModel, ConfigDict, Field


class PreviewInspectionIssue(BaseModel):
    model_config = ConfigDict(extra="forbid")

    at_sec: float | None = Field(default=None, ge=0)
    end_sec: float | None = Field(default=None, ge=0)
    severity: Literal["info", "warning", "error"]
    category: str
    description: str
    metric: str | None = None
    suggested_action: str | None = None


class PreviewInspectionResult(BaseModel):
    model_config = ConfigDict(extra="forbid")

    summary: str
    degraded: bool
    issues: list[PreviewInspectionIssue] = Field(default_factory=list)
