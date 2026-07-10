"""understand.materials（理解子代理派发，Spec C §C3）。

``understand.materials`` 是 async handler：turn 内为每个 asset 起一个理解子代理，
``asyncio.as_completed`` 逐素材增量完成（``Semaphore`` 上限、单素材 ``wait_for`` 超时），
单素材一完成即落库发事件、不必等最慢项。缓存命中（fingerprint/model/prompt_version 都匹配、
无新 focus）直接返回、不起子代理，承接原 ``asset.read_summary`` 的只读取用。

handler 只有只读连接：落库与发事件走 loop 注入的 ``metadata["partial_result_sink"]``
（每素材即时提交 material_summary_rows / transcript_rows + Started/Completed 或 Started/Failed）；
无 sink 的旧路径（直调/REST）退回把产物经 ``ToolResult.data`` / ``ToolResult.events`` 批量回填。
"""

from __future__ import annotations

import asyncio
import logging
import os
from collections.abc import Callable, Mapping
from dataclasses import dataclass, replace
from datetime import UTC, datetime
from pathlib import Path
from typing import Any
from uuid import uuid4

from sqlalchemy import select
from sqlalchemy.engine import Connection

from contracts.events import (
    AssetIndexReady,
    DomainEventBase,
    MaterialUnderstandingCompleted,
    MaterialUnderstandingFailed,
    MaterialUnderstandingStarted,
)
from contracts.tool_result import ToolError, ToolResult
from contracts.transcript import TranscriptDocument
from media import Shot, split_shots
from providers import (
    VLM_UNDERSTANDING,
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
from tools.specs import UnderstandMaterialsInput

from .asr import transcribe_to_document
from .subagent import (
    UNDERSTAND_PROMPT_VERSION,
    SubagentOutcome,
    SubagentSpec,
    TranscribeResult,
    run_understanding_subagent,
)

logger = logging.getLogger(__name__)

DEFAULT_CONCURRENCY = 3
DEFAULT_TIMEOUT_S = 300.0

# 增量提交回调（loop 注入 metadata["partial_result_sink"]）：rows 是 material_summary_rows /
# transcript_rows 形态的落库字典，events 是同批领域事件；先落库后发事件由 loop 侧保证。
PartialResultSink = Callable[[Mapping[str, list[dict[str, Any]]], list[dict[str, Any]]], None]


@dataclass(frozen=True, slots=True)
class _AssetInfo:
    asset_id: str
    filename: str
    kind: str
    duration_sec: float | None
    index_json: dict[str, Any] | None
    has_audio: bool
    path: Path | None
    mtime: int | None = None
    size: int | None = None
    thumbnail_object_hash: str | None = None


@dataclass(frozen=True, slots=True)
class _Pending:
    version: int
    prior_summary: dict[str, Any] | None


async def materials(
    input_model: UnderstandMaterialsInput, context: ToolExecutionContext
) -> ToolResult:
    tool_name = "understand.materials"
    connection = context.readonly_connection
    if connection is None:
        return _failed(tool_name, context, "missing_connection", "需要只读仓库连接")

    draft_id = _draft_id(context)
    focus = (input_model.focus or "").strip() or None
    asset_ids = list(dict.fromkeys(input_model.asset_ids))
    gateway = _provider_gateway(context)
    progress = _progress(context)
    sink = _partial_sink(context)
    model = _vlm_model()

    summaries_repo = MaterialSummariesRepository(connection)
    latest = summaries_repo.list_latest_for_assets(asset_ids)
    asset_info = _load_asset_info(connection, asset_ids, context)

    cached: dict[str, dict[str, Any]] = {}
    missing: list[str] = []
    pending: dict[str, _Pending] = {}
    for asset_id in asset_ids:
        info = asset_info.get(asset_id)
        if info is None:
            missing.append(asset_id)
            continue
        prior = latest.get(asset_id)
        if prior is not None and focus is None and _cache_valid(prior, info, model):
            cached[asset_id] = dict(prior["summary_json"])
            continue
        version = int(prior["version"]) + 1 if prior is not None else 1
        pending[asset_id] = _Pending(
            version=version,
            prior_summary=dict(prior["summary_json"]) if prior is not None else None,
        )

    outcomes: dict[str, SubagentOutcome] = {}
    index_events: list[dict[str, Any]] = []
    if gateway is None:
        # 无 VLM 通道：不派子代理、不算分镜（分镜的用处是喂子代理，理解注定失败就没必要烧 CPU）。
        for asset_id in pending:
            outcome = SubagentOutcome(
                asset_id=asset_id,
                status="failed",
                failure_reason="VLM 通道不可用，无法理解素材",
                failure_code="vlm_error",
            )
            outcomes[asset_id] = outcome
            if sink is not None:
                _sink_outcome(sink, outcome, context, asset_info.get(asset_id), focus, draft_id)
    else:
        # 逐素材增量完成：单素材（含其按需分镜）一到就经 sink 立即落库发事件（UI 卡片
        # 逐个变绿），不必等最慢的那个算完分镜/理解；无 sink 的旧路径退回批量回填。
        # tasks 由本函数直接持有，消费循环包 try/finally，退出前 cancel + gather 收拢在飞任务。
        tasks = _spawn_understanding_tasks(
            pending,
            asset_info,
            context,
            gateway=gateway,
            focus=focus,
            model=model,
            progress=progress,
            sink=sink,
            draft_id=draft_id,
        )
        try:
            for completed in asyncio.as_completed(tasks):
                outcome, asset_index_events = await completed
                outcomes[outcome.asset_id] = outcome
                if sink is not None:
                    _sink_outcome(
                        sink, outcome, context, asset_info.get(outcome.asset_id), focus, draft_id
                    )
                else:
                    index_events.extend(asset_index_events)
        finally:
            # 消费循环任何异常（如 sink 落库抛错）或回合取消都不能弃置在飞任务：
            # 否则它们继续在后台跑、经 sink 脏写库发事件、VLM 费用照烧。
            for task in tasks:
                if not task.done():
                    task.cancel()
            await asyncio.gather(*tasks, return_exceptions=True)

    return _assemble_result(
        tool_name,
        context,
        asset_ids=asset_ids,
        asset_info=asset_info,
        draft_id=draft_id,
        focus=focus,
        cached=cached,
        missing=missing,
        outcomes=outcomes,
        incremental=sink is not None,
        index_events=index_events,
    )


def _spawn_understanding_tasks(
    pending: Mapping[str, _Pending],
    asset_info: Mapping[str, _AssetInfo],
    context: ToolExecutionContext,
    *,
    gateway: Any,
    focus: str | None,
    model: str,
    progress: Any,
    sink: PartialResultSink | None,
    draft_id: str | None,
) -> list[asyncio.Task[tuple[SubagentOutcome, list[dict[str, Any]]]]]:
    """给每个 pending 素材起一个理解任务，返回 Task 列表交调用方持有并收拢。

    每个素材一个任务：Semaphore 并发内先按需算分镜（与其他素材的 VLM 调用重叠）、再派子代理，
    因此第 1 个素材完成不必等最慢项算完分镜。**回填 + 建 spec + 子代理整体纳入单素材 wait_for
    超时**：长视频在无硬解、回落原片的机器上 PySceneDetect 可无界运行，若把回填放在 wait_for 之外
    就会挂死回合、超时永不触发。``index_events`` 在 sink 存在时已即时发出、返回空；无 sink 时随
    outcome 带回、由调用方批量回填（回填先于子代理执行，故超时也保留已产出的分镜事件）。
    """

    semaphore = asyncio.Semaphore(_concurrency())
    timeout = _timeout_seconds()

    async def _one(asset_id: str, item: _Pending) -> tuple[SubagentOutcome, list[dict[str, Any]]]:
        async with semaphore:
            produced_events: list[dict[str, Any]] = []

            async def _run() -> SubagentOutcome:
                info, index_events = await _backfill_shots_one(
                    asset_info[asset_id], context, sink=sink, draft_id=draft_id
                )
                produced_events.extend(index_events)
                spec = _make_spec(
                    context,
                    info,
                    gateway=gateway,
                    focus=focus,
                    version=item.version,
                    prior_summary=item.prior_summary,
                    model=model,
                    progress=progress,
                )
                return await run_understanding_subagent(spec)

            try:
                outcome = await asyncio.wait_for(_run(), timeout=timeout)
            except TimeoutError:
                outcome = SubagentOutcome(
                    asset_id=asset_id,
                    status="failed",
                    failure_reason="理解超时",
                    failure_code="timeout",
                )
            except Exception as exc:  # never let one asset break the batch
                outcome = SubagentOutcome(
                    asset_id=asset_id,
                    status="failed",
                    failure_reason=f"理解异常：{exc}",
                    failure_code="vlm_error",
                )
            return outcome, produced_events

    return [asyncio.ensure_future(_one(asset_id, item)) for asset_id, item in pending.items()]


def _sink_outcome(
    sink: PartialResultSink,
    outcome: SubagentOutcome,
    context: ToolExecutionContext,
    info: _AssetInfo | None,
    focus: str | None,
    draft_id: str | None,
) -> None:
    """单素材 outcome 即时落库 + 发事件（每素材事件序列仍是 Started→Completed/Failed）。"""

    events: list[dict[str, Any]] = [
        _event(MaterialUnderstandingStarted, outcome.asset_id, draft_id)
    ]
    rows: dict[str, list[dict[str, Any]]] = {}
    if outcome.status == "ready" and outcome.summary is not None:
        rows["material_summary_rows"] = [
            _summary_row(outcome.summary, focus=focus, fingerprint=_fingerprint(info))
        ]
        transcript_rows = [
            _transcript_row(context.turn_id, outcome.asset_id, seq, result)
            for seq, result in enumerate(outcome.transcribe_results, start=1)
        ]
        if transcript_rows:
            rows["transcript_rows"] = transcript_rows
        events.append(_event(MaterialUnderstandingCompleted, outcome.asset_id, draft_id))
    else:
        events.append(
            _event(
                MaterialUnderstandingFailed,
                outcome.asset_id,
                draft_id,
                payload=_failure_payload(outcome),
            )
        )
    sink(rows, events)


def _assemble_result(
    tool_name: str,
    context: ToolExecutionContext,
    *,
    asset_ids: list[str],
    asset_info: Mapping[str, _AssetInfo],
    draft_id: str | None,
    focus: str | None,
    cached: Mapping[str, dict[str, Any]],
    missing: list[str],
    outcomes: Mapping[str, SubagentOutcome],
    incremental: bool,
    index_events: list[dict[str, Any]],
) -> ToolResult:
    events: list[dict[str, Any]] = []
    summary_rows: list[dict[str, Any]] = []
    transcript_rows: list[dict[str, Any]] = []
    results: dict[str, dict[str, Any]] = {}
    lines: list[str] = []
    counts = {"ready": 0, "failed": 0, "cached": 0}

    # 增量路径下产物已由 sink 逐素材落库/发事件，最终 ToolResult 只保留汇总，避免 loop 双写。
    if not incremental:
        events.extend(index_events)

    for asset_id in asset_ids:
        info = asset_info.get(asset_id)
        filename = info.filename if info is not None else asset_id
        if asset_id in cached:
            summary = cached[asset_id]
            results[asset_id] = {"status": "cached", "summary": summary}
            lines.append("（缓存命中）" + _summary_text(asset_id, filename, summary))
            counts["cached"] += 1
            continue
        if asset_id in missing:
            results[asset_id] = {"status": "failed", "reason": "素材不存在或未链接到项目"}
            lines.append(f"【{asset_id}】理解失败：素材不存在或未链接到项目。")
            counts["failed"] += 1
            continue
        outcome = outcomes[asset_id]
        if outcome.status == "ready" and outcome.summary is not None:
            summary = outcome.summary.model_dump(mode="json")
            results[asset_id] = {"status": "ready", "summary": summary}
            lines.append(_summary_text(asset_id, filename, summary))
            counts["ready"] += 1
            if not incremental:
                # 只对真实存在的素材派事件：给不存在的 asset 发事件会在 reducer 里插出幽灵行。
                events.append(_event(MaterialUnderstandingStarted, asset_id, draft_id))
                summary_rows.append(
                    _summary_row(outcome.summary, focus=focus, fingerprint=_fingerprint(info))
                )
                for seq, result in enumerate(outcome.transcribe_results, start=1):
                    transcript_rows.append(_transcript_row(context.turn_id, asset_id, seq, result))
                events.append(_event(MaterialUnderstandingCompleted, asset_id, draft_id))
        else:
            reason = outcome.failure_reason or "未知原因"
            code = outcome.failure_code
            results[asset_id] = {"status": "failed", "reason": reason, "failure_code": code}
            suffix = f"（{code}）" if code else ""
            lines.append(f"【{asset_id}/{filename}】理解失败{suffix}：{reason}。")
            counts["failed"] += 1
            if not incremental:
                events.append(_event(MaterialUnderstandingStarted, asset_id, draft_id))
                events.append(
                    _event(
                        MaterialUnderstandingFailed,
                        asset_id,
                        draft_id,
                        payload=_failure_payload(outcome),
                    )
                )

    summary_line = (
        f"共 {counts['ready']} 个理解完成、{counts['failed']} 个失败、"
        f"{counts['cached']} 个缓存命中。"
    )
    data: dict[str, Any] = {"results": results}
    if summary_rows:
        data["material_summary_rows"] = summary_rows
    if transcript_rows:
        data["transcript_rows"] = transcript_rows
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name=tool_name,
        status="succeeded",
        observation="\n".join([summary_line, *lines]) if lines else "没有需要理解的素材。",
        data=data,
        events=events,
    )


