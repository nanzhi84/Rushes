"""understand.materials（理解子代理派发）与 asset.read_summary 工具（Spec C §C3）。

``understand.materials`` 是 async handler：turn 内为每个 asset 起一个理解子代理，
``asyncio.gather`` 并发（``Semaphore`` 上限、单素材 ``wait_for`` 超时），把每个 asset 的
摘要全文或失败原因回灌主代理。缓存命中（已有 ready 摘要且无新 focus）直接返回、不起子代理。

handler 只有只读连接：产出的 material_summaries / transcripts 行通过
``ToolResult.data`` 的 ``material_summary_rows`` / ``transcript_rows`` 键交给 loop 的
``_persist_tool_result_data`` 落库；理解状态经 ``ToolResult.events``
（MaterialUnderstandingStarted/Completed/Failed）走 reducer。
"""

from __future__ import annotations

import asyncio
import os
from collections.abc import Callable, Mapping
from dataclasses import dataclass
from datetime import UTC, datetime
from pathlib import Path
from typing import Any
from uuid import uuid4

from sqlalchemy import select
from sqlalchemy.engine import Connection

from contracts.events import (
    DomainEventBase,
    MaterialUnderstandingCompleted,
    MaterialUnderstandingFailed,
    MaterialUnderstandingStarted,
)
from contracts.tool_result import ToolError, ToolResult
from contracts.transcript import TranscriptDocument
from providers import (
    VLM_ANNOTATION,
    ProviderGateway,
    ProviderRegistry,
    ProviderRequest,
)
from providers.aliyun import AliyunParaformerASRProvider, aliyun_paraformer_asr_descriptor
from providers.openai_compatible.vlm import DEFAULT_OPENAI_COMPATIBLE_VLM_MODEL
from storage import schema
from storage.repositories import MaterialSummariesRepository
from storage.repositories._json import load_json
from storage.workspace_paths import WorkspacePaths, resolve_asset_path
from tools.context import ToolExecutionContext
from tools.media_tools import extract_frame_data_uri
from tools.specs import AssetReadSummaryInput, UnderstandMaterialsInput

from .asr import transcribe_to_document
from .subagent import (
    SubagentOutcome,
    SubagentSpec,
    TranscribeResult,
    run_understanding_subagent,
)

DEFAULT_CONCURRENCY = 3
DEFAULT_TIMEOUT_S = 300.0


@dataclass(frozen=True, slots=True)
class _AssetInfo:
    asset_id: str
    filename: str
    kind: str
    duration_sec: float | None
    index_json: dict[str, Any] | None
    has_audio: bool
    path: Path | None


async def materials(
    input_model: UnderstandMaterialsInput, context: ToolExecutionContext
) -> ToolResult:
    tool_name = "understand.materials"
    connection = context.readonly_connection
    if connection is None:
        return _failed(tool_name, context, "missing_connection", "需要只读仓库连接")

    project_id = _project_id(context)
    focus = (input_model.focus or "").strip() or None
    asset_ids = list(dict.fromkeys(input_model.asset_ids))
    gateway = _provider_gateway(context)
    progress = _progress(context)
    model = _vlm_model()

    summaries_repo = MaterialSummariesRepository(connection)
    latest = summaries_repo.list_latest_for_assets(asset_ids)
    asset_info = _load_asset_info(connection, asset_ids, context)

    specs: dict[str, SubagentSpec] = {}
    cached: dict[str, dict[str, Any]] = {}
    missing: list[str] = []
    for asset_id in asset_ids:
        info = asset_info.get(asset_id)
        if info is None:
            missing.append(asset_id)
            continue
        prior = latest.get(asset_id)
        if prior is not None and focus is None:
            cached[asset_id] = dict(prior["summary_json"])
            continue
        version = int(prior["version"]) + 1 if prior is not None else 1
        specs[asset_id] = _make_spec(
            context,
            info,
            gateway=gateway,
            focus=focus,
            version=version,
            prior_summary=dict(prior["summary_json"]) if prior is not None else None,
            model=model,
            progress=progress,
        )

    if gateway is None:
        outcomes = {
            asset_id: SubagentOutcome(
                asset_id=asset_id,
                status="failed",
                failure_reason="VLM 通道不可用，无法理解素材",
            )
            for asset_id in specs
        }
    else:
        outcomes = await _run_subagents(specs)

    return _assemble_result(
        tool_name,
        context,
        asset_ids=asset_ids,
        asset_info=asset_info,
        project_id=project_id,
        focus=focus,
        cached=cached,
        missing=missing,
        outcomes=outcomes,
    )


