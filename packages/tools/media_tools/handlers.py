"""Frame viewing media tool handlers."""

from __future__ import annotations

import asyncio
import base64
import subprocess
import threading
from collections.abc import Awaitable, Callable, Sequence
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Literal, cast

from contracts.provider import ProviderResult
from contracts.timeline import TimelineMediaClip, TimelineState
from contracts.tool_result import ToolError, ToolResult
from providers import VLM_ANNOTATION, ProviderRequest
from storage.workspace_paths import WorkspacePaths, resolve_asset_path
from timeline import get_timeline_version
from tools.context import ToolExecutionContext
from tools.specs import MediaViewFramesInput

FrameStatus = Literal["ready", "no_clip", "extract_failed", "sampled"]

DEFAULT_QUESTION = "请用简体中文描述每帧的主体、场景、动作、构图和画面质量，并给出整体判断。"


@dataclass(frozen=True, slots=True)
class FrameTarget:
    requested_sec: float
    status: FrameStatus
    asset_id: str | None = None
    source_sec: float | None = None
    path: Path | None = None
    timeline_version: int | None = None
    timeline_clip_id: str | None = None
    clip_id: str | None = None
    note: str | None = None


@dataclass(frozen=True, slots=True)
class ExtractedFrame:
    frame_index: int
    target: FrameTarget
    data_uri: str


@dataclass(frozen=True, slots=True)
class VlmAnswer:
    frame_descriptions: dict[int, str]
    overall_answer: str


class FrameExtractionError(RuntimeError):
    """Raised when ffmpeg cannot extract a requested frame."""


def view_frames(input_model: MediaViewFramesInput, context: ToolExecutionContext) -> ToolResult:
    tool_name = "media.view_frames"
    case_state = context.case_state
    if case_state is None:
        return _failed(tool_name, context, "missing_case", "需要当前 Case")
    if context.readonly_connection is None:
        return _failed(tool_name, context, "missing_connection", "需要只读仓库连接")
    if len(input_model.target.at_sec) > input_model.max_frames:
        return _failed(
            tool_name,
            context,
            "invalid_request",
            "请求的抽帧数量超过 max_frames",
        )

    try:
        paths = _workspace_paths(context)
        targets = _frame_targets(input_model, context, paths=paths)
    except FileNotFoundError as exc:
        return _failed(tool_name, context, "asset_not_found", str(exc))
    except KeyError as exc:
        return _failed(tool_name, context, "timeline_not_found", str(exc))
    except ValueError as exc:
        error_code = "missing_workspace" if "workspace_path" in str(exc) else "timeline_missing"
        return _failed(tool_name, context, error_code, str(exc))

    gateway_call = _provider_gateway_call(context)
    if gateway_call is None:
        return _degraded_result(
            tool_name,
            context,
            targets,
            reason="VLM 通道不可用，无法描述画面。",
            question=input_model.question,
        )

    extracted: list[ExtractedFrame] = []
    observed_targets: list[FrameTarget] = []
    frame_metadata: list[dict[str, Any]] = []
    for target in targets:
        if target.status == "no_clip":
            observed_targets.append(target)
            frame_metadata.append(_frame_metadata(target))
            continue
        if target.path is None or target.source_sec is None:
            failed_target = _target_with_status(target, "extract_failed", "缺少素材路径或时间")
            observed_targets.append(failed_target)
            frame_metadata.append(_frame_metadata(failed_target))
            continue
        try:
            data_uri = _extract_frame_data_uri(target.path, target.source_sec)
        except FrameExtractionError as exc:
            failed_target = _target_with_status(target, "extract_failed", str(exc))
            observed_targets.append(failed_target)
            frame_metadata.append(_frame_metadata(failed_target))
            continue
        sampled_target = _target_with_status(target, "sampled", None)
        observed_targets.append(sampled_target)
        frame = ExtractedFrame(
            frame_index=len(extracted) + 1,
            target=sampled_target,
            data_uri=data_uri,
        )
        extracted.append(frame)
        frame_metadata.append(_frame_metadata(sampled_target, frame_index=frame.frame_index))

    if not extracted:
        if any(item["status"] == "extract_failed" for item in frame_metadata):
            return _failed(
                tool_name,
                context,
                "frame_extract_failed",
                "所有请求帧都抽取失败",
                data={"frames": frame_metadata},
            )
        return ToolResult(
            tool_call_id=context.tool_call_id,
            tool_name=tool_name,
            status="succeeded",
            observation=_observation_without_vlm(
                observed_targets,
                reason="没有可抽取的画面帧。",
            ),
            data={
                "case_id": case_state.case_id,
                "question": input_model.question,
                "frames": frame_metadata,
                "overall_answer": "没有可抽取的画面帧。",
            },
        )

    request = ProviderRequest(
        capability=VLM_ANNOTATION,
        request_id=f"view_frames_{context.tool_call_id}",
        case_id=case_state.case_id,
        payload={
            "messages": _vlm_messages(extracted, question=input_model.question),
            "params": {"temperature": 0, "response_format": {"type": "json_object"}},
            "question": input_model.question,
            "frames": [
                _frame_metadata(frame.target, frame_index=frame.frame_index) for frame in extracted
            ],
        },
    )
    try:
        gateway_result = _run_async_sync(gateway_call(request))
    except Exception as exc:
        return _degraded_result(
            tool_name,
            context,
            observed_targets,
            reason=f"VLM 调用失败：{exc}",
            question=input_model.question,
            frame_metadata=frame_metadata,
        )
    result = cast(ProviderResult, gateway_result.result)
    if result.error is not None:
        return _degraded_result(
            tool_name,
            context,
            observed_targets,
            reason=f"VLM 调用失败：{result.error.error_code}: {result.error.message}",
            question=input_model.question,
            frame_metadata=frame_metadata,
        )

    answer = _parse_vlm_answer(result.normalized_output, extracted)
    observation = _observation_from_answer(observed_targets, extracted, answer)
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name=tool_name,
        status="succeeded",
        observation=observation,
        data={
            "case_id": case_state.case_id,
            "question": input_model.question,
            "frames": frame_metadata,
            "overall_answer": answer.overall_answer,
            "provider_request_id": result.request_id,
        },
    )


