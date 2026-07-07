"""PolicyGate pre-tool pruning and post-tool-call adjudication."""

from __future__ import annotations

import hashlib
import json
from collections.abc import Mapping, Sequence
from dataclasses import dataclass, field
from datetime import UTC, datetime
from typing import Any, Literal, Protocol, cast
from uuid import uuid4

from pydantic import BaseModel, ConfigDict, Field, ValidationError

from contracts.decision import (
    Decision,
    DecisionOption,
    DecisionScopeType,
    DecisionType,
    PendingToolCall,
)
from contracts.draft import DraftState, PostprocessPlan
from contracts.events import DecisionCreated, DomainEventBase, PolicyRefusal
from contracts.tool import PatchOpSpec, ToolSpec
from domain.preconditions import (
    PreconditionContext,
    evaluate_precondition,
    evaluate_preconditions,
)
from domain.subtitle_templates import list_subtitle_templates

VerdictStatus = Literal["deny", "ask", "defer", "allow"]

# 拦截 LLM 往参数里塞低层帧号/时间码/编码字段。注意不含裸 "source_start"/"source_end"：
# 帧号变体（source_start_frame）已被 "frame" 覆盖、时间码变体被 "timecode" 覆盖，而
# timeline.compose_initial 合法使用以秒为单位的 source_start_s/source_end_s 组装初剪（Spec C §C4）。
_PROHIBITED_ARGUMENT_KEY_PARTS = frozenset(
    {
        "frame",
        "source_timecode",
        "timecode",
        "ffmpeg",
        "filter_complex",
        "codec",
        "bitrate",
        "crf",
        "preset",
        "pix_fmt",
    }
)
_PROHIBITED_ARGUMENT_KEY_NAMES = frozenset(
    {
        "path",
        "file",
        "file_path",
        "source_path",
        "reference_path",
        "workspace_object_uri",
        "local_path",
        "argv",
        "vf",
        "af",
    }
)


class ToolCall(BaseModel):
    model_config = ConfigDict(extra="forbid")

    tool_name: str
    arguments: dict[str, Any] = Field(default_factory=dict)
    tool_call_id: str | None = None
    idempotency_key: str | None = None

    @classmethod
    def from_input(cls, value: ToolCall | Mapping[str, Any]) -> ToolCall:
        if isinstance(value, ToolCall):
            return value
        data = dict(value)
        if "tool_name" not in data and "name" in data:
            data["tool_name"] = data.pop("name")
        if "arguments" not in data and "args" in data:
            data["arguments"] = data.pop("args")
        return cls.model_validate(data)


class PolicyContext(BaseModel):
    """PolicyGate's pure context view for one agent turn."""

    model_config = ConfigDict(extra="forbid", arbitrary_types_allowed=True)

    preconditions: PreconditionContext
    decisions: tuple[Decision, ...] = Field(default_factory=tuple)
    pending_decision: Decision | None = None
    allowed_tools: tuple[ToolSpec, ...] = Field(default_factory=tuple)
    allowed_tool_names: frozenset[str] | None = None


@dataclass(frozen=True, slots=True)
class Verdict:
    status: VerdictStatus
    reason: str = ""
    tool_call: ToolCall | None = None
    events: tuple[DomainEventBase, ...] = ()
    decision: Decision | None = None
    pending_tool_call: PendingToolCall | None = None
    validated_arguments: Mapping[str, Any] = field(default_factory=dict)


class DecisionReplayRepository(Protocol):
    def mark_pending_tool_call_replayed(
        self,
        decision_id: str,
        *,
        consumed_at: str,
        replayed_tool_call_id: str,
    ) -> bool:
        """Atomically transition approved -> replayed."""


