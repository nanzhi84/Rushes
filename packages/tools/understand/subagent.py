"""素材理解子代理：复用 harness 设施的多模态 mini-loop（Spec C §C3）。

子代理自己就是 VLM：每步把「便宜索引摘要 + 已看帧图 + 已得转写 + 动作菜单」喂给
多模态模型，模型回一个 JSON 动作（view_frames / transcribe / emit_summary），循环
推进直到产出结构化 :class:`MaterialSummary` 或耗尽预算/超时。

本模块**不碰网络、不碰 DB、不解析 provider 报文**：``vlm`` / ``extract_frame`` /
``transcribe`` 全部由调用方注入（handler 负责接线，测试直接喂脚本化动作序列）。
"""

from __future__ import annotations

import asyncio
import json
from collections.abc import Awaitable, Callable, Mapping, Sequence
from dataclasses import dataclass, field
from datetime import UTC, datetime
from typing import Any

from pydantic import ValidationError

from contracts.understanding import MaterialSummary, SummarySpent

DEFAULT_STEP_BUDGET = 12
MAX_ILLEGAL_JSON = 3
MAX_EMIT_ATTEMPTS = 2
MAX_FRAMES_PER_VIEW = 6
_AUDIO_KINDS = frozenset({"video", "audio"})


@dataclass(frozen=True, slots=True)
class TranscribeResult:
    """一次转写动作的产物：喂回子代理的文本 + 待落库的结构化字段。"""

    text: str
    language: str | None
    provider_id: str
    raw_preserved: bool
    utterances: list[dict[str, Any]] = field(default_factory=list)
    vad_segments: list[dict[str, Any]] = field(default_factory=list)
    seconds: float = 0.0


# 注入契约（handler 接线，测试打桩）：
# - vlm：喂多模态 messages，返回一个「动作 JSON 字典」（provider 报文解析在 handler 侧完成）
# - extract_frame：同步 ffmpeg 抽帧，返回 data: URI（子代理内用 to_thread 包裹避免阻塞事件循环）
# - transcribe：异步 ASR，(start_s, end_s) -> TranscribeResult
# - progress：turn-stream 进度回调，payload 里**不放 `type` 键**
VlmCall = Callable[[list[dict[str, Any]]], Awaitable[dict[str, Any]]]
ExtractFrame = Callable[[float], str]
Transcribe = Callable[[float | None, float | None], Awaitable[TranscribeResult]]
Progress = Callable[[Mapping[str, Any]], None]
NowFn = Callable[[], str]


def _now_iso() -> str:
    return datetime.now(UTC).isoformat()


def _noop_progress(_payload: Mapping[str, Any]) -> None:
    return None


@dataclass(frozen=True, slots=True)
class SubagentSpec:
    asset_id: str
    filename: str
    kind: str
    duration_sec: float | None
    index_summary: str
    version: int
    model: str
    vlm: VlmCall
    extract_frame: ExtractFrame
    transcribe: Transcribe
    focus: str | None = None
    prior_summary: dict[str, Any] | None = None
    progress: Progress = _noop_progress
    now: NowFn = _now_iso
    step_budget: int = DEFAULT_STEP_BUDGET


@dataclass(frozen=True, slots=True)
class SubagentOutcome:
    asset_id: str
    status: str  # "ready" | "failed"
    summary: MaterialSummary | None = None
    failure_reason: str | None = None
    transcribe_results: tuple[TranscribeResult, ...] = ()
    frames_viewed: int = 0
    asr_seconds: float = 0.0
    steps: int = 0


@dataclass(slots=True)
class _RunState:
    viewed: list[tuple[str, str | None]] = field(default_factory=list)
    transcripts_text: list[str] = field(default_factory=list)
    transcribe_results: list[TranscribeResult] = field(default_factory=list)
    frames_viewed: int = 0
    asr_seconds: float = 0.0
    steps: int = 0


