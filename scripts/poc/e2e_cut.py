"""M-1.2 VLM annotation to segmented vertical render POC."""

from __future__ import annotations

import argparse
import json
from dataclasses import dataclass
from pathlib import Path
from typing import cast

from _common import (
    EXIT_SKIP,
    DashScopeClient,
    JsonObject,
    PocError,
    PocSkip,
    ensure_dir,
    ffprobe_duration_s,
    first_string,
    image_data_url,
    iter_json_objects,
    load_dotenv,
    require_env,
    require_executable,
    run_command,
    strip_json_fence,
    timestamp,
    write_json,
)
from pydantic import BaseModel, ConfigDict, Field, ValidationError

VIDEO_EXTENSIONS = frozenset({".mp4", ".mov", ".m4v", ".avi", ".mkv", ".webm"})
TARGET_SECONDS = 30.0
MIN_CLIP_SECONDS = 2.0
MAX_CLIP_SECONDS = 5.0


class ClipAnalysis(BaseModel):
    model_config = ConfigDict(extra="forbid")

    summary: str = Field(min_length=1)
    role: str = Field(min_length=1)
    quality: float = Field(ge=0.0, le=1.0)


@dataclass(frozen=True)
class SourceClip:
    path: Path
    duration_s: float
    analysis: ClipAnalysis


@dataclass(frozen=True)
class TimelineClip:
    source: SourceClip
    source_start_s: float
    duration_s: float
    segment_path: Path


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="真实素材 VLM 标注 + 30s 竖屏分段渲染 POC。")
    parser.add_argument("--footage-dir", type=Path, required=True, help="真实视频素材目录。")
    return parser.parse_args()


def list_videos(footage_dir: Path) -> list[Path]:
    if not footage_dir.exists() or not footage_dir.is_dir():
        raise PocSkip(f"素材目录不存在：{footage_dir}。请传入包含 mp4/mov 等视频的 --footage-dir。")
    videos = sorted(
        path
        for path in footage_dir.iterdir()
        if path.is_file() and path.suffix.lower() in VIDEO_EXTENSIONS
    )
    if not videos:
        raise PocSkip(
            f"素材目录为空或没有视频：{footage_dir}。请放入真实素材后重跑，例如："
            " python scripts/poc/e2e_cut.py --footage-dir /path/to/footage"
        )
    return videos


def extract_frames(video_path: Path, frames_dir: Path) -> list[Path]:
    require_executable("ffmpeg")
    ensure_dir(frames_dir)
    pattern = frames_dir / f"{video_path.stem}_%02d.jpg"
    run_command(
        [
            "ffmpeg",
            "-y",
            "-i",
            str(video_path),
            "-vf",
            "fps=1/2,scale=640:-1",
            "-frames:v",
            "6",
            "-q:v",
            "3",
            str(pattern),
        ],
        description=f"抽取关键帧 {video_path.name}",
    )
    frames = sorted(frames_dir.glob(f"{video_path.stem}_*.jpg"))
    if not frames:
        raise PocError(f"ffmpeg 没有从 {video_path} 抽出关键帧。")
    return frames


def analyze_clip(
    client: DashScopeClient,
    video_path: Path,
    frames: list[Path],
    run_id: str,
) -> ClipAnalysis:
    content: list[JsonObject] = [
        {
            "type": "text",
            "text": (
                "你是短视频剪辑素材标注员。根据这些每 2 秒抽取的关键帧，"
                "只输出 JSON object，字段必须是："
                '{"summary": string, "role": string, "quality": number 0-1}。'
                "role 建议取 opening/detail/process/product/broll/bad。"
            ),
        }
    ]
    for frame in frames:
        content.append({"type": "image_url", "image_url": {"url": image_data_url(frame)}})
    payload: JsonObject = {
        "model": "qwen-vl-max",
        "messages": [{"role": "user", "content": content}],
        "response_format": {"type": "json_object"},
        "temperature": 0,
    }
    response = client.chat_completions(payload)
    sample_path = Path("research/vlm_samples") / f"qwen_vl_max_{run_id}_{video_path.stem}.json"
    write_json(
        sample_path,
        {
            "model": "qwen-vl-max",
            "video": str(video_path),
            "frames": [str(frame) for frame in frames],
            "response": response,
        },
    )
    content_text = extract_chat_content(response)
    try:
        return ClipAnalysis.model_validate_json(strip_json_fence(content_text))
    except ValidationError as exc:
        raise PocError(f"VLM 响应不符合 ClipAnalysis schema：{content_text}\n{exc}") from exc


def extract_chat_content(response: JsonObject) -> str:
    choices = response.get("choices")
    if not isinstance(choices, list) or not choices:
        raise PocError(f"chat 响应缺少 choices：{response}")
    first_choice = choices[0]
    if not isinstance(first_choice, dict):
        raise PocError(f"chat choices[0] 不是 object：{response}")
    message = first_choice.get("message")
    if not isinstance(message, dict):
        raise PocError(f"chat choices[0].message 不是 object：{response}")
    content = message.get("content")
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        pieces: list[str] = []
        for item in content:
            if isinstance(item, dict):
                text = first_string(cast(dict[str, object], item), ("text", "content"))
                if text is not None:
                    pieces.append(text)
        if pieces:
            return "\n".join(pieces)
    for mapping in iter_json_objects(response):
        text = first_string(mapping, ("content", "text"))
        if text is not None and text.strip().startswith("{"):
            return text
    raise PocError(f"chat 响应没有可解析文本：{response}")