def _frame_targets(
    input_model: MediaViewFramesInput,
    context: ToolExecutionContext,
    *,
    paths: WorkspacePaths,
) -> list[FrameTarget]:
    target = input_model.target
    if target.asset_id is not None:
        path = _resolve_existing_asset_path(target.asset_id, context, paths=paths)
        return [
            FrameTarget(
                requested_sec=seconds,
                status="ready",
                asset_id=target.asset_id,
                source_sec=seconds,
                path=path,
            )
            for seconds in target.at_sec
        ]
    return _timeline_frame_targets(input_model, context, paths=paths)


def _timeline_frame_targets(
    input_model: MediaViewFramesInput,
    context: ToolExecutionContext,
    *,
    paths: WorkspacePaths,
) -> list[FrameTarget]:
    case_state = context.case_state
    assert case_state is not None
    assert context.readonly_connection is not None
    version = (
        input_model.target.timeline_version
        if input_model.target.timeline_version is not None
        else case_state.timeline_current_version
    )
    if version is None:
        raise ValueError("当前 Case 没有时间线版本")
    record = get_timeline_version(context.readonly_connection, case_state.case_id, version)
    if record is None:
        raise KeyError(f"找不到时间线版本：v{version}")
    clips = _visual_base_clips(record.timeline)
    path_cache: dict[str, Path] = {}
    targets: list[FrameTarget] = []
    for seconds in input_model.target.at_sec:
        clip = _covering_visual_clip(record.timeline, clips, seconds)
        if clip is None:
            targets.append(
                FrameTarget(
                    requested_sec=seconds,
                    status="no_clip",
                    timeline_version=record.version,
                    note="该时刻无画面 clip",
                )
            )
            continue
        path = path_cache.get(clip.asset_id)
        if path is None:
            path = _resolve_existing_asset_path(clip.asset_id, context, paths=paths)
            path_cache[clip.asset_id] = path
        source_sec = _source_second_for_clip(record.timeline, clip, seconds)
        targets.append(
            FrameTarget(
                requested_sec=seconds,
                status="ready",
                asset_id=clip.asset_id,
                source_sec=source_sec,
                path=path,
                timeline_version=record.version,
                timeline_clip_id=clip.timeline_clip_id,
                clip_id=clip.clip_id,
            )
        )
    return targets


