"""Content plan creation and revision tool handlers."""

from __future__ import annotations

import asyncio
import json
import threading
from collections.abc import Mapping, Sequence
from dataclasses import dataclass
from typing import Any, cast

from sqlalchemy import and_, select

from contracts.case import CaseState, CutPlan
from contracts.events import ContentPlanUpdated, CutPlanUpdated
from contracts.provider import ProviderResult
from contracts.tool_result import ToolError, ToolResult
from providers import LLM_CHAT, ProviderRequest
from storage import schema
from tools.context import ToolExecutionContext
from tools.specs import ContentCreatePlanInput, ContentRevisePlanInput

TOOL_CREATE = "content.create_plan"
TOOL_REVISE = "content.revise_plan"
DEFAULT_TARGET_DURATION_SEC = 30.0


@dataclass(frozen=True, slots=True)
class _AssetSummary:
    asset_id: str
    filename: str
    summary: str
    quality_score: float | None


@dataclass(frozen=True, slots=True)
class _PlanResult:
    content_plan: dict[str, Any]
    cut_plan: dict[str, Any]
    source: str


def create_plan(input_model: ContentCreatePlanInput, context: ToolExecutionContext) -> ToolResult:
    case_state = context.case_state
    if case_state is None:
        return _failed(TOOL_CREATE, context, "missing_case", "active case required")
    if context.readonly_connection is None:
        return _failed(TOOL_CREATE, context, "missing_connection", "repository access required")

    assets = _asset_summaries(context)
    target_duration = _target_duration(input_model.target_duration_sec, case_state)
    fallback = _fallback_plan(
        case_state,
        assets,
        target_duration=target_duration,
        storyline_hint=input_model.storyline_hint,
        slot_count=input_model.slot_count,
        existing_content_plan=None,
        existing_cut_plan=None,
    )
    needs_cut = _should_emit_cut_plan(case_state)
    plan = _create_with_llm(
        context,
        case_state,
        assets,
        target_duration=target_duration,
        storyline_hint=input_model.storyline_hint,
        slot_count=input_model.slot_count,
        fallback=fallback,
        needs_cut=needs_cut,
    )
    if plan is None:
        plan = fallback
    return _succeeded(TOOL_CREATE, context, case_state, plan, emit_cut_plan=needs_cut)


def revise_plan(input_model: ContentRevisePlanInput, context: ToolExecutionContext) -> ToolResult:
    case_state = context.case_state
    if case_state is None:
        return _failed(TOOL_REVISE, context, "missing_case", "active case required")
    if context.readonly_connection is None:
        return _failed(TOOL_REVISE, context, "missing_connection", "repository access required")
    if case_state.content_plan is None:
        return _failed(
            TOOL_REVISE,
            context,
            "missing_content_plan",
            "content.revise_plan requires an existing content_plan",
        )

    assets = _asset_summaries(context)
    target_duration = (
        case_state.cut_plan.total_target_duration_sec
        if case_state.cut_plan is not None
        else _target_duration(None, case_state)
    )
    existing_cut = (
        case_state.cut_plan.model_dump(mode="json", by_alias=True)
        if case_state.cut_plan is not None
        else None
    )
    fallback = _fallback_plan(
        case_state,
        assets,
        target_duration=target_duration,
        storyline_hint=input_model.revision_hint,
        slot_count=None,
        existing_content_plan=case_state.content_plan,
        existing_cut_plan=existing_cut,
    )
    needs_cut = _should_emit_cut_plan(case_state)
    plan = _revise_with_llm(
        context,
        case_state,
        assets,
        target_duration=target_duration,
        revision_hint=input_model.revision_hint,
        fallback=fallback,
        needs_cut=needs_cut,
    )
    if plan is None:
        plan = fallback
    return _succeeded(TOOL_REVISE, context, case_state, plan, emit_cut_plan=needs_cut)


