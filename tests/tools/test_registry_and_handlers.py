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

    expected_tools = {
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
        "project.create",
        "project.rename",
        "project.delete",
        "project.copy",
        "project.create_case",
        "project.move_case",
        "project.close_case",
        "project.list_tree",
        "asset.upload_complete",
        "asset.import_local_file",
        "asset.import_url",
        "asset.link_to_project",
        "asset.unlink_from_project",
        "asset.select_for_case",
        "asset.disable_for_case",
        "asset.list_project_assets",
        "asset.list_case_scope",
        "audio.inspect_sources",
        "audio.asr_original",
        "audio.rough_cut_speech",
        "audio.generate_tts",
        "audio.align_uploaded_voiceover",
        "annotation.enqueue",
        "annotation.status",
        "annotation.retry",
        "annotation.inspect",
        "retrieval.search_candidates",
        "timeline.plan_from_candidates",
        "timeline.validate",
        "timeline.inspect",
        "timeline.restore_version",
        "render.preview",
        "render.final_mp4",
        "render.status",
    }
    assert {spec.name for spec in tool_specs()} == expected_tools
    assert {spec.name for spec in registry.list_stable()} == {spec.name for spec in tool_specs()}
    assert {"case.rename", "case.delete", "case.copy"}.isdisjoint(
        {spec.name for spec in registry.list_stable()}
    )
    project_delete = registry.require("project.delete").spec
    assert project_delete.requires_confirmation is True
    assert project_delete.confirmation_decision_type == "destructive_project_action"
    assert project_delete.emits_events == ["ProjectTrashed"]
    project_move_case = registry.require("project.move_case").spec
    assert project_move_case.requires_confirmation is True
    assert project_move_case.confirmation_decision_type == "destructive_project_action"
    assert project_move_case.emits_events == ["CaseMoved", "AssetLinked"]
    assert registry.require("project.list_tree").spec.side_effects == []
    asset_import_url = registry.require("asset.import_url").spec
    assert asset_import_url.requires_confirmation is True
    assert asset_import_url.confirmation_decision_type == "url_import"
    assert asset_import_url.is_long_running is True
    assert registry.require("asset.list_project_assets").spec.side_effects == []
    assert registry.require("audio.inspect_sources").spec.emits_events == [
        "AssetProbed",
        "CapabilityDegraded",
    ]
    audio_asr = registry.require("audio.asr_original").spec
    assert audio_asr.is_long_running is True
    assert audio_asr.requires_artifacts == [
        "audio_mode_in(keep_original,rough_cut)",
        "audio_source_has_audio",
    ]
    audio_rough_cut = registry.require("audio.rough_cut_speech").spec
    assert audio_rough_cut.requires_artifacts == [
        "audio_mode_in(rough_cut)",
        "transcript_with_vad_exists",
    ]
    assert audio_rough_cut.emits_events == [
        "DecisionCreated",
        "CapabilityDegraded",
        "ProviderCallRecorded",
    ]
    audio_tts = registry.require("audio.generate_tts").spec
    assert audio_tts.is_long_running is True
    assert audio_tts.requires_artifacts == ["audio_mode_in(tts)", "content_plan_exists"]
    audio_align = registry.require("audio.align_uploaded_voiceover").spec
    assert audio_align.is_long_running is True
    assert audio_align.requires_artifacts == [
        "audio_mode_in(uploaded_voiceover)",
        "voiceover_asset_exists",
    ]
    assert registry.require("annotation.enqueue").spec.emits_events == ["JobEnqueued"]
    assert registry.require("annotation.status").spec.side_effects == []
    retrieval = registry.require("retrieval.search_candidates").spec
    assert retrieval.requires_artifacts == [
        "audio_plan_confirmed",
        "cut_plan_exists",
        "usable_asset_exists",
    ]
    assert retrieval.emits_events == [
        "CandidatePackCreated",
        "CapabilityDegraded",
        "ProviderCallRecorded",
    ]
    plan_from_candidates = registry.require("timeline.plan_from_candidates").spec
    assert plan_from_candidates.requires_artifacts == ["candidate_pack_exists"]
    assert plan_from_candidates.emits_events == [
        "TimelineVersionCreated",
        "TimelineValidated",
        "TimelineValidationFailed",
        "DecisionCreated",
    ]
    assert registry.require("timeline.validate").spec.requires_artifacts == ["timeline_exists"]
    assert registry.require("timeline.inspect").spec.side_effects == []
    assert registry.require("timeline.restore_version").spec.emits_events == [
        "TimelineVersionRestored"
    ]
    render_preview = registry.require("render.preview").spec
    assert render_preview.requires_artifacts == ["timeline_validated"]
    assert render_preview.is_long_running is True
    render_final = registry.require("render.final_mp4").spec
    assert render_final.requires_confirmation is True
    assert render_final.confirmation_decision_type == "export"
    assert render_final.requires_artifacts == [
        "timeline_validated",
        "preview_for_current_version_exists",
    ]
    assert registry.require("render.status").spec.side_effects == []
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