async def _backfill_shots_one(
    info: _AssetInfo,
    context: ToolExecutionContext,
    *,
    sink: PartialResultSink | None,
    draft_id: str | None,
) -> tuple[_AssetInfo, list[dict[str, Any]]]:
    """给缺 shots 的单个 video 素材按需算分镜，合并进 index_json 并发 AssetIndexReady。

    在子代理任务内、``run_understanding_subagent`` 之前执行（与其他素材的 VLM 调用重叠），
    返回回填后的 ``_AssetInfo``（喂当前子代理索引）与需批量回填的事件列表。sink 存在时即时
    落库/发事件、返回空事件；分镜失败降级为记日志、shots 缺失继续理解，绝不让素材理解失败。

    ``index_json`` 为 None（素材在 index job 完成前就被理解）也放行回填，否则理解成功后缓存永久
    命中、shots 从此没有计算机会。事件 payload **只带 shots**，交 reducer 按键合并——不再用启动时
    快照的整份 index_json/缩略图去覆盖排队期间 worker 并发写入的新索引/新缩略图。
    """

    index = info.index_json
    if info.kind != "video" or info.path is None:
        return info, []
    if isinstance(index, dict) and "shots" in index:
        return info, []  # 已有 shots，无需回填
    paths = _workspace_paths_or_none(context)
    if paths is None:
        return info, []
    try:
        shots = await asyncio.to_thread(split_shots, info.path, paths=paths)
    except Exception as exc:  # 分镜是廉价加成，失败只降级不阻塞理解
        logger.warning("按需分镜失败，跳过 shots 回填：asset_id=%s err=%s", info.asset_id, exc)
        return info, []
    shots_payload = [_shot_payload(shot) for shot in shots]
    base = index if isinstance(index, dict) else {}
    updated = replace(info, index_json={**base, "shots": shots_payload})
    event = AssetIndexReady(
        asset_id=info.asset_id,
        draft_id=draft_id,
        payload={"index_json": {"shots": shots_payload}},
    ).model_dump(mode="json")
    if sink is not None:
        sink({}, [event])
        return updated, []
    return updated, [event]