class PolicyGate:
    def __init__(
        self,
        *,
        tool_specs: Mapping[str, ToolSpec],
        patch_op_specs: Mapping[str, PatchOpSpec],
    ) -> None:
        self._tool_specs = dict(tool_specs)
        self._patch_op_specs = dict(patch_op_specs)

    def compute_allowed_tools(self, context: PolicyContext) -> list[ToolSpec]:
        pending_decision = _pending_blocking_decision(context)
        allowed: list[ToolSpec] = []
        for spec in self._tool_specs.values():
            if spec.status != "stable" or spec.exposure != "llm":
                continue
            if pending_decision is not None:
                if _is_pending_decision_whitelisted(spec):
                    allowed.append(spec)
                continue
            if not _active_requirements_pass(spec, context.preconditions):
                continue
            if not evaluate_preconditions(spec.requires_artifacts, context.preconditions):
                continue
            allowed.append(spec)
        return allowed

    def adjudicate(
        self,
        tool_call: ToolCall | Mapping[str, Any],
        context: PolicyContext,
    ) -> Verdict:
        parsed_call = ToolCall.from_input(tool_call)
        spec = self._tool_specs.get(parsed_call.tool_name)
        if spec is None:
            return self._deny(parsed_call, context, "tool is not registered")

        allowed_tool_names = _allowed_tool_names(context)
        if (
            not allowed_tool_names
            and context.allowed_tool_names is None
            and not context.allowed_tools
        ):
            allowed_tool_names = frozenset(
                allowed_spec.name for allowed_spec in self.compute_allowed_tools(context)
            )
        if parsed_call.tool_name not in allowed_tool_names:
            # 报告未满足的前置：哑 deny 会让 planner 反复重试同一工具（M9 实测）
            unmet = [
                name
                for name in spec.requires_artifacts
                if not evaluate_precondition(name, context.preconditions)
            ]
            reason = "tool is not in this turn's allowed_tools"
            if unmet:
                reason += f"；未满足前置：{', '.join(unmet)}。请先完成对应步骤，不要重复调用本工具"
            return self._deny(parsed_call, context, reason)

        prohibited_key = _first_prohibited_argument_key(parsed_call.arguments)
        if prohibited_key is not None:
            return self._deny(
                parsed_call,
                context,
                f"argument field is prohibited by PolicyGate: {prohibited_key}",
            )

        try:
            validated_model = spec.input_model.model_validate(parsed_call.arguments)
        except ValidationError as exc:
            return self._deny(
                parsed_call,
                context,
                "input_model strict validation failed",
                details={"validation_errors": exc.errors(include_url=False)},
            )
        validated_arguments = validated_model.model_dump(mode="json", by_alias=True)

        if not _active_requirements_pass(spec, context.preconditions):
            return self._deny(parsed_call, context, "tool active scope requirements failed")

        if not evaluate_preconditions(spec.requires_artifacts, context.preconditions):
            return self._deny(parsed_call, context, "tool artifact preconditions failed")

        if parsed_call.tool_name == "timeline.apply_patch":
            op_verdict = self._adjudicate_patch_op(parsed_call, validated_arguments, context)
            if op_verdict is not None:
                return op_verdict

        confirmation_arguments = _confirmation_arguments(
            spec,
            validated_arguments,
            context.preconditions,
        )
        if spec.requires_confirmation and not _answered_confirmation_exists(
            context,
            tool_name=spec.name,
            decision_type=spec.confirmation_decision_type,
            arguments=confirmation_arguments,
        ):
            return self._ask(parsed_call, spec, confirmation_arguments, context)

        pending_decision = _pending_blocking_decision(context)
        if (
            pending_decision is not None
            and spec.side_effects
            and not _is_pending_decision_whitelisted(spec)
        ):
            return self._deny(
                parsed_call,
                context,
                "tool side effects are incompatible with the pending blocking decision",
            )

        if spec.is_long_running:
            return Verdict(
                status="defer",
                reason="tool is long-running and must be enqueued",
                tool_call=parsed_call,
                validated_arguments=validated_arguments,
            )

        return Verdict(
            status="allow",
            reason="policy checks passed",
            tool_call=parsed_call,
            validated_arguments=validated_arguments,
        )

    def _adjudicate_patch_op(
        self,
        tool_call: ToolCall,
        arguments: Mapping[str, Any],
        context: PolicyContext,
    ) -> Verdict | None:
        op = arguments.get("op")
        if not isinstance(op, Mapping):
            return self._deny(tool_call, context, "timeline.apply_patch requires an op object")
        kind = op.get("kind")
        if not isinstance(kind, str):
            return self._deny(tool_call, context, "timeline.apply_patch op requires kind")
        op_spec = self._patch_op_specs.get(kind)
        if op_spec is None:
            return self._deny(tool_call, context, f"patch op is not registered: {kind}")

        try:
            op_spec.params_model.model_validate(op)
        except ValidationError as exc:
            return self._deny(
                tool_call,
                context,
                "patch op params_model strict validation failed",
                details={"validation_errors": exc.errors(include_url=False)},
            )

        if op_spec.requires_confirmation:
            decision_type = op_spec.confirmation_decision_type
            if not _postprocess_plan_item_exists(context.preconditions.draft_state, decision_type):
                tool_spec = self._tool_specs[tool_call.tool_name]
                return self._ask(
                    tool_call,
                    tool_spec,
                    arguments,
                    context,
                    decision_type=decision_type,
                )
            if not evaluate_preconditions(op_spec.requires_artifacts, context.preconditions):
                return self._deny(
                    tool_call,
                    context,
                    "patch op artifact preconditions failed after postprocess_plan was present",
                )
        elif not evaluate_preconditions(op_spec.requires_artifacts, context.preconditions):
            return self._deny(tool_call, context, "patch op artifact preconditions failed")
        return None

    def _ask(
        self,
        tool_call: ToolCall,
        spec: ToolSpec,
        arguments: Mapping[str, Any],
        context: PolicyContext,
        *,
        decision_type: str | None = None,
    ) -> Verdict:
        effective_decision_type = decision_type or spec.confirmation_decision_type
        if effective_decision_type is None:
            return self._deny(
                tool_call,
                context,
                "requires_confirmation tool has no confirmation_decision_type",
            )
        argument_fingerprint = fingerprint(arguments)
        decision_id = _decision_id(
            effective_decision_type,
            tool_call.tool_name,
            argument_fingerprint,
        )
        pending_tool_call = PendingToolCall(
            tool_name=tool_call.tool_name,
            arguments=dict(arguments),
            idempotency_key=_idempotency_key(spec, tool_call, arguments, decision_id),
            argument_fingerprint=argument_fingerprint,
            original_tool_call_id=tool_call.tool_call_id,
        )
        scope_type = _confirmation_scope_type(effective_decision_type, context.preconditions)
        draft_id = _scope_draft_id(scope_type, context.preconditions)
        decision = Decision(
            decision_id=decision_id,
            scope_type=scope_type,
            draft_id=draft_id,
            type=cast(DecisionType, effective_decision_type),
            question=_confirmation_question(effective_decision_type, tool_call.tool_name, context),
            options=_confirmation_options(effective_decision_type, context),
            allow_free_text=False,
            status="pending",
            answer=None,
            pending_tool_call=pending_tool_call,
            pending_tool_call_status="pending",
            blocking=scope_type == "draft",
            created_by_tool_call_id=tool_call.tool_call_id,
        )
        event = DecisionCreated(
            decision_id=decision.decision_id,
            scope_type=decision.scope_type,
            draft_id=decision.draft_id,
            payload={
                "decision": decision.model_dump(mode="json"),
                "type": decision.type,
                "question": decision.question,
                "options": [option.model_dump(mode="json") for option in decision.options],
                "pending_tool_call": pending_tool_call.model_dump(mode="json"),
                "pending_tool_call_status": "pending",
                "blocking": decision.blocking,
                "created_by_tool_call_id": tool_call.tool_call_id,
            },
        )
        return Verdict(
            status="ask",
            reason="human confirmation is required",
            tool_call=tool_call,
            events=(event,),
            decision=decision,
            pending_tool_call=pending_tool_call,
            validated_arguments=arguments,
        )

    def _deny(
        self,
        tool_call: ToolCall,
        context: PolicyContext,
        reason: str,
        *,
        details: Mapping[str, Any] | None = None,
    ) -> Verdict:
        draft_state = context.preconditions.draft_state
        payload: dict[str, Any] = {
            "tool_name": tool_call.tool_name,
            "reason": reason,
            "arguments_fingerprint": fingerprint(tool_call.arguments),
        }
        if details is not None:
            payload["details"] = dict(details)
        event = PolicyRefusal(
            refusal_id=_refusal_id(tool_call, reason),
            draft_id=draft_state.draft_id if draft_state is not None else None,
            payload=payload,
        )
        return Verdict(status="deny", reason=reason, tool_call=tool_call, events=(event,))