def _resolve_existing_asset_path(
    asset_id: str,
    context: ToolExecutionContext,
    *,
    paths: WorkspacePaths,
) -> Path:
    assert context.readonly_connection is not None
    try:
        path = resolve_asset_path(asset_id, connection=context.readonly_connection, paths=paths)
    except FileNotFoundError as exc:
        raise FileNotFoundError(f"素材不存在或文件不可读：{asset_id}") from exc
    if not path.is_file():
        raise FileNotFoundError(f"素材不存在或文件不可读：{asset_id}")
    return path


def extract_frame_data_uri(
    path: Path,
    seconds: float,
    *,
    ffmpeg_bin: str = "ffmpeg",
) -> str:
    """Extract a single frame at ``seconds`` and return a JPEG ``data:`` URI.

    Public so the understanding subagent (Spec C §C3) can reuse the exact ffmpeg
    抽帧 behavior without view_frames' embedded VLM call. ``view_frames`` keeps
    calling the ``_extract_frame_data_uri`` alias below so existing monkeypatch
    seams stay intact.
    """

    command = [
        ffmpeg_bin,
        "-hide_banner",
        "-loglevel",
        "error",
        "-ss",
        f"{seconds:.6f}",
        "-i",
        str(path),
        "-frames:v",
        "1",
        "-vf",
        "scale=768:768:force_original_aspect_ratio=decrease",
        "-f",
        "image2pipe",
        "-vcodec",
        "mjpeg",
        "pipe:1",
    ]
    result = subprocess.run(command, capture_output=True, check=False)
    if result.returncode != 0:
        raise FrameExtractionError(_stderr_summary(result.stderr) or "ffmpeg 抽帧失败")
    if not result.stdout:
        raise FrameExtractionError("ffmpeg 没有输出帧")
    payload = base64.b64encode(result.stdout).decode("ascii")
    return f"data:image/jpeg;base64,{payload}"


# Backward-compatible private alias: ``view_frames`` (and its tests) call this
# name; keeping it lets monkeypatching the private name keep working unchanged.
_extract_frame_data_uri = extract_frame_data_uri


def _vlm_messages(
    frames: Sequence[ExtractedFrame],
    *,
    question: str | None,
) -> list[dict[str, Any]]:
    prompt = (
        "请只返回 JSON，格式为 "
        '{"frames":[{"frame_index":1,"description":"..."}],'
        '"overall_answer":"..."}。'
        "frame_index 必须使用输入中的编号。"
        f"问题：{question or DEFAULT_QUESTION}"
    )
    content: list[dict[str, Any]] = [{"type": "text", "text": prompt}]
    for frame in frames:
        target = frame.target
        label = (
            f"frame_index={frame.frame_index}; "
            f"timeline_t={_format_sec(target.requested_sec)}s; "
            f"asset_id={target.asset_id}; "
            f"source_t={_format_sec(target.source_sec or 0.0)}s; "
            f"timeline_clip_id={target.timeline_clip_id or ''}; "
            f"clip_id={target.clip_id or ''}"
        )
        content.append({"type": "text", "text": label})
        content.append({"type": "image_url", "image_url": {"url": frame.data_uri}})
    return [{"role": "user", "content": content}]