def read_summary(input_model: AssetReadSummaryInput, context: ToolExecutionContext) -> ToolResult:
    tool_name = "asset.read_summary"
    connection = context.readonly_connection
    if connection is None:
        return _failed(tool_name, context, "missing_connection", "需要只读仓库连接")
    asset_ids = list(dict.fromkeys(input_model.asset_ids))
    latest = MaterialSummariesRepository(connection).list_latest_for_assets(asset_ids)
    summaries: dict[str, dict[str, Any]] = {}
    lines: list[str] = []
    for asset_id in asset_ids:
        row = latest.get(asset_id)
        if row is None:
            lines.append(f"【{asset_id}】尚无已理解摘要。")
            continue
        summary = dict(row["summary_json"])
        summaries[asset_id] = summary
        lines.append(_summary_text(asset_id, _filename_hint(summary, asset_id), summary))
    observation = "\n".join(lines) if lines else "没有可返回的素材摘要。"
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name=tool_name,
        status="succeeded",
        observation=observation,
        data={"summaries": summaries, "missing": [a for a in asset_ids if a not in summaries]},
    )


async def _run_subagents(specs: Mapping[str, SubagentSpec]) -> dict[str, SubagentOutcome]:
    if not specs:
        return {}
    semaphore = asyncio.Semaphore(_concurrency())
    timeout = _timeout_seconds()

    async def _one(asset_id: str, spec: SubagentSpec) -> SubagentOutcome:
        async with semaphore:
            try:
                return await asyncio.wait_for(run_understanding_subagent(spec), timeout=timeout)
            except TimeoutError:
                return SubagentOutcome(
                    asset_id=asset_id, status="failed", failure_reason="理解超时"
                )
            except Exception as exc:  # never let one asset break the batch
                return SubagentOutcome(
                    asset_id=asset_id,
                    status="failed",
                    failure_reason=f"理解异常：{exc}",
                )

    items = list(specs.items())
    results = await asyncio.gather(*(_one(asset_id, spec) for asset_id, spec in items))
    return {asset_id: outcome for (asset_id, _), outcome in zip(items, results, strict=True)}


def _assemble_result(
    tool_name: str,
    context: ToolExecutionContext,
    *,
    asset_ids: list[str],
    asset_info: Mapping[str, _AssetInfo],
    project_id: str | None,
    focus: str | None,
    cached: Mapping[str, dict[str, Any]],
    missing: list[str],
    outcomes: Mapping[str, SubagentOutcome],
) -> ToolResult:
    events: list[dict[str, Any]] = []
    summary_rows: list[dict[str, Any]] = []
    transcript_rows: list[dict[str, Any]] = []
    results: dict[str, dict[str, Any]] = {}
    lines: list[str] = []
    transcript_seq = 0

    for asset_id in asset_ids:
        info = asset_info.get(asset_id)
        filename = info.filename if info is not None else asset_id
        if asset_id in cached:
            summary = cached[asset_id]
            results[asset_id] = {"status": "cached", "summary": summary}
            lines.append("（缓存命中）" + _summary_text(asset_id, filename, summary))
            continue
        if asset_id in missing:
            results[asset_id] = {"status": "failed", "reason": "素材不存在或未链接到项目"}
            lines.append(f"【{asset_id}】理解失败：素材不存在或未链接到项目。")
            continue
        outcome = outcomes.get(asset_id)
        if outcome is None:  # defensive; should not happen
            continue
        # 只对真实存在的素材派事件：给不存在的 asset 发事件会在 reducer 里插出幽灵行。
        events.append(_event(MaterialUnderstandingStarted, asset_id, project_id))
        if outcome.status == "ready" and outcome.summary is not None:
            summary = outcome.summary.model_dump(mode="json")
            summary_rows.append(_summary_row(outcome.summary, focus=focus))
            for result in outcome.transcribe_results:
                transcript_seq += 1
                transcript_rows.append(
                    _transcript_row(context.turn_id, asset_id, transcript_seq, result)
                )
            events.append(_event(MaterialUnderstandingCompleted, asset_id, project_id))
            results[asset_id] = {"status": "ready", "summary": summary}
            lines.append(_summary_text(asset_id, filename, summary))
        else:
            reason = outcome.failure_reason or "未知原因"
            events.append(_event(MaterialUnderstandingFailed, asset_id, project_id))
            results[asset_id] = {"status": "failed", "reason": reason}
            lines.append(f"【{asset_id}/{filename}】理解失败：{reason}。")

    data: dict[str, Any] = {"results": results}
    if summary_rows:
        data["material_summary_rows"] = summary_rows
    if transcript_rows:
        data["transcript_rows"] = transcript_rows
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name=tool_name,
        status="succeeded",
        observation="\n".join(lines) if lines else "没有需要理解的素材。",
        data=data,
        events=events,
    )


