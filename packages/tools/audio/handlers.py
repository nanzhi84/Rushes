"""Audio inspection and ASR queueing tool handlers."""

from __future__ import annotations

import asyncio
import hashlib
import json
import threading
from collections.abc import Callable
from dataclasses import dataclass
from importlib import import_module
from pathlib import Path
from typing import Any, cast

from sqlalchemy import select

from contracts.decision import Decision, DecisionOption
from contracts.events import AssetProbed, CapabilityDegraded, DecisionCreated, JobEnqueued
from contracts.provider import ProviderResult
from contracts.tool_result import ToolError, ToolResult
from contracts.transcript import TranscriptDocument
from media.rough_cut import (
    removed_ranges_from_proposals,
    rule_based_proposals,
    semantic_proposals,
    utterance_prompt_rows,
)
from providers import LLM_CHAT, ProviderRequest
from storage import schema
from storage.repositories._json import load_json
from storage.workspace_paths import WorkspacePaths, resolve_asset_path
from tools.context import ToolExecutionContext
from tools.specs import (
    AudioAlignUploadedVoiceoverInput,
    AudioAsrOriginalInput,
    AudioGenerateTtsInput,
    AudioInspectSourcesInput,
    AudioRoughCutSpeechInput,
)


def inspect_sources(
    input_model: AudioInspectSourcesInput,
    context: ToolExecutionContext,
) -> ToolResult:
    draft_state = context.draft_state
    if draft_state is None:
        return _failed("audio.inspect_sources", context, "missing_draft", "active draft required")
    if context.readonly_connection is None:
        return _failed(
            "audio.inspect_sources",
            context,
            "missing_connection",
            "audio.inspect_sources requires repository access",
        )
    paths = _workspace_paths(context)
    asset_ids = _draft_asset_ids(context, requested_ids=input_model.asset_ids)
    rows = _asset_rows(context, asset_ids)
    sources: list[dict[str, Any]] = []
    degraded: list[dict[str, Any]] = []
    events: list[dict[str, Any]] = []
    silero_degraded = False
    for row in rows:
        asset_id = str(row["asset_id"])
        source = _inspect_one_asset(asset_id, paths=paths, context=context)
        sources.append(source)
        probe_payload = source.get("probe")
        if isinstance(probe_payload, dict):
            events.append(
                AssetProbed(
                    # 素材是全局实体，探测事件不挂草稿作用域。
                    asset_id=asset_id,
                    payload={"probe": probe_payload, "ingest_status": "probed"},
                ).model_dump(mode="json")
            )
        if source.get("degraded_reason") == "silero_model_missing" and not silero_degraded:
            silero_degraded = True
            degradation = {
                "capability": "audio.vad",
                "provider_id": "silero_onnx",
                "reason": str(source.get("degraded_message") or "Silero model missing"),
                "asset_id": asset_id,
            }
            degraded.append(degradation)
            events.append(
                CapabilityDegraded(
                    degradation_id=f"degraded_{context.tool_call_id}_silero_vad",
                    draft_id=draft_state.draft_id,
                    capability="audio.vad",
                    provider_id="silero_onnx",
                    reason=degradation["reason"],
                    payload={"asset_id": asset_id, "tool_call_id": context.tool_call_id},
                ).model_dump(mode="json")
            )
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name="audio.inspect_sources",
        status="succeeded",
        observation=_inspect_sources_observation(sources, degraded),
        data={"draft_id": draft_state.draft_id, "sources": sources, "degraded": degraded},
        events=events,
    )


def _inspect_sources_observation(
    sources: list[dict[str, Any]],
    degraded: list[dict[str, Any]],
) -> str:
    # LLM 只读 observation 不读 data：探测结论必须完整落在这里，否则模型
    # 拿不到"有无原声"的依据会反复重调本工具（M9 路径 1 实测卡死点）。
    if not sources:
        return "未找到可检查的素材。"
    lines: list[str] = []
    for source in sources:
        asset_id = source.get("asset_id")
        if source.get("error"):
            lines.append(f"{asset_id}: 探测失败（{source['error']}）")
            continue
        duration = source.get("duration_sec")
        duration_text = f"{duration:.1f}s" if isinstance(duration, int | float) else "未知时长"
        if not source.get("has_audio"):
            lines.append(f"{asset_id}: 无音轨，{duration_text}")
            continue
        speech_ratio = source.get("speech_ratio")
        speech_text = (
            f"语音占比 {speech_ratio:.0%}"
            if isinstance(speech_ratio, int | float)
            else "语音占比未知"
        )
        segments = source.get("vad_segments")
        segment_count = len(segments) if isinstance(segments, list) else 0
        lines.append(
            f"{asset_id}: 有音轨，{duration_text}，{speech_text}，语音片段 {segment_count} 段"
        )
    if degraded:
        reasons = "；".join(str(item.get("reason", "")) for item in degraded)
        lines.append(f"能力降级：{reasons}")
    return "音频源检查结果：" + "；".join(lines)


