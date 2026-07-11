"""Deterministic inspection of rendered preview pixels, audio, and streams."""

from __future__ import annotations

import json
import re
from collections.abc import Iterable, Mapping
from dataclasses import dataclass
from fractions import Fraction
from pathlib import Path
from typing import Any, Literal

from contracts.preview_inspection import PreviewInspectionIssue

from .process import run_media_command

InspectionCheck = Literal["streams", "decode", "black", "freeze", "silence", "loudness"]
ALL_INSPECTION_CHECKS: tuple[InspectionCheck, ...] = (
    "streams",
    "decode",
    "black",
    "freeze",
    "silence",
    "loudness",
)
INSPECTION_TIMEOUT_S = 180.0
DETERMINISTIC_INSPECTION_VERSION = "v1"


@dataclass(frozen=True, slots=True)
class PreviewSnapshot:
    width: int | None = None
    height: int | None = None
    fps: float | None = None
    duration_sec: float | None = None


@dataclass(frozen=True, slots=True)
class PreviewMediaInfo:
    width: int | None
    height: int | None
    fps: float | None
    duration_sec: float | None
    has_video: bool
    has_audio: bool
    channels: int | None


@dataclass(frozen=True, slots=True)
class DeterministicPreviewInspection:
    info: PreviewMediaInfo | None
    issues: tuple[PreviewInspectionIssue, ...]


def inspect_preview_file(
    path: Path,
    *,
    expected: PreviewSnapshot,
    checks: Iterable[InspectionCheck] | None = None,
    ffmpeg_bin: str = "ffmpeg",
    ffprobe_bin: str = "ffprobe",
) -> DeterministicPreviewInspection:
    selected = frozenset(ALL_INSPECTION_CHECKS if checks is None else checks)
    issues: list[PreviewInspectionIssue] = []
    try:
        info = _probe(path, ffprobe_bin=ffprobe_bin)
    except Exception as exc:
        issues.append(
            _issue(
                severity="error",
                category="stream_probe_failed",
                description=f"无法读取成片流信息：{exc}",
                suggested_action="重新渲染预览并检查源素材完整性。",
            )
        )
        if "decode" in selected:
            issues.extend(_decode_issues(path, ffmpeg_bin=ffmpeg_bin))
        return DeterministicPreviewInspection(None, tuple(issues))

    if "streams" in selected:
        issues.extend(_stream_issues(info, expected))
    if "decode" in selected:
        issues.extend(_decode_issues(path, ffmpeg_bin=ffmpeg_bin))
    if "black" in selected and info.has_video:
        issues.extend(_black_issues(path, ffmpeg_bin=ffmpeg_bin))
    if "freeze" in selected and info.has_video:
        issues.extend(_freeze_issues(path, info.duration_sec, ffmpeg_bin=ffmpeg_bin))
    if "silence" in selected and info.has_audio:
        issues.extend(_silence_issues(path, info.duration_sec, ffmpeg_bin=ffmpeg_bin))
    if "loudness" in selected and info.has_audio:
        issues.extend(_loudness_issues(path, ffmpeg_bin=ffmpeg_bin))
    return DeterministicPreviewInspection(info, tuple(issues))


def _probe(path: Path, *, ffprobe_bin: str) -> PreviewMediaInfo:
    result = run_media_command(
        [
            ffprobe_bin,
            "-v",
            "error",
            "-print_format",
            "json",
            "-show_format",
            "-show_streams",
            str(path),
        ],
        timeout=INSPECTION_TIMEOUT_S,
    )
    if result.returncode != 0:
        raise RuntimeError(_tail(result.stderr) or "ffprobe failed")
    payload = json.loads(result.stdout)
    if not isinstance(payload, Mapping):
        raise RuntimeError("ffprobe returned invalid JSON")
    streams = payload.get("streams")
    stream_rows = streams if isinstance(streams, list) else []
    video = next(
        (
            item
            for item in stream_rows
            if isinstance(item, Mapping) and item.get("codec_type") == "video"
        ),
        None,
    )
    audio = next(
        (
            item
            for item in stream_rows
            if isinstance(item, Mapping) and item.get("codec_type") == "audio"
        ),
        None,
    )
    raw_format = payload.get("format")
    format_row: Mapping[str, Any] = raw_format if isinstance(raw_format, Mapping) else {}
    duration = _number(format_row.get("duration"))
    if duration is None and isinstance(video, Mapping):
        duration = _number(video.get("duration"))
    fps = _fraction(video.get("avg_frame_rate")) if isinstance(video, Mapping) else None
    return PreviewMediaInfo(
        width=_integer(video.get("width")) if isinstance(video, Mapping) else None,
        height=_integer(video.get("height")) if isinstance(video, Mapping) else None,
        fps=fps,
        duration_sec=duration,
        has_video=video is not None,
        has_audio=audio is not None,
        channels=_integer(audio.get("channels")) if isinstance(audio, Mapping) else None,
    )


