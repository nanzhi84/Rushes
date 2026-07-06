"""Pure DecisionAnswer effects from PRD §7.6.1."""

from __future__ import annotations

from collections.abc import Callable, Sequence
from dataclasses import dataclass, field
from typing import Any, Literal, get_args

from contracts.case import (
    AudioPlan,
    Brief,
    CaseState,
    CutPlan,
    PostprocessPlan,
    RemovedRange,
)
from contracts.decision import Decision, DecisionAnswer, DecisionType, PendingToolCall
from contracts.events import (
    AudioPlanUpdated,
    BriefUpdated,
    ContentPlanUpdated,
    CutPlanUpdated,
    DomainEventBase,
    PostprocessPlanUpdated,
)

FollowupKind = Literal[
    "replay_pending_tool_call",
    "enqueue_memory_save",
    "discard_memory_candidate",
    "enqueue_delete_range_patches",
]
ReduceTarget = Literal["brief.confirmed_facts", "scratch_memory"]


@dataclass(frozen=True, slots=True)
class HarnessFollowup:
    """A post-commit instruction for the harness; reducers never execute it."""

    kind: FollowupKind
    decision_id: str
    payload: dict[str, Any] = field(default_factory=dict)


@dataclass(frozen=True, slots=True)
class DecisionEffectResult:
    """Pure state delta plus audit events and post-commit followups."""

    state_patch: dict[str, Any] = field(default_factory=dict)
    followup_events: tuple[DomainEventBase, ...] = ()
    followups: tuple[HarnessFollowup, ...] = ()


type EffectFn = Callable[[CaseState, Decision, DecisionAnswer], DecisionEffectResult]


class MissingDecisionEffectError(ValueError):
    """Raised when a Decision.type has no reducer mapping."""


def validate_decision_type_registered(decision_type: str) -> None:
    if decision_type not in decision_effects_registry:
        raise MissingDecisionEffectError(f"Decision type has no effect mapping: {decision_type}")


def validate_decision_registered(decision: Decision) -> None:
    validate_decision_type_registered(decision.type)


def validate_all_decision_types_registered() -> None:
    missing = sorted(set(get_args(DecisionType)) - set(decision_effects_registry))
    if missing:
        raise MissingDecisionEffectError(
            "Decision types missing effect mappings: " + ", ".join(missing)
        )


def reduce_decision_answer(
    case_state: CaseState,
    decision: Decision,
    answer: DecisionAnswer,
) -> DecisionEffectResult:
    validate_decision_registered(decision)
    effect = decision_effects_registry[decision.type](case_state, decision, answer)
    return _with_side_intents(case_state, effect, answer)


def _audio_mode_effect(
    case_state: CaseState,
    decision: Decision,
    answer: DecisionAnswer,
) -> DecisionEffectResult:
    del decision
    mode = str(answer.payload.get("mode") or answer.option_id or "")
    audio_plan = _model_dump(case_state.audio_plan) if case_state.audio_plan is not None else {}
    audio_plan["mode"] = mode
    validated = AudioPlan.model_validate(audio_plan).model_dump(mode="json")
    return DecisionEffectResult(
        state_patch={"audio_plan": validated},
        followup_events=(
            AudioPlanUpdated(case_id=case_state.case_id, payload={"audio_plan": validated}),
        ),
    )


def _approve_content_plan_effect(
    case_state: CaseState,
    decision: Decision,
    answer: DecisionAnswer,
) -> DecisionEffectResult:
    del decision, answer
    content_plan = dict(case_state.content_plan or {})
    content_plan["status"] = "approved"
    return DecisionEffectResult(
        state_patch={"content_plan": content_plan},
        followup_events=(
            ContentPlanUpdated(
                case_id=case_state.case_id,
                payload={"content_plan": content_plan},
            ),
        ),
    )