def _succeeded(
    tool_name: str,
    context: ToolExecutionContext,
    case_state: CaseState,
    plan: _PlanResult,
    *,
    emit_cut_plan: bool,
) -> ToolResult:
    events = [
        ContentPlanUpdated(
            project_id=case_state.project_id,
            case_id=case_state.case_id,
            payload={"content_plan": plan.content_plan},
        ).model_dump(mode="json")
    ]
    if emit_cut_plan:
        events.append(
            CutPlanUpdated(
                project_id=case_state.project_id,
                case_id=case_state.case_id,
                payload={"cut_plan": plan.cut_plan},
            ).model_dump(mode="json")
        )
    # observation 必须指路：audio_plan 未定时模型需要知道 cut_plan 还没产出、
    # 下一步是什么，否则会反复重调本工具（M9 风景混剪实测）
    observation = _observation(plan)
    if emit_cut_plan:
        observation += "。cut_plan 已产出，下一步可直接 retrieval.search_candidates。"
    elif case_state.audio_plan is None:
        observation += (
            "。注意：audio_plan 尚未确定，本次未产出 cut_plan——请先用 "
            "interaction.ask_user 创建 audio_mode 决策（选定后若为静音模式，"
            "重新调用 content.create_plan 即可产出 cut_plan）。"
        )
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name=tool_name,
        status="succeeded",
        observation=observation,
        data={
            "case_id": case_state.case_id,
            "content_plan": plan.content_plan,
            "cut_plan": plan.cut_plan if emit_cut_plan else None,
            "source": plan.source,
        },
        events=events,
    )


def _create_with_llm(
    context: ToolExecutionContext,
    case_state: CaseState,
    assets: list[_AssetSummary],
    *,
    target_duration: float,
    storyline_hint: str | None,
    slot_count: int | None,
    fallback: _PlanResult,
    needs_cut: bool,
) -> _PlanResult | None:
    output = _call_llm(
        context,
        request_id=f"content_create_{context.tool_call_id}",
        payload={
            "mode": "create",
            "brief": case_state.brief.model_dump(mode="json"),
            "audio_plan": None
            if case_state.audio_plan is None
            else case_state.audio_plan.model_dump(mode="json"),
            "assets": [_asset_payload(asset) for asset in assets],
            "storyline_hint": storyline_hint,
            "target_duration_sec": target_duration,
            "slot_count": slot_count,
        },
    )
    if output is None:
        return None
    return _plan_from_llm_output(
        output,
        fallback=fallback,
        target_duration=target_duration,
        needs_cut=needs_cut,
        fallback_source="llm",
    )


def _revise_with_llm(
    context: ToolExecutionContext,
    case_state: CaseState,
    assets: list[_AssetSummary],
    *,
    target_duration: float,
    revision_hint: str,
    fallback: _PlanResult,
    needs_cut: bool,
) -> _PlanResult | None:
    output = _call_llm(
        context,
        request_id=f"content_revise_{context.tool_call_id}",
        payload={
            "mode": "revise",
            "revision_hint": revision_hint,
            "brief": case_state.brief.model_dump(mode="json"),
            "audio_plan": None
            if case_state.audio_plan is None
            else case_state.audio_plan.model_dump(mode="json"),
            "content_plan": case_state.content_plan,
            "cut_plan": fallback.cut_plan if needs_cut else None,
            "assets": [_asset_payload(asset) for asset in assets],
            "target_duration_sec": target_duration,
        },
    )
    if output is None:
        return None
    return _plan_from_llm_output(
        output,
        fallback=fallback,
        target_duration=target_duration,
        needs_cut=needs_cut,
        fallback_source="llm",
    )


def _call_llm(
    context: ToolExecutionContext,
    *,
    request_id: str,
    payload: dict[str, Any],
) -> Mapping[str, Any] | None:
    gateway = context.metadata.get("provider_gateway") or context.metadata.get("llm_gateway")
    if gateway is None or not hasattr(gateway, "call"):
        return None
    request = ProviderRequest(
        capability=LLM_CHAT,
        request_id=request_id,
        case_id=context.case_state.case_id if context.case_state is not None else None,
        payload={
            "messages": [
                {
                    "role": "system",
                    "content": (
                        "你是短视频剪辑内容策划器。只输出 JSON，格式为 "
                        '{"content_plan":{"schema":"ContentPlan.v1","storyline":"...",'
                        '"sections":[{"section_id":"...","intent":"...","notes":"..."}],'
                        '"status":"draft"},"cut_plan":{"schema":"CutPlan.v1","slots":'
                        '[{"slot_id":"...","brief":"...","target_duration_sec":[1,3]}],'
                        '"removed_ranges":[],"total_target_duration_sec":30}}。'
                        "不要编造输入素材没有支持的画面。"
                    ),
                },
                {"role": "user", "content": json.dumps(payload, ensure_ascii=False)},
            ],
            "tool_choice": {"type": "function", "function": {"name": "write_content_plan"}},
            "tools": [
                {
                    "type": "function",
                    "function": {
                        "name": "write_content_plan",
                        "description": "Return a content_plan and optional cut_plan JSON object.",
                        "parameters": {"type": "object"},
                    },
                }
            ],
        },
    )
    try:
        gateway_result = _run_async_sync(gateway.call(request))
    except Exception:
        return None
    result = cast(ProviderResult, gateway_result.result)
    if result.error is not None:
        return None
    return result.normalized_output