def _parse_vlm_answer(
    output: dict[str, Any],
    frames: Sequence[ExtractedFrame],
) -> VlmAnswer:
    descriptions: dict[int, str] = {}
    raw_frames = output.get("frames")
    if isinstance(raw_frames, list):
        fallback_index = 1
        for item in raw_frames:
            if not isinstance(item, dict):
                continue
            raw_index = item.get("frame_index", item.get("index", fallback_index))
            frame_index = raw_index if isinstance(raw_index, int) else fallback_index
            raw_description = item.get("description", item.get("summary", item.get("answer")))
            if isinstance(raw_description, str) and raw_description.strip():
                descriptions[frame_index] = raw_description.strip()
            fallback_index += 1

    overall = _first_string(
        output,
        ("overall_answer", "answer", "summary", "content", "text"),
    )
    if overall is None and descriptions:
        overall = "已按帧返回描述。"
    if overall is None:
        overall = "VLM 已返回结果，但没有可解析的文字。"
    for frame in frames:
        descriptions.setdefault(frame.frame_index, "VLM 未返回该帧描述。")
    return VlmAnswer(frame_descriptions=descriptions, overall_answer=overall)


def _observation_from_answer(
    targets: Sequence[FrameTarget],
    extracted: Sequence[ExtractedFrame],
    answer: VlmAnswer,
) -> str:
    frame_index_by_key = {_target_key(frame.target): frame.frame_index for frame in extracted}
    lines: list[str] = []
    for target in targets:
        if target.status == "no_clip":
            lines.append(f"[t={_format_sec(target.requested_sec)}s] 该时刻无画面 clip。")
            continue
        if target.status == "extract_failed":
            lines.append(
                f"[t={_format_sec(target.requested_sec)}s] 抽帧失败：{target.note or '未知原因'}。"
            )
            continue
        frame_index = frame_index_by_key.get(_target_key(target))
        description = (
            answer.frame_descriptions.get(frame_index)
            if frame_index is not None
            else "VLM 未返回该帧描述。"
        )
        lines.append(f"[t={_format_sec(target.requested_sec)}s] {description}")
    lines.append(f"整体回答：{answer.overall_answer}")
    return "\n".join(lines)


def _observation_without_vlm(
    targets: Sequence[FrameTarget],
    *,
    reason: str,
) -> str:
    lines: list[str] = []
    for target in targets:
        if target.status == "no_clip":
            lines.append(f"[t={_format_sec(target.requested_sec)}s] 该时刻无画面 clip。")
        else:
            lines.append(f"[t={_format_sec(target.requested_sec)}s] {reason}")
    lines.append(f"整体回答：{reason}")
    return "\n".join(lines)


def _degraded_result(
    tool_name: str,
    context: ToolExecutionContext,
    targets: Sequence[FrameTarget],
    *,
    reason: str,
    question: str | None,
    frame_metadata: list[dict[str, Any]] | None = None,
) -> ToolResult:
    case_id = context.case_state.case_id if context.case_state is not None else None
    frames = frame_metadata if frame_metadata is not None else [_frame_metadata(t) for t in targets]
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name=tool_name,
        status="succeeded",
        observation=_observation_without_vlm(targets, reason=reason),
        data={
            "case_id": case_id,
            "question": question,
            "frames": frames,
            "degraded": {"capability": VLM_ANNOTATION, "reason": reason},
            "overall_answer": reason,
        },
    )


def _visual_base_clips(timeline: TimelineState) -> tuple[TimelineMediaClip, ...]:
    for track in timeline.tracks:
        if track.track_id == "visual_base":
            return tuple(clip for clip in track.clips if isinstance(clip, TimelineMediaClip))
    return ()


def _covering_visual_clip(
    timeline: TimelineState,
    clips: Sequence[TimelineMediaClip],
    seconds: float,
) -> TimelineMediaClip | None:
    frame = int(seconds * timeline.fps)
    for clip in clips:
        if clip.timeline_start_frame <= frame < clip.timeline_end_frame:
            return clip
    return None


