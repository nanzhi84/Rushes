from typing import Any, Literal

from pydantic import BaseModel, ConfigDict

from agent_harness.policy_gate import (
    PolicyContext,
    PolicyGate,
    fingerprint,
    mark_replayed,
    next_replay,
)
from contracts.case import CaseState
from contracts.decision import Decision
from contracts.patch import GenerateSubtitlesOp, SetSubtitleStyleOp, TimelinePatchRequest
from contracts.project import ProjectState
from contracts.tool import PatchOpSpec, ToolSpec
from domain.preconditions import PreconditionContext, ProjectArtifactStats


class EmptyInput(BaseModel):
    model_config = ConfigDict(extra="forbid")


class ProjectDeleteInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    project_id: str


class FinalMp4Input(BaseModel):
    model_config = ConfigDict(extra="forbid")

    case_id: str


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
    specs = {
        "decision.answer": _tool(
            "decision.answer",
            namespace="decision",
            side_effects=["case"],
        ),
        "timeline.inspect": _tool("timeline.inspect", namespace="timeline", side_effects=[]),
        "interaction.cancel": _tool(
            "interaction.cancel",
            namespace="interaction",
            side_effects=["case"],
        ),
        "respond": _tool("respond", namespace="interaction", side_effects=[]),
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
        "project.delete": _tool(
            "project.delete",
            namespace="project",
            input_model=ProjectDeleteInput,
            requires_confirmation=True,
            confirmation_decision_type="destructive_project_action",
            side_effects=["project"],
        ),
        "timeline.apply_patch": _tool(
            "timeline.apply_patch",
            namespace="timeline",
            input_model=TimelinePatchRequest,
            requires_artifacts=["timeline_exists"],
            requires_active_case=True,
            side_effects=["timeline"],
        ),
    }
    return specs


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
        "set_subtitle_style": PatchOpSpec(
            kind="set_subtitle_style",
            params_model=SetSubtitleStyleOp,
            requires_confirmation=False,
            requires_artifacts=[],
            description="set subtitle style",
        ),
    }


def _case_state(**overrides: object) -> CaseState:
    data = {
        "case_id": "case_1",
        "project_id": "project_1",
        "name": "Case",
        "brief": {"goal": "make a video", "confirmed_facts": []},
        "timeline_current_version": 3,
        "timeline_validated": True,
        "preview_current_id": "preview_3",
        "rough_cut_approved": True,
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
            "created_at": "2026-07-04T00:00:00Z",
            "updated_at": "2026-07-04T00:00:00Z",
        }
    )


def _context(
    case_state: CaseState | None = None,
    *,
    decisions: tuple[Decision, ...] = (),
    allowed_tool_names: frozenset[str] | None = None,
) -> PolicyContext:
    return PolicyContext(
        preconditions=PreconditionContext(
            case_state=case_state or _case_state(),
            project_state=_project_state(),
            project_artifacts=ProjectArtifactStats(usable_asset_count=1),
        ),
        decisions=decisions,
        allowed_tool_names=allowed_tool_names,
    )


def _gate() -> PolicyGate:
    return PolicyGate(tool_specs=_tool_specs(), patch_op_specs=_patch_op_specs())


def _pending_decision() -> Decision:
    return Decision.model_validate(
        {
            "decision_id": "decision_pending",
            "scope_type": "case",
            "project_id": "project_1",
            "case_id": "case_1",
            "type": "generic",
            "question": "Confirm?",
            "status": "pending",
            "blocking": True,
        }
    )


def test_pending_decision_narrows_allowed_tools() -> None:
    decision = _pending_decision()
    case_state = _case_state(pending_decision_id=decision.decision_id)
    allowed = _gate().compute_allowed_tools(_context(case_state, decisions=(decision,)))

    names = {spec.name for spec in allowed}
    assert {"decision.answer", "timeline.inspect", "interaction.cancel", "respond"} <= names
    assert "render.final_mp4" not in names


def test_unregistered_tool_denies_with_policy_refusal() -> None:
    verdict = _gate().adjudicate(
        {"tool_name": "shell.exec", "arguments": {}},
        _context(allowed_tool_names=frozenset({"shell.exec"})),
    )

    assert verdict.status == "deny"
    assert verdict.events[0].event == "PolicyRefusal"


