"""Tool registry contracts."""

from typing import Literal

from pydantic import BaseModel, ConfigDict, Field


class ToolSpec(BaseModel):
    model_config = ConfigDict(extra="forbid", arbitrary_types_allowed=True)

    name: str
    namespace: str
    version: str
    status: Literal["stable", "experimental", "deprecated"] = "stable"
    input_model: type[BaseModel]
    result_model: type[BaseModel] | None = None
    handler_ref: str
    allowed_scopes: list[str]
    requires_artifacts: list[str]
    requires_active_draft: bool = True
    requires_confirmation: bool = False
    confirmation_decision_type: str | None = None
    side_effects: list[Literal["draft", "asset", "timeline", "memory", "object_store", "job"]]
    idempotency_key_fields: list[str] = Field(default_factory=list)
    emits_events: list[str]
    is_long_running: bool = False
    exposure: Literal["llm", "harness_only"] = "llm"
    # 供渐进披露排序：free=纯读本地状态；cheap=本地计算/写状态；expensive=云端模型或长任务。
    cost_tier: Literal["free", "cheap", "expensive"] = "cheap"
    description: str


class PatchOpSpec(BaseModel):
    model_config = ConfigDict(extra="forbid", arbitrary_types_allowed=True)

    kind: str
    params_model: type[BaseModel]
    requires_confirmation: bool = False
    confirmation_decision_type: str | None = None
    requires_artifacts: list[str] = Field(default_factory=list)
    ripple_semantics: Literal["ripple", "in_place", "n/a"] = "n/a"
    description: str