def _approve_speech_cut_effect(
    case_state: CaseState,
    decision: Decision,
    answer: DecisionAnswer,
) -> DecisionEffectResult:
    payload = _merged_answer_payload(decision, answer)
    ranges_payload = _expect_sequence(payload.get("removed_ranges"), "removed_ranges")
    removed_ranges = [
        RemovedRange.model_validate(item).model_dump(mode="json") for item in ranges_payload
    ]
    cut_plan = _model_dump(case_state.cut_plan) if case_state.cut_plan is not None else {}
    cut_plan.setdefault("schema", "CutPlan.v1")
    cut_plan.setdefault("slots", [])
    cut_plan.setdefault("total_target_duration_sec", payload.get("total_target_duration_sec", 0.0))
    cut_plan["removed_ranges"] = removed_ranges
    validated = CutPlan.model_validate(cut_plan).model_dump(mode="json", by_alias=True)
    followups: list[HarnessFollowup] = []
    if case_state.timeline_current_version is not None:
        followups.append(
            HarnessFollowup(
                kind="enqueue_delete_range_patches",
                decision_id=decision.decision_id,
                payload={
                    "case_id": case_state.case_id,
                    "timeline_version": case_state.timeline_current_version,
                    "removed_ranges": removed_ranges,
                },
            )
        )
    return DecisionEffectResult(
        state_patch={"cut_plan": validated},
        followup_events=(
            CutPlanUpdated(case_id=case_state.case_id, payload={"cut_plan": validated}),
        ),
        followups=tuple(followups),
    )


def _approve_rough_cut_effect(
    case_state: CaseState,
    decision: Decision,
    answer: DecisionAnswer,
) -> DecisionEffectResult:
    payload = _merged_answer_payload(decision, answer)
    timeline_version = payload.get("timeline_version")
    if not isinstance(timeline_version, int):
        raise ValueError("approve_rough_cut requires integer timeline_version")
    return DecisionEffectResult(
        state_patch={
            "rough_cut_approved": True,
            "rough_cut_approved_version": timeline_version,
        }
    )


def _subtitle_effect(
    case_state: CaseState,
    decision: Decision,
    answer: DecisionAnswer,
) -> DecisionEffectResult:
    payload = _merged_answer_payload(decision, answer)
    enabled = _enabled_from_answer(answer, payload)
    subtitle = {
        "enabled": enabled,
        "style_template_id": payload.get("style_template_id") if enabled else None,
    }
    if enabled and subtitle["style_template_id"] is None and answer.option_id is not None:
        subtitle["style_template_id"] = answer.option_id
    postprocess_plan = (
        _model_dump(case_state.postprocess_plan) if case_state.postprocess_plan is not None else {}
    )
    postprocess_plan["subtitle"] = subtitle
    validated = PostprocessPlan.model_validate(postprocess_plan).model_dump(mode="json")
    return DecisionEffectResult(
        state_patch={"postprocess_plan": validated},
        followup_events=(
            PostprocessPlanUpdated(
                case_id=case_state.case_id,
                payload={"postprocess_plan": validated},
            ),
        ),
        followups=_replay_followups(decision, answer),
    )


def _bgm_effect(
    case_state: CaseState,
    decision: Decision,
    answer: DecisionAnswer,
) -> DecisionEffectResult:
    """Reduce BGM decisions.

    The upload option intentionally does not settle postprocess_plan.bgm or replay
    the pending add_bgm patch. The reducer marks that pending call discarded via
    pending_tool_call_status_after_answer and records a short pending intent so
    the agent can ask the user to upload BGM before starting a fresh add_bgm flow.
    """

    payload = _merged_answer_payload(decision, answer)
    if _answer_requests_bgm_upload(answer, payload):
        return DecisionEffectResult(
            state_patch={
                "scratch_memory": _scratch_memory_with_pending_intent(
                    case_state,
                    "请上传 BGM 素材，上传完成后重新发起添加 BGM。",
                )
            }
        )
    enabled = _enabled_from_answer(answer, payload)
    bgm = {
        "enabled": enabled,
        "asset_id": payload.get("asset_id") if enabled else None,
        "gain_db": payload.get("gain_db") if enabled else None,
        "duck": payload.get("duck") if enabled else None,
    }
    if enabled and bgm["asset_id"] is None and answer.option_id is not None:
        bgm["asset_id"] = answer.option_id
    postprocess_plan = (
        _model_dump(case_state.postprocess_plan) if case_state.postprocess_plan is not None else {}
    )
    postprocess_plan["bgm"] = bgm
    validated = PostprocessPlan.model_validate(postprocess_plan).model_dump(mode="json")
    return DecisionEffectResult(
        state_patch={"postprocess_plan": validated},
        followup_events=(
            PostprocessPlanUpdated(
                case_id=case_state.case_id,
                payload={"postprocess_plan": validated},
            ),
        ),
        followups=_replay_followups(decision, answer),
    )


def _export_effect(
    case_state: CaseState,
    decision: Decision,
    answer: DecisionAnswer,
) -> DecisionEffectResult:
    del case_state
    return DecisionEffectResult(followups=_replay_followups(decision, answer))