def asr_original(input_model: AudioAsrOriginalInput, context: ToolExecutionContext) -> ToolResult:
    draft_state = context.draft_state
    if draft_state is None:
        return _failed("audio.asr_original", context, "missing_draft", "active draft required")
    if draft_state.audio_plan is None:
        return _failed(
            "audio.asr_original",
            context,
            "audio_plan_missing",
            "audio.asr_original requires a confirmed audio_plan",
        )
    if str(draft_state.audio_plan.mode) not in {"keep_original", "rough_cut"}:
        return _failed(
            "audio.asr_original",
            context,
            "audio_mode_not_supported",
            "audio.asr_original only supports keep_original or rough_cut",
        )
    asset_id = _asr_asset_id(input_model, context)
    if asset_id is None:
        return _failed(
            "audio.asr_original",
            context,
            "missing_audio_asset",
            "audio.asr_original requires an audio source asset",
        )
    # 幂等短路：转写已存在时直接报告结果，不再重复排 job——否则 agent
    # 只能拿到"job queued"这种无信息 observation，无法推进（M9 实测）
    existing = None
    if context.readonly_connection is not None:
        existing = context.readonly_connection.execute(
            select(schema.transcripts)
            .where(schema.transcripts.c.asset_id == asset_id)
            .order_by(schema.transcripts.c.transcript_id.desc())
            .limit(1)
        ).first()
    if existing is not None:
        values = dict(existing._mapping)
        utterances = load_json(str(values["utterances"]))
        count = len(utterances) if isinstance(utterances, list) else 0
        return ToolResult(
            tool_call_id=context.tool_call_id,
            tool_name="audio.asr_original",
            status="succeeded",
            observation=(
                f"ASR 已完成：transcript {values['transcript_id']}，共 {count} 句转写。"
                "下一步可调用 audio.rough_cut_speech 生成粗剪提案。"
            ),
            data={
                "draft_id": draft_state.draft_id,
                "asset_id": asset_id,
                "transcript_id": values["transcript_id"],
                "utterance_count": count,
            },
        )

    idempotency_key = f"draft:{draft_state.draft_id}:asr:{asset_id}"
    event = JobEnqueued(
        job_id=_job_id("asr", idempotency_key),
        draft_id=draft_state.draft_id,
        requested_by_draft_id=draft_state.draft_id,
        payload={
            "kind": "asr",
            "asset_id": asset_id,
            "idempotency_key": idempotency_key,
            "job_payload": {
                "asset_id": asset_id,
                "draft_id": draft_state.draft_id,
                "provider_id": input_model.provider_id,
            },
            "attempts": 0,
            "max_retries": 2,
        },
    )
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name="audio.asr_original",
        status="running",
        observation=f"asr job queued: {event.job_id}",
        data={"draft_id": draft_state.draft_id, "asset_id": asset_id, "job_id": event.job_id},
        events=[event.model_dump(mode="json")],
    )