def _plan_from_llm_output(
    output: Mapping[str, Any],
    *,
    fallback: _PlanResult,
    target_duration: float,
    needs_cut: bool,
    fallback_source: str,
) -> _PlanResult | None:
    parsed = _mapping_from_output(output)
    if parsed is None:
        return None
    content_plan = _content_plan_from_mapping(parsed, fallback.content_plan)
    if content_plan is None:
        return None
    cut_plan = fallback.cut_plan
    if needs_cut:
        normalized_cut = _cut_plan_from_mapping(
            parsed,
            fallback.cut_plan,
            target_duration=target_duration,
        )
        if normalized_cut is None:
            return None
        cut_plan = normalized_cut
    return _PlanResult(content_plan=content_plan, cut_plan=cut_plan, source=fallback_source)


def _mapping_from_output(output: Mapping[str, Any]) -> Mapping[str, Any] | None:
    if _looks_like_plan_mapping(output):
        return output
    for key in ("content", "text"):
        content = output.get(key)
        if isinstance(content, Mapping) and _looks_like_plan_mapping(content):
            return cast(Mapping[str, Any], content)
        if isinstance(content, str):
            parsed = _json_mapping(content)
            if parsed is not None and _looks_like_plan_mapping(parsed):
                return parsed
    if "arguments" in output:
        parsed = _mapping_from_tool_call(output)
        if parsed is not None:
            return parsed
    tool_call = output.get("tool_call")
    if isinstance(tool_call, Mapping):
        parsed = _mapping_from_tool_call(tool_call)
        if parsed is not None:
            return parsed
    tool_calls = output.get("tool_calls")
    if isinstance(tool_calls, Sequence) and not isinstance(tool_calls, str | bytes):
        for item in tool_calls:
            if isinstance(item, Mapping):
                parsed = _mapping_from_tool_call(item)
                if parsed is not None:
                    return parsed
    return None


def _mapping_from_tool_call(value: Mapping[str, Any]) -> Mapping[str, Any] | None:
    raw: Any = value
    function = value.get("function")
    if isinstance(function, Mapping) and "arguments" in function:
        raw = function["arguments"]
    elif "arguments" in value:
        raw = value["arguments"]
    if isinstance(raw, str):
        parsed = _json_mapping(raw)
        if parsed is None:
            return None
        raw = parsed
    if isinstance(raw, Mapping) and _looks_like_plan_mapping(raw):
        return cast(Mapping[str, Any], raw)
    return None


def _looks_like_plan_mapping(value: Mapping[str, Any]) -> bool:
    keys = ("content_plan", "cut_plan", "storyline", "sections", "slots")
    return any(key in value for key in keys)


def _json_mapping(value: str) -> Mapping[str, Any] | None:
    try:
        parsed = json.loads(value.strip())
    except json.JSONDecodeError:
        return None
    return cast(Mapping[str, Any], parsed) if isinstance(parsed, Mapping) else None


def _content_plan_from_mapping(
    parsed: Mapping[str, Any],
    fallback: Mapping[str, Any],
) -> dict[str, Any] | None:
    raw = parsed.get("content_plan")
    source = cast(Mapping[str, Any], raw) if isinstance(raw, Mapping) else parsed
    storyline = _first_text(source, ("storyline", "summary", "outline"))
    if storyline is None:
        storyline = _first_text(fallback, ("storyline", "summary", "outline"))
    sections = _sections_from_value(source.get("sections"))
    if not sections:
        sections = _sections_from_value(fallback.get("sections"))
    if storyline is None and not sections:
        return None
    if storyline is None:
        storyline = str(sections[0]["intent"])
    status = _first_text(source, ("status",)) or _first_text(fallback, ("status",)) or "draft"
    return {
        "schema": "ContentPlan.v1",
        "storyline": storyline,
        "sections": sections or _sections_from_storyline(storyline),
        "status": status,
    }