def _memory_scope_effect(
    case_state: CaseState,
    decision: Decision,
    answer: DecisionAnswer,
) -> DecisionEffectResult:
    payload = _merged_answer_payload(decision, answer)
    candidate_id = payload.get("candidate_id")
    scope = payload.get("scope") or answer.option_id
    if not isinstance(candidate_id, str):
        raise ValueError("memory_scope requires candidate_id")
    if scope not in {"user", "project", "skip"}:
        raise ValueError("memory_scope scope must be user, project, or skip")
    if scope == "skip":
        return DecisionEffectResult(
            followups=(
                HarnessFollowup(
                    kind="discard_memory_candidate",
                    decision_id=decision.decision_id,
                    payload={
                        "case_id": case_state.case_id,
                        "candidate_id": candidate_id,
                    },
                ),
            )
        )
    return DecisionEffectResult(
        followups=(
            HarnessFollowup(
                kind="enqueue_memory_save",
                decision_id=decision.decision_id,
                payload={
                    "case_id": case_state.case_id,
                    "candidate_id": candidate_id,
                    "scope": scope,
                },
            ),
        )
    )


def _destructive_project_action_effect(
    case_state: CaseState,
    decision: Decision,
    answer: DecisionAnswer,
) -> DecisionEffectResult:
    del case_state
    return DecisionEffectResult(followups=_replay_followups(decision, answer))


def _url_import_effect(
    case_state: CaseState,
    decision: Decision,
    answer: DecisionAnswer,
) -> DecisionEffectResult:
    del case_state
    return DecisionEffectResult(followups=_replay_followups(decision, answer))


def _generic_effect(
    case_state: CaseState,
    decision: Decision,
    answer: DecisionAnswer,
) -> DecisionEffectResult:
    payload = _merged_answer_payload(decision, answer)
    reduce_target = payload.get("reduce_target")
    text = payload.get("value") or answer.free_text or answer.option_id
    if not isinstance(text, str) or text == "":
        raise ValueError("generic decisions require a reducible text value")
    if reduce_target == "brief.confirmed_facts":
        brief = _model_dump(case_state.brief)
        facts = list(brief.get("confirmed_facts", []))
        if text not in facts:
            facts.append(text)
        brief["confirmed_facts"] = facts
        validated = Brief.model_validate(brief).model_dump(mode="json")
        return DecisionEffectResult(
            state_patch={"brief": validated},
            followup_events=(
                BriefUpdated(case_id=case_state.case_id, payload={"brief": validated}),
            ),
        )
    if reduce_target == "scratch_memory":
        key = payload.get("key")
        scratch_memory = dict(case_state.scratch_memory)
        scratch_memory[str(key or decision.decision_id)] = text
        return DecisionEffectResult(state_patch={"scratch_memory": scratch_memory})
    # 历史/异常数据缺 reduce_target：按 scratch_memory 兜底，不让 answer 炸掉
    key = payload.get("key")
    scratch_memory = dict(case_state.scratch_memory)
    scratch_memory[str(key or decision.decision_id)] = text
    return DecisionEffectResult(state_patch={"scratch_memory": scratch_memory})


def _replay_followups(decision: Decision, answer: DecisionAnswer) -> tuple[HarnessFollowup, ...]:
    if _answer_blocks_pending_replay(decision, answer) or decision.pending_tool_call is None:
        return ()
    return (
        HarnessFollowup(
            kind="replay_pending_tool_call",
            decision_id=decision.decision_id,
            payload={
                "pending_tool_call": decision.pending_tool_call.model_dump(mode="json"),
                "scope_type": decision.scope_type,
                "project_id": decision.project_id,
                "case_id": decision.case_id,
            },
        ),
    )


def _merged_answer_payload(decision: Decision, answer: DecisionAnswer) -> dict[str, Any]:
    payload = dict(answer.payload)
    option_payload = _selected_option_payload(decision, answer.option_id)
    merged = dict(option_payload)
    merged.update(payload)
    return merged


def _selected_option_payload(decision: Decision, option_id: str | None) -> dict[str, Any]:
    if option_id is None:
        return {}
    for option in decision.options:
        if option.option_id == option_id:
            return dict(option.payload)
    return {}


def _enabled_from_answer(answer: DecisionAnswer, payload: dict[str, Any]) -> bool:
    explicit_enabled = payload.get("enabled")
    if isinstance(explicit_enabled, bool):
        return explicit_enabled
    return not _answer_is_rejection(answer)