async def run_understanding_subagent(spec: SubagentSpec) -> SubagentOutcome:
    """驱动一个素材理解子代理，返回 ready/failed 的 :class:`SubagentOutcome`。"""

    state = _RunState()
    illegal_json = 0
    emit_attempts = 0
    correction: str | None = None

    for step in range(spec.step_budget):
        state.steps = step + 1
        messages = _build_messages(spec, state, correction)
        correction = None
        try:
            raw_action = await spec.vlm(messages)
        except Exception as exc:  # provider/transport failure surfaces as subagent failure
            return _failed(spec, state, f"VLM 调用失败：{exc}")

        action = _coerce_action(raw_action)
        if action is None:
            illegal_json += 1
            if illegal_json >= MAX_ILLEGAL_JSON:
                return _failed(spec, state, "连续多次未返回合法的动作 JSON")
            correction = (
                "上一步没有返回合法的动作 JSON。请只返回一个对象，形如 "
                '{"action":"view_frames|transcribe|emit_summary", ...}。'
            )
            continue
        illegal_json = 0
        name = action.get("action")

        if name == "view_frames":
            correction = await _do_view_frames(spec, state, action)
            continue
        if name == "transcribe":
            correction = await _do_transcribe(spec, state, action)
            continue
        if name == "emit_summary":
            emit_attempts += 1
            summary, error = _build_summary(spec, state, action.get("summary"))
            if summary is not None:
                spec.progress({"asset_id": spec.asset_id, "note": "已产出素材摘要"})
                return SubagentOutcome(
                    asset_id=spec.asset_id,
                    status="ready",
                    summary=summary,
                    transcribe_results=tuple(state.transcribe_results),
                    frames_viewed=state.frames_viewed,
                    asr_seconds=round(state.asr_seconds, 3),
                    steps=state.steps,
                )
            if emit_attempts >= MAX_EMIT_ATTEMPTS:
                return _failed(spec, state, f"摘要 schema 校验失败：{error}")
            correction = f"emit_summary 的 summary 不符合契约：{error}。请修正后重新提交。"
            continue

        illegal_json += 1
        if illegal_json >= MAX_ILLEGAL_JSON:
            return _failed(spec, state, "连续多次返回未知动作")
        correction = f"未知动作 {name!r}。可用动作：view_frames、transcribe、emit_summary。"

    return _failed(spec, state, "步数预算耗尽仍未产出摘要")


async def _do_view_frames(
    spec: SubagentSpec,
    state: _RunState,
    action: Mapping[str, Any],
) -> str | None:
    timestamps = _coerce_timestamps(action.get("timestamps_s"))[:MAX_FRAMES_PER_VIEW]
    if not timestamps:
        return "view_frames 需要至少一个非负的 timestamps_s（秒）。"
    for seconds in timestamps:
        spec.progress({"asset_id": spec.asset_id, "note": f"正在查看 {_fmt_clock(seconds)} 画面"})
        try:
            data_uri = await asyncio.to_thread(spec.extract_frame, seconds)
        except Exception as exc:
            state.viewed.append((f"t={_fmt_sec(seconds)}s 抽帧失败：{exc}", None))
            continue
        state.frames_viewed += 1
        state.viewed.append((f"asset_t={_fmt_sec(seconds)}s", data_uri))
    return None


async def _do_transcribe(
    spec: SubagentSpec,
    state: _RunState,
    action: Mapping[str, Any],
) -> str | None:
    if spec.kind not in _AUDIO_KINDS:
        return "该素材没有音轨，无法转写；请依据画面与索引产出摘要。"
    start = _coerce_float(action.get("start_s"))
    end = _coerce_float(action.get("end_s"))
    window = f"{_fmt_clock(start or 0.0)}-{_fmt_clock(end or spec.duration_sec or 0.0)}"
    spec.progress({"asset_id": spec.asset_id, "note": f"正在转写 {window}"})
    try:
        result = await spec.transcribe(start, end)
    except Exception as exc:
        return f"转写失败：{exc}。可改看画面或直接产出摘要。"
    state.transcribe_results.append(result)
    state.asr_seconds += max(0.0, result.seconds)
    label = f"[{_fmt_sec(start or 0.0)}-{_fmt_sec(end or 0.0)}s]"
    text = result.text.strip()
    state.transcripts_text.append(f"{label} {text}" if text else f"{label} （无语音）")
    return None


def _build_summary(
    spec: SubagentSpec,
    state: _RunState,
    payload: Any,
) -> tuple[MaterialSummary | None, str | None]:
    if not isinstance(payload, Mapping):
        return None, "summary 必须是一个对象"
    data = dict(payload)
    # 系统统一填充的字段一律以 handler/子代理为准，忽略模型自填的值。
    data["asset_id"] = spec.asset_id
    data["version"] = spec.version
    data["focus"] = spec.focus
    data["generated_at"] = spec.now()
    data["model"] = spec.model
    data["spent"] = SummarySpent(
        frames_viewed=state.frames_viewed,
        asr_seconds=round(state.asr_seconds, 3),
    ).model_dump()
    try:
        return MaterialSummary.model_validate(data), None
    except ValidationError as exc:
        return None, _first_validation_error(exc)


def _failed(spec: SubagentSpec, state: _RunState, reason: str) -> SubagentOutcome:
    spec.progress({"asset_id": spec.asset_id, "note": f"理解失败：{reason}"})
    return SubagentOutcome(
        asset_id=spec.asset_id,
        status="failed",
        failure_reason=reason,
        transcribe_results=tuple(state.transcribe_results),
        frames_viewed=state.frames_viewed,
        asr_seconds=round(state.asr_seconds, 3),
        steps=state.steps,
    )