def fingerprint(arguments: Mapping[str, Any]) -> str:
    """Return sha256 of canonical JSON arguments."""

    encoded = json.dumps(
        _jsonable(arguments),
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
    )
    return hashlib.sha256(encoded.encode("utf-8")).hexdigest()


def next_replay(decision: Decision) -> PendingToolCall | None:
    if decision.pending_tool_call_status != "approved":
        return None
    return decision.pending_tool_call


def mark_replayed(
    repository: DecisionReplayRepository,
    decision_id: str,
    *,
    replayed_tool_call_id: str,
    consumed_at: str | None = None,
) -> bool:
    return repository.mark_pending_tool_call_replayed(
        decision_id,
        consumed_at=consumed_at or _now_iso(),
        replayed_tool_call_id=replayed_tool_call_id,
    )


def _active_requirements_pass(spec: ToolSpec, context: PreconditionContext) -> bool:
    return not (
        spec.requires_active_draft
        and not (context.draft_state is not None and context.draft_state.status == "active")
    )


def _allowed_tool_names(context: PolicyContext) -> frozenset[str]:
    if context.allowed_tool_names is not None:
        return context.allowed_tool_names
    if context.allowed_tools:
        return frozenset(spec.name for spec in context.allowed_tools)
    return frozenset()


def _pending_blocking_decision(context: PolicyContext) -> Decision | None:
    if (
        context.pending_decision is not None
        and context.pending_decision.scope_type == "draft"
        and context.pending_decision.status == "pending"
        and context.pending_decision.blocking
    ):
        return context.pending_decision
    draft_state = context.preconditions.draft_state
    if draft_state is None or draft_state.pending_decision_id is None:
        return None
    for decision in context.decisions:
        if (
            decision.decision_id == draft_state.pending_decision_id
            and decision.scope_type == "draft"
            and decision.status == "pending"
            and decision.blocking
        ):
            return decision
    return None


