"""ToolSpec declarations for the Rushes tool registry."""

from __future__ import annotations

from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field

from contracts.decision import (
    DecisionAnswer,
    DecisionOption,
    DecisionScopeType,
    DecisionType,
    PendingToolCall,
)
from contracts.patch import (
    AddBgmOp,
    AdjustGainOp,
    DeleteRangeOp,
    EditSubtitleTextOp,
    GenerateSubtitlesOp,
    InsertCandidateOp,
    RemoveTrackClipsOp,
    ReorderBlocksOp,
    ReplaceClipOp,
    SetPlaybackRateOp,
    SetSubtitleStyleOp,
    TrimClipOp,
)
from contracts.tool import PatchOpSpec, ToolSpec

from .registry import PatchOpRegistry, ToolHandler, ToolRegistry


class RespondInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    message: str
    message_id: str | None = None


class RefuseInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    reason: str
    message: str | None = None
    message_id: str | None = None


class FinishTurnInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    reason: str | None = None


class DecisionAnswerInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    decision_id: str
    answer: DecisionAnswer


class AskUserInput(BaseModel):
    model_config = ConfigDict(extra="forbid", populate_by_name=True)

    question: str
    scope_type: DecisionScopeType = "case"
    project_id: str | None = None
    case_id: str | None = None
    decision_id: str | None = None
    decision_type: DecisionType = Field(default="generic", alias="type")
    options: list[DecisionOption] = Field(default_factory=list)
    allow_free_text: bool = True
    blocking: bool = True
    reduce_target: Literal["brief.confirmed_facts", "scratch_memory"] | None = None
    metadata: dict[str, object] = Field(default_factory=dict)


class ConfirmActionInput(BaseModel):
    model_config = ConfigDict(extra="forbid", populate_by_name=True)

    question: str
    scope_type: DecisionScopeType = "case"
    project_id: str | None = None
    case_id: str | None = None
    decision_id: str | None = None
    decision_type: DecisionType = Field(default="generic", alias="type")
    options: list[DecisionOption] = Field(default_factory=list)
    pending_tool_call: PendingToolCall | None = None
    blocking: bool = True
    metadata: dict[str, object] = Field(default_factory=dict)


class ShowProgressInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    title: str
    body: str | None = None
    progress: float | None = None
    metadata: dict[str, object] = Field(default_factory=dict)


class ShowPreviewInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    preview_id: str
    title: str = "预览已生成"
    body: str | None = None
    media_ref: str | None = None
    metadata: dict[str, object] = Field(default_factory=dict)


class ShowTimelineInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    timeline_summary: str
    title: str = "时间线"
    body: str | None = None
    metadata: dict[str, object] = Field(default_factory=dict)


class ShowErrorInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    title: str
    body: str
    error_code: str
    retryable: bool = False
    metadata: dict[str, object] = Field(default_factory=dict)


class ProjectCreateInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    project_id: str | None = None
    name: str = "Untitled Project"
    defaults: dict[str, Any] = Field(default_factory=dict)


class ProjectRenameInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    project_id: str | None = None
    name: str


class ProjectDeleteInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    project_id: str | None = None


class ProjectCopyInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    source_project_id: str | None = None
    project_id: str | None = None
    name: str | None = None


class ProjectCreateCaseInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    project_id: str | None = None
    case_id: str | None = None
    name: str = "Untitled Case"
    goal: str | None = None
    brief: dict[str, Any] = Field(default_factory=dict)


class ProjectMoveCaseInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    case_id: str | None = None
    target_project_id: str


class ProjectCloseCaseInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    case_id: str | None = None


class ProjectListTreeInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    include_trashed: bool = True


