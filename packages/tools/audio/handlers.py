"""Audio inspection and ASR queueing tool handlers."""

from __future__ import annotations

import hashlib
from collections.abc import Callable
from importlib import import_module
from pathlib import Path
from typing import Any, cast

from sqlalchemy import select

from contracts.events import AssetProbed, CapabilityDegraded, JobEnqueued
from contracts.tool_result import ToolError, ToolResult
from storage import schema
from storage.workspace_paths import WorkspacePaths, resolve_asset_path
from tools.context import ToolExecutionContext
from tools.specs import AudioAsrOriginalInput, AudioInspectSourcesInput


def inspect_sources(
    input_model: AudioInspectSourcesInput,
    context: ToolExecutionContext,
) -> ToolResult:
    case_state = context.case_state
    if case_state is None:
        return _failed("audio.inspect_sources", context, "missing_case", "active case required")
    if context.readonly_connection is None:
        return _failed(
            "audio.inspect_sources",
            context,
            "missing_connection",
            "audio.inspect_sources requires repository access",
        )
    paths = _workspace_paths(context)
    asset_ids = _case_asset_ids(context, requested_ids=input_model.asset_ids)
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
                    project_id=case_state.project_id,
                    case_id=case_state.case_id,
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
                    project_id=case_state.project_id,
                    case_id=case_state.case_id,
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
        observation="inspected audio sources",
        data={"case_id": case_state.case_id, "sources": sources, "degraded": degraded},
        events=events,
    )


def asr_original(input_model: AudioAsrOriginalInput, context: ToolExecutionContext) -> ToolResult:
    case_state = context.case_state
    if case_state is None:
        return _failed("audio.asr_original", context, "missing_case", "active case required")
    if case_state.audio_plan is None:
        return _failed(
            "audio.asr_original",
            context,
            "audio_plan_missing",
            "audio.asr_original requires a confirmed audio_plan",
        )
    if str(case_state.audio_plan.mode) not in {"keep_original", "rough_cut"}:
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
    idempotency_key = f"case:{case_state.case_id}:asr:{asset_id}"
    event = JobEnqueued(
        job_id=_job_id("asr", idempotency_key),
        project_id=case_state.project_id,
        case_id=case_state.case_id,
        requested_by_case_id=case_state.case_id,
        payload={
            "kind": "asr",
            "asset_id": asset_id,
            "idempotency_key": idempotency_key,
            "job_payload": {
                "asset_id": asset_id,
                "case_id": case_state.case_id,
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
        data={"case_id": case_state.case_id, "asset_id": asset_id, "job_id": event.job_id},
        events=[event.model_dump(mode="json")],
    )


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


def _case_asset_ids(
    context: ToolExecutionContext,
    *,
    requested_ids: list[str],
) -> list[str]:
    case_state = context.case_state
    if case_state is None:
        return []
    if requested_ids:
        return _dedupe(requested_ids)
    if case_state.selected_asset_ids:
        return _dedupe(case_state.selected_asset_ids)
    if context.readonly_connection is None:
        return []
    rows = context.readonly_connection.execute(
        select(schema.project_asset_links.c.asset_id)
        .where(schema.project_asset_links.c.project_id == case_state.project_id)
        .where(schema.project_asset_links.c.enabled.is_(True))
        .order_by(schema.project_asset_links.c.asset_id)
    ).all()
    disabled = set(case_state.disabled_asset_ids)
    return [
        str(row._mapping["asset_id"]) for row in rows if row._mapping["asset_id"] not in disabled
    ]


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
    case_state = context.case_state
    if case_state is None:
        return None
    if case_state.audio_plan is not None and case_state.audio_plan.source_asset_ids:
        return case_state.audio_plan.source_asset_ids[0]
    if case_state.selected_asset_ids:
        return case_state.selected_asset_ids[0]
    return None


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
