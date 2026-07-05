"""Generate a Chinese filler-speech audio fixture for M-1 ASR contract checks."""

from __future__ import annotations

import argparse
import tempfile
from pathlib import Path

from _common import (
    EXIT_SKIP,
    PocError,
    PocSkip,
    ensure_dir,
    require_executable,
    run_command,
)

FIXTURE_TEXT = (
    "呃，大家好，今天我想简单讲一下这个产品。[[slnc 800]]"
    "嗯，我大概用了三周，然后然后发现它最大的好处，是可以先把素材整理清楚。"
    "[[slnc 800]]啊，如果中间说错了，也不用紧张，后面粗剪的时候可以再删。"
)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="生成含口癖的中文 16kHz 单声道 wav fixture。")
    parser.add_argument(
        "--output",
        type=Path,
        default=Path("scripts/poc/fixtures/filler_speech.wav"),
        help="输出 wav 路径，默认 scripts/poc/fixtures/filler_speech.wav。",
    )
    parser.add_argument(
        "--text-output",
        type=Path,
        default=Path("scripts/poc/fixtures/filler_speech.txt"),
        help="同步写入的参考文本路径。",
    )
    return parser.parse_args()


def available_voice() -> str:
    require_executable("say")
    output = run_command(["say", "-v", "?"], description="探测 macOS say 语音")
    voices = output.stdout
    for candidate in ("Tingting", "Meijia", "Mei-Jia"):
        if candidate in voices:
            return candidate
    raise PocSkip("没有找到 Tingting/Meijia 中文语音，请在系统语音设置里安装中文语音后重跑。")


def convert_to_wav(aiff_path: Path, wav_path: Path) -> None:
    ensure_dir(wav_path.parent)
    if _has_executable("afconvert"):
        run_command(
            [
                "afconvert",
                "-f",
                "WAVE",
                "-d",
                "LEI16@16000",
                "-c",
                "1",
                str(aiff_path),
                str(wav_path),
            ],
            description="afconvert 转换 16kHz 单声道 wav",
        )
        return
    require_executable("ffmpeg")
    run_command(
        [
            "ffmpeg",
            "-y",
            "-i",
            str(aiff_path),
            "-ac",
            "1",
            "-ar",
            "16000",
            "-sample_fmt",
            "s16",
            str(wav_path),
        ],
        description="ffmpeg 转换 16kHz 单声道 wav",
    )


def _has_executable(name: str) -> bool:
    try:
        require_executable(name)
    except PocSkip:
        return False
    return True


def main() -> int:
    args = parse_args()
    try:
        voice = available_voice()
        ensure_dir(args.output.parent)
        ensure_dir(args.text_output.parent)
        args.text_output.write_text(FIXTURE_TEXT + "\n", encoding="utf-8")
        with tempfile.TemporaryDirectory(prefix="rushes_poc_fixture_") as temp_dir:
            aiff_path = Path(temp_dir) / "filler_speech.aiff"
            run_command(
                ["say", "-v", voice, "-o", str(aiff_path), FIXTURE_TEXT],
                description=f"用 macOS say/{voice} 合成 aiff",
            )
            convert_to_wav(aiff_path, args.output)
        print(f"已生成音频：{args.output}")
        print(f"已写入参考文本：{args.text_output}")
        print(f"使用语音：{voice}")
        return 0
    except PocSkip as exc:
        print(f"SKIP: {exc}")
        return EXIT_SKIP
    except PocError as exc:
        print(f"ERROR: {exc}")
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
