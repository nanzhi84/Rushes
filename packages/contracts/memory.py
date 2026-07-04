"""Long-term memory contracts."""

from typing import Literal

from pydantic import BaseModel, ConfigDict, Field, model_validator


class Memory(BaseModel):
    model_config = ConfigDict(extra="forbid")

    memory_id: str
    scope: Literal["user", "project"]
    project_id: str | None = None
    content: str
    tags: list[str] = Field(default_factory=list)
    created_from_case_id: str | None = None
    created_at: str

    @model_validator(mode="after")
    def validate_scope(self) -> "Memory":
        if self.scope == "project" and self.project_id is None:
            raise ValueError("project-scoped memory requires project_id")
        if self.scope == "user" and self.project_id is not None:
            raise ValueError("user-scoped memory must not set project_id")
        return self


class MemoryCandidate(BaseModel):
    model_config = ConfigDict(extra="forbid")

    candidate_id: str
    case_id: str
    content: str
    suggested_scope: Literal["user", "project"]
    status: Literal["pending", "saved", "discarded"] = "pending"
    saved_memory_id: str | None = None
    created_at: str