def rough_cut_speech(
    input_model: AudioRoughCutSpeechInput,
    context: ToolExecutionContext,
) -> ToolResult:
    draft_state = context.draft_state
    if draft_state is None:
        return _failed("audio.rough_cut_speech", context, "missing_draft", "active draft required")
    if context.readonly_connection is None:
        return _failed(
            "audio.rough_cut_speech",
            context,
            "missing_connection",
            "audio.rough_cut_speech requires repository access",
        )
    transcript = _rough_cut_transcript(input_model, context)
    if transcript is None:
        return _failed(
            "audio.rough_cut_speech",
            context,
            "transcript_missing",
            "rough-cut speech requires a TranscriptDocument with VAD segments",
        )

    degraded_events: list[dict[str, Any]] = []
    include_fillers = transcript.raw_preserved
    if not transcript.raw_preserved:
        degraded_events.append(
            CapabilityDegraded(
                degradation_id=f"degraded_{context.tool_call_id}_raw_transcript",
                draft_id=draft_state.draft_id,
                capability="audio.rough_cut_speech.filler_detection",
                provider_id=transcript.provider_id,
                reason="raw transcript is not preserved; filler/off-topic detection disabled",
                payload={
                    "transcript_id": transcript.transcript_id,
                    "tool_call_id": context.tool_call_id,
                },
            ).model_dump(mode="json")
        )
    proposals = rule_based_proposals(
        transcript,
        filler_words=set(input_model.filler_words) if input_model.filler_words else None,
        pause_threshold_ms=input_model.pause_threshold_ms,
        repeat_similarity_threshold=input_model.repeat_similarity_threshold,
        include_fillers=include_fillers,
    )
    provider_events: tuple[dict[str, Any], ...] = ()
    if transcript.raw_preserved:
        llm_result = _semantic_suggestions(input_model, context, transcript)
        provider_events = llm_result.events
        if llm_result.degraded_reason is not None:
            degraded_events.append(
                CapabilityDegraded(
                    degradation_id=f"degraded_{context.tool_call_id}_rough_cut_llm",
                    draft_id=draft_state.draft_id,
                    capability="audio.rough_cut_speech.semantic",
                    provider_id=input_model.llm_provider_id,
                    reason=llm_result.degraded_reason,
                    payload={
                        "transcript_id": transcript.transcript_id,
                        "tool_call_id": context.tool_call_id,
                    },
                ).model_dump(mode="json")
            )
        else:
            proposals.extend(semantic_proposals(transcript, llm_result.suggestions))

    proposals = sorted(
        {proposal.model_dump_json(): proposal for proposal in proposals}.values(),
        key=lambda proposal: (proposal.range_ms.start_ms, proposal.range_ms.end_ms, proposal.kind),
    )
    proposal_payload = [proposal.model_dump(mode="json") for proposal in proposals]
    removed_ranges = removed_ranges_from_proposals(proposals)
    decision = Decision(
        decision_id=_decision_id("speech_cut", draft_state.draft_id, proposal_payload),
        scope_type="draft",
        draft_id=draft_state.draft_id,
        type="approve_speech_cut",
        question="这些口播粗剪候选需要确认后才会删除，是否应用？",
        options=[
            DecisionOption(
                option_id="apply_all",
                label="应用全部候选",
                payload={
                    "rough_cut_proposal": proposal_payload,
                    "removed_ranges": removed_ranges,
                    "total_target_duration_sec": _cut_plan_total_duration(draft_state),
                },
            ),
            DecisionOption(
                option_id="skip",
                label="暂不删除",
                payload={
                    "rough_cut_proposal": proposal_payload,
                    "removed_ranges": [],
                    "total_target_duration_sec": _cut_plan_total_duration(draft_state),
                },
            ),
        ],
        allow_free_text=True,
        status="pending",
        blocking=True,
        created_by_tool_call_id=context.tool_call_id,
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
            "blocking": decision.blocking,
            "created_by_tool_call_id": context.tool_call_id,
        },
    )
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name="audio.rough_cut_speech",
        status="requires_user",
        observation="rough-cut speech proposals require approval",
        data={
            "draft_id": draft_state.draft_id,
            "transcript_id": transcript.transcript_id,
            "rough_cut_proposal": proposal_payload,
            "decision_id": decision.decision_id,
            "degraded": [event["payload"] for event in degraded_events],
        },
        events=[*provider_events, *degraded_events, event.model_dump(mode="json")],
    )


def generate_tts(
    input_model: AudioGenerateTtsInput,
    context: ToolExecutionContext,
) -> ToolResult:
    return _enqueue_audio_job(
        "audio.generate_tts",
        "tts",
        context,
        {
            "provider_id": input_model.provider_id,
            "asr_provider_id": input_model.asr_provider_id,
            "voice_type": input_model.voice_type,
        },
    )


def align_uploaded_voiceover(
    input_model: AudioAlignUploadedVoiceoverInput,
    context: ToolExecutionContext,
) -> ToolResult:
    return _enqueue_audio_job(
        "audio.align_uploaded_voiceover",
        "align",
        context,
        {
            "script_text": input_model.script_text,
            "asset_id": input_model.asset_id,
            "provider_id": input_model.provider_id,
        },
    )


