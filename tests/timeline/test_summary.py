from __future__ import annotations

from typing import Any

from contracts.timeline import TimelineState
from timeline import render_timeline_summary


def test_summary_renders_empty_tracks_and_no_voiceover() -> None:
    summary = render_timeline_summary(_timeline([], duration_frames=0), aspect_ratio="9:16")

    assert summary.splitlines() == [
        "Timeline v1 · 0.0s @30fps · 9:16",
        "audio: voiceover(无) · bgm: 无 · 原声: 关",
    ]


def test_summary_renders_visual_subtitles_and_audio_tracks() -> None:
    long_title = "title " * 30
    summary = render_timeline_summary(
        _timeline(
            [
                _visual_clip("tc_1", 0, 30, role="a_roll", effects=[{"label": "host"}]),
                _visual_clip("tc_2", 30, 60, role="image", effects=[{"title": long_title}]),
            ],
            duration_frames=60,
            voiceover=[_audio_clip("vo_1", "voiceover", "asset_vo", 0, 60)],
            bgm=[_audio_clip("bgm_1", "bgm", "asset_bgm", 0, 60)],
            original_audio=[_audio_clip("orig_1", "original_audio", "asset_orig", 0, 60)],
            subtitles=[
                {
                    "timeline_clip_id": "sub_1",
                    "track_id": "subtitles",
                    "text": "subtitle text",
                    "timeline_start_frame": 35,
                    "timeline_end_frame": 50,
                    "style_template_id": "subtitle_default",
                    "binding": {"kind": "manual"},
                    "safe_area_check": "ok",
                }
            ],
        ),
        aspect_ratio="1:1",
    )

    assert "[00.0-01.0] tc_1  A-roll asset_1/clip_1 「host」" in summary
    assert f"[01.0-02.0] tc_2  image asset_1/clip_1 「{long_title}」" in summary
    assert '字幕:"subtitle text"' in summary
    assert "voiceover(asset_vo 00.0-02.0s)" in summary
    assert "bgm: asset_bgm 00.0-02.0s" in summary
    assert "原声: 开" in summary


def _timeline(
    visual_clips: list[dict[str, Any]],
    *,
    duration_frames: int,
    voiceover: list[dict[str, Any]] | None = None,
    bgm: list[dict[str, Any]] | None = None,
    original_audio: list[dict[str, Any]] | None = None,
    subtitles: list[dict[str, Any]] | None = None,
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
                {"track_id": "visual_overlay", "track_type": "visual_overlay", "clips": []},
                {
                    "track_id": "original_audio",
                    "track_type": "audio",
                    "clips": original_audio or [],
                },
                {"track_id": "voiceover", "track_type": "audio", "clips": voiceover or []},
                {"track_id": "bgm", "track_type": "audio", "clips": bgm or []},
                {"track_id": "subtitles", "track_type": "text", "clips": subtitles or []},
            ],
        }
    )


def _visual_clip(
    timeline_clip_id: str,
    start: int,
    end: int,
    *,
    role: str = "b_roll",
    effects: list[dict[str, Any]] | None = None,
) -> dict[str, Any]:
    return {
        "timeline_clip_id": timeline_clip_id,
        "track_id": "visual_base",
        "asset_id": "asset_1",
        "clip_id": "clip_1",
        "role": role,
        "timeline_start_frame": start,
        "timeline_end_frame": end,
        "source_start_frame": start,
        "source_end_frame": end,
        "effects": effects or [],
    }


def _audio_clip(
    timeline_clip_id: str,
    track_id: str,
    asset_id: str,
    start: int,
    end: int,
) -> dict[str, Any]:
    return {
        "timeline_clip_id": timeline_clip_id,
        "track_id": track_id,
        "asset_id": asset_id,
        "clip_id": None,
        "role": track_id,
        "timeline_start_frame": start,
        "timeline_end_frame": end,
        "source_start_frame": start,
        "source_end_frame": end,
    }