def _make_spec(
    context: ToolExecutionContext,
    info: _AssetInfo,
    *,
    gateway: Any,
    focus: str | None,
    version: int,
    prior_summary: dict[str, Any] | None,
    model: str,
    progress: Any,
) -> SubagentSpec:
    case_id = context.case_state.case_id if context.case_state is not None else None

    async def _vlm(messages: list[dict[str, Any]]) -> dict[str, Any]:
        request = ProviderRequest(
            capability=VLM_ANNOTATION,
            request_id=f"understand_vlm_{info.asset_id}_{uuid4().hex}",
            case_id=case_id,
            model=model,
            payload={
                "messages": messages,
                "params": {"temperature": 0, "response_format": {"type": "json_object"}},
            },
        )
        gateway_result = await gateway.call(request)
        result = gateway_result.result
        if result.error is not None:
            raise RuntimeError(f"{result.error.error_code}: {result.error.message}")
        return _action_from_output(result.normalized_output)

    def _extract(seconds: float) -> str:
        if info.path is None:
            raise FileNotFoundError(f"素材文件不可读：{info.asset_id}")
        return extract_frame_data_uri(info.path, seconds)

    async def _transcribe(start_s: float | None, end_s: float | None) -> TranscribeResult:
        return await _transcribe_segment(context, info, start_s, end_s)

    return SubagentSpec(
        asset_id=info.asset_id,
        filename=info.filename,
        kind=info.kind,
        duration_sec=info.duration_sec,
        index_summary=_index_summary(info),
        version=version,
        model=model,
        vlm=_vlm,
        extract_frame=_extract,
        transcribe=_transcribe,
        has_audio=info.has_audio,
        focus=focus,
        prior_summary=prior_summary,
        progress=progress,
    )


async def _transcribe_segment(
    context: ToolExecutionContext,
    info: _AssetInfo,
    start_s: float | None,
    end_s: float | None,
) -> TranscribeResult:
    """Production transcribe action: run the real ASR pipeline and window the result.

    Tests monkeypatch this module-level function to avoid network/ffmpeg; the
    subagent resolves it lazily through a closure so the patch always applies.
    """

    if info.path is None:
        raise FileNotFoundError(f"素材文件不可读：{info.asset_id}")
    paths = _workspace_paths(context)
    document = await transcribe_to_document(
        info.path,
        paths=paths,
        gateway=_build_asr_gateway(),
        asset_id=info.asset_id,
        transcript_id=f"tr_understand_{uuid4().hex}",
        request_id=f"understand_asr_{uuid4().hex}",
    )
    return _windowed_transcribe_result(document, start_s, end_s, info.duration_sec)


def _windowed_transcribe_result(
    document: TranscriptDocument,
    start_s: float | None,
    end_s: float | None,
    duration_sec: float | None,
) -> TranscribeResult:
    start_ms = int(start_s * 1000) if start_s is not None else None
    end_ms = int(end_s * 1000) if end_s is not None else None

    def _in_window(item_start_ms: int, item_end_ms: int) -> bool:
        if start_ms is not None and item_end_ms <= start_ms:
            return False
        return not (end_ms is not None and item_start_ms >= end_ms)

    utterances = [
        utterance
        for utterance in document.utterances
        if _in_window(utterance.start_ms, utterance.end_ms)
    ]
    vad = [
        segment for segment in document.vad_segments if _in_window(segment.start_ms, segment.end_ms)
    ]
    text = " ".join(utterance.text for utterance in utterances if utterance.text).strip()
    if start_s is not None and end_s is not None:
        seconds = max(0.0, end_s - start_s)
    else:
        seconds = duration_sec or 0.0
    return TranscribeResult(
        text=text,
        language=document.language,
        provider_id=document.provider_id,
        raw_preserved=document.raw_preserved,
        utterances=[utterance.model_dump(mode="json") for utterance in utterances],
        vad_segments=[segment.model_dump(mode="json") for segment in vad],
        seconds=seconds,
    )


def _build_asr_gateway() -> ProviderGateway:
    registry = ProviderRegistry()
    registry.register(
        aliyun_paraformer_asr_descriptor(),
        AliyunParaformerASRProvider(api_key=os.environ.get("RUSHES_DASHSCOPE_API_KEY")),
    )
    return ProviderGateway(registry=registry)