def _stream_issues(
    info: PreviewMediaInfo, expected: PreviewSnapshot
) -> list[PreviewInspectionIssue]:
    issues: list[PreviewInspectionIssue] = []
    if not info.has_video:
        issues.append(
            _issue(
                severity="error",
                category="video_stream_missing",
                description="成片没有视频流。",
                suggested_action="重新渲染预览。",
            )
        )
    if not info.has_audio:
        issues.append(
            _issue(
                severity="error",
                category="audio_stream_missing",
                description="成片没有音频流。",
                suggested_action="检查时间线音轨与渲染混音。",
            )
        )
    elif info.channels is None or info.channels < 1 or info.channels > 2:
        issues.append(
            _issue(
                severity="warning",
                category="audio_channels",
                description=f"音频声道数异常：{info.channels!s}。",
                metric=f"channels={info.channels!s}",
                suggested_action="检查声道映射并输出单声道或双声道。",
            )
        )
    expected_values = (expected.width, expected.height, expected.fps, expected.duration_sec)
    if all(value is None for value in expected_values):
        issues.append(
            _issue(
                severity="info",
                category="snapshot_unavailable",
                description="该存量预览没有渲染参数快照，已跳过尺寸、帧率和时长对账。",
            )
        )
        return issues
    if expected.width is not None and info.width != expected.width:
        issues.append(_mismatch("width", info.width, expected.width))
    if expected.height is not None and info.height != expected.height:
        issues.append(_mismatch("height", info.height, expected.height))
    if expected.fps is not None and (info.fps is None or abs(info.fps - expected.fps) > 0.01):
        issues.append(_mismatch("fps", info.fps, expected.fps))
    if expected.duration_sec is not None and (
        info.duration_sec is None or abs(info.duration_sec - expected.duration_sec) > 0.5
    ):
        issues.append(_mismatch("duration_sec", info.duration_sec, expected.duration_sec))
    return issues


def _decode_issues(path: Path, *, ffmpeg_bin: str) -> list[PreviewInspectionIssue]:
    result = _ffmpeg(
        [ffmpeg_bin, "-v", "error", "-xerror", "-i", str(path), "-map", "0", "-f", "null", "-"],
    )
    if result.returncode == 0:
        return []
    return [
        _issue(
            severity="error",
            category="decode_integrity",
            description=f"全片解码失败：{_tail(result.stderr) or '未知解码错误'}",
            suggested_action="重新渲染；若仍失败，检查源素材是否损坏。",
        )
    ]


def _black_issues(path: Path, *, ffmpeg_bin: str) -> list[PreviewInspectionIssue]:
    result = _ffmpeg(
        [
            ffmpeg_bin,
            "-hide_banner",
            "-nostats",
            "-loglevel",
            "info",
            "-i",
            str(path),
            "-an",
            "-vf",
            "blackdetect=d=0.10:pix_th=0.10",
            "-f",
            "null",
            "-",
        ]
    )
    if result.returncode != 0:
        return [_check_failed("black", result.stderr)]
    text = result.stderr
    return [
        _issue(
            at_sec=float(start),
            end_sec=float(end),
            severity="warning",
            category="black_frame",
            description="检测到连续黑帧。",
            metric=f"duration={float(duration):.3f}s",
            suggested_action="检查该时间段是否缺素材或转场渲染异常。",
        )
        for start, end, duration in re.findall(
            r"black_start:([0-9.]+)\s+black_end:([0-9.]+)\s+black_duration:([0-9.]+)",
            text,
        )
    ]


def _freeze_issues(
    path: Path, duration_sec: float | None, *, ffmpeg_bin: str
) -> list[PreviewInspectionIssue]:
    result = _ffmpeg(
        [
            ffmpeg_bin,
            "-hide_banner",
            "-nostats",
            "-loglevel",
            "info",
            "-i",
            str(path),
            "-an",
            "-vf",
            "freezedetect=n=-60dB:d=0.20",
            "-f",
            "null",
            "-",
        ]
    )
    if result.returncode != 0:
        return [_check_failed("freeze", result.stderr)]
    starts = [float(value) for value in re.findall(r"freeze_start:\s*([0-9.]+)", result.stderr)]
    ends = [float(value) for value in re.findall(r"freeze_end:\s*([0-9.]+)", result.stderr)]
    issues: list[PreviewInspectionIssue] = []
    for index, start in enumerate(starts):
        end = ends[index] if index < len(ends) else duration_sec
        issues.append(
            _issue(
                at_sec=start,
                end_sec=end,
                severity="warning",
                category="freeze_frame",
                description="检测到持续冻结画面。",
                metric=None if end is None else f"duration={max(0.0, end - start):.3f}s",
                suggested_action="确认该静止画面是否符合剪辑意图。",
            )
        )
    return issues