def _is_pending_decision_whitelisted(spec: ToolSpec) -> bool:
    if spec.name == "decision.answer":
        return True
    if not spec.side_effects:
        return True
    return _is_cancel_interaction_tool(spec)


def _is_cancel_interaction_tool(spec: ToolSpec) -> bool:
    if spec.namespace != "interaction" and not spec.name.startswith("interaction."):
        return False
    lowered = spec.name.lower()
    return any(token in lowered for token in ("cancel", "dismiss", "discard", "decline"))


def _answered_confirmation_exists(
    context: PolicyContext,
    *,
    tool_name: str,
    decision_type: str | None,
    arguments: Mapping[str, Any],
) -> bool:
    if decision_type is None:
        return False
    argument_fingerprint = fingerprint(arguments)
    for decision in context.decisions:
        pending_tool_call = decision.pending_tool_call
        if (
            decision.status == "answered"
            and decision.type == decision_type
            and decision.pending_tool_call_status in {"approved", "replayed"}
            and pending_tool_call is not None
            and pending_tool_call.tool_name == tool_name
            and pending_tool_call.argument_fingerprint == argument_fingerprint
        ):
            return True
    return False


def _confirmation_arguments(
    spec: ToolSpec,
    arguments: Mapping[str, Any],
    context: PreconditionContext,
) -> dict[str, Any]:
    normalized = dict(arguments)
    draft_state = context.draft_state
    if (
        spec.requires_active_draft
        and "draft_id" in normalized
        and normalized["draft_id"] is None
        and draft_state is not None
    ):
        normalized["draft_id"] = draft_state.draft_id
    return normalized


def _postprocess_plan_item_exists(
    draft_state: DraftState | None, decision_type: str | None
) -> bool:
    if draft_state is None or draft_state.postprocess_plan is None:
        return False
    plan: PostprocessPlan = draft_state.postprocess_plan
    if decision_type == "subtitle":
        return plan.subtitle is not None
    if decision_type == "bgm":
        return plan.bgm is not None
    return False


def _confirmation_scope_type(
    decision_type: str,
    context: PreconditionContext,
) -> DecisionScopeType:
    # 单级草稿模型：有草稿即 draft 域（strict/可阻塞），否则 workspace 域（merge/非阻塞）。
    del decision_type
    if context.draft_state is not None:
        return "draft"
    return "workspace"


def _scope_draft_id(
    scope_type: DecisionScopeType,
    context: PreconditionContext,
) -> str | None:
    if scope_type == "draft":
        if context.draft_state is None:
            raise ValueError("draft-scoped confirmation requires draft_state")
        return context.draft_state.draft_id
    return None


def _confirmation_question(
    decision_type: str,
    tool_name: str,
    context: PolicyContext,
) -> str:
    if decision_type == "export":
        draft_state = context.preconditions.draft_state
        if (
            draft_state is not None
            and draft_state.preview_current_id is not None
            and draft_state.last_viewed_preview_id != draft_state.preview_current_id
        ):
            return "你还没看最新预览。确认开始最终导出？"
        return "确认开始最终导出？"
    if decision_type == "subtitle":
        return "确认新增字幕后处理？"
    if decision_type == "bgm":
        return "确认新增 BGM 后处理？"
    if decision_type == "url_import":
        return "确认从 URL 导入素材？"
    return f"确认执行 {tool_name}？"