def test_confirmation_ask_stores_pending_call_and_fingerprint_mismatch_reasks() -> None:
    gate = _gate()
    context = _context(allowed_tool_names=frozenset({"project.delete"}))
    first = gate.adjudicate(
        {"tool_name": "project.delete", "arguments": {"project_id": "project_1"}},
        context,
    )

    assert first.status == "ask"
    assert first.pending_tool_call is not None
    assert first.pending_tool_call.argument_fingerprint == fingerprint({"project_id": "project_1"})

    answered = first.decision.model_copy(
        update={
            "status": "answered",
            "answer": {"option_id": "approve", "answered_via": "button"},
            "pending_tool_call_status": "approved",
        }
    )
    same = gate.adjudicate(
        {"tool_name": "project.delete", "arguments": {"project_id": "project_1"}},
        _context(decisions=(answered,), allowed_tool_names=frozenset({"project.delete"})),
    )
    changed = gate.adjudicate(
        {"tool_name": "project.delete", "arguments": {"project_id": "project_2"}},
        _context(decisions=(answered,), allowed_tool_names=frozenset({"project.delete"})),
    )

    assert same.status == "allow"
    assert changed.status == "ask"
    assert changed.pending_tool_call is not None
    assert (
        changed.pending_tool_call.argument_fingerprint
        != first.pending_tool_call.argument_fingerprint
    )


def test_next_replay_and_mark_replayed_consume_approved_decision() -> None:
    decision = Decision.model_validate(
        {
            "decision_id": "decision_1",
            "scope_type": "project",
            "project_id": "project_1",
            "type": "destructive_project_action",
            "question": "Confirm?",
            "status": "answered",
            "answer": {"option_id": "approve", "answered_via": "button"},
            "pending_tool_call": {
                "tool_name": "project.delete",
                "arguments": {"project_id": "project_1"},
                "idempotency_key": "idem",
                "argument_fingerprint": "fp",
            },
            "pending_tool_call_status": "approved",
        }
    )

    class Repo:
        called = 0

        def mark_pending_tool_call_replayed(
            self,
            decision_id: str,
            *,
            consumed_at: str,
            replayed_tool_call_id: str,
        ) -> bool:
            assert decision_id == "decision_1"
            assert consumed_at
            assert replayed_tool_call_id == "tc_replay"
            self.called += 1
            return self.called == 1

    repo = Repo()
    assert next_replay(decision) == decision.pending_tool_call
    assert mark_replayed(repo, "decision_1", replayed_tool_call_id="tc_replay")
    assert not mark_replayed(repo, "decision_1", replayed_tool_call_id="tc_replay")


def test_patch_op_gate_asks_allows_and_exempts_by_registry() -> None:
    gate = _gate()
    allowed = frozenset({"timeline.apply_patch"})
    base_args = {
        "case_id": "case_1",
        "reference": {"timeline_version": 3, "preview_id": "preview_3"},
        "reason": "add subtitles",
    }
    ask = gate.adjudicate(
        {
            "tool_name": "timeline.apply_patch",
            "arguments": {
                **base_args,
                "op": {
                    "kind": "generate_subtitles",
                    "source": "voiceover",
                    "style_template_id": "subtitle_default",
                },
            },
        },
        _context(allowed_tool_names=allowed),
    )
    allow = gate.adjudicate(
        {
            "tool_name": "timeline.apply_patch",
            "arguments": {
                **base_args,
                "op": {
                    "kind": "generate_subtitles",
                    "source": "voiceover",
                    "style_template_id": "subtitle_default",
                },
            },
        },
        _context(
            _case_state(postprocess_plan={"subtitle": {"enabled": True, "style_template_id": "s"}}),
            allowed_tool_names=allowed,
        ),
    )
    exempt = gate.adjudicate(
        {
            "tool_name": "timeline.apply_patch",
            "arguments": {
                **base_args,
                "op": {
                    "kind": "set_subtitle_style",
                    "style_template_id": "s2",
                    "range": {"kind": "all"},
                },
            },
        },
        _context(_case_state(rough_cut_approved=False), allowed_tool_names=allowed),
    )

    assert ask.status == "ask"
    assert ask.decision is not None
    assert ask.decision.type == "subtitle"
    assert allow.status == "allow"
    assert exempt.status == "allow"


def test_blacklisted_llm_argument_key_denies() -> None:
    verdict = _gate().adjudicate(
        {
            "tool_name": "timeline.apply_patch",
            "arguments": {
                "case_id": "case_1",
                "reference": {"timeline_version": 3, "preview_id": "preview_3"},
                "op": {
                    "kind": "set_subtitle_style",
                    "style_template_id": "s",
                    "range": {"kind": "all"},
                    "timeline_start_frame": 30,
                },
                "reason": "bad frame field",
            },
        },
        _context(allowed_tool_names=frozenset({"timeline.apply_patch"})),
    )

    assert verdict.status == "deny"
    assert "prohibited" in verdict.reason
