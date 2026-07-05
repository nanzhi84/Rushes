from __future__ import annotations

import json
import shutil
import subprocess
from pathlib import Path

import pytest

from contracts.timeline import TimelineState
from media.preview import render_preview
from media.segment_render import MediaSource
from storage.workspace_paths import WorkspacePaths

pytestmark = pytest.mark.ffmpeg


@pytest.mark.skipif(
    shutil.which("ffmpeg") is None or shutil.which("ffprobe") is None,
    reason="ffmpeg/ffprobe not installed",
)
async def test_preview_renders_two_lavfi_segments_to_expected_shape(tmp_path: Path) -> None:
    source_a = tmp_path / "a.mp4"
    source_b = tmp_path / "b.mp4"
    _make_fixture(source_a, "testsrc=duration=1:size=160x160:rate=30")
    _make_fixture(source_b, "testsrc2=duration=1:size=160x160:rate=30")
    output = tmp_path / "preview.mp4"
    timeline = _timeline()

    await render_preview(
        timeline,
        sources={
            "asset_a": MediaSource("asset_a", source_a, "hash_a"),
            "asset_b": MediaSource("asset_b", source_b, "hash_b"),
        },
        paths=WorkspacePaths.from_root(tmp_path / "workspace"),
        output_path=output,
    )
    probe = _ffprobe(output)

    stream = probe["streams"][0]
    duration = float(probe["format"]["duration"])
    assert stream["width"] == 540
    assert stream["height"] == 960
    assert duration == pytest.approx(2.0, abs=0.15)


def _make_fixture(path: Path, source: str) -> None:
    subprocess.run(
        [
            "ffmpeg",
            "-y",
            "-f",
            "lavfi",
            "-i",
            source,
            "-pix_fmt",
            "yuv420p",
            str(path),
        ],
        check=True,
        capture_output=True,
        text=True,
    )


def _ffprobe(path: Path) -> dict[str, object]:
    result = subprocess.run(
        [
            "ffprobe",
            "-v",
            "error",
            "-show_streams",
            "-show_format",
            "-of",
            "json",
            str(path),
        ],
        check=True,
        capture_output=True,
        text=True,
    )
    payload = json.loads(result.stdout)
    assert isinstance(payload, dict)
    return payload


def _timeline() -> TimelineState:
    return TimelineState.model_validate(
        {
            "timeline_id": "case_1:v1",
            "case_id": "case_1",
            "version": 1,
            "fps": 30,
            "duration_frames": 60,
            "tracks": [
                {
                    "track_id": "visual_base",
                    "track_type": "primary_visual",
                    "clips": [
                        _clip("tc_a", "asset_a", 0, 30),
                        _clip("tc_b", "asset_b", 30, 60),
                    ],
                },
                {"track_id": "visual_overlay", "track_type": "visual_overlay", "clips": []},
                {"track_id": "original_audio", "track_type": "audio", "clips": []},
                {"track_id": "voiceover", "track_type": "audio", "clips": []},
                {"track_id": "bgm", "track_type": "audio", "clips": []},
                {"track_id": "subtitles", "track_type": "text", "clips": []},
            ],
            "validation_report": {"valid": True, "checks": []},
        }
    )


def _clip(timeline_clip_id: str, asset_id: str, start: int, end: int) -> dict[str, object]:
    return {
        "timeline_clip_id": timeline_clip_id,
        "track_id": "visual_base",
        "asset_id": asset_id,
        "clip_id": f"clip_{asset_id}",
        "role": "b_roll",
        "timeline_start_frame": start,
        "timeline_end_frame": end,
        "source_start_frame": 0,
        "source_end_frame": end - start,
    }


async def test_preview_mixes_voiceover_and_bgm_tracks(tmp_path: Path) -> None:
    source_a = tmp_path / "a.mp4"
    _make_fixture(source_a, "testsrc=duration=2:size=160x160:rate=30")
    voiceover = tmp_path / "vo.wav"
    bgm = tmp_path / "bgm.wav"
    for path, freq in ((voiceover, 440), (bgm, 220)):
        subprocess.run(
            ["ffmpeg", "-y", "-f", "lavfi", "-i", f"sine=frequency={freq}:duration=2", str(path)],
            check=True,
            capture_output=True,
            text=True,
        )
    output = tmp_path / "preview_mix.mp4"

    timeline = TimelineState.model_validate(
        {
            "timeline_id": "case_1:v2",
            "case_id": "case_1",
            "version": 2,
            "fps": 30,
            "duration_frames": 60,
            "tracks": [
                {
                    "track_id": "visual_base",
                    "track_type": "primary_visual",
                    "clips": [_clip("tc_a", "asset_a", 0, 60)],
                },
                {"track_id": "visual_overlay", "track_type": "visual_overlay", "clips": []},
                {"track_id": "original_audio", "track_type": "audio", "clips": []},
                {
                    "track_id": "voiceover",
                    "track_type": "audio",
                    "clips": [
                        {
                            "timeline_clip_id": "tc_vo",
                            "track_id": "voiceover",
                            "asset_id": "asset_vo",
                            "clip_id": "clip_vo",
                            "role": "voiceover",
                            "timeline_start_frame": 0,
                            "timeline_end_frame": 60,
                            "source_start_frame": 0,
                            "source_end_frame": 60,
                        }
                    ],
                },
                {
                    "track_id": "bgm",
                    "track_type": "audio",
                    "clips": [
                        {
                            "timeline_clip_id": "tc_bgm",
                            "track_id": "bgm",
                            "asset_id": "asset_bgm",
                            "clip_id": "clip_bgm",
                            "role": "bgm",
                            "timeline_start_frame": 0,
                            "timeline_end_frame": 60,
                            "source_start_frame": 0,
                            "source_end_frame": 60,
                            "gain_db": -6.0,
                        }
                    ],
                },
                {"track_id": "subtitles", "track_type": "text", "clips": []},
            ],
        }
    )

    await render_preview(
        timeline,
        sources={
            "asset_a": MediaSource("asset_a", source_a, "hash_a"),
            "asset_vo": MediaSource("asset_vo", voiceover, "hash_vo"),
            "asset_bgm": MediaSource("asset_bgm", bgm, "hash_bgm"),
        },
        paths=WorkspacePaths.from_root(tmp_path / "workspace"),
        output_path=output,
    )
    probe = _ffprobe(output)

    codec_types = {stream["codec_type"] for stream in probe["streams"]}
    assert codec_types == {"video", "audio"}
    duration = float(probe["format"]["duration"])
    assert duration == pytest.approx(2.0, abs=0.2)