@dataclass(frozen=True, slots=True)
class _SemanticSuggestionResult:
    suggestions: list[dict[str, Any]]
    events: tuple[dict[str, Any], ...] = ()
    degraded_reason: str | None = None


def _rough_cut_transcript(
    input_model: AudioRoughCutSpeechInput,
    context: ToolExecutionContext,
) -> TranscriptDocument | None:
    assert context.readonly_connection is not None
    draft_state = context.draft_state
    if draft_state is None:
        return None
    transcript_id = input_model.transcript_id
    if transcript_id is None and draft_state.audio_plan is not None:
        transcript_id = draft_state.audio_plan.transcript_id
    row = None
    if transcript_id is not None:
        row = context.readonly_connection.execute(
            select(schema.transcripts).where(schema.transcripts.c.transcript_id == transcript_id)
        ).first()
    if row is None:
        asset_id = input_model.asset_id
        if asset_id is None and draft_state.audio_plan is not None:
            source_ids = draft_state.audio_plan.source_asset_ids
            asset_id = source_ids[0] if source_ids else None
        if asset_id is not None:
            row = context.readonly_connection.execute(
                select(schema.transcripts)
                .where(schema.transcripts.c.asset_id == asset_id)
                .order_by(schema.transcripts.c.transcript_id.desc())
                .limit(1)
            ).first()
    if row is None:
        return None
    values = dict(row._mapping)
    utterances = load_json(str(values["utterances"]))
    vad_segments = load_json(str(values["vad_segments"]))
    if not isinstance(vad_segments, list) or not vad_segments:
        return None
    return TranscriptDocument.model_validate(
        {
            "schema": "TranscriptDocument.v1",
            "transcript_id": values["transcript_id"],
            "asset_id": values["asset_id"],
            "language": "und",
            "provider_id": values["provider_id"],
            "raw_preserved": values["raw_preserved"],
            "utterances": utterances,
            "vad_segments": vad_segments,
            "warnings": [],
        }
    )


def _semantic_suggestions(
    input_model: AudioRoughCutSpeechInput,
    context: ToolExecutionContext,
    transcript: TranscriptDocument,
) -> _SemanticSuggestionResult:
    gateway = context.metadata.get("provider_gateway") or context.metadata.get("llm_gateway")
    if gateway is None or not hasattr(gateway, "call"):
        return _SemanticSuggestionResult(
            suggestions=[],
            degraded_reason="llm gateway is not configured",
        )
    request = ProviderRequest(
        capability=LLM_CHAT,
        request_id=f"rough_cut_{context.tool_call_id}",
        payload={
            "messages": [
                {
                    "role": "system",
                    "content": (
                        "你是口播粗剪审核器。只允许输出 JSON，格式为 "
                        '{"suggestions":[{"utterance_id":"...",'
                        '"reason":"...","confidence":0.0}]}。'
                        "只能引用输入里的 utterance_id，不要输出毫秒。"
                    ),
                },
                {
                    "role": "user",
                    "content": json.dumps(
                        {
                            "utterances": utterance_prompt_rows(transcript),
                            "task": "找出废句、离题句或应删除的语义重复句。",
                        },
                        ensure_ascii=False,
                    ),
                },
            ],
            "tool_choice": {
                "type": "function",
                "function": {"name": "propose_speech_cuts"},
            },
            "tools": [
                {
                    "type": "function",
                    "function": {
                        "name": "propose_speech_cuts",
                        "description": "Return utterance_id level semantic deletion candidates.",
                        "parameters": {
                            "type": "object",
                            "additionalProperties": False,
                            "properties": {
                                "suggestions": {
                                    "type": "array",
                                    "items": {
                                        "type": "object",
                                        "additionalProperties": False,
                                        "properties": {
                                            "utterance_id": {"type": "string"},
                                            "reason": {"type": "string"},
                                            "confidence": {"type": "number"},
                                        },
                                        "required": [
                                            "utterance_id",
                                            "reason",
                                            "confidence",
                                        ],
                                    },
                                }
                            },
                            "required": ["suggestions"],
                        },
                        "strict": True,
                    },
                }
            ],
        },
    )
    try:
        gateway_result = _run_async_sync(
            gateway.call(request, provider_id=input_model.llm_provider_id)
        )
    except Exception as exc:
        return _SemanticSuggestionResult(
            suggestions=[],
            degraded_reason=f"llm call failed: {exc}",
        )
    events = tuple(dict(event) for event in getattr(gateway_result, "events", ()) or ())
    result = cast(ProviderResult, gateway_result.result)
    if result.error is not None:
        return _SemanticSuggestionResult(
            suggestions=[],
            events=events,
            degraded_reason=f"{result.error.error_code}: {result.error.message}",
        )
    suggestions = _parse_semantic_suggestions(result.normalized_output)
    return _SemanticSuggestionResult(suggestions=suggestions, events=events)


