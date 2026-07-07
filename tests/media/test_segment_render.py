from __future__ import annotations

from pathlib import Path

from contracts.timeline import TimelineState
from media.segment_render import (
    MediaSource,
    RenderProfile,
    compute_segment_cache_key,
    parse_ffmpeg_progress,
    split_timeline_segments,
)

PROFILE = RenderProfile(name="preview", width=540, height=960, fps=30, preset="ultrafast", crf=32)
FFMPEG_VERSION = "ffmpeg version test"


def test_split_merges_contiguous_same_source_and_filter_chain(tmp_path: Path) -> None:
    timeline = _timeline(
        [
            _clip("tc_1", "asset_1", 0, 30, 0, 30),
            _clip("tc_2", "asset_1", 30, 60, 30, 60),
        ],
        duration_frames=60,
    )

    segments = split_timeline_segments(timeline)

    assert len(segments) == 1
    assert segments[0].timeline_start_frame == 0
    assert segments[0].timeline_end_frame == 60
    assert segments[0].base.source_range == (0, 60)


def test_split_keeps_overlay_stack_as_single_conservative_segment() -> None:
    timeline = _timeline(
        [_clip("base", "asset_base", 0, 90, 0, 90)],
        overlays=[
            _clip("ov_1", "asset_overlay_1", 30, 60, 0, 30, track_id="visual_overlay"),
            _clip("ov_2", "asset_overlay_2", 30, 60, 0, 30, track_id="visual_overlay"),
        ],
        duration_frames=90,
    )

    segments = split_timeline_segments(timeline)

    assert [(segment.timeline_start_frame, segment.timeline_end_frame) for segment in segments] == [
        (0, 30),
        (30, 60),
        (60, 90),
    ]
    assert [clip.asset_id for clip in segments[1].overlays] == [
        "asset_overlay_1",
        "asset_overlay_2",
    ]


def test_patch_only_changes_cache_key_for_affected_segment(tmp_path: Path) -> None:
    before = _timeline(
        [
            _clip("tc_1", "asset_1", 0, 30, 0, 30),
            _clip("tc_2", "asset_2", 30, 60, 0, 30),
            _clip("tc_3", "asset_3", 60, 90, 0, 30),
        ],
        duration_frames=90,
    )
    after = _timeline(
        [
            _clip("tc_1", "asset_1", 0, 30, 0, 30),
            _clip(
                "tc_2",
                "asset_2",
                30,
                60,
                0,
                30,
                effects=[{"kind": "brightness", "value": 0.1}],
            ),
            _clip("tc_3", "asset_3", 60, 90, 0, 30),
        ],
        duration_frames=90,
    )
    sources = {
        f"asset_{index}": MediaSource(
            asset_id=f"asset_{index}",
            path=tmp_path / f"asset_{index}.mp4",
            asset_hash=f"hash_{index}",
        )
        for index in range(1, 4)
    }

    before_keys = _cache_keys(before, sources)
    after_keys = _cache_keys(after, sources)

    assert before_keys[0] == after_keys[0]
    assert before_keys[1] != after_keys[1]
    assert before_keys[2] == after_keys[2]


def test_parse_ffmpeg_progress_samples() -> None:
    samples = parse_ffmpeg_progress(
        "\n".join(
            [
                "frame=10",
                "out_time_ms=500000",
                "progress=continue",
                "out_time_ms=1000000",
                "progress=end",
            ]
        )
    )

    assert [sample.out_time_ms for sample in samples] == [500000, 1000000]
    assert [sample.progress for sample in samples] == ["continue", "end"]


def _cache_keys(timeline: TimelineState, sources: dict[str, MediaSource]) -> list[str]:
    return [
        compute_segment_cache_key(
            segment,
            sources=sources,
            profile=PROFILE,
            ffmpeg_version=FFMPEG_VERSION,
        )
        for segment in split_timeline_segments(timeline)
    ]


def _timeline(
    visual_clips: list[dict[str, object]],
    *,
    duration_frames: int,
    overlays: list[dict[str, object]] | None = None,
) -> TimelineState:
    return TimelineState.model_validate(
        {
            "timeline_id": "draft_1:v1",
            "draft_id": "draft_1",
            "version": 1,
            "fps": 30,
            "duration_frames": duration_frames,
            "tracks": [
                {"track_id": "visual_base", "track_type": "primary_visual", "clips": visual_clips},
                {
                    "track_id": "visual_overlay",
                    "track_type": "visual_overlay",
                    "clips": overlays or [],
                },
                {"track_id": "original_audio", "track_type": "audio", "clips": []},
                {"track_id": "voiceover", "track_type": "audio", "clips": []},
                {"track_id": "bgm", "track_type": "audio", "clips": []},
                {"track_id": "subtitles", "track_type": "text", "clips": []},
            ],
        }
    )


def _clip(
    timeline_clip_id: str,
    asset_id: str,
    start: int,
    end: int,
    source_start: int,
    source_end: int,
    *,
    track_id: str = "visual_base",
    effects: list[dict[str, object]] | None = None,
) -> dict[str, object]:
    return {
        "timeline_clip_id": timeline_clip_id,
        "track_id": track_id,
        "asset_id": asset_id,
        "clip_id": f"clip_{asset_id}",
        "role": "b_roll",
        "timeline_start_frame": start,
        "timeline_end_frame": end,
        "source_start_frame": source_start,
        "source_end_frame": source_end,
        "effects": effects or [],
    }
