from __future__ import annotations

from pathlib import Path
from typing import Any, Literal

from pydantic import BaseModel, ConfigDict
from sqlalchemy import func, select
from sqlalchemy.engine import Connection

from agent_harness.policy_gate import PolicyContext, PolicyGate, mark_replayed, next_replay
from contracts.case import CaseState
from contracts.decision import DecisionAnswer
from contracts.patch import AddBgmOp, GenerateSubtitlesOp, TimelinePatchRequest
from contracts.project import ProjectState
from contracts.tool import PatchOpSpec, ToolSpec
from domain.decision_effects import pending_tool_call_status_after_answer, reduce_decision_answer
from domain.preconditions import PreconditionContext, ProjectArtifactStats, ProjectBgmAsset
from domain.subtitle_templates import list_subtitle_templates
from storage import schema
from storage.db import create_workspace_engine
from storage.repositories._json import dump_json, load_json

NOW = "2026-07-05T00:00:00+00:00"


class EmptyInput(BaseModel):
    model_config = ConfigDict(extra="forbid")


class FinalMp4Input(BaseModel):
    model_config = ConfigDict(extra="forbid")

    case_id: str


def test_subtitle_gate_options_reduce_and_replay_or_discard() -> None:
    gate = _gate()
    call = _timeline_patch_call(
        {
            "kind": "generate_subtitles",
            "source": "voiceover",
            "style_template_id": "clean_bottom",
        }
    )

    ask = gate.adjudicate(call, _context(allowed_tool_names=frozenset({"timeline.apply_patch"})))

    assert ask.status == "ask"
    assert ask.decision is not None
    assert ask.pending_tool_call is not None
    # outbox 存的是校验后的规范化参数（补默认字段），与原始输入比语义等价
    assert ask.pending_tool_call.arguments == TimelinePatchRequest.model_validate(
        call["arguments"]
    ).model_dump(mode="json", by_alias=True)
    assert ask.decision.type == "subtitle"
    option_ids = {option.option_id for option in ask.decision.options}
    assert len(option_ids & {template.template_id for template in list_subtitle_templates()}) >= 6
    assert "skip" in option_ids

    template_answer = DecisionAnswer.model_validate(
        {"option_id": "clean_bottom", "answered_via": "button", "payload": {}}
    )
    template_result = reduce_decision_answer(_case_state(), ask.decision, template_answer)

    assert template_result.state_patch["postprocess_plan"]["subtitle"] == {
        "enabled": True,
        "style_template_id": "clean_bottom",
    }
    assert template_result.followup_events[0].event == "PostprocessPlanUpdated"
    assert template_result.followups[0].payload["pending_tool_call"] == (
        ask.pending_tool_call.model_dump(mode="json")
    )

    skip_answer = DecisionAnswer.model_validate(
        {"option_id": "skip", "answered_via": "button", "payload": {}}
    )
    skip_result = reduce_decision_answer(_case_state(), ask.decision, skip_answer)

    assert skip_result.state_patch["postprocess_plan"]["subtitle"] == {
        "enabled": False,
        "style_template_id": None,
    }
    assert skip_result.followups == ()
    assert (
        pending_tool_call_status_after_answer(
            ask.decision.pending_tool_call,
            skip_answer,
            decision=ask.decision,
        )
        == "discarded"
    )


def test_bgm_gate_uses_upload_default_skip_or_project_assets() -> None:
    gate = _gate()
    call = _timeline_patch_call(
        {"kind": "add_bgm", "asset_id": "default_bgm_calm", "gain_db": -12.0, "duck": True}
    )

    no_asset = gate.adjudicate(
        call,
        _context(allowed_tool_names=frozenset({"timeline.apply_patch"})),
    )
    with_asset = gate.adjudicate(
        call,
        _context(
            allowed_tool_names=frozenset({"timeline.apply_patch"}),
            project_bgm_assets=(ProjectBgmAsset(asset_id="asset_bgm_1", filename="配乐.m4a"),),
        ),
    )

    assert no_asset.decision is not None
    assert [option.option_id for option in no_asset.decision.options] == [
        "upload_bgm",
        "default_bgm",
        "skip",
    ]
    assert no_asset.decision.options[1].payload["asset_id"] == "default_bgm_calm"
    assert "默认无版权 BGM" in no_asset.decision.options[1].label
    upload_answer = DecisionAnswer.model_validate(
        {"option_id": "upload_bgm", "answered_via": "button", "payload": {}}
    )
    upload_result = reduce_decision_answer(_case_state(), no_asset.decision, upload_answer)
    assert "postprocess_plan" not in upload_result.state_patch
    assert upload_result.state_patch["scratch_memory"]["pending_intents"] == [
        "请上传 BGM 素材，上传完成后重新发起添加 BGM。"
    ]
    assert (
        pending_tool_call_status_after_answer(
            no_asset.decision.pending_tool_call,
            upload_answer,
            decision=no_asset.decision,
        )
        == "discarded"
    )
    assert with_asset.decision is not None
    option_by_id = {option.option_id: option for option in with_asset.decision.options}
    assert option_by_id["asset_bgm_1"].payload == {
        "enabled": True,
        "asset_id": "asset_bgm_1",
        "gain_db": -12.0,
        "duck": True,
    }
    assert {"default_bgm", "skip"} <= set(option_by_id)