def _answer_is_rejection(answer: DecisionAnswer) -> bool:
    if answer.payload.get("approved") is False:
        return True
    if answer.payload.get("enabled") is False:
        return True
    return answer.option_id in {"reject", "rejected", "deny", "denied", "no", "skip", "cancel"}


def _answer_blocks_pending_replay(decision: Decision, answer: DecisionAnswer) -> bool:
    payload = _merged_answer_payload(decision, answer)
    return _answer_is_rejection(answer) or _answer_requests_bgm_upload(answer, payload)


def _answer_requests_bgm_upload(answer: DecisionAnswer, payload: dict[str, Any]) -> bool:
    return payload.get("action") == "upload" or answer.option_id == "upload_bgm"


def _scratch_memory_with_pending_intent(case_state: CaseState, intent: str) -> dict[str, Any]:
    scratch_memory = dict(case_state.scratch_memory)
    existing = scratch_memory.get("pending_intents")
    pending_intents: list[str] = []
    if isinstance(existing, Sequence) and not isinstance(existing, str | bytes):
        pending_intents = [item for item in existing if isinstance(item, str)]
    if intent not in pending_intents:
        pending_intents.append(intent)
    scratch_memory["pending_intents"] = pending_intents
    return scratch_memory


def _expect_sequence(value: Any, field_name: str) -> Sequence[Any]:
    if not isinstance(value, Sequence) or isinstance(value, str | bytes):
        raise ValueError(f"{field_name} must be a sequence")
    return value


def _model_dump(value: Any) -> dict[str, Any]:
    if hasattr(value, "model_dump"):
        dumped = value.model_dump(mode="json", by_alias=True)
        if not isinstance(dumped, dict):
            raise TypeError("model_dump must return an object")
        return dumped
    if isinstance(value, dict):
        return dict(value)
    raise TypeError("expected a pydantic model or dict")


def _with_side_intents(
    case_state: CaseState,
    effect: DecisionEffectResult,
    answer: DecisionAnswer,
) -> DecisionEffectResult:
    side_intents = _side_intents_from_answer(answer)
    if not side_intents:
        return effect
    state_patch = dict(effect.state_patch)
    scratch_memory = dict(case_state.scratch_memory)
    existing_patch = state_patch.get("scratch_memory")
    if isinstance(existing_patch, dict):
        scratch_memory.update(existing_patch)
    existing = scratch_memory.get("pending_intents")
    pending_intents: list[str] = []
    if isinstance(existing, Sequence) and not isinstance(existing, str | bytes):
        pending_intents = [item for item in existing if isinstance(item, str)]
    for intent in side_intents:
        if intent not in pending_intents:
            pending_intents.append(intent)
    scratch_memory["pending_intents"] = pending_intents
    state_patch["scratch_memory"] = scratch_memory
    return DecisionEffectResult(
        state_patch=state_patch,
        followup_events=effect.followup_events,
        followups=effect.followups,
    )


def _side_intents_from_answer(answer: DecisionAnswer) -> list[str]:
    value = answer.payload.get("side_intents")
    if not isinstance(value, Sequence) or isinstance(value, str | bytes):
        return []
    return [item for item in value if isinstance(item, str) and item != ""]


def pending_tool_call_will_replay(decision: Decision, answer: DecisionAnswer) -> bool:
    return decision.pending_tool_call is not None and not _answer_blocks_pending_replay(
        decision, answer
    )


def pending_tool_call_status_after_answer(
    pending_tool_call: PendingToolCall | None,
    answer: DecisionAnswer,
    *,
    decision: Decision | None = None,
) -> Literal["approved", "discarded"] | None:
    if pending_tool_call is None:
        return None
    payload = (
        _merged_answer_payload(decision, answer) if decision is not None else dict(answer.payload)
    )
    if _answer_is_rejection(answer) or _answer_requests_bgm_upload(answer, payload):
        return "discarded"
    return "approved"


decision_effects_registry: dict[DecisionType, EffectFn] = {
    "audio_mode": _audio_mode_effect,
    "approve_content_plan": _approve_content_plan_effect,
    "approve_speech_cut": _approve_speech_cut_effect,
    "approve_rough_cut": _approve_rough_cut_effect,
    "subtitle": _subtitle_effect,
    "bgm": _bgm_effect,
    "export": _export_effect,
    "memory_scope": _memory_scope_effect,
    "destructive_project_action": _destructive_project_action_effect,
    "url_import": _url_import_effect,
    "generic": _generic_effect,
}

validate_all_decision_types_registered()
