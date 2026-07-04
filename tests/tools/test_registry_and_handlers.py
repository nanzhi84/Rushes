from typing import Literal

import pytest
from pydantic import BaseModel, ConfigDict

from contracts.case import CaseState
from contracts.decision import Decision, DecisionAnswer
from contracts.tool import ToolSpec
from tools import PATCH_OP_REGISTRY, ToolExecutionContext, build_default_tool_registry, tool_specs
from tools.builtin import decision_answer, respond
from tools.registry import ToolRegistry
from tools.specs import DecisionAnswerInput, RespondInput


class EmptyInput(BaseModel):
    model_config = ConfigDict(extra="forbid")


def _spec(
    name: str,
    *,
    version: str,
    status: Literal["stable", "experimental", "deprecated"] = "stable",
    emits_events: list[str] | None = None,
    requires_artifacts: list[str] | None = None,
    requires_confirmation: bool = False,
    confirmation_decision_type: str | None = None,
) -> ToolSpec:
    return ToolSpec(
        name=name,
        namespace=name.split(".")[0],
        version=version,
        status=status,
        input_model=EmptyInput,
        result_model=None,
        handler_ref=f"handlers.{name}",
        allowed_scopes=["case_agent_console"],
        requires_artifacts=requires_artifacts or [],
        requires_active_project=False,
        requires_active_case=False,
        requires_confirmation=requires_confirmation,
        confirmation_decision_type=confirmation_decision_type,
        side_effects=[],
        emits_events=emits_events or [],
        description=name,
    )


def _handler(input_model: EmptyInput, context: ToolExecutionContext):
    del input_model
    return respond(RespondInput(message="ok"), context)


def test_default_tool_and_patch_registries_match_m0_surface() -> None:
    registry = build_default_tool_registry()

    assert {spec.name for spec in tool_specs()} == {
        "respond",
        "refuse",
        "finish_turn",
        "decision.answer",
        "interaction.ask_user",
        "interaction.confirm_action",
        "interaction.show_progress",
        "interaction.show_preview",
        "interaction.show_timeline",
        "interaction.show_error",
    }
    assert {spec.name for spec in registry.list_stable()} == {spec.name for spec in tool_specs()}
    assert {spec.kind for spec in PATCH_OP_REGISTRY.list()} == {
        "delete_range",
        "replace_clip",
        "reorder_blocks",
        "trim_clip",
        "insert_candidate",
        "generate_subtitles",
        "set_subtitle_style",
        "edit_subtitle_text",
        "remove_track_clips",
        "add_bgm",
        "adjust_gain",
        "set_playback_rate",
    }
    assert PATCH_OP_REGISTRY.require("generate_subtitles").confirmation_decision_type == "subtitle"
    assert PATCH_OP_REGISTRY.require("add_bgm").requires_artifacts == ["rough_cut_approved"]


def test_registry_exposes_latest_stable_unless_experimental_enabled() -> None:
    registry = ToolRegistry()
    registry.register(_spec("x.echo", version="1"), _handler)
    registry.register(_spec("x.echo", version="2", status="experimental"), _handler)

    assert registry.require("x.echo").spec.version == "1"
    assert registry.require("x.echo", include_experimental=True).spec.version == "2"


def test_registry_validates_events_preconditions_and_confirmation_mapping() -> None:
    registry = ToolRegistry()

    with pytest.raises(ValueError, match="unknown events"):
        registry.register(_spec("x.bad_event", version="1", emits_events=["Nope"]), _handler)
    with pytest.raises(ValueError, match="unknown precondition"):
        registry.register(
            _spec("x.bad_precondition", version="1", requires_artifacts=["missing_predicate"]),
            _handler,
        )
    with pytest.raises(ValueError, match="requires confirmation_decision_type"):
        registry.register(
            _spec("x.bad_confirmation", version="1", requires_confirmation=True),
            _handler,
        )
    with pytest.raises(ValueError, match="Decision type has no effect mapping"):
        registry.register(
            _spec(
                "x.bad_decision",
                version="1",
                requires_confirmation=True,
                confirmation_decision_type="not_registered",
            ),
            _handler,
        )


def test_builtin_handlers_return_data_and_events_without_writing_state() -> None:
    context = ToolExecutionContext(
        tool_call_id="tc_1",
        turn_id="turn_1",
        case_state=CaseState.model_validate(
            {
                "case_id": "case_1",
                "project_id": "project_1",
                "name": "Case",
                "brief": {"goal": "test"},
            }
        ),
    )

    result = respond(RespondInput(message="hello"), context)

    assert result.status == "succeeded"
    assert result.data["message_row"]["role"] == "assistant"
    assert result.events[0]["event"] == "TurnEnded"


def test_decision_answer_builds_decision_answered_event() -> None:
    decision = Decision.model_validate(
        {
            "decision_id": "decision_1",
            "scope_type": "case",
            "project_id": "project_1",
            "case_id": "case_1",
            "type": "generic",
            "question": "OK?",
            "status": "pending",
            "blocking": True,
        }
    )
    context = ToolExecutionContext(
        tool_call_id="tc_1",
        turn_id="turn_1",
        decisions=(decision,),
    )

    result = decision_answer(
        DecisionAnswerInput(
            decision_id="decision_1",
            answer=DecisionAnswer(
                option_id="yes",
                answered_via="button",
                payload={"reduce_target": "scratch_memory", "value": "yes"},
            ),
        ),
        context,
    )

    assert result.status == "succeeded"
    assert result.events[0]["event"] == "DecisionAnswered"
    assert result.events[0]["decision_id"] == "decision_1"