def test_export_gate_persists_pending_replay_and_mentions_unviewed_preview() -> None:
    verdict = _gate().adjudicate(
        {"tool_name": "render.final_mp4", "arguments": {"case_id": "case_1"}},
        _context(
            _case_state(preview_current_id="preview_new", last_viewed_preview_id="preview_old"),
            allowed_tool_names=frozenset({"render.final_mp4"}),
        ),
    )

    assert verdict.status == "ask"
    assert verdict.decision is not None
    assert verdict.pending_tool_call is not None
    assert verdict.decision.type == "export"
    assert "你还没看最新预览" in verdict.decision.question
    assert verdict.decision.pending_tool_call == verdict.pending_tool_call

    answered = verdict.decision.model_copy(
        update={
            "status": "answered",
            "answer": {"option_id": "approve", "answered_via": "button"},
            "pending_tool_call_status": "approved",
        }
    )

    class Repo:
        calls: int = 0

        def mark_pending_tool_call_replayed(
            self,
            decision_id: str,
            *,
            consumed_at: str,
            replayed_tool_call_id: str,
        ) -> bool:
            assert decision_id == answered.decision_id
            assert consumed_at
            assert replayed_tool_call_id == "tc_replay"
            self.calls += 1
            return self.calls == 1

    repo = Repo()
    assert next_replay(answered) == verdict.pending_tool_call
    assert mark_replayed(repo, answered.decision_id, replayed_tool_call_id="tc_replay")
    assert not mark_replayed(repo, answered.decision_id, replayed_tool_call_id="tc_replay")


def test_subtitle_patch_does_not_create_timeline_version_before_decision(
    tmp_path: Path,
) -> None:
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)
        _seed_project_case_and_timeline(connection)
        call = _timeline_patch_call(
            {
                "kind": "generate_subtitles",
                "source": "voiceover",
                "style_template_id": "clean_bottom",
            }
        )
        verdict = _gate().adjudicate(
            call,
            _context(allowed_tool_names=frozenset({"timeline.apply_patch"})),
        )
        timeline_count = connection.execute(
            select(func.count()).select_from(schema.timeline_versions)
        ).scalar_one()
        timeline_row = connection.execute(select(schema.timeline_versions)).one()

    document = load_json(str(timeline_row._mapping["document_json"]))
    subtitle_track = next(track for track in document["tracks"] if track["track_id"] == "subtitles")
    assert verdict.status == "ask"
    assert timeline_count == 1
    assert subtitle_track["clips"] == []


def _tool(
    name: str,
    *,
    namespace: str | None = None,
    input_model: type[BaseModel] = EmptyInput,
    requires_artifacts: list[str] | None = None,
    requires_active_case: bool = False,
    requires_confirmation: bool = False,
    confirmation_decision_type: str | None = None,
    side_effects: list[Any] | None = None,
    is_long_running: bool = False,
    exposure: Literal["llm", "harness_only"] = "llm",
) -> ToolSpec:
    return ToolSpec(
        name=name,
        namespace=namespace or name.split(".")[0],
        version="1",
        status="stable",
        input_model=input_model,
        result_model=None,
        handler_ref=f"handlers.{name}",
        allowed_scopes=["case_agent_console"],
        requires_artifacts=requires_artifacts or [],
        requires_active_project=True,
        requires_active_case=requires_active_case,
        requires_confirmation=requires_confirmation,
        confirmation_decision_type=confirmation_decision_type,
        side_effects=side_effects or [],
        idempotency_key_fields=[],
        emits_events=[],
        is_long_running=is_long_running,
        exposure=exposure,
        description=name,
    )


def _tool_specs() -> dict[str, ToolSpec]:
    return {
        "timeline.apply_patch": _tool(
            "timeline.apply_patch",
            namespace="timeline",
            input_model=TimelinePatchRequest,
            requires_artifacts=["timeline_exists"],
            requires_active_case=True,
            side_effects=["timeline"],
        ),
        "render.final_mp4": _tool(
            "render.final_mp4",
            namespace="render",
            input_model=FinalMp4Input,
            requires_artifacts=["timeline_validated", "preview_for_current_version_exists"],
            requires_active_case=True,
            requires_confirmation=True,
            confirmation_decision_type="export",
            side_effects=["job"],
            is_long_running=True,
        ),
    }