def _parse_semantic_suggestions(output: dict[str, Any]) -> list[dict[str, Any]]:
    candidates: list[Any] = []
    if isinstance(output.get("suggestions"), list):
        candidates = cast(list[Any], output["suggestions"])
    tool_call = output.get("tool_call")
    if not candidates and isinstance(tool_call, dict):
        candidates = _suggestions_from_arguments(tool_call)
    tool_calls = output.get("tool_calls")
    if not candidates and isinstance(tool_calls, list):
        for item in tool_calls:
            if isinstance(item, dict):
                candidates = _suggestions_from_arguments(item)
                if candidates:
                    break
    content = output.get("content")
    if not candidates and isinstance(content, str):
        candidates = _suggestions_from_arguments(content)
    return [dict(item) for item in candidates if isinstance(item, dict)]


def _suggestions_from_arguments(value: Any) -> list[Any]:
    raw = value
    if isinstance(value, dict):
        function = value.get("function")
        if isinstance(function, dict) and "arguments" in function:
            raw = function["arguments"]
        else:
            suggestions = value.get("suggestions")
            return suggestions if isinstance(suggestions, list) else []
    if isinstance(raw, str):
        try:
            parsed = json.loads(raw)
        except json.JSONDecodeError:
            return []
        if isinstance(parsed, dict):
            suggestions = parsed.get("suggestions")
            return suggestions if isinstance(suggestions, list) else []
    return []


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


def _enqueue_audio_job(
    tool_name: str,
    kind: str,
    context: ToolExecutionContext,
    arguments: dict[str, Any],
) -> ToolResult:
    draft_state = context.draft_state
    if draft_state is None:
        return _failed(tool_name, context, "missing_draft", "active draft required")
    clean_arguments = {key: value for key, value in arguments.items() if value is not None}
    idempotency_key = (
        f"draft:{draft_state.draft_id}:{kind}:"
        f"{hashlib.sha256(json.dumps(clean_arguments, sort_keys=True).encode()).hexdigest()}"
    )
    event = JobEnqueued(
        job_id=_job_id(kind, idempotency_key),
        draft_id=draft_state.draft_id,
        requested_by_draft_id=draft_state.draft_id,
        payload={
            "kind": kind,
            "idempotency_key": idempotency_key,
            "job_payload": {
                "tool_name": tool_name,
                "arguments": clean_arguments,
                "tool_call_id": context.tool_call_id,
                "turn_id": context.turn_id,
            },
            "attempts": 0,
            "max_retries": 2,
        },
    )
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name=tool_name,
        status="running",
        observation=f"job queued: {event.job_id}",
        data={"draft_id": draft_state.draft_id, "job_id": event.job_id, "job_kind": kind},
        events=[event.model_dump(mode="json")],
    )


def _decision_id(prefix: str, draft_id: str, payload: Any) -> str:
    raw = json.dumps({"draft_id": draft_id, "payload": payload}, sort_keys=True)
    digest = hashlib.sha256(raw.encode()).hexdigest()
    return f"dec_{prefix}_{digest[:16]}"


def _cut_plan_total_duration(draft_state: Any) -> float:
    cut_plan = getattr(draft_state, "cut_plan", None)
    if cut_plan is not None:
        value = getattr(cut_plan, "total_target_duration_sec", None)
        if isinstance(value, (int, float)):
            return float(value)
    return 0.0


