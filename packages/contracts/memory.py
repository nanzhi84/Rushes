"""Long-term memory contracts."""

from typing import Literal

from pydantic import BaseModel, ConfigDict, Field


class Memory(BaseModel):
    model_config = ConfigDict(extra="forbid")

    memory_id: str
    scope: Literal["user"] = "user"
    content: str
    tags: list[str] = Field(default_factory=list)
    created_from_draft_id: str | None = None
    created_at: str


class MemoryCandidate(BaseModel):
    model_config = ConfigDict(extra="forbid")

    candidate_id: str
    draft_id: str
    content: str
    suggested_scope: Literal["user"] = "user"
    status: Literal["pending", "saved", "discarded"] = "pending"
    saved_memory_id: str | None = None
    created_at: str
