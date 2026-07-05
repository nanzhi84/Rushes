"""Prompt-safe TimelineState summary rendering."""

from __future__ import annotations

from collections.abc import Sequence

from contracts.subtitle import SubtitleClip
from contracts.timeline import TimelineMediaClip, TimelineState


def render_timeline_summary(timeline: TimelineState, *, aspect_ratio: str) -> str:
    duration_sec = timeline.duration_frames / timeline.fps
    lines = [
        f"Timeline v{timeline.version} · {duration_sec:.1f}s @{timeline.fps}fps · {aspect_ratio}"
    ]
    subtitle_clips = _subtitle_clips(timeline)
    visual_clips = [
        clip
        for track in timeline.tracks
        if track.track_id in {"visual_base", "visual_overlay"}
        for clip in track.clips
        if isinstance(clip, TimelineMediaClip)
    ]
    for clip in sorted(visual_clips, key=lambda item: item.timeline_start_frame):
        start = clip.timeline_start_frame / timeline.fps
        end = clip.timeline_end_frame / timeline.fps
        slot = clip.parent_block_id or clip.timeline_clip_id
        role = _role_label(clip.role)
        summary = _clip_summary(clip)
        subtitle = _overlapping_subtitle_text(clip, subtitle_clips)
        subtitle_part = f' 字幕:"{subtitle}"' if subtitle else ""
        lines.append(
            f"[{_format_sec(start)}-{_format_sec(end)}] {slot}  {role} "
            f"{clip.asset_id}/{clip.clip_id or '-'} 「{summary}」{subtitle_part}"
        )
    lines.append(_render_audio_line(timeline))
    return "\n".join(lines)


def _subtitle_clips(timeline: TimelineState) -> list[SubtitleClip]:
    return [
        clip
        for track in timeline.tracks
        if track.track_id == "subtitles"
        for clip in track.clips
        if isinstance(clip, SubtitleClip)
    ]


def _overlapping_subtitle_text(
    clip: TimelineMediaClip,
    subtitle_clips: Sequence[SubtitleClip],
) -> str | None:
    for subtitle in subtitle_clips:
        if (
            subtitle.timeline_start_frame < clip.timeline_end_frame
            and subtitle.timeline_end_frame > clip.timeline_start_frame
        ):
            return subtitle.text
    return None


def _render_audio_line(timeline: TimelineState) -> str:
    voiceover = _audio_track_summary(timeline, "voiceover")
    bgm = _audio_track_summary(timeline, "bgm")
    original_audio = _audio_track_summary(timeline, "original_audio")
    return (
        f"audio: voiceover({voiceover or '无'}) · "
        f"bgm: {bgm or '无'} · 原声: {'开' if original_audio else '关'}"
    )


def _audio_track_summary(timeline: TimelineState, track_id: str) -> str | None:
    for track in timeline.tracks:
        if track.track_id != track_id:
            continue
        clips = [clip for clip in track.clips if isinstance(clip, TimelineMediaClip)]
        if not clips:
            return None
        start = min(clip.timeline_start_frame for clip in clips) / timeline.fps
        end = max(clip.timeline_end_frame for clip in clips) / timeline.fps
        assets = ",".join(sorted({clip.asset_id for clip in clips}))
        return f"{assets} {_format_sec(start)}-{_format_sec(end)}s"
    return None


def _role_label(role: str) -> str:
    return {"a_roll": "A-roll", "b_roll": "B-roll"}.get(role, role)


def _clip_summary(clip: TimelineMediaClip) -> str:
    for effect in clip.effects:
        for key in ("summary", "label", "title"):
            value = effect.get(key)
            if isinstance(value, str) and value:
                return value
    return ""


def _format_sec(value: float) -> str:
    return f"{value:04.1f}"