def _inspect_one_asset(
    asset_id: str,
    *,
    paths: WorkspacePaths,
    context: ToolExecutionContext,
) -> dict[str, Any]:
    assert context.readonly_connection is not None
    try:
        source_path = resolve_asset_path(
            asset_id,
            connection=context.readonly_connection,
            paths=paths,
        )
        probe_media = _media_callable("media.probe", "probe_media")
        probe = probe_media(source_path)
        payload: dict[str, Any] = {
            "asset_id": asset_id,
            "path": str(source_path),
            "has_audio": bool(probe.has_audio),
            "duration_sec": probe.duration_sec,
            "probe": probe.model_dump(mode="json"),
            "speech_ratio": None,
            "vad_segments": [],
        }
        if not probe.has_audio:
            return payload
        extract_audio_to_wav = _media_callable("media.audio_extract", "extract_audio_to_wav")
        run_silero_vad = _media_callable("media.vad", "run_silero_vad")
        silero_missing = _media_attr("media.vad", "SileroModelMissing")
        extracted = extract_audio_to_wav(source_path, paths=paths)
        try:
            vad = run_silero_vad(extracted.path, paths=paths)
        except silero_missing as exc:
            payload["degraded_reason"] = "silero_model_missing"
            payload["degraded_message"] = str(exc)
            return payload
        payload["speech_ratio"] = vad.speech_ratio
        payload["vad_segments"] = [segment.model_dump(mode="json") for segment in vad.segments]
        return payload
    except Exception as exc:
        return {
            "asset_id": asset_id,
            "has_audio": False,
            "duration_sec": None,
            "speech_ratio": None,
            "vad_segments": [],
            "error": {"error_code": "audio_inspect_failed", "message": str(exc)},
        }


def _draft_asset_ids(
    context: ToolExecutionContext,
    *,
    requested_ids: list[str],
) -> list[str]:
    draft_state = context.draft_state
    if draft_state is None:
        return []
    if requested_ids:
        return _dedupe(requested_ids)
    if context.readonly_connection is None:
        return []
    rows = context.readonly_connection.execute(
        select(schema.draft_asset_links.c.asset_id)
        .where(schema.draft_asset_links.c.draft_id == draft_state.draft_id)
        .order_by(schema.draft_asset_links.c.asset_id)
    ).all()
    return [str(row._mapping["asset_id"]) for row in rows]


def _asset_rows(context: ToolExecutionContext, asset_ids: list[str]) -> list[dict[str, Any]]:
    if context.readonly_connection is None or not asset_ids:
        return []
    rows = context.readonly_connection.execute(
        select(schema.assets)
        .where(schema.assets.c.asset_id.in_(asset_ids))
        .where(schema.assets.c.usable.is_(True))
        .order_by(schema.assets.c.asset_id)
    ).all()
    order = {asset_id: index for index, asset_id in enumerate(asset_ids)}
    values = [dict(row._mapping) for row in rows]
    return sorted(values, key=lambda row: order.get(str(row["asset_id"]), 10**9))


def _asr_asset_id(input_model: AudioAsrOriginalInput, context: ToolExecutionContext) -> str | None:
    if input_model.asset_id:
        return input_model.asset_id
    draft_state = context.draft_state
    if draft_state is None:
        return None
    if draft_state.audio_plan is not None and draft_state.audio_plan.source_asset_ids:
        return draft_state.audio_plan.source_asset_ids[0]
    linked = _draft_asset_ids(context, requested_ids=[])
    return linked[0] if linked else None


def _workspace_paths(context: ToolExecutionContext) -> WorkspacePaths:
    raw_paths = context.metadata.get("workspace_paths")
    if isinstance(raw_paths, WorkspacePaths):
        return raw_paths.initialize()
    raw_root = context.metadata.get("workspace_path")
    if isinstance(raw_root, str | Path):
        return WorkspacePaths.from_root(raw_root).initialize()
    raise ValueError("audio tool requires workspace_paths metadata")


def _media_callable(module_name: str, attr: str) -> Callable[..., Any]:
    value = getattr(import_module(module_name), attr)
    return cast(Callable[..., Any], value)


def _media_attr(module_name: str, attr: str) -> type[BaseException]:
    value = getattr(import_module(module_name), attr)
    return cast(type[BaseException], value)


def _dedupe(values: list[str]) -> list[str]:
    return list(dict.fromkeys(values))


def _job_id(kind: str, idempotency_key: str) -> str:
    digest = hashlib.sha256(f"{kind}:{idempotency_key}".encode()).hexdigest()
    return f"job_{digest[:20]}"


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