def _sections_from_value(value: Any) -> list[dict[str, str]]:
    if not isinstance(value, Sequence) or isinstance(value, str | bytes):
        return []
    sections: list[dict[str, str]] = []
    for index, item in enumerate(value, start=1):
        if isinstance(item, str):
            text = item.strip()
            if text:
                sections.append(
                    {
                        "section_id": f"section_{index:03d}",
                        "intent": text,
                        "notes": text,
                    }
                )
            continue
        if not isinstance(item, Mapping):
            continue
        section = cast(Mapping[str, Any], item)
        section_id = _first_text(section, ("section_id", "id")) or f"section_{index:03d}"
        intent = _first_text(section, ("intent", "title", "brief", "summary", "narration"))
        notes = _first_text(section, ("notes", "detail", "description")) or intent
        if intent is None:
            continue
        sections.append({"section_id": section_id, "intent": intent, "notes": notes or ""})
    return sections


def _cut_plan_from_mapping(
    parsed: Mapping[str, Any],
    fallback: Mapping[str, Any],
    *,
    target_duration: float,
) -> dict[str, Any] | None:
    raw = parsed.get("cut_plan")
    source = cast(Mapping[str, Any], raw) if isinstance(raw, Mapping) else parsed
    raw_slots = source.get("slots")
    if not isinstance(raw_slots, Sequence) or isinstance(raw_slots, str | bytes):
        return None
    fallback_slots = fallback.get("slots")
    fallback_slot_list = (
        [cast(Mapping[str, Any], item) for item in fallback_slots if isinstance(item, Mapping)]
        if isinstance(fallback_slots, Sequence) and not isinstance(fallback_slots, str | bytes)
        else []
    )
    slots: list[dict[str, Any]] = []
    for index, item in enumerate(raw_slots, start=1):
        if not isinstance(item, Mapping):
            continue
        fallback_slot: Mapping[str, Any] = (
            fallback_slot_list[index - 1] if index - 1 < len(fallback_slot_list) else {}
        )
        slot = _slot_from_mapping(cast(Mapping[str, Any], item), fallback_slot, index)
        if slot is not None:
            slots.append(slot)
    if not slots:
        return None
    return _validated_cut_plan(slots, target_duration)


def _slot_from_mapping(
    item: Mapping[str, Any],
    fallback: Mapping[str, Any],
    index: int,
) -> dict[str, Any] | None:
    slot_id = _first_text(item, ("slot_id", "id")) or _first_text(fallback, ("slot_id", "id"))
    brief = _first_text(item, ("brief", "summary", "intent", "notes", "title"))
    if brief is None:
        brief = _first_text(fallback, ("brief", "summary", "intent", "notes", "title"))
    if brief is None:
        return None
    window = _duration_window_from_value(item.get("target_duration_sec"))
    if window is None:
        window = _duration_window_from_value(fallback.get("target_duration_sec"))
    if window is None:
        window = _duration_window(3.0)
    return {
        "slot_id": slot_id or f"slot_{index:03d}",
        "brief": _trim(brief, 160),
        "target_duration_sec": window,
    }


def _fallback_plan(
    case_state: CaseState,
    assets: list[_AssetSummary],
    *,
    target_duration: float,
    storyline_hint: str | None,
    slot_count: int | None,
    existing_content_plan: Mapping[str, Any] | None,
    existing_cut_plan: Mapping[str, Any] | None,
) -> _PlanResult:
    if existing_content_plan is not None:
        content_plan = dict(existing_content_plan)
        content_plan.setdefault("schema", "ContentPlan.v1")
        content_plan.setdefault("status", "draft")
    else:
        storyline = _fallback_storyline(case_state, assets, storyline_hint)
        content_plan = {
            "schema": "ContentPlan.v1",
            "storyline": storyline,
            "sections": _fallback_sections(storyline, assets, slot_count),
            "status": "draft",
        }
    if existing_cut_plan is not None:
        cut_plan = CutPlan.model_validate(existing_cut_plan).model_dump(mode="json", by_alias=True)
    else:
        cut_plan = _fallback_cut_plan(case_state, assets, target_duration, slot_count)
    return _PlanResult(content_plan=content_plan, cut_plan=cut_plan, source="fallback")


