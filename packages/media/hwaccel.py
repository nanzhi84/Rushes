"""macOS VideoToolbox 硬件解码探测（proxy / thumbnails / shots 共用，进程级缓存）。

对齐 `proxy.py` 的编码器探测缓存模式：每个 ffmpeg_bin 只探一次「videotoolbox 解码是否可用」，
把 `-hwaccel videotoolbox` 放在 `-i` 之前作为输入解码参数。某条具体命令硬解运行期失败，
由各调用方各自回落软解——本模块只回答「本机 ffmpeg 是否编入了 videotoolbox 硬解」。
"""

from __future__ import annotations

import subprocess
import sys

_HWACCEL = "videotoolbox"
# 每个 ffmpeg_bin 只探测一次 `-hwaccels` 列表（探测要起一次 ffmpeg 子进程）。
_decode_available_cache: dict[str, bool] = {}


def is_macos() -> bool:
    return sys.platform == "darwin"


def videotoolbox_decode_available(ffmpeg_bin: str = "ffmpeg") -> bool:
    cached = _decode_available_cache.get(ffmpeg_bin)
    if cached is not None:
        return cached
    available = _probe_decode_available(ffmpeg_bin)
    _decode_available_cache[ffmpeg_bin] = available
    return available


def hwaccel_decode_args(ffmpeg_bin: str = "ffmpeg") -> list[str]:
    """返回放在 `-i` 之前的输入硬解参数；不可用时返回空列表（软解）。"""

    if videotoolbox_decode_available(ffmpeg_bin):
        return ["-hwaccel", _HWACCEL]
    return []


def _probe_decode_available(ffmpeg_bin: str) -> bool:
    # 仅 macOS 才考虑硬件解码；探测本机 ffmpeg 是否编入 videotoolbox。
    if not is_macos():
        return False
    try:
        result = subprocess.run(
            [ffmpeg_bin, "-hide_banner", "-hwaccels"],
            capture_output=True,
            check=False,
            text=True,
            timeout=10,
        )
    except (OSError, subprocess.SubprocessError):
        return False
    return result.returncode == 0 and _HWACCEL in result.stdout
