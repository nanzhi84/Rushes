from typing import Literal

import pytest
from pydantic import BaseModel, ConfigDict

from contracts.decision import Decision, DecisionAnswer
from contracts.tool import ToolSpec
from contracts.tool_result import ToolResult
from tools import PATCH_OP_REGISTRY, ToolExecutionContext, build_default_tool_registry, tool_specs
from tools.builtin import decision_answer
from tools.registry import ToolRegistry
from tools.specs import DecisionAnswerInput


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
        allowed_scopes=["draft_editor"],
        requires_artifacts=requires_artifacts or [],
        requires_active_draft=False,
        requires_confirmation=requires_confirmation,
        confirmation_decision_type=confirmation_decision_type,
        side_effects=[],
        emits_events=emits_events or [],
        description=name,
    )


def _handler(input_model: EmptyInput, context: ToolExecutionContext) -> ToolResult:
    del input_model
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name="x.echo",
        status="succeeded",
        observation="ok",
    )


def test_default_tool_and_patch_registries_match_surface() -> None:
    registry = build_default_tool_registry()

    expected_tools = {
        "asset.import_local_file",
        "asset.import_url",
        "asset.list_assets",
        "audio.inspect_sources",
        "audio.asr_original",
        "audio.rough_cut_speech",
        "audio.generate_tts",
        "audio.align_uploaded_voiceover",
        "content.create_plan",
        "content.revise_plan",
        "decision.answer",
        "interaction.ask_user",
        "interaction.confirm_action",
        "interaction.show_progress",
        "interaction.show_preview",
        "interaction.show_timeline",
        "interaction.show_error",
        "media.view_frames",
        "memory.extract_from_draft",
        "memory.ask_scope",
        "memory.save",
        "memory.search_relevant",
        "render.preview",
        "render.final_mp4",
        "render.status",
        "timeline.compose_initial",
        "timeline.apply_patch",
        "timeline.validate",
        "timeline.inspect",
        "timeline.restore_version",
        "understand.materials",
    }
    assert {spec.name for spec in tool_specs()} == expected_tools
    assert len(expected_tools) == 31
    assert {spec.name for spec in registry.list_stable()} == {spec.name for spec in tool_specs()}

    asset_import_url = registry.require("asset.import_url").spec
    assert asset_import_url.requires_confirmation is True
    assert asset_import_url.confirmation_decision_type == "url_import"
    assert asset_import_url.is_long_running is True
    assert registry.require("asset.list_assets").spec.side_effects == []
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
    content_create = registry.require("content.create_plan").spec
    assert content_create.exposure == "llm"
    assert content_create.requires_active_draft is True
    assert content_create.requires_confirmation is False
    assert content_create.emits_events == ["ContentPlanUpdated", "CutPlanUpdated"]
    content_revise = registry.require("content.revise_plan").spec
    assert content_revise.exposure == "llm"
    assert content_revise.requires_active_draft is True
    assert content_revise.emits_events == ["ContentPlanUpdated", "CutPlanUpdated"]
    media_view_frames = registry.require("media.view_frames").spec
    assert media_view_frames.side_effects == []
    assert media_view_frames.requires_confirmation is False
    assert media_view_frames.emits_events == []
    apply_patch = registry.require("timeline.apply_patch").spec
    assert apply_patch.requires_artifacts == ["timeline_exists"]
    assert apply_patch.emits_events == [
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
    memory_extract = registry.require("memory.extract_from_draft").spec
    assert memory_extract.emits_events == [
        "MemoryCandidateExtracted",
        "CapabilityDegraded",
        "ProviderCallRecorded",
    ]
    assert registry.require("memory.ask_scope").spec.emits_events == ["DecisionCreated"]
    memory_save = registry.require("memory.save").spec
    assert memory_save.exposure == "harness_only"
    assert memory_save.requires_confirmation is False
    assert memory_save.emits_events == ["MemorySaved"]
    llm_tools = {spec.name for spec in registry.list_stable(exposure="llm")}
    assert "memory.save" not in llm_tools
    assert "memory.search_relevant" in llm_tools
    assert {spec.kind for spec in PATCH_OP_REGISTRY.list()} == {
        "delete_range",
        "replace_clip",
        "reorder_blocks",
        "trim_clip",
        "insert_clip",
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


def test_every_tool_spec_declares_cost_tier() -> None:
    specs = {spec.name: spec for spec in tool_specs()}
    assert len(specs) == 31
    # 31 个 spec 全部显式落在三档之内，无遗漏
    assert all(spec.cost_tier in {"free", "cheap", "expensive"} for spec in specs.values())

    # free 组只含只读本地状态工具（渐进披露里模型可随手取证）
    free_tools = {name for name, spec in specs.items() if spec.cost_tier == "free"}
    assert free_tools == {
        "asset.list_assets",
        "timeline.inspect",
        "render.status",
        "memory.search_relevant",
    }

    # 碰云端模型 / 长任务的工具必须是 expensive
    for name in (
        "understand.materials",
        "media.view_frames",
        "audio.asr_original",
        "audio.rough_cut_speech",
        "audio.generate_tts",
        "render.preview",
        "render.final_mp4",
        "memory.extract_from_draft",
    ):
        assert specs[name].cost_tier == "expensive", name

    # 本地写状态类是 cheap
    for name in ("interaction.ask_user", "content.create_plan", "timeline.apply_patch"):
        assert specs[name].cost_tier == "cheap", name


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


def test_decision_answer_builds_decision_answered_event() -> None:
    decision = Decision.model_validate(
        {
            "decision_id": "decision_1",
            "scope_type": "draft",
            "draft_id": "draft_1",
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