def _load_asset_info(
    connection: Connection,
    asset_ids: list[str],
    context: ToolExecutionContext,
) -> dict[str, _AssetInfo]:
    if not asset_ids:
        return {}
    rows = connection.execute(
        select(schema.assets).where(schema.assets.c.asset_id.in_(asset_ids))
    ).all()
    paths = _workspace_paths_or_none(context)
    info: dict[str, _AssetInfo] = {}
    for row in rows:
        values = dict(row._mapping)
        asset_id = str(values["asset_id"])
        index_json = _load_optional_json(values.get("index_json"))
        probe = _load_optional_json(values.get("probe"))
        kind = str(values.get("kind"))
        info[asset_id] = _AssetInfo(
            asset_id=asset_id,
            filename=str(values.get("filename") or asset_id),
            kind=kind,
            duration_sec=_duration_sec(index_json, probe),
            index_json=index_json,
            has_audio=_has_audio(kind, index_json, probe),
            path=_resolve_path(asset_id, connection, paths),
        )
    return info


def _resolve_path(
    asset_id: str,
    connection: Connection,
    paths: WorkspacePaths | None,
) -> Path | None:
    if paths is None:
        return None
    try:
        path = resolve_asset_path(asset_id, connection=connection, paths=paths)
    except FileNotFoundError:
        return None
    return path if path.is_file() else None


def _index_summary(info: _AssetInfo) -> str:
    lines: list[str] = [
        f"时长：{_fmt_sec(info.duration_sec)}s；有音轨：{'是' if info.has_audio else '否'}。"
    ]
    index = info.index_json or {}
    shots = index.get("shots")
    if isinstance(shots, list) and shots:
        preview = ", ".join(
            f"{_fmt_sec(shot.get('start_sec'))}-{_fmt_sec(shot.get('end_sec'))}"
            for shot in shots[:8]
            if isinstance(shot, Mapping)
        )
        lines.append(f"分镜 {len(shots)} 段：{preview}{' …' if len(shots) > 8 else ''}。")
    vad = index.get("vad")
    if isinstance(vad, list) and vad:
        speech = sum(1 for seg in vad if isinstance(seg, Mapping) and seg.get("kind") == "speech")
        lines.append(f"VAD：{len(vad)} 段（其中语音 {speech} 段）。")
    if isinstance(index.get("peaks"), list) and index.get("peaks"):
        lines.append("含波形概要。")
    font_meta = index.get("font_meta")
    if isinstance(font_meta, Mapping):
        lines.append(f"字体：{font_meta.get('family')} / {font_meta.get('style')}。")
    return "\n".join(lines)


def _summary_row(summary: Any, *, focus: str | None) -> dict[str, Any]:
    payload = summary.model_dump(mode="json")
    return {
        "summary_id": f"ms_{summary.asset_id}_v{summary.version}_{uuid4().hex[:8]}",
        "asset_id": summary.asset_id,
        "version": summary.version,
        "focus": focus,
        "status": "ready",
        "summary_json": payload,
        "model": summary.model,
        "created_at": summary.generated_at or _now_iso(),
    }


def _transcript_row(
    turn_id: str,
    asset_id: str,
    seq: int,
    result: TranscribeResult,
) -> dict[str, Any]:
    safe_turn = turn_id.replace(":", "_")
    return {
        "transcript_id": f"tr_us_{safe_turn}_{asset_id}_{seq}",
        "asset_id": asset_id,
        "provider_id": result.provider_id,
        "raw_preserved": result.raw_preserved,
        "utterances": result.utterances,
        "vad_segments": result.vad_segments,
    }


def _summary_text(asset_id: str, filename: str, summary: Mapping[str, Any]) -> str:
    role = summary.get("semantic_role", "other")
    language = summary.get("language")
    version = summary.get("version")
    header = f"【{asset_id}/{filename}】role={role}"
    if language:
        header += f" 语言={language}"
    if version is not None:
        header += f"（v{version}）"
    lines = [header, f"概述：{summary.get('overall', '')}"]
    segments = summary.get("segments")
    if isinstance(segments, list) and segments:
        lines.append("段落：")
        for segment in segments:
            if not isinstance(segment, Mapping):
                continue
            span = f"{_fmt_sec(segment.get('start_s'))}-{_fmt_sec(segment.get('end_s'))}s"
            quality = segment.get("quality", "usable")
            piece = f"  [{span}/{quality}] {segment.get('description', '')}"
            transcript = segment.get("transcript")
            if transcript:
                piece += f" /转写：{transcript}"
            lines.append(piece)
    return "\n".join(lines)


