"""Generate manual M9 path fixtures with real Volcengine TTS plus ffmpeg."""

from __future__ import annotations

import argparse
import asyncio
import shutil
import sys
from pathlib import Path
from typing import Any

_REPO_ROOT_FOR_IMPORTS = Path(__file__).resolve().parents[2]
for _import_path in (_REPO_ROOT_FOR_IMPORTS / "packages", _REPO_ROOT_FOR_IMPORTS):
    if str(_import_path) not in sys.path:
        sys.path.insert(0, str(_import_path))

from client import (  # noqa: E402
    REPO_ROOT,
    RunError,
    ffprobe_duration_s,
    load_dotenv,
    require_executable,
    run_command,
    stage_log,
    unique_id,
)

from providers import TTS_SPEECH, ProviderRequest  # noqa: E402
from providers.volcengine import VolcengineTTSProvider  # noqa: E402

PATH1_TEXT = (
    "呃，大家好，今天我想把这个便携冲牙器的真实使用感受讲清楚。"
    "我之前早晚刷牙都挺认真，但是牙缝里的残留还是经常清不干净。"
    "就是说，第一次用的时候水压不要开太大，先从低档慢慢适应。"
    "我连续用了差不多一周，饭后异味变少，牙套附近也更容易冲干净。"
    "中间有一段我说得比较绕，后面粗剪可以直接删掉。"
    "最后建议大家根据牙龈敏感程度选档位，不要一上来就追求最大水压。"
)

PATH2_SCRIPT = (
    "今天想种草一款桌面香薰加湿器。它的优势不是夸张出雾，"
    "而是小桌面也能放得下，夜间灯光很柔和。通勤回家以后加一点清水，"
    "再开低档雾量，房间不会闷，键盘旁边也不会明显积水。"
    "如果你想要一个提升氛围、又不占地方的小物件，这款更适合放在卧室和书桌。"
)

BROLL_SOURCES = (
    (
        "path2_broll_01.mp4",
        "testsrc2=size=720x1280:rate=30:duration=9",
        "生成 B-roll 1",
    ),
    (
        "path2_broll_02.mp4",
        "smptebars=size=720x1280:rate=30:duration=8",
        "生成 B-roll 2",
    ),
    (
        "path2_broll_03.mp4",
        (
            "color=c=0x1b8a5a:size=720x1280:rate=30:duration=10,"
            "drawbox=x=80:y=140:w=560:h=1000:color=0xf6d365@0.85:t=fill,"
            "drawgrid=w=80:h=80:t=2:c=white@0.45"
        ),
        "生成 B-roll 3",
    ),
)

IMAGE_SOURCE = (
    "color=c=0xf7f3e8:size=1080x1440,"
    "drawbox=x=150:y=220:w=780:h=1000:color=0x2a9d8f@0.9:t=fill,"
    "drawbox=x=260:y=360:w=560:h=700:color=0xffffff@0.92:t=fill,"
    "drawbox=x=355:y=470:w=370:h=500:color=0xf4a261@0.95:t=fill"
)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="生成 M9 路径 1/2 手动验收合成素材。")
    parser.add_argument(
        "--out-dir",
        type=Path,
        default=REPO_ROOT / ".e2e-paths-fixtures",
        help="素材输出目录，默认 .e2e-paths-fixtures/。",
    )
    parser.add_argument(
        "--voice-type",
        default=None,
        help="覆盖 RUSHES_VOLC_TTS_VOICE_TYPE 的火山音色。",
    )
    parser.add_argument(
        "--target-voiceover-duration",
        type=float,
        default=30.0,
        help="路径 1 口播音频若不在 25-35 秒内，拉伸到该秒数。",
    )
    return parser.parse_args()


async def synthesize_tts(text: str, *, voice_type: str | None) -> bytes:
    provider = VolcengineTTSProvider(voice_type=voice_type)
    payload: dict[str, Any] = {"text": text}
    if voice_type:
        payload["voice_type"] = voice_type
    result = await provider.invoke(
        ProviderRequest(
            capability=TTS_SPEECH,
            request_id=unique_id("m9_fixture_tts"),
            payload=payload,
        )
    )
    if result.error is not None:
        raise RunError(f"火山 TTS 失败：{result.error.error_code} {result.error.message}")
    audio = result.normalized_output.get("audio_bytes")
    if isinstance(audio, bytes):
        return audio
    if isinstance(audio, bytearray):
        return bytes(audio)
    raise RunError("火山 TTS 返回缺少 audio_bytes。")


def write_text_if_missing(path: Path, text: str, *, label: str) -> None:
    if path.exists():
        stage_log(f"{label} 已存在，跳过：{path}")
        return
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text + "\n", encoding="utf-8")
    stage_log(f"{label} 已写入：{path}")


def synthesize_audio_if_missing(path: Path, text: str, *, voice_type: str | None) -> None:
    if path.exists():
        stage_log(f"路径 1 原始 TTS 已存在，跳过：{path}")
        return
    stage_log("调用火山 TTS 生成路径 1 口播音频")
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(asyncio.run(synthesize_tts(text, voice_type=voice_type)))
    stage_log(f"路径 1 原始 TTS 已写入：{path}")


