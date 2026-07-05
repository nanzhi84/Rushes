"""ASS subtitle generation for per-segment burn-in rendering."""

from __future__ import annotations

import importlib
from collections.abc import Callable, Mapping, Sequence
from functools import lru_cache
from pathlib import Path
from typing import Any, cast

from contracts.subtitle import SubtitleClip, SubtitleStyleTemplate

SubtitleTemplateMap = Mapping[str, SubtitleStyleTemplate]

_STYLE_FORMAT = (
    "Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, "
    "BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, "
    "BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding"
)
_EVENT_FORMAT = "Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text"
_ALIGNMENT_BY_POSITION = {"bottom": 2, "center": 5, "top": 8}


def ass_style_line(template: SubtitleStyleTemplate) -> str:
    """Convert a subtitle style template to an ASS Style line."""

    alignment = _ALIGNMENT_BY_POSITION[template.position]
    primary = _ass_color(template.primary_color)
    outline = _ass_color(template.outline_color)
    return ",".join(
        [
            f"Style: {template.template_id}",
            template.font_family,
            str(template.font_size),
            primary,
            primary,
            outline,
            "&H00000000",
            "0",
            "0",
            "0",
            "0",
            "100",
            "100",
            "0",
            "0",
            "1",
            str(template.outline_width),
            "0",
            str(alignment),
            "0",
            "0",
            str(template.margin_v),
            "1",
        ]
    )


def build_segment_ass(
    clips: Sequence[SubtitleClip],
    *,
    segment_start_frame: int,
    fps: float,
    play_res: tuple[int, int],
    subtitle_templates: SubtitleTemplateMap | None = None,
    segment_end_frame: int | None = None,
) -> str:
    """Build a complete ASS document for subtitle clips attached to one segment."""

    templates = resolve_subtitle_templates(subtitle_templates)
    events = _segment_events(
        clips,
        segment_start_frame=segment_start_frame,
        segment_end_frame=segment_end_frame,
    )
    style_templates = _style_templates_for_events(events, templates)
    width, height = play_res
    lines = [
        "[Script Info]",
        "ScriptType: v4.00+",
        "WrapStyle: 0",
        "ScaledBorderAndShadow: yes",
        f"PlayResX: {width}",
        f"PlayResY: {height}",
        "",
        "[V4+ Styles]",
        _STYLE_FORMAT,
    ]
    lines.extend(ass_style_line(template) for template in style_templates)
    lines.extend(
        [
            "",
            "[Events]",
            _EVENT_FORMAT,
        ]
    )
    lines.extend(
        (
            f"Dialogue: 0,{_ass_timecode(event['start_frame'], fps)},"
            f"{_ass_timecode(event['end_frame'], fps)},{event['style_template_id']},,"
            f"0,0,0,,{_escape_dialogue_text(event['text'])}"
        )
        for event in events
    )
    return "\n".join(lines) + "\n"


def write_segment_ass(
    path: Path,
    clips: Sequence[SubtitleClip],
    *,
    segment_start_frame: int,
    fps: float,
    play_res: tuple[int, int],
    subtitle_templates: SubtitleTemplateMap | None = None,
    segment_end_frame: int | None = None,
) -> Path:
    """Write the ASS document for one segment to the caller-provided path."""

    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(
        build_segment_ass(
            clips,
            segment_start_frame=segment_start_frame,
            segment_end_frame=segment_end_frame,
            fps=fps,
            play_res=play_res,
            subtitle_templates=subtitle_templates,
        ),
        encoding="utf-8",
    )
    return path


def subtitle_cache_payload(
    clips: Sequence[SubtitleClip],
    *,
    segment_start_frame: int,
    segment_end_frame: int,
    subtitle_templates: SubtitleTemplateMap,
) -> list[dict[str, Any]]:
    """Return render-relevant subtitle data for segment cache keys."""

    payload: list[dict[str, Any]] = []
    for event in _segment_events(
        clips,
        segment_start_frame=segment_start_frame,
        segment_end_frame=segment_end_frame,
    ):
        template = subtitle_templates[event["style_template_id"]]
        payload.append(
            {
                "text": event["text"],
                "range_frames": [event["start_frame"], event["end_frame"]],
                "style_template": template.model_dump(mode="json"),
            }
        )
    return payload


def resolve_subtitle_templates(
    subtitle_templates: SubtitleTemplateMap | None,
) -> SubtitleTemplateMap:
    if subtitle_templates is not None:
        return subtitle_templates
    return _default_subtitle_templates()


def _segment_events(
    clips: Sequence[SubtitleClip],
    *,
    segment_start_frame: int,
    segment_end_frame: int | None,
) -> list[dict[str, Any]]:
    events: list[dict[str, Any]] = []
    for clip in sorted(
        clips,
        key=lambda item: (
            item.timeline_start_frame,
            item.timeline_end_frame,
            item.timeline_clip_id,
        ),
    ):
        start = max(clip.timeline_start_frame, segment_start_frame)
        end = clip.timeline_end_frame
        if segment_end_frame is not None:
            end = min(end, segment_end_frame)
        if end <= start:
            continue
        events.append(
            {
                "text": clip.text,
                "start_frame": start - segment_start_frame,
                "end_frame": end - segment_start_frame,
                "style_template_id": clip.style_template_id,
            }
        )
    return events


def _style_templates_for_events(
    events: Sequence[dict[str, Any]],
    subtitle_templates: SubtitleTemplateMap,
) -> list[SubtitleStyleTemplate]:
    style_templates: list[SubtitleStyleTemplate] = []
    seen: set[str] = set()
    for event in events:
        template_id = event["style_template_id"]
        if template_id in seen:
            continue
        style_templates.append(subtitle_templates[template_id])
        seen.add(template_id)
    return style_templates


def _ass_color(value: str) -> str:
    raw = value.strip()
    if raw.startswith("#"):
        raw = raw[1:]
    if len(raw) == 6:
        red, green, blue, alpha = raw[0:2], raw[2:4], raw[4:6], "00"
    elif len(raw) == 8:
        red, green, blue, alpha = raw[0:2], raw[2:4], raw[4:6], raw[6:8]
    else:
        raise ValueError("subtitle colors must be #RRGGBB or #RRGGBBAA")
    int(raw, 16)
    return f"&H{alpha}{blue}{green}{red}".upper()


def _ass_timecode(frames: int, fps: float) -> str:
    centiseconds = int((frames / fps * 100) + 0.5)
    hours, remainder = divmod(centiseconds, 360_000)
    minutes, remainder = divmod(remainder, 6_000)
    seconds, centiseconds = divmod(remainder, 100)
    return f"{hours}:{minutes:02d}:{seconds:02d}.{centiseconds:02d}"


def _escape_dialogue_text(text: str) -> str:
    normalized = text.replace("\r\n", "\n").replace("\r", "\n")
    return normalized.replace("{", r"\{").replace("}", r"\}").replace("\n", r"\N")


@lru_cache(maxsize=1)
def _default_subtitle_templates() -> dict[str, SubtitleStyleTemplate]:
    try:
        module = importlib.import_module("domain.subtitle_templates")
    except ModuleNotFoundError:
        return {}
    list_templates = cast(
        Callable[[], Sequence[SubtitleStyleTemplate]],
        vars(module)["list_subtitle_templates"],
    )
    templates = list_templates()
    return {template.template_id: template for template in templates}
