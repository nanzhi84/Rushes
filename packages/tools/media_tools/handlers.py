"""Internal frame extraction and VLM-question primitives.

This module deliberately exposes no LLM tool handler. ``media.view_frames`` was
folded into ``understand.materials``; deep understanding, scan, and preview QA
reuse these lower-level primitives inside the harness.
"""

from __future__ import annotations

import base64
import json
import threading
from collections.abc import Mapping, Sequence
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from media.process import run_media_command
from providers import VLM_UNDERSTANDING, ProviderRequest

DEFAULT_FRAME_QUESTION = "请用简体中文描述每帧的主体、场景、动作、构图和画面质量，并给出整体判断。"
FRAME_COMMAND_TIMEOUT_S = 60.0
_IMAGE_MIME_BY_SUFFIX = {
    ".jpg": "image/jpeg",
    ".jpeg": "image/jpeg",
    ".png": "image/png",
    ".gif": "image/gif",
    ".webp": "image/webp",
    ".bmp": "image/bmp",
    ".tif": "image/tiff",
    ".tiff": "image/tiff",
    ".heic": "image/heic",
    ".heif": "image/heif",
    ".svg": "image/svg+xml",
}


class FrameExtractionError(RuntimeError):
    """Raised when ffmpeg cannot extract a requested frame."""


@dataclass(frozen=True, slots=True)
class LabeledImage:
    label: str
    data_uri: str


@dataclass(frozen=True, slots=True)
class FrameQuestionAnswer:
    descriptions: tuple[str, ...]
    overall_answer: str


def extract_frame_data_uri(
    path: Path,
    seconds: float,
    *,
    ffmpeg_bin: str = "ffmpeg",
    cancel_event: threading.Event | None = None,
) -> str:
    """Extract one bounded-size JPEG frame as a data URI."""

    command = [
        ffmpeg_bin,
        "-hide_banner",
        "-loglevel",
        "error",
        "-threads",
        "1",
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
    result = run_media_command(
        command,
        text=False,
        timeout=FRAME_COMMAND_TIMEOUT_S,
        decode_intensive=True,
        cancel_event=cancel_event,
    )
    if result.returncode != 0:
        raise FrameExtractionError(_stderr_summary(result.stderr) or "ffmpeg 抽帧失败")
    if not result.stdout:
        raise FrameExtractionError("ffmpeg 没有输出帧")
    payload = base64.b64encode(result.stdout).decode("ascii")
    return f"data:image/jpeg;base64,{payload}"


def image_path_data_uri(path: Path, *, mime: str | None = None) -> str:
    """Read an already-materialized poster/image into a prompt-safe data URI."""

    raw = path.read_bytes()
    payload = base64.b64encode(raw).decode("ascii")
    resolved_mime = (
        mime
        or _IMAGE_MIME_BY_SUFFIX.get(path.suffix.lower())
        or _image_mime_from_magic(raw)
        or "application/octet-stream"
    )
    return f"data:{resolved_mime};base64,{payload}"


def _image_mime_from_magic(payload: bytes) -> str | None:
    """Identify common images stored under extensionless object hashes."""

    if payload.startswith(b"\xff\xd8\xff"):
        return "image/jpeg"
    if payload.startswith(b"\x89PNG\r\n\x1a\n"):
        return "image/png"
    if payload.startswith((b"GIF87a", b"GIF89a")):
        return "image/gif"
    if payload.startswith(b"RIFF") and payload[8:12] == b"WEBP":
        return "image/webp"
    if payload.startswith(b"BM"):
        return "image/bmp"
    if payload.startswith((b"II*\x00", b"MM\x00*")):
        return "image/tiff"
    if payload[4:12] in {b"ftypheic", b"ftypheix", b"ftyphevc", b"ftyphevx"}:
        return "image/heic"
    if payload[4:12] in {b"ftypmif1", b"ftypmsf1", b"ftypheif"}:
        return "image/heif"
    if payload.lstrip().startswith((b"<svg", b"<?xml")):
        return "image/svg+xml"
    return None


def multimodal_messages(prompt: str, images: Sequence[LabeledImage]) -> list[dict[str, Any]]:
    """Build the common labeled multi-image message format used by internal callers."""

    content: list[dict[str, Any]] = [{"type": "text", "text": prompt}]
    for image in images:
        content.append({"type": "text", "text": image.label})
        content.append({"type": "image_url", "image_url": {"url": image.data_uri}})
    return [{"role": "user", "content": content}]


async def ask_vlm_about_frames(
    images: Sequence[LabeledImage],
    *,
    gateway: Any,
    request_id: str,
    question: str | None = None,
    draft_id: str | None = None,
    model: str | None = None,
) -> FrameQuestionAnswer:
    """Ask VLM about extracted frames without exposing a public tool surface."""

    prompt = (
        "只返回 JSON："
        '{"frames":[{"index":1,"description":"..."}],"overall_answer":"..."}。'
        f"问题：{question or DEFAULT_FRAME_QUESTION}"
    )
    request = ProviderRequest(
        capability=VLM_UNDERSTANDING,
        request_id=request_id,
        draft_id=draft_id,
        model=model,
        payload={
            "messages": multimodal_messages(prompt, images),
            "params": {"temperature": 0, "response_format": {"type": "json_object"}},
        },
    )
    gateway_result = await gateway.call(request)
    result = gateway_result.result
    if result.error is not None:
        raise RuntimeError(f"{result.error.error_code}: {result.error.message}")
    output = _mapping_output(result.normalized_output)
    descriptions = ["VLM 未返回该帧描述。"] * len(images)
    raw_frames = output.get("frames")
    if isinstance(raw_frames, list):
        for fallback, item in enumerate(raw_frames, start=1):
            if not isinstance(item, Mapping):
                continue
            raw_index = item.get("index", item.get("frame_index", fallback))
            description = item.get("description")
            if (
                isinstance(raw_index, int)
                and 1 <= raw_index <= len(descriptions)
                and isinstance(description, str)
                and description.strip()
            ):
                descriptions[raw_index - 1] = description.strip()
    overall = output.get("overall_answer")
    if not isinstance(overall, str) or not overall.strip():
        overall = "已返回逐帧观察。"
    return FrameQuestionAnswer(tuple(descriptions), overall.strip())


def _mapping_output(output: Any) -> dict[str, Any]:
    if isinstance(output, Mapping):
        content = output.get("content")
        if isinstance(content, str):
            try:
                parsed = json.loads(content)
            except ValueError:
                parsed = None
            if isinstance(parsed, Mapping):
                return dict(parsed)
        return dict(output)
    if isinstance(output, str):
        try:
            parsed = json.loads(output)
        except ValueError:
            return {}
        return dict(parsed) if isinstance(parsed, Mapping) else {}
    return {}


def _stderr_summary(stderr: bytes, *, max_lines: int = 12) -> str:
    text = stderr.decode(errors="replace")
    return "\n".join(line for line in text.strip().splitlines()[-max_lines:] if line)