def _confirmation_options(decision_type: str, context: PolicyContext) -> list[DecisionOption]:
    if decision_type == "subtitle":
        options = [
            DecisionOption(
                option_id=template.template_id,
                label=template.display_name,
                payload={"enabled": True, "style_template_id": template.template_id},
            )
            for template in list_subtitle_templates()
        ]
        options.append(
            DecisionOption(option_id="skip", label="跳过字幕", payload={"enabled": False})
        )
        return options
    if decision_type == "bgm":
        return _bgm_confirmation_options(context)
    return [
        DecisionOption(option_id="approve", label="确认", payload={"approved": True}),
        DecisionOption(option_id="reject", label="取消", payload={"approved": False}),
    ]


def _bgm_confirmation_options(context: PolicyContext) -> list[DecisionOption]:
    upload_option = DecisionOption(
        option_id="upload_bgm",
        label="上传 BGM 素材",
        payload={"enabled": True, "action": "upload"},
    )
    skip_option = DecisionOption(option_id="skip", label="跳过 BGM", payload={"enabled": False})
    draft_assets = context.preconditions.draft_audio_assets[:5]
    options = [
        DecisionOption(
            option_id=asset.asset_id,
            label=f"使用素材：{asset.filename}",
            payload={"enabled": True, "asset_id": asset.asset_id, "gain_db": -12.0, "duck": True},
        )
        for asset in draft_assets
    ]
    options.extend((upload_option, skip_option))
    return options


def _idempotency_key(
    spec: ToolSpec,
    tool_call: ToolCall,
    arguments: Mapping[str, Any],
    decision_id: str,
) -> str:
    if tool_call.idempotency_key:
        base = tool_call.idempotency_key
    elif spec.idempotency_key_fields:
        parts = [str(arguments.get(field, "")) for field in spec.idempotency_key_fields]
        base = f"{spec.name}:{':'.join(parts)}"
    else:
        base = f"{spec.name}:{fingerprint(arguments)[:16]}"
    return f"{base}:decision:{decision_id}"


def _decision_id(decision_type: str, tool_name: str, argument_fingerprint: str) -> str:
    safe_tool_name = tool_name.replace(".", "_")
    return f"dec_{decision_type}_{safe_tool_name}_{argument_fingerprint[:16]}"


def _refusal_id(tool_call: ToolCall, reason: str) -> str:
    # 每次拒绝都是独立事件（§4.5 merge 键为"各自 id"）：加入随机分量，
    # 避免同回合内对同一调用的多次重试被 merge 幂等键合并成一条。
    payload = f"{tool_call.tool_name}:{reason}:{fingerprint(tool_call.arguments)}:{uuid4().hex}"
    return "pref_" + hashlib.sha256(payload.encode("utf-8")).hexdigest()[:16]


def _first_prohibited_argument_key(value: Any) -> str | None:
    if isinstance(value, Mapping):
        for key, child in value.items():
            if isinstance(key, str) and _is_prohibited_argument_key(key):
                return key
            nested = _first_prohibited_argument_key(child)
            if nested is not None:
                return nested
    elif isinstance(value, Sequence) and not isinstance(value, str | bytes | bytearray):
        for child in value:
            nested = _first_prohibited_argument_key(child)
            if nested is not None:
                return nested
    return None


def _is_prohibited_argument_key(key: str) -> bool:
    lowered = key.lower()
    if lowered in _PROHIBITED_ARGUMENT_KEY_NAMES:
        return True
    if lowered.endswith("_path"):
        return True
    return any(part in lowered for part in _PROHIBITED_ARGUMENT_KEY_PARTS)


def _jsonable(value: Any) -> Any:
    if isinstance(value, BaseModel):
        return value.model_dump(mode="json", by_alias=True)
    if isinstance(value, Mapping):
        return {str(key): _jsonable(child) for key, child in value.items()}
    if isinstance(value, Sequence) and not isinstance(value, str | bytes | bytearray):
        return [_jsonable(child) for child in value]
    return value


def _now_iso() -> str:
    return datetime.now(UTC).isoformat()