def _shot_payload(shot: Shot) -> dict[str, Any]:
    return {"shot_id": shot.shot_id, "start_sec": shot.start_sec, "end_sec": shot.end_sec}


def _failure_payload(outcome: SubagentOutcome) -> dict[str, Any]:
    return {
        "failure_code": outcome.failure_code,
        "failure_reason": outcome.failure_reason,
    }


def _cache_valid(prior: Mapping[str, Any], info: _AssetInfo, model: str) -> bool:
    """缓存命中判据：fingerprint / model / prompt_version 有值且不匹配才判过期。

    历史行三者为 NULL 时一律视为命中，不惩罚存量摘要（只在「有值且不匹配」时重新理解）。
    """

    prior_fingerprint = prior.get("fingerprint")
    if prior_fingerprint is not None and prior_fingerprint != _fingerprint(info):
        return False
    prior_prompt_version = prior.get("prompt_version")
    if prior_prompt_version is not None and prior_prompt_version != UNDERSTAND_PROMPT_VERSION:
        return False
    prior_model = prior.get("model")
    return prior_model is None or prior_model == model


def _fingerprint(info: _AssetInfo | None) -> str | None:
    # 刻意用 {size}:{mtime} 而非 assets.hash：canonical hash 后台补算会变，会造成缓存误失效。
    if info is None or info.mtime is None or info.size is None:
        return None
    return f"{info.size}:{info.mtime}"


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
    async def _vlm(messages: list[dict[str, Any]]) -> dict[str, Any]:
        request = ProviderRequest(
            capability=VLM_UNDERSTANDING,
            request_id=f"understand_vlm_{info.asset_id}_{uuid4().hex}",
            model=model,
            draft_id=_draft_id(context),
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
            mtime=_as_int(values.get("mtime")),
            size=_as_int(values.get("size")),
            thumbnail_object_hash=_as_str_or_none(values.get("thumbnail_object_hash")),
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


def _summary_row(summary: Any, *, focus: str | None, fingerprint: str | None) -> dict[str, Any]:
    payload = summary.model_dump(mode="json")
    return {
        "summary_id": f"ms_{summary.asset_id}_v{summary.version}_{uuid4().hex[:8]}",
        "asset_id": summary.asset_id,
        "version": summary.version,
        "focus": focus,
        "status": "ready",
        "summary_json": payload,
        "model": summary.model,
        "fingerprint": fingerprint,
        "prompt_version": UNDERSTAND_PROMPT_VERSION,
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
        # 加随机段（仿 summary_id）：同一回合对同素材二次理解（focus 深挖）再转写时，
        # 裸 tr_us_{turn}_{asset}_{seq} 会撞主键让整个工具失败。
        "transcript_id": f"tr_us_{safe_turn}_{asset_id}_{seq}_{uuid4().hex[:8]}",
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
    draft_id: str | None,
    *,
    payload: dict[str, Any] | None = None,
) -> dict[str, Any]:
    event = event_class(asset_id=asset_id, draft_id=draft_id, payload=payload or {})
    return event.model_dump(mode="json")


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


def _draft_id(context: ToolExecutionContext) -> str | None:
    if context.draft_state is not None:
        return context.draft_state.draft_id
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


def _partial_sink(context: ToolExecutionContext) -> PartialResultSink | None:
    # 无 sink（如某些直调 handler 的旧测试/REST 路径）时退回批量回填，零破坏。
    sink = context.metadata.get("partial_result_sink")
    return sink if callable(sink) else None


def _as_int(value: Any) -> int | None:
    return value if isinstance(value, int) and not isinstance(value, bool) else None


def _as_str_or_none(value: Any) -> str | None:
    return value if isinstance(value, str) and value else None


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