def _silence_issues(
    path: Path, duration_sec: float | None, *, ffmpeg_bin: str
) -> list[PreviewInspectionIssue]:
    result = _ffmpeg(
        [
            ffmpeg_bin,
            "-hide_banner",
            "-nostats",
            "-loglevel",
            "info",
            "-i",
            str(path),
            "-vn",
            "-af",
            "silencedetect=noise=-50dB:d=0.10",
            "-f",
            "null",
            "-",
        ]
    )
    if result.returncode != 0:
        return [_check_failed("silence", result.stderr)]
    starts = [float(value) for value in re.findall(r"silence_start:\s*([0-9.]+)", result.stderr)]
    ends = [float(value) for value in re.findall(r"silence_end:\s*([0-9.]+)", result.stderr)]
    issues: list[PreviewInspectionIssue] = []
    for index, start in enumerate(starts):
        end = ends[index] if index < len(ends) else duration_sec
        issues.append(
            _issue(
                at_sec=start,
                end_sec=end,
                severity="warning",
                category="silence",
                description="检测到连续静音段。",
                metric=None if end is None else f"duration={max(0.0, end - start):.3f}s",
                suggested_action="确认静音是否有意；否则检查混音与素材音轨。",
            )
        )
    return issues


def _loudness_issues(path: Path, *, ffmpeg_bin: str) -> list[PreviewInspectionIssue]:
    result = _ffmpeg(
        [
            ffmpeg_bin,
            "-hide_banner",
            "-nostats",
            "-loglevel",
            "info",
            "-i",
            str(path),
            "-vn",
            "-filter_complex",
            "ebur128=peak=true",
            "-f",
            "null",
            "-",
        ]
    )
    if result.returncode != 0:
        return [_check_failed("loudness", result.stderr)]
    integrated = _last_number(result.stderr, r"\bI:\s*(-?(?:inf|[0-9.]+))\s+LUFS")
    peak = _last_number(result.stderr, r"\bPeak:\s*(-?(?:inf|[0-9.]+))\s+dBFS")
    issues: list[PreviewInspectionIssue] = []
    if integrated is not None and (integrated < -24 or integrated > -10):
        issues.append(
            _issue(
                severity="warning",
                category="loudness",
                description="综合响度偏离常用成片范围。",
                metric=f"integrated_lufs={integrated:.2f}",
                suggested_action="复核响度归一化目标。",
            )
        )
    if peak is not None and peak > -1.0:
        issues.append(
            _issue(
                severity="error",
                category="clipping_risk",
                description="True Peak 过高，存在削波风险。",
                metric=f"true_peak_dbfs={peak:.2f}",
                suggested_action="降低增益并重新渲染。",
            )
        )
    return issues


def _ffmpeg(command: list[str]) -> Any:
    command[1:1] = ["-threads", "1"]
    return run_media_command(
        command,
        timeout=INSPECTION_TIMEOUT_S,
        decode_intensive=True,
    )


def _mismatch(field: str, actual: object, expected: object) -> PreviewInspectionIssue:
    return _issue(
        severity="error",
        category="render_snapshot_mismatch",
        description=f"实际 {field} 与渲染快照不一致。",
        metric=f"actual={actual!s}; expected={expected!s}",
        suggested_action="重新渲染该时间线版本。",
    )


def _check_failed(check: str, stderr: str) -> PreviewInspectionIssue:
    return _issue(
        severity="error",
        category=f"{check}_check_failed",
        description=f"成片 {check} 检查执行失败：{_tail(stderr) or '未知 ffmpeg 错误'}",
        suggested_action="重试成片检查；若仍失败，检查预览文件与本机 ffmpeg。",
    )


def _issue(
    *,
    severity: Literal["info", "warning", "error"],
    category: str,
    description: str,
    at_sec: float | None = None,
    end_sec: float | None = None,
    metric: str | None = None,
    suggested_action: str | None = None,
) -> PreviewInspectionIssue:
    return PreviewInspectionIssue(
        at_sec=at_sec,
        end_sec=end_sec,
        severity=severity,
        category=category,
        description=description,
        metric=metric,
        suggested_action=suggested_action,
    )


def _number(value: Any) -> float | None:
    if isinstance(value, bool) or value is None:
        return None
    try:
        return float(value)
    except (TypeError, ValueError):
        return None


def _integer(value: Any) -> int | None:
    number = _number(value)
    return None if number is None else int(number)


def _fraction(value: Any) -> float | None:
    if not isinstance(value, str) or not value or value == "0/0":
        return None
    try:
        return float(Fraction(value))
    except (ValueError, ZeroDivisionError):
        return None


def _last_number(text: str, pattern: str) -> float | None:
    matches = re.findall(pattern, text, flags=re.IGNORECASE)
    if not matches:
        return None
    raw = matches[-1]
    if str(raw).lower() in {"inf", "-inf"}:
        return None
    return float(raw)


def _tail(text: str, *, max_lines: int = 12) -> str:
    return "\n".join(line for line in text.strip().splitlines()[-max_lines:] if line)