def build_timeline(clips: list[SourceClip], out_dir: Path) -> list[TimelineClip]:
    selected: list[TimelineClip] = []
    elapsed = 0.0
    sorted_clips = sorted(clips, key=lambda clip: clip.analysis.quality, reverse=True)
    for index, source in enumerate(sorted_clips, start=1):
        if elapsed >= TARGET_SECONDS:
            break
        remaining = TARGET_SECONDS - elapsed
        if remaining < MIN_CLIP_SECONDS:
            break
        if source.duration_s <= 0:
            continue
        duration = min(MAX_CLIP_SECONDS, remaining, source.duration_s)
        if duration < MIN_CLIP_SECONDS:
            continue
        selected.append(
            TimelineClip(
                source=source,
                source_start_s=0.0,
                duration_s=duration,
                segment_path=out_dir / f"segment_{index:03d}.mp4",
            )
        )
        elapsed += duration
    if not selected:
        raise PocError("没有可渲染的 timeline clip。")
    return selected


def render_segment(clip: TimelineClip) -> None:
    filter_chain = "scale=1080:1920:force_original_aspect_ratio=increase,crop=1080:1920,setsar=1"
    run_command(
        [
            "ffmpeg",
            "-y",
            "-ss",
            f"{clip.source_start_s:.3f}",
            "-t",
            f"{clip.duration_s:.3f}",
            "-i",
            str(clip.source.path),
            "-vf",
            filter_chain,
            "-an",
            "-r",
            "30",
            "-c:v",
            "libx264",
            "-preset",
            "ultrafast",
            "-crf",
            "28",
            "-pix_fmt",
            "yuv420p",
            str(clip.segment_path),
        ],
        description=f"分段渲染 {clip.source.path.name}",
    )


def concat_segments(clips: list[TimelineClip], output_path: Path) -> None:
    concat_list = output_path.with_suffix(".concat.txt")
    lines = [f"file '{clip.segment_path.resolve()}'" for clip in clips]
    concat_list.write_text("\n".join(lines) + "\n", encoding="utf-8")
    run_command(
        [
            "ffmpeg",
            "-y",
            "-f",
            "concat",
            "-safe",
            "0",
            "-i",
            str(concat_list),
            "-c",
            "copy",
            str(output_path),
        ],
        description="concat demuxer 合并分段",
    )


def timeline_to_json(clips: list[TimelineClip]) -> list[JsonObject]:
    rows: list[JsonObject] = []
    cursor = 0.0
    for clip in clips:
        rows.append(
            {
                "path": str(clip.source.path),
                "summary": clip.source.analysis.summary,
                "role": clip.source.analysis.role,
                "quality": clip.source.analysis.quality,
                "source_start_s": clip.source_start_s,
                "duration_s": clip.duration_s,
                "timeline_start_s": cursor,
                "timeline_end_s": cursor + clip.duration_s,
            }
        )
        cursor += clip.duration_s
    return rows


def main() -> int:
    args = parse_args()
    try:
        load_dotenv()
        videos = list_videos(args.footage_dir)
        api_key = require_env("RUSHES_DASHSCOPE_API_KEY")
        run_id = timestamp()
        frames_dir = ensure_dir(Path("scripts/poc/out") / f"frames_{run_id}")
        out_dir = ensure_dir(Path("scripts/poc/out") / f"segments_{run_id}")
        sources: list[SourceClip] = []
        with DashScopeClient(api_key, timeout_s=120.0) as client:
            for video in videos:
                duration = ffprobe_duration_s(video)
                frames = extract_frames(video, frames_dir)
                analysis = analyze_clip(client, video, frames, run_id)
                print(
                    f"VLM: {video.name} quality={analysis.quality:.2f} "
                    f"role={analysis.role} summary={analysis.summary}"
                )
                sources.append(SourceClip(path=video, duration_s=duration, analysis=analysis))
        timeline = build_timeline(sources, out_dir)
        for clip in timeline:
            render_segment(clip)
        output_path = Path("scripts/poc/out") / f"e2e_cut_{run_id}.mp4"
        concat_segments(timeline, output_path)
        timeline_path = output_path.with_suffix(".timeline.json")
        write_json(timeline_path, {"clips": timeline_to_json(timeline)})
        print("E2E cut report")
        print(f"- source videos: {len(videos)}")
        print(f"- timeline clips: {len(timeline)}")
        print(f"- duration: {sum(clip.duration_s for clip in timeline):.2f}s")
        print(f"- output: {output_path}")
        print(f"- timeline: {timeline_path}")
        print("- VLM samples: research/vlm_samples/")
        return 0
    except PocSkip as exc:
        print(f"SKIP: {exc}")
        return EXIT_SKIP
    except (PocError, ValidationError, json.JSONDecodeError) as exc:
        print(f"ERROR: {exc}")
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