def tool_specs() -> tuple[ToolSpec, ...]:
    return (
        ToolSpec(
            name="respond",
            namespace="builtin",
            version="1",
            input_model=RespondInput,
            result_model=None,
            handler_ref="tools.builtin.respond",
            allowed_scopes=["case_agent_console"],
            requires_artifacts=[],
            requires_active_project=False,
            requires_active_case=False,
            side_effects=[],
            emits_events=["TurnEnded"],
            description="Append an assistant message and end the current turn.",
        ),
        ToolSpec(
            name="refuse",
            namespace="builtin",
            version="1",
            input_model=RefuseInput,
            result_model=None,
            handler_ref="tools.builtin.refuse",
            allowed_scopes=["case_agent_console"],
            requires_artifacts=[],
            requires_active_project=False,
            requires_active_case=False,
            side_effects=[],
            emits_events=["TurnEnded"],
            description="Return an out-of-scope refusal and end the current turn.",
        ),
        ToolSpec(
            name="finish_turn",
            namespace="builtin",
            version="1",
            input_model=FinishTurnInput,
            result_model=None,
            handler_ref="tools.builtin.finish_turn",
            allowed_scopes=["case_agent_console"],
            requires_artifacts=[],
            requires_active_project=False,
            requires_active_case=False,
            side_effects=[],
            emits_events=["TurnEnded"],
            description="End the current turn without adding an assistant message.",
        ),
        ToolSpec(
            name="decision.answer",
            namespace="decision",
            version="1",
            input_model=DecisionAnswerInput,
            result_model=None,
            handler_ref="tools.builtin.decision_answer",
            allowed_scopes=["case_agent_console"],
            requires_artifacts=[],
            requires_active_project=False,
            requires_active_case=False,
            side_effects=["case"],
            emits_events=["DecisionAnswered"],
            description="Convert a structured user answer into a DecisionAnswered event.",
        ),
        ToolSpec(
            name="interaction.ask_user",
            namespace="interaction",
            version="1",
            input_model=AskUserInput,
            result_model=None,
            handler_ref="tools.interaction.ask_user",
            allowed_scopes=["case_agent_console"],
            requires_artifacts=[],
            requires_active_project=False,
            requires_active_case=False,
            side_effects=["case"],
            emits_events=["DecisionCreated"],
            description="Ask an open or multiple-choice question through a Decision.",
        ),
        ToolSpec(
            name="interaction.confirm_action",
            namespace="interaction",
            version="1",
            input_model=ConfirmActionInput,
            result_model=None,
            handler_ref="tools.interaction.confirm_action",
            allowed_scopes=["case_agent_console"],
            requires_artifacts=[],
            requires_active_project=False,
            requires_active_case=False,
            side_effects=["case"],
            emits_events=["DecisionCreated"],
            description="Create a confirmation Decision for an action.",
        ),
        ToolSpec(
            name="interaction.show_progress",
            namespace="interaction",
            version="1",
            input_model=ShowProgressInput,
            result_model=None,
            handler_ref="tools.interaction.show_progress",
            allowed_scopes=["case_agent_console"],
            requires_artifacts=[],
            requires_active_project=False,
            requires_active_case=False,
            side_effects=[],
            emits_events=[],
            description="Emit a frontend-renderable progress interaction.",
        ),
        ToolSpec(
            name="interaction.show_preview",
            namespace="interaction",
            version="1",
            input_model=ShowPreviewInput,
            result_model=None,
            handler_ref="tools.interaction.show_preview",
            allowed_scopes=["case_agent_console"],
            requires_artifacts=[],
            requires_active_project=False,
            requires_active_case=False,
            side_effects=[],
            emits_events=[],
            description="Emit a frontend-renderable preview interaction.",
        ),
        ToolSpec(
            name="interaction.show_timeline",
            namespace="interaction",
            version="1",
            input_model=ShowTimelineInput,
            result_model=None,
            handler_ref="tools.interaction.show_timeline",
            allowed_scopes=["case_agent_console"],
            requires_artifacts=[],
            requires_active_project=False,
            requires_active_case=False,
            side_effects=[],
            emits_events=[],
            description="Emit a frontend-renderable timeline summary.",
        ),
        ToolSpec(
            name="interaction.show_error",
            namespace="interaction",
            version="1",
            input_model=ShowErrorInput,
            result_model=None,
            handler_ref="tools.interaction.show_error",
            allowed_scopes=["case_agent_console"],
            requires_artifacts=[],
            requires_active_project=False,
            requires_active_case=False,
            side_effects=[],
            emits_events=[],
            description="Emit a frontend-renderable structured error.",
        ),
        ToolSpec(
            name="project.create",
            namespace="project",
            version="1",
            input_model=ProjectCreateInput,
            result_model=None,
            handler_ref="tools.project.create",
            allowed_scopes=["case_agent_console", "project_page"],
            requires_artifacts=[],
            requires_active_project=False,
            requires_active_case=False,
            side_effects=["project"],
            idempotency_key_fields=["project_id"],
            emits_events=["ProjectCreated"],
            description="Create a new Project.",
        ),
        ToolSpec(
            name="project.rename",
            namespace="project",
            version="1",
            input_model=ProjectRenameInput,
            result_model=None,
            handler_ref="tools.project.rename",
            allowed_scopes=["case_agent_console", "project_page"],
            requires_artifacts=[],
            requires_active_project=True,
            requires_active_case=False,
            side_effects=["project"],
            idempotency_key_fields=["project_id", "name"],
            emits_events=["ProjectRenamed"],
            description="Rename the active Project.",
        ),
        ToolSpec(
            name="project.delete",
            namespace="project",
            version="1",
            input_model=ProjectDeleteInput,
            result_model=None,
            handler_ref="tools.project.delete",
            allowed_scopes=["case_agent_console", "project_page"],
            requires_artifacts=[],
            requires_active_project=True,
            requires_active_case=False,
            requires_confirmation=True,
            confirmation_decision_type="destructive_project_action",
            side_effects=["project"],
            idempotency_key_fields=["project_id"],
            emits_events=["ProjectTrashed"],
            description="Soft-delete the active Project after confirmation.",
        ),
        ToolSpec(
            name="project.copy",
            namespace="project",
            version="1",
            input_model=ProjectCopyInput,
            result_model=None,
            handler_ref="tools.project.copy",
            allowed_scopes=["case_agent_console", "project_page"],
            requires_artifacts=[],
            requires_active_project=True,
            requires_active_case=False,
            side_effects=["project"],
            idempotency_key_fields=["source_project_id", "project_id"],
            emits_events=["ProjectCopied"],
            description="Copy the active Project and its asset links without copying Cases.",
        ),
        ToolSpec(
            name="project.create_case",
            namespace="project",
            version="1",
            input_model=ProjectCreateCaseInput,
            result_model=None,
            handler_ref="tools.project.create_case",
            allowed_scopes=["case_agent_console", "project_page"],
            requires_artifacts=[],
            requires_active_project=True,
            requires_active_case=False,
            side_effects=["project", "case"],
            idempotency_key_fields=["project_id", "case_id"],
            emits_events=["CaseCreated"],
            description="Create a Case in the active Project.",
        ),
        ToolSpec(
            name="project.move_case",
            namespace="project",
            version="1",
            input_model=ProjectMoveCaseInput,
            result_model=None,
            handler_ref="tools.project.move_case",
            allowed_scopes=["case_agent_console", "project_page"],
            requires_artifacts=[],
            requires_active_project=True,
            requires_active_case=True,
            requires_confirmation=True,
            confirmation_decision_type="destructive_project_action",
            side_effects=["project", "case", "asset"],
            idempotency_key_fields=["case_id", "target_project_id"],
            emits_events=["CaseMoved", "AssetLinked"],
            description="Move the active Case to another Project after confirmation.",
        ),
        ToolSpec(
            name="project.close_case",
            namespace="project",
            version="1",
            input_model=ProjectCloseCaseInput,
            result_model=None,
            handler_ref="tools.project.close_case",
            allowed_scopes=["case_agent_console", "project_page"],
            requires_artifacts=[],
            requires_active_project=True,
            requires_active_case=True,
            side_effects=["case"],
            idempotency_key_fields=["case_id"],
            emits_events=["CaseClosed"],
            description="Close the active Case without deleting it.",
        ),
        ToolSpec(
            name="project.list_tree",
            namespace="project",
            version="1",
            input_model=ProjectListTreeInput,
            result_model=None,
            handler_ref="tools.project.list_tree",
            allowed_scopes=["case_agent_console", "project_page"],
            requires_artifacts=[],
            requires_active_project=False,
            requires_active_case=False,
            side_effects=[],
            emits_events=[],
            description="Return the Project/Case two-level tree.",
        ),
    )


