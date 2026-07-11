"""ffprobe + PyAV media probing for AssetProbe contracts."""

from __future__ import annotations

import importlib
import json
from fractions import Fraction
from pathlib import Path
from typing import Any, cast

from contracts.asset import AssetKind, AssetProbe

from .process import run_media_command

# 浏览器（macOS Safari/Chrome）可直接播放的编码：源可直读即播，无需转码代理兜底。
# 视频 h264/hevc 走 macOS 硬解；其余（prores/dnxhd/vp9/av1/未知等）才需要 proxy 回落。
BROWSER_PLAYABLE_VIDEO_FORMATS = {
    "h264": frozenset({"yuv420p", "yuvj420p"}),
    "hevc": frozenset({"yuv420p", "yuv420p10le"}),
}
BROWSER_PLAYABLE_AUDIO_CODECS = frozenset(
    {
        "aac",
        "mp3",
        "flac",
        "opus",
        "vorbis",
        "alac",
        "pcm_s16le",
        "pcm_s24le",
        "pcm_s32le",
        "pcm_u8",
        "pcm_f32le",
    }
)


class MediaProbeError(RuntimeError):
    """Raised when ffprobe/PyAV cannot read media metadata."""


def probe_media(path: str | Path, *, ffprobe_bin: str = "ffprobe") -> AssetProbe:
    source = Path(path).expanduser().resolve(strict=True)
    payload = _run_ffprobe(source, ffprobe_bin=ffprobe_bin)
    probe = _probe_from_ffprobe(payload)
    return _correct_with_pyav(source, probe)


def probe_stream_codec(
    path: str | Path,
    *,
    stream_type: str,
    ffprobe_bin: str = "ffprobe",
) -> str | None:
    """轻量读取首条指定类型流的 `codec_name`（只读元数据、不解码，毫秒级）。

    ``stream_type`` 为 ffprobe 的流选择符（``"v"`` 视频 / ``"a"`` 音频）。探测失败/无该流返回
    ``None``，调用方按「未知」兜底处理。
    """

    source = Path(path).expanduser().resolve(strict=False)
    command = [
        ffprobe_bin,
        "-v",
        "error",
        "-select_streams",
        f"{stream_type}:0",
        "-show_entries",
        "stream=codec_name",
        "-of",
        "default=noprint_wrappers=1:nokey=1",
        str(source),
    ]
    try:
        result = run_media_command(command, text=True, timeout=30)
    except (OSError, TimeoutError):
        return None
    if result.returncode != 0:
        return None
    for line in result.stdout.strip().splitlines():
        codec = line.strip().lower()
        if codec:
            return codec
    return None


def probe_video_stream_format(
    path: str | Path,
    *,
    ffprobe_bin: str = "ffprobe",
) -> tuple[str, str] | None:
    """Read the first video stream's codec and pixel format for browser playability."""

    source = Path(path).expanduser().resolve(strict=False)
    try:
        result = run_media_command(
            [
                ffprobe_bin,
                "-v",
                "error",
                "-select_streams",
                "v:0",
                "-show_entries",
                "stream=codec_name,pix_fmt",
                "-of",
                "json",
                str(source),
            ],
            text=True,
            timeout=30,
        )
    except (OSError, TimeoutError):
        return None
    if result.returncode != 0:
        return None
    try:
        payload = json.loads(result.stdout)
    except json.JSONDecodeError:
        return None
    streams = payload.get("streams") if isinstance(payload, dict) else None
    if not isinstance(streams, list) or not streams or not isinstance(streams[0], dict):
        return None
    codec = streams[0].get("codec_name")
    pixel_format = streams[0].get("pix_fmt")
    if not isinstance(codec, str) or not isinstance(pixel_format, str):
        return None
    return codec.lower(), pixel_format.lower()


def asset_needs_proxy(
    kind: AssetKind | str,
    source_path: str | Path,
    *,
    ffprobe_bin: str = "ffprobe",
) -> bool:
    """代理只为「浏览器播不动的格式」兜底回落：可播即无需转码代理，省掉整段转码。

    - 视频：h264/hevc 且像素格式为浏览器硬解支持的 4:2:0 组合才可播；其余需代理。
    - 音频：编码 ∈ 常见可播集（aac/mp3/flac/opus/pcm…）可播，其余需代理。
    - 图片：走缩略图/原图直连，从不用媒体代理。
    - 字体：无媒体代理。
    未知/探测失败一律按「需代理」兜底（宁可多转码，也不让素材无法预览）。
    """

    kind_value = kind.value if isinstance(kind, AssetKind) else str(kind)
    if kind_value in (AssetKind.FONT.value, AssetKind.IMAGE.value):
        return False
    if kind_value == AssetKind.VIDEO.value:
        stream_format = probe_video_stream_format(source_path, ffprobe_bin=ffprobe_bin)
        if stream_format is None:
            return True
        codec, pixel_format = stream_format
        return pixel_format not in BROWSER_PLAYABLE_VIDEO_FORMATS.get(codec, frozenset())
    if kind_value == AssetKind.AUDIO.value:
        audio_codec = probe_stream_codec(source_path, stream_type="a", ffprobe_bin=ffprobe_bin)
        return audio_codec is None or audio_codec not in BROWSER_PLAYABLE_AUDIO_CODECS
    return True