def _fallback_storyline(
    case_state: CaseState,
    assets: list[_AssetSummary],
    storyline_hint: str | None,
) -> str:
    if storyline_hint is not None and storyline_hint.strip():
        return storyline_hint.strip()
    asset_text = "，".join(_summary_text(asset) for asset in assets[:3])
    style = "；".join(case_state.brief.style_notes[:2])
    parts = [case_state.brief.goal, style, asset_text]
    if any(parts):
        return "；".join(part for part in parts if part)
    return "根据现有素材组织一条清晰的视觉故事线。"


def _fallback_sections(
    storyline: str,
    assets: list[_AssetSummary],
    slot_count: int | None,
) -> list[dict[str, str]]:
    if assets:
        return [
            {
                "section_id": f"section_{index:03d}",
                "intent": _summary_text(asset),
                "notes": f"素材 {asset.asset_id}",
            }
            for index, asset in enumerate(assets, start=1)
        ]
    count = max(1, slot_count or 1)
    return [
        {
            "section_id": f"section_{index:03d}",
            "intent": storyline,
            "notes": storyline,
        }
        for index in range(1, count + 1)
    ]


def _sections_from_storyline(storyline: str) -> list[dict[str, str]]:
    return [{"section_id": "section_001", "intent": storyline, "notes": storyline}]


def _fallback_cut_plan(
    case_state: CaseState,
    assets: list[_AssetSummary],
    target_duration: float,
    slot_count: int | None,
) -> dict[str, Any]:
    del case_state
    if assets:
        briefs = [_summary_text(asset) for asset in assets]
    else:
        briefs = ["根据现有信息选择合适画面"] * max(1, slot_count or 1)
    share = max(0.1, target_duration / max(1, len(briefs)))
    slots = [
        {
            "slot_id": f"slot_{index:03d}",
            "brief": _trim(brief, 160),
            "target_duration_sec": _duration_window(share),
        }
        for index, brief in enumerate(briefs, start=1)
    ]
    return _validated_cut_plan(slots, target_duration)


def _validated_cut_plan(
    slots: Sequence[Mapping[str, Any]],
    total_duration: float,
) -> dict[str, Any]:
    payload = {
        "schema": "CutPlan.v1",
        "slots": [dict(slot) for slot in slots],
        "removed_ranges": [],
        "total_target_duration_sec": max(0.1, round(float(total_duration), 3)),
    }
    return CutPlan.model_validate(payload).model_dump(mode="json", by_alias=True)


def _asset_summaries(context: ToolExecutionContext) -> list[_AssetSummary]:
    assert context.case_state is not None
    assert context.readonly_connection is not None
    case_state = context.case_state
    join_clause = schema.assets.join(
        schema.project_asset_links,
        schema.project_asset_links.c.asset_id == schema.assets.c.asset_id,
    ).outerjoin(
        schema.annotation_clip_projection,
        and_(
            schema.annotation_clip_projection.c.asset_id == schema.assets.c.asset_id,
            schema.annotation_clip_projection.c.usable.is_(True),
        ),
    )
    statement = (
        select(
            schema.assets.c.asset_id,
            schema.assets.c.filename,
            schema.annotation_clip_projection.c.summary,
            schema.annotation_clip_projection.c.quality_score,
        )
        .select_from(join_clause)
        .where(schema.project_asset_links.c.project_id == case_state.project_id)
        .where(schema.project_asset_links.c.enabled.is_(True))
        .where(schema.assets.c.usable.is_(True))
        .order_by(
            schema.assets.c.asset_id,
            schema.annotation_clip_projection.c.quality_score.desc(),
            schema.annotation_clip_projection.c.clip_id,
        )
    )
    if case_state.selected_asset_ids:
        statement = statement.where(schema.assets.c.asset_id.in_(case_state.selected_asset_ids))
    elif case_state.disabled_asset_ids:
        statement = statement.where(schema.assets.c.asset_id.not_in(case_state.disabled_asset_ids))
    rows = context.readonly_connection.execute(statement).all()
    assets: dict[str, _AssetSummary] = {}
    for row in rows:
        values = row._mapping
        asset_id = str(values["asset_id"])
        if asset_id in assets:
            continue
        summary_value = values.get("summary")
        filename = str(values.get("filename") or asset_id)
        assets[asset_id] = _AssetSummary(
            asset_id=asset_id,
            filename=filename,
            summary=str(summary_value).strip() if isinstance(summary_value, str) else "",
            quality_score=(
                float(values["quality_score"]) if values.get("quality_score") is not None else None
            ),
        )
    return list(assets.values())