def _patch_op_specs() -> dict[str, PatchOpSpec]:
    return {
        "generate_subtitles": PatchOpSpec(
            kind="generate_subtitles",
            params_model=GenerateSubtitlesOp,
            requires_confirmation=True,
            confirmation_decision_type="subtitle",
            requires_artifacts=["rough_cut_approved"],
            description="generate subtitles",
        ),
        "add_bgm": PatchOpSpec(
            kind="add_bgm",
            params_model=AddBgmOp,
            requires_confirmation=True,
            confirmation_decision_type="bgm",
            requires_artifacts=["rough_cut_approved"],
            description="add bgm",
        ),
    }


def _gate() -> PolicyGate:
    return PolicyGate(tool_specs=_tool_specs(), patch_op_specs=_patch_op_specs())


def _context(
    case_state: CaseState | None = None,
    *,
    allowed_tool_names: frozenset[str],
    project_bgm_assets: tuple[ProjectBgmAsset, ...] = (),
) -> PolicyContext:
    return PolicyContext(
        preconditions=PreconditionContext(
            case_state=case_state or _case_state(),
            project_state=_project_state(),
            project_artifacts=ProjectArtifactStats(usable_asset_count=1),
            project_bgm_assets=project_bgm_assets,
        ),
        allowed_tool_names=allowed_tool_names,
    )


def _case_state(**overrides: object) -> CaseState:
    data = {
        "case_id": "case_1",
        "project_id": "project_1",
        "name": "Case",
        "brief": {"goal": "make a video", "confirmed_facts": []},
        "timeline_current_version": 1,
        "timeline_validated": True,
        "preview_current_id": "preview_1",
        "last_viewed_preview_id": "preview_1",
        "rough_cut_approved": True,
        "rough_cut_approved_version": 1,
        "selected_asset_ids": [],
        "disabled_asset_ids": [],
        "scratch_memory": {},
    }
    data.update(overrides)
    return CaseState.model_validate(data)


def _project_state() -> ProjectState:
    return ProjectState.model_validate(
        {
            "project_id": "project_1",
            "name": "Project",
            "status": "active",
            "asset_links": [],
            "case_ids": ["case_1"],
            "memory_ids": [],
            "created_at": NOW,
            "updated_at": NOW,
        }
    )


def _timeline_patch_call(op: dict[str, Any]) -> dict[str, Any]:
    return {
        "tool_name": "timeline.apply_patch",
        "arguments": {
            "case_id": "case_1",
            "reference": {"timeline_version": 1, "preview_id": "preview_1"},
            "op": op,
            "reason": "postprocess",
        },
    }


def _seed_project_case_and_timeline(connection: Connection) -> None:
    connection.execute(
        schema.projects.insert().values(
            project_id="project_1",
            name="Project",
            status="active",
            defaults=dump_json({"aspect_ratio": "9:16", "fps": 30}),
            created_at=NOW,
            updated_at=NOW,
        )
    )
    connection.execute(
        schema.cases.insert().values(
            case_id="case_1",
            project_id="project_1",
            name="Case",
            state_version=0,
            status="active",
            timeline_current_version=1,
            timeline_validated=True,
            rough_cut_approved=True,
            rough_cut_approved_version=1,
            running_jobs="[]",
            brief=dump_json({"goal": "make a video", "confirmed_facts": []}),
            selected_asset_ids="[]",
            disabled_asset_ids="[]",
            scratch_memory="{}",
        )
    )
    connection.execute(
        schema.timeline_versions.insert().values(
            timeline_id="case_1:v1",
            case_id="case_1",
            version=1,
            parent_version=None,
            created_by_patch_id=None,
            document_json=dump_json(_timeline_doc()),
            validation_report=None,
            created_at=NOW,
        )
    )


def _timeline_doc() -> dict[str, Any]:
    return {
        "timeline_id": "case_1:v1",
        "case_id": "case_1",
        "version": 1,
        "fps": 30,
        "duration_frames": 90,
        "tracks": [
            {"track_id": "visual_base", "track_type": "primary_visual", "clips": []},
            {"track_id": "visual_overlay", "track_type": "visual_overlay", "clips": []},
            {"track_id": "original_audio", "track_type": "audio", "clips": []},
            {"track_id": "voiceover", "track_type": "audio", "clips": []},
            {"track_id": "bgm", "track_type": "audio", "clips": []},
            {"track_id": "subtitles", "track_type": "text", "clips": []},
        ],
    }