def _source_second_for_clip(
    timeline: TimelineState,
    clip: TimelineMediaClip,
    seconds: float,
) -> float:
    timeline_frame = int(seconds * timeline.fps)
    offset = round((timeline_frame - clip.timeline_start_frame) * clip.playback_rate)
    source_frame = clip.source_start_frame + offset
    source_frame = max(clip.source_start_frame, min(clip.source_end_frame - 1, source_frame))
    return source_frame / timeline.fps


def _workspace_paths(context: ToolExecutionContext) -> WorkspacePaths:
    raw_paths = context.metadata.get("workspace_paths")
    if isinstance(raw_paths, WorkspacePaths):
        return raw_paths
    raw_root = context.metadata.get("workspace_path")
    if isinstance(raw_root, str | Path):
        return WorkspacePaths.from_root(raw_root)
    raise ValueError("media.view_frames 需要 workspace_path 元数据")


def _provider_gateway_call(
    context: ToolExecutionContext,
) -> Callable[[ProviderRequest], Awaitable[Any]] | None:
    gateway = context.metadata.get("provider_gateway")
    call = getattr(gateway, "call", None)
    if not callable(call):
        return None
    return cast(Callable[[ProviderRequest], Awaitable[Any]], call)


def _target_with_status(
    target: FrameTarget,
    status: FrameStatus,
    note: str | None,
) -> FrameTarget:
    return FrameTarget(
        requested_sec=target.requested_sec,
        status=status,
        asset_id=target.asset_id,
        source_sec=target.source_sec,
        path=target.path,
        timeline_version=target.timeline_version,
        timeline_clip_id=target.timeline_clip_id,
        clip_id=target.clip_id,
        note=note,
    )


def _frame_metadata(
    target: FrameTarget,
    *,
    frame_index: int | None = None,
) -> dict[str, Any]:
    data: dict[str, Any] = {
        "requested_sec": target.requested_sec,
        "status": target.status,
        "asset_id": target.asset_id,
        "source_sec": target.source_sec,
        "timeline_version": target.timeline_version,
        "timeline_clip_id": target.timeline_clip_id,
        "clip_id": target.clip_id,
    }
    if frame_index is not None:
        data["frame_index"] = frame_index
    if target.note is not None:
        data["note"] = target.note
    return data


def _target_key(target: FrameTarget) -> tuple[float, str | None, str | None, float | None]:
    return (
        target.requested_sec,
        target.asset_id,
        target.timeline_clip_id,
        target.source_sec,
    )


def _first_string(output: dict[str, Any], keys: Sequence[str]) -> str | None:
    for key in keys:
        value = output.get(key)
        if isinstance(value, str) and value.strip():
            return value.strip()
    return None


def _format_sec(value: float) -> str:
    text = f"{value:.3f}".rstrip("0").rstrip(".")
    return text or "0"


def _stderr_summary(stderr: bytes, *, max_lines: int = 12) -> str:
    text = stderr.decode(errors="replace")
    return "\n".join(line for line in text.strip().splitlines()[-max_lines:] if line)


def _run_async_sync(awaitable: Awaitable[Any]) -> Any:
    async def _wrapped() -> Any:
        return await awaitable

    try:
        asyncio.get_running_loop()
    except RuntimeError:
        return asyncio.run(_wrapped())

    result: dict[str, Any] = {}

    def runner() -> None:
        try:
            result["value"] = asyncio.run(_wrapped())
        except BaseException as exc:  # pragma: no cover - defensive thread bridge
            result["error"] = exc

    thread = threading.Thread(target=runner, daemon=True)
    thread.start()
    thread.join()
    error = result.get("error")
    if isinstance(error, BaseException):
        raise error
    return result["value"]


def _failed(
    tool_name: str,
    context: ToolExecutionContext,
    error_code: str,
    message: str,
    *,
    data: dict[str, Any] | None = None,
) -> ToolResult:
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name=tool_name,
        status="failed",
        observation=message,
        data=data or {},
        error=ToolError(error_code=error_code, message=message, retryable=False),
    )