def normalize_voiceover_audio(
    raw_path: Path,
    output_path: Path,
    *,
    target_duration: float,
) -> None:
    if target_duration <= 0:
        raise RunError(f"--target-voiceover-duration 必须大于 0：{target_duration}")
    if output_path.exists():
        stage_log(f"路径 1 口播音频已存在，跳过：{output_path}")
        return
    require_executable("ffmpeg")
    duration = ffprobe_duration_s(raw_path)
    if 25.0 <= duration <= 35.0:
        shutil.copyfile(raw_path, output_path)
        stage_log(f"路径 1 口播音频时长 {duration:.2f}s，直接复用。")
        return
    atempo = _atempo_chain(duration / target_duration)
    run_command(
        [
            "ffmpeg",
            "-y",
            "-i",
            str(raw_path),
            "-filter:a",
            atempo,
            "-vn",
            "-c:a",
            "libmp3lame",
            "-q:a",
            "4",
            str(output_path),
        ],
        description="拉伸路径 1 口播音频",
    )
    stage_log(f"路径 1 口播音频已拉伸到约 {target_duration:.1f}s：{output_path}")


def make_voiceover_video(audio_path: Path, output_path: Path) -> None:
    if output_path.exists():
        stage_log(f"路径 1 口播视频已存在，跳过：{output_path}")
        return
    require_executable("ffmpeg")
    duration = max(1.0, ffprobe_duration_s(audio_path))
    run_command(
        [
            "ffmpeg",
            "-y",
            "-f",
            "lavfi",
            "-i",
            f"testsrc2=size=720x1280:rate=30:duration={duration:.3f}",
            "-i",
            str(audio_path),
            "-map",
            "0:v:0",
            "-map",
            "1:a:0",
            "-c:v",
            "libx264",
            "-pix_fmt",
            "yuv420p",
            "-c:a",
            "aac",
            "-shortest",
            "-movflags",
            "+faststart",
            str(output_path),
        ],
        description="混流路径 1 口播视频",
    )
    stage_log(f"路径 1 口播视频已生成：{output_path}")


def make_broll_if_missing(path: Path, source: str, *, label: str) -> None:
    if path.exists():
        stage_log(f"{label} 已存在，跳过：{path}")
        return
    require_executable("ffmpeg")
    run_command(
        [
            "ffmpeg",
            "-y",
            "-f",
            "lavfi",
            "-i",
            source,
            "-an",
            "-c:v",
            "libx264",
            "-pix_fmt",
            "yuv420p",
            "-movflags",
            "+faststart",
            str(path),
        ],
        description=label,
    )
    stage_log(f"{label} 已生成：{path}")


def make_image_if_missing(path: Path) -> None:
    if path.exists():
        stage_log(f"路径 2 图片已存在，跳过：{path}")
        return
    require_executable("ffmpeg")
    run_command(
        [
            "ffmpeg",
            "-y",
            "-f",
            "lavfi",
            "-i",
            IMAGE_SOURCE,
            "-frames:v",
            "1",
            str(path),
        ],
        description="生成路径 2 图片",
    )
    stage_log(f"路径 2 图片已生成：{path}")


def _atempo_chain(factor: float) -> str:
    if factor <= 0:
        raise RunError(f"非法 atempo factor：{factor}")
    filters: list[str] = []
    while factor < 0.5:
        filters.append("atempo=0.5")
        factor /= 0.5
    while factor > 2.0:
        filters.append("atempo=2.0")
        factor /= 2.0
    filters.append(f"atempo={factor:.6f}")
    return ",".join(filters)


def main() -> int:
    args = parse_args()
    load_dotenv()
    out_dir = args.out_dir.expanduser().resolve()
    out_dir.mkdir(parents=True, exist_ok=True)

    try:
        path1_text = out_dir / "path1_voiceover_text.txt"
        raw_audio = out_dir / "path1_voiceover_tts_raw.mp3"
        audio = out_dir / "path1_voiceover_audio.mp3"
        voiceover_video = out_dir / "path1_voiceover_video.mp4"
        path2_script = out_dir / "path2_script.txt"

        write_text_if_missing(path1_text, PATH1_TEXT, label="路径 1 口播文案")
        synthesize_audio_if_missing(raw_audio, PATH1_TEXT, voice_type=args.voice_type)
        normalize_voiceover_audio(raw_audio, audio, target_duration=args.target_voiceover_duration)
        make_voiceover_video(audio, voiceover_video)

        write_text_if_missing(path2_script, PATH2_SCRIPT, label="路径 2 种草文案")
        for filename, source, label in BROLL_SOURCES:
            make_broll_if_missing(out_dir / filename, source, label=label)
        make_image_if_missing(out_dir / "path2_product_image.png")

        stage_log(f"素材生成完成：{out_dir}")
        return 0
    except RunError as exc:
        stage_log(f"失败：{exc}")
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