_SYSTEM_PROMPT = (
    "你是「素材理解员」子代理。职责：忠实地理解单个素材，产出带时间戳、可直接用于剪辑"
    "决策的结构化摘要。你可以多轮调用动作逐步收集证据，证据足够后再产出摘要。\n"
    "每一步只返回一个 JSON 对象，必须是下列动作之一：\n"
    '- {"action":"view_frames","timestamps_s":[秒,...]}（单次最多 6 帧）\n'
    '- {"action":"transcribe","start_s":秒,"end_s":秒}（仅对有音轨的素材有效）\n'
    '- {"action":"emit_summary","summary":{...}}（终结：提交 MaterialSummary）\n'
    "MaterialSummary 契约：{semantic_role, overall, language?, segments:[{start_s,end_s,"
    "description,transcript?,tags[],quality:good|usable|avoid,notes?}]}；asset_id/version/"
    "focus/generated_at/model/spent 由系统填充，你不必提供。语言未知或无语音可省略 language。"
)


def _build_messages(
    spec: SubagentSpec,
    state: _RunState,
    correction: str | None,
) -> list[dict[str, Any]]:
    lines: list[str] = [
        f"素材：asset_id={spec.asset_id}；文件名={spec.filename}；类型={spec.kind}；"
        f"时长={_fmt_sec(spec.duration_sec)}s。",
        "便宜本地索引摘要：",
        spec.index_summary or "（无索引信息）",
    ]
    if spec.focus:
        lines.append(f"本次深挖关注点（focus）：{spec.focus}")
    if spec.prior_summary is not None:
        lines.append("已有摘要（请在其基础上增量深挖并合并）：")
        lines.append(json.dumps(spec.prior_summary, ensure_ascii=False)[:1500])
    if state.transcripts_text:
        lines.append("已获得的转写片段：")
        lines.extend(state.transcripts_text)
    if state.viewed:
        lines.append(f"已查看 {state.frames_viewed} 帧画面（图像见下）。")
    else:
        lines.append("尚未查看任何画面。")
    if correction:
        lines.append(f"注意：{correction}")
    lines.append("请给出下一个动作 JSON。")

    content: list[dict[str, Any]] = [{"type": "text", "text": "\n".join(lines)}]
    for label, data_uri in state.viewed:
        content.append({"type": "text", "text": label})
        if data_uri is not None:
            content.append({"type": "image_url", "image_url": {"url": data_uri}})
    return [
        {"role": "system", "content": _SYSTEM_PROMPT},
        {"role": "user", "content": content},
    ]


def _coerce_action(raw: Any) -> dict[str, Any] | None:
    if isinstance(raw, Mapping):
        if "action" in raw:
            return dict(raw)
        # 兜底：某些多模态模型把动作 JSON 塞在 content 字符串里。
        content = raw.get("content")
        if isinstance(content, str) and content.strip():
            parsed = _try_json(content)
            if isinstance(parsed, Mapping) and "action" in parsed:
                return dict(parsed)
    if isinstance(raw, str):
        parsed = _try_json(raw)
        if isinstance(parsed, Mapping) and "action" in parsed:
            return dict(parsed)
    return None


def _try_json(value: str) -> Any:
    try:
        return json.loads(value)
    except (json.JSONDecodeError, ValueError):
        return None


def _coerce_timestamps(value: Any) -> list[float]:
    if not isinstance(value, Sequence) or isinstance(value, str | bytes):
        return []
    seen: dict[float, None] = {}
    for item in value:
        seconds = _coerce_float(item)
        if seconds is not None and seconds >= 0:
            seen.setdefault(round(seconds, 3), None)
    return list(seen)


def _coerce_float(value: Any) -> float | None:
    if isinstance(value, bool):
        return None
    if isinstance(value, int | float):
        return float(value)
    if isinstance(value, str):
        try:
            return float(value)
        except ValueError:
            return None
    return None


def _first_validation_error(exc: ValidationError) -> str:
    errors = exc.errors(include_url=False)
    if not errors:
        return str(exc)
    first = errors[0]
    location = ".".join(str(part) for part in first.get("loc", ())) or "summary"
    return f"{location}: {first.get('msg', 'invalid')}"


def _fmt_sec(value: float | None) -> str:
    if value is None:
        return "?"
    text = f"{value:.3f}".rstrip("0").rstrip(".")
    return text or "0"


def _fmt_clock(seconds: float) -> str:
    total = max(0, round(seconds))
    return f"{total // 60:02d}:{total % 60:02d}"