def patch_op_registry() -> PatchOpRegistry:
    return PatchOpRegistry(
        (
            PatchOpSpec(
                kind="delete_range",
                params_model=DeleteRangeOp,
                ripple_semantics="ripple",
                description="Delete a time range from one or more tracks.",
            ),
            PatchOpSpec(
                kind="replace_clip",
                params_model=ReplaceClipOp,
                ripple_semantics="in_place",
                description="Replace an existing timeline clip with a candidate.",
            ),
            PatchOpSpec(
                kind="reorder_blocks",
                params_model=ReorderBlocksOp,
                ripple_semantics="ripple",
                description="Reorder semantic timeline blocks.",
            ),
            PatchOpSpec(
                kind="trim_clip",
                params_model=TrimClipOp,
                ripple_semantics="ripple",
                description="Trim a clip head or tail by user-referenced seconds.",
            ),
            PatchOpSpec(
                kind="insert_candidate",
                params_model=InsertCandidateOp,
                ripple_semantics="ripple",
                description="Insert a candidate at a user-referenced position.",
            ),
            PatchOpSpec(
                kind="generate_subtitles",
                params_model=GenerateSubtitlesOp,
                requires_confirmation=True,
                confirmation_decision_type="subtitle",
                requires_artifacts=["rough_cut_approved"],
                ripple_semantics="in_place",
                description="Generate subtitle clips from speech timestamps.",
            ),
            PatchOpSpec(
                kind="set_subtitle_style",
                params_model=SetSubtitleStyleOp,
                ripple_semantics="in_place",
                description="Change subtitle styling.",
            ),
            PatchOpSpec(
                kind="edit_subtitle_text",
                params_model=EditSubtitleTextOp,
                ripple_semantics="in_place",
                description="Edit an existing subtitle clip's text.",
            ),
            PatchOpSpec(
                kind="remove_track_clips",
                params_model=RemoveTrackClipsOp,
                ripple_semantics="ripple",
                description="Remove clips from a track.",
            ),
            PatchOpSpec(
                kind="add_bgm",
                params_model=AddBgmOp,
                requires_confirmation=True,
                confirmation_decision_type="bgm",
                requires_artifacts=["rough_cut_approved"],
                ripple_semantics="in_place",
                description="Add a BGM track after BGM confirmation.",
            ),
            PatchOpSpec(
                kind="adjust_gain",
                params_model=AdjustGainOp,
                ripple_semantics="in_place",
                description="Adjust audio track gain.",
            ),
            PatchOpSpec(
                kind="set_playback_rate",
                params_model=SetPlaybackRateOp,
                ripple_semantics="ripple",
                description="Set clip playback rate.",
            ),
        )
    )


PATCH_OP_REGISTRY = patch_op_registry()


def build_default_tool_registry() -> ToolRegistry:
    from .builtin import decision_answer, finish_turn, refuse, respond
    from .interaction import (
        ask_user,
        confirm_action,
        show_error,
        show_preview,
        show_progress,
        show_timeline,
    )
    from .project import (
        close_case,
        copy,
        create,
        create_case,
        delete,
        list_tree,
        move_case,
        rename,
    )

    handlers: dict[str, ToolHandler] = {
        "respond": respond,
        "refuse": refuse,
        "finish_turn": finish_turn,
        "decision.answer": decision_answer,
        "interaction.ask_user": ask_user,
        "interaction.confirm_action": confirm_action,
        "interaction.show_progress": show_progress,
        "interaction.show_preview": show_preview,
        "interaction.show_timeline": show_timeline,
        "interaction.show_error": show_error,
        "project.create": create,
        "project.rename": rename,
        "project.delete": delete,
        "project.copy": copy,
        "project.create_case": create_case,
        "project.move_case": move_case,
        "project.close_case": close_case,
        "project.list_tree": list_tree,
    }
    registry = ToolRegistry()
    for spec in tool_specs():
        registry.register(spec, handlers[spec.name])
    return registry