def _run_ffprobe(path: Path, *, ffprobe_bin: str) -> dict[str, Any]:
    command = [
        ffprobe_bin,
        "-v",
        "error",
        "-print_format",
        "json",
        "-show_format",
        "-show_streams",
        str(path),
    ]
    result = run_media_command(command, text=True)
    if result.returncode != 0:
        raise MediaProbeError(_stderr_summary(result.stderr) or "ffprobe failed")
    try:
        payload = json.loads(result.stdout)
    except json.JSONDecodeError as exc:
        raise MediaProbeError("ffprobe did not return valid JSON") from exc
    if not isinstance(payload, dict):
        raise MediaProbeError("ffprobe JSON root must be an object")
    return payload


def _probe_from_ffprobe(payload: dict[str, Any]) -> AssetProbe:
    streams = payload.get("streams", [])
    if not isinstance(streams, list):
        streams = []
    video_stream = _first_stream(streams, "video")
    audio_stream = _first_stream(streams, "audio")
    duration = _duration(payload, video_stream, audio_stream)
    return AssetProbe(
        duration_sec=max(0.0, duration),
        fps=_rate(video_stream.get("avg_frame_rate") if video_stream is not None else None),
        width=_optional_int(video_stream.get("width") if video_stream is not None else None),
        height=_optional_int(video_stream.get("height") if video_stream is not None else None),
        has_audio=audio_stream is not None,
    )


def _correct_with_pyav(path: Path, probe: AssetProbe) -> AssetProbe:
    try:
        av = cast(Any, importlib.import_module("av"))
    except ImportError:
        return probe

    try:
        container = av.open(str(path), mode="r")
    except Exception:
        return probe
    try:
        video_stream = next(
            (stream for stream in container.streams if stream.type == "video"),
            None,
        )
        if video_stream is None:
            return probe
        fps = _float_rate(getattr(video_stream, "average_rate", None)) or probe.fps
        frame_count = _video_frame_count(video_stream, container)
        duration = probe.duration_sec
        if fps is not None and frame_count > 0:
            duration = frame_count / fps
        return probe.model_copy(
            update={
                "duration_sec": max(0.0, duration),
                "fps": fps,
                "width": int(getattr(video_stream, "width", 0) or probe.width or 0) or None,
                "height": int(getattr(video_stream, "height", 0) or probe.height or 0) or None,
            }
        )
    finally:
        container.close()


def _video_frame_count(video_stream: Any, container: Any) -> int:
    """视频帧数：优先容器索引元数据 stream.frames（不解码，毫秒级）。

    元数据帧数在 1080p60 HEVC 上把逐帧解码计数的 ~8 CPU 秒/次省成近乎零（probe_media 在
    poster/index/proxy 各调一次，那是导入风扇狂叫的第二大头），实测样本上两者结果一致。
    但部分容器/流（尤其非 mp4/mov 或转封装产物）nb_frames 缺失或为 0——此时回落到逐帧解码
    计数这条准确但慢的旧路径（罕见，接受其代价），避免时长退化。
    """

    frames = _stream_frame_count(video_stream)
    if frames > 0:
        return frames
    return _decoded_video_frames(container)


def _stream_frame_count(video_stream: Any) -> int:
    try:
        return int(getattr(video_stream, "frames", 0) or 0)
    except (TypeError, ValueError):
        return 0


def _decoded_video_frames(container: Any) -> int:
    count = 0
    try:
        for _frame in container.decode(video=0):
            count += 1
    except Exception:
        return 0
    return count


def _first_stream(streams: list[Any], codec_type: str) -> dict[str, Any] | None:
    for stream in streams:
        if isinstance(stream, dict) and stream.get("codec_type") == codec_type:
            return stream
    return None


def _duration(
    payload: dict[str, Any],
    video_stream: dict[str, Any] | None,
    audio_stream: dict[str, Any] | None,
) -> float:
    candidates: list[Any] = []
    format_payload = payload.get("format")
    if isinstance(format_payload, dict):
        candidates.append(format_payload.get("duration"))
    if video_stream is not None:
        candidates.append(video_stream.get("duration"))
    if audio_stream is not None:
        candidates.append(audio_stream.get("duration"))
    for candidate in candidates:
        try:
            if candidate is not None:
                return float(candidate)
        except (TypeError, ValueError):
            continue
    return 0.0


def _rate(value: Any) -> float | None:
    if not isinstance(value, str) or value in {"", "0/0", "N/A"}:
        return None
    try:
        rate = float(Fraction(value))
    except (ValueError, ZeroDivisionError):
        return None
    return rate if rate > 0 else None


def _float_rate(value: Any) -> float | None:
    if value is None:
        return None
    try:
        rate = float(value)
    except (TypeError, ValueError, ZeroDivisionError):
        return None
    return rate if rate > 0 else None


def _optional_int(value: Any) -> int | None:
    try:
        parsed = int(value)
    except (TypeError, ValueError):
        return None
    return parsed if parsed > 0 else None


def _stderr_summary(stderr: str) -> str:
    return "\n".join(line for line in stderr.strip().splitlines()[-8:] if line)
