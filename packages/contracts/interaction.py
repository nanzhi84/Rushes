"""Frontend-renderable structured interaction contracts."""

from __future__ import annotations

from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field

InteractionKind = Literal[
    "question",
    "confirmation",
    "progress",
    "preview",
    "timeline",
    "error",
]


class InteractionOption(BaseModel):
    model_config = ConfigDict(extra="forbid")

    option_id: str
    label: str
    description: str | None = None
    payload: dict[str, Any] = Field(default_factory=dict)


class StructuredInteractionEvent(BaseModel):
    """Schema consumed by the Draft Editor renderer."""

    model_config = ConfigDict(extra="forbid")

    kind: InteractionKind
    title: str
    body: str | None = None
    options: list[InteractionOption] = Field(default_factory=list)
    media_ref: str | None = None
    timeline_summary: str | None = None
    progress: float | None = None
    error_code: str | None = None
    retryable: bool | None = None
    metadata: dict[str, Any] = Field(default_factory=dict)