def _asset_payload(asset: _AssetSummary) -> dict[str, Any]:
    return {
        "asset_id": asset.asset_id,
        "filename": asset.filename,
        "summary": asset.summary,
        "quality_score": asset.quality_score,
    }


def _target_duration(requested: float | None, case_state: CaseState) -> float:
    value = requested
    if value is None:
        value = case_state.brief.target_duration_sec
    if value is None and case_state.cut_plan is not None:
        value = case_state.cut_plan.total_target_duration_sec
    if value is None:
        value = DEFAULT_TARGET_DURATION_SEC
    return max(0.1, float(value))


def _duration_window_from_value(value: Any) -> tuple[float, float] | None:
    if isinstance(value, int | float):
        return _duration_window(float(value))
    if not isinstance(value, Sequence) or isinstance(value, str | bytes) or len(value) != 2:
        return None
    try:
        start = float(value[0])
        end = float(value[1])
    except (TypeError, ValueError):
        return None
    if start >= end:
        return None
    return (round(max(0.0, start), 3), round(end, 3))


def _duration_window(duration_sec: float) -> tuple[float, float]:
    low = max(0.1, duration_sec * 0.9)
    high = max(low + 0.1, duration_sec * 1.1)
    return (round(low, 3), round(high, 3))


def _should_emit_cut_plan(case_state: CaseState) -> bool:
    return case_state.audio_plan is not None and str(case_state.audio_plan.mode) == "silent"


def _observation(plan: _PlanResult) -> str:
    slots = plan.cut_plan.get("slots")
    slot_count = len(slots) if isinstance(slots, list) else 0
    total = plan.cut_plan.get("total_target_duration_sec")
    total_text = f"{float(total):.1f}s" if isinstance(total, int | float) else "未知时长"
    storyline = _first_text(plan.content_plan, ("storyline",)) or "已生成内容计划"
    return (
        f"已生成内容计划：{slot_count} 个镜头槽，总目标 {total_text}。"
        f"故事线：{_trim(storyline, 80)}"
    )


def _summary_text(asset: _AssetSummary) -> str:
    summary = asset.summary.strip()
    if summary:
        return summary
    return asset.filename or asset.asset_id


def _first_text(values: Mapping[str, Any], keys: tuple[str, ...]) -> str | None:
    for key in keys:
        value = values.get(key)
        if isinstance(value, str) and value.strip():
            return value.strip()
        if isinstance(value, Sequence) and not isinstance(value, str | bytes):
            parts = [str(item).strip() for item in value if str(item).strip()]
            if parts:
                return "；".join(parts)
    return None


def _trim(value: str, limit: int) -> str:
    text = value.strip()
    if len(text) <= limit:
        return text
    return text[: limit - 3] + "..."


def _run_async_sync(awaitable: Any) -> Any:
    try:
        asyncio.get_running_loop()
    except RuntimeError:
        return asyncio.run(awaitable)

    result: dict[str, Any] = {}

    def runner() -> None:
        try:
            result["value"] = asyncio.run(awaitable)
        except BaseException as exc:  # pragma: no cover - defensive thread bridge
            result["error"] = exc

    thread = threading.Thread(target=runner, daemon=True)
    thread.start()
    thread.join()
    if "error" in result:
        raise result["error"]
    return result["value"]


def _failed(
    tool_name: str,
    context: ToolExecutionContext,
    error_code: str,
    message: str,
) -> ToolResult:
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name=tool_name,
        status="failed",
        observation=message,
        error=ToolError(error_code=error_code, message=message, retryable=False),
    )