def _event(
    event_class: Callable[..., DomainEventBase],
    asset_id: str,
    project_id: str | None,
) -> dict[str, Any]:
    payload: dict[str, Any] = event_class(asset_id=asset_id, project_id=project_id).model_dump(
        mode="json"
    )
    return payload


def _action_from_output(output: Any) -> dict[str, Any]:
    if isinstance(output, Mapping):
        if "action" in output:
            return dict(output)
        content = output.get("content")
        if isinstance(content, str) and content.strip():
            parsed = _try_json(content)
            if isinstance(parsed, Mapping):
                return dict(parsed)
        return dict(output)
    if isinstance(output, str):
        parsed = _try_json(output)
        if isinstance(parsed, Mapping):
            return dict(parsed)
    return {}


def _try_json(value: str) -> Any:
    import json

    try:
        return json.loads(value)
    except (ValueError, TypeError):
        return None


def _load_optional_json(raw: Any) -> dict[str, Any] | None:
    if not isinstance(raw, str) or not raw:
        return None
    parsed = load_json(raw)
    return parsed if isinstance(parsed, dict) else None


def _duration_sec(index_json: dict[str, Any] | None, probe: dict[str, Any] | None) -> float | None:
    for source in (index_json, probe):
        if isinstance(source, Mapping):
            value = source.get("duration_sec")
            if isinstance(value, int | float):
                return float(value)
    return None


def _has_audio(kind: str, index_json: dict[str, Any] | None, probe: dict[str, Any] | None) -> bool:
    if kind == "audio":
        return True
    if kind != "video":
        return False
    if isinstance(probe, Mapping) and probe.get("has_audio") is True:
        return True
    return bool(
        isinstance(index_json, Mapping) and (index_json.get("vad") or index_json.get("peaks"))
    )


def _filename_hint(summary: Mapping[str, Any], asset_id: str) -> str:
    return asset_id


def _project_id(context: ToolExecutionContext) -> str | None:
    if context.project_state is not None:
        return context.project_state.project_id
    if context.case_state is not None:
        return context.case_state.project_id
    return None


def _provider_gateway(context: ToolExecutionContext) -> Any:
    gateway = context.metadata.get("provider_gateway")
    if gateway is not None and callable(getattr(gateway, "call", None)):
        return gateway
    return None


def _progress(context: ToolExecutionContext) -> Any:
    progress = context.metadata.get("turn_progress")
    if callable(progress):
        return progress
    return lambda _payload: None


def _workspace_paths(context: ToolExecutionContext) -> WorkspacePaths:
    paths = _workspace_paths_or_none(context)
    if paths is None:
        raise ValueError("understand.materials 需要 workspace_path 元数据")
    return paths


def _workspace_paths_or_none(context: ToolExecutionContext) -> WorkspacePaths | None:
    raw_paths = context.metadata.get("workspace_paths")
    if isinstance(raw_paths, WorkspacePaths):
        return raw_paths
    raw_root = context.metadata.get("workspace_path")
    if isinstance(raw_root, str | Path):
        return WorkspacePaths.from_root(raw_root)
    return None


def _vlm_model() -> str:
    return os.environ.get("RUSHES_VLM_MODEL") or DEFAULT_OPENAI_COMPATIBLE_VLM_MODEL


def _concurrency() -> int:
    return _env_int("RUSHES_UNDERSTAND_CONCURRENCY", DEFAULT_CONCURRENCY, minimum=1)


def _timeout_seconds() -> float:
    return _env_float("RUSHES_UNDERSTAND_TIMEOUT_S", DEFAULT_TIMEOUT_S, minimum=1.0)


def _env_int(name: str, default: int, *, minimum: int) -> int:
    raw = os.environ.get(name)
    if raw is None:
        return default
    try:
        return max(minimum, int(raw))
    except ValueError:
        return default


def _env_float(name: str, default: float, *, minimum: float) -> float:
    raw = os.environ.get(name)
    if raw is None:
        return default
    try:
        return max(minimum, float(raw))
    except ValueError:
        return default


def _fmt_sec(value: Any) -> str:
    if not isinstance(value, int | float):
        return "?"
    text = f"{float(value):.3f}".rstrip("0").rstrip(".")
    return text or "0"


def _now_iso() -> str:
    return datetime.now(UTC).isoformat()


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
