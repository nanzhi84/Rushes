"""Decision and pending tool-call contracts."""

from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field, model_validator

DecisionScopeType = Literal["workspace", "project", "case"]
DecisionType = Literal[
    "audio_mode",
    "approve_content_plan",
    "approve_speech_cut",
    "approve_rough_cut",
    "subtitle",
    "bgm",
    "export",
    "memory_scope",
    "destructive_project_action",
    "url_import",
    "generic",
]


class DecisionOption(BaseModel):
    model_config = ConfigDict(extra="forbid")

    option_id: str
    label: str
    description: str | None = None
    payload: dict[str, Any] = Field(default_factory=dict)


class DecisionAnswer(BaseModel):
    model_config = ConfigDict(extra="forbid")

    option_id: str | None = None
    free_text: str | None = None
    answered_via: Literal["button", "natural_language"]
    payload: dict[str, Any] = Field(default_factory=dict)


class PendingToolCall(BaseModel):
    model_config = ConfigDict(extra="forbid")

    tool_name: str
    arguments: dict[str, Any]
    idempotency_key: str
    argument_fingerprint: str
    original_tool_call_id: str | None = None


class Decision(BaseModel):
    model_config = ConfigDict(extra="forbid")

    decision_id: str
    scope_type: DecisionScopeType
    project_id: str | None = None
    case_id: str | None = None
    type: DecisionType
    question: str
    options: list[DecisionOption] = Field(default_factory=list)
    allow_free_text: bool = False
    status: Literal["pending", "answered", "cancelled"] = "pending"
    answer: DecisionAnswer | None = None
    pending_tool_call: PendingToolCall | None = None
    pending_tool_call_status: Literal["pending", "approved", "replayed", "discarded"] | None = None
    consumed_at: str | None = None
    replayed_tool_call_id: str | None = None
    blocking: bool = False
    created_by_tool_call_id: str | None = None

    @model_validator(mode="after")
    def validate_scope_and_outbox(self) -> "Decision":
        if self.scope_type == "case":
            if self.project_id is None or self.case_id is None:
                raise ValueError("scope_type=case requires project_id and case_id")
        elif self.scope_type == "project":
            if self.project_id is None:
                raise ValueError("scope_type=project requires project_id")
            if self.case_id is not None:
                raise ValueError("scope_type=project requires case_id to be None")
            if self.blocking:
                raise ValueError("project-scoped decisions must not block a case loop")
        else:
            if self.project_id is not None or self.case_id is not None:
                raise ValueError("scope_type=workspace requires project_id and case_id to be None")
            if self.blocking:
                raise ValueError("workspace-scoped decisions must not block a case loop")

        if self.pending_tool_call is None and self.pending_tool_call_status is not None:
            raise ValueError("pending_tool_call_status requires pending_tool_call")
        if self.pending_tool_call is not None and self.pending_tool_call_status is None:
            raise ValueError("pending_tool_call requires pending_tool_call_status")
        if self.status == "answered" and self.answer is None:
            raise ValueError("answered decisions require answer")
        return self
