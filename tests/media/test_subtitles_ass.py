from __future__ import annotations

import json
import shutil
import subprocess
from pathlib import Path

import pytest

from contracts.subtitle import SubtitleClip, SubtitleStyleTemplate
from contracts.timeline import TimelineState
from domain.subtitle_templates import get_subtitle_template
from media.preview import render_preview
from media.segment_render import (
    MediaSource,
    RenderProfile,
    SegmentClip,
    TimelineSegment,
    build_segment_command,
    compute_segment_cache_key,
    split_timeline_segments,
)
from media.subtitles_ass import ass_style_line, build_segment_ass, write_segment_ass
from storage.workspace_paths import WorkspacePaths

PROFILE = RenderProfile(name="preview", width=540, height=960, fps=30, preset="ultrafast", crf=32)
FFMPEG_VERSION = "ffmpeg version test"


def test_ass_style_line_converts_colors_alignment_and_margin() -> None:
    line = ass_style_line(
        _template(
            "alpha_top",
            primary_color="#33669980",
            outline_color="#112233",
            position="top",
            margin_v=64,
        )
    )
    fields = line.split(",")

    assert fields[3] == "&H80996633"
    assert fields[5] == "&H00332211"
    assert fields[18] == "8"
    assert fields[21] == "64"
    assert ass_style_line(_template("bottom", position="bottom")).split(",")[18] == "2"
    assert ass_style_line(_template("center", position="center")).split(",")[18] == "5"


def test_build_segment_ass_escapes_text_and_uses_segment_relative_intersection() -> None:
    template = _template("clean_bottom")
    ass = build_segment_ass(
        [
            _subtitle(
                "sub_1",
                "第一行{重点},逗号\n第二行}",
                20,
                80,
                template.template_id,
            )
        ],
        segment_start_frame=30,
        segment_end_frame=60,
        fps=30,
        play_res=(540, 960),
        subtitle_templates={template.template_id: template},
    )

    assert "PlayResX: 540" in ass
    assert "PlayResY: 960" in ass
    assert (
        "Dialogue: 0,0:00:00.00,0:00:01.00,clean_bottom,,0,0,0,,"
        r"第一行\{重点\},逗号\N第二行\}"
    ) in ass


def test_build_segment_ass_deduplicates_styles() -> None:
    clean = _template("clean_bottom")
    top = _template("minimal_top", position="top")
    ass = build_segment_ass(
        [
            _subtitle("sub_1", "一", 0, 10, clean.template_id),
            _subtitle("sub_2", "二", 10, 20, clean.template_id),
            _subtitle("sub_3", "三", 20, 30, top.template_id),
        ],
        segment_start_frame=0,
        segment_end_frame=30,
        fps=30,
        play_res=(540, 960),
        subtitle_templates={clean.template_id: clean, top.template_id: top},
    )

    assert ass.count("Style: clean_bottom") == 1
    assert ass.count("Style: minimal_top") == 1


def test_build_segment_ass_rounds_frame_timecodes_to_centiseconds() -> None:
    template = _template("clean_bottom")
    ass = build_segment_ass(
        [_subtitle("sub_1", "短字幕", 0, 2, template.template_id)],
        segment_start_frame=0,
        segment_end_frame=2,
        fps=30,
        play_res=(540, 960),
        subtitle_templates={template.template_id: template},
    )

    assert "Dialogue: 0,0:00:00.00,0:00:00.07,clean_bottom" in ass


def test_write_segment_ass_uses_caller_path(tmp_path: Path) -> None:
    template = _template("clean_bottom")
    path = tmp_path / "segment.ass"

    written = write_segment_ass(
        path,
        [_subtitle("sub_1", "字幕", 0, 30, template.template_id)],
        segment_start_frame=0,
        segment_end_frame=30,
        fps=30,
        play_res=(540, 960),
        subtitle_templates={template.template_id: template},
    )

    assert written == path
    assert path.read_text(encoding="utf-8").startswith("[Script Info]")


def test_split_attaches_subtitles_without_creating_boundaries() -> None:
    timeline = _timeline(subtitles=[_subtitle_payload("sub_1", "字幕", 10, 20, "clean_bottom")])

    [segment] = split_timeline_segments(timeline)

    assert (segment.timeline_start_frame, segment.timeline_end_frame) == (0, 60)
    assert [clip.timeline_clip_id for clip in segment.subtitles] == ["sub_1"]


def test_subtitle_cache_key_changes_for_add_edit_delete_and_template_params(
    tmp_path: Path,
) -> None:
    template = _template("clean_bottom")
    changed_template = _template("clean_bottom", font_size=48)
    sources = _sources(tmp_path)

    no_subtitle = _cache_key(_timeline(), sources, {template.template_id: template})
    empty_subtitle = _cache_key(_timeline(subtitles=[]), sources, {template.template_id: template})
    added = _cache_key(
        _timeline(subtitles=[_subtitle_payload("sub_1", "字幕", 0, 60, template.template_id)]),
        sources,
        {template.template_id: template},
    )
    edited = _cache_key(
        _timeline(subtitles=[_subtitle_payload("sub_1", "字幕改", 0, 60, template.template_id)]),
        sources,
        {template.template_id: template},
    )
    template_changed = _cache_key(
        _timeline(subtitles=[_subtitle_payload("sub_1", "字幕", 0, 60, template.template_id)]),
        sources,
        {template.template_id: changed_template},
    )

    assert no_subtitle == empty_subtitle
    assert added != no_subtitle
    assert edited != added
    assert template_changed != added


def test_build_segment_command_appends_subtitles_filter_to_vf_path(tmp_path: Path) -> None:
    segment = _segment_with_subtitles()
    output_path = tmp_path / "seg:one\\two'out.mp4"

    command = build_segment_command(
        segment,
        sources=_command_sources(tmp_path),
        profile=PROFILE,
        output_path=output_path,
    )

    vf_filter = command[command.index("-vf") + 1]
    assert "subtitles=filename=" in vf_filter
    assert r"seg\:one\\two\'out.ass" in vf_filter


def test_build_segment_command_appends_subtitles_filter_to_overlay_filtergraph(
    tmp_path: Path,
) -> None:
    segment = _segment_with_subtitles(
        overlays=(
            SegmentClip(
                timeline_clip_id="ov",
                track_id="visual_overlay",
                asset_id="asset_overlay",
                clip_id="clip_overlay",
                timeline_start_frame=0,
                timeline_end_frame=60,
                source_start_frame=0,
                source_end_frame=60,
                playback_rate=1.0,
                effect_chain=(),
            ),
        )
    )
    command = build_segment_command(
        segment,
        sources=_command_sources(tmp_path),
        profile=PROFILE,
        output_path=tmp_path / "overlay:seg.mp4",
    )

    filtergraph = command[command.index("-filter_complex") + 1]
    assert ";[v1]subtitles=filename=" in filtergraph
    assert r"overlay\:seg.ass[vsub]" in filtergraph
    assert command[command.index("-map") + 1] == "[vsub]"


@pytest.mark.ffmpeg
@pytest.mark.skipif(
    shutil.which("ffmpeg") is None or shutil.which("ffprobe") is None,
    reason="ffmpeg/ffprobe not installed",
)
async def test_preview_burns_in_chinese_ass_subtitles(tmp_path: Path) -> None:
    source = tmp_path / "color.mp4"
    _make_color_fixture(source)
    output = tmp_path / "preview.mp4"
    template = get_subtitle_template("clean_bottom")

    await render_preview(
        _timeline(subtitles=[_subtitle_payload("sub_1", "中文字幕", 15, 45, template.template_id)]),
        sources={"asset_1": MediaSource("asset_1", source, "hash_asset_1")},
        paths=WorkspacePaths.from_root(tmp_path / "workspace"),
        output_path=output,
        subtitle_templates={template.template_id: template},
    )
    probe = _ffprobe(output)

    assert output.stat().st_size > 0
    assert float(probe["format"]["duration"]) == pytest.approx(2.0, abs=0.15)


def _cache_key(
    timeline: TimelineState,
    sources: dict[str, MediaSource],
    subtitle_templates: dict[str, SubtitleStyleTemplate],
) -> str:
    [segment] = split_timeline_segments(timeline)
    return compute_segment_cache_key(
        segment,
        sources=sources,
        profile=PROFILE,
        ffmpeg_version=FFMPEG_VERSION,
        subtitle_templates=subtitle_templates,
    )


def _template(
    template_id: str,
    *,
    font_size: int = 42,
    primary_color: str = "#FFFFFF",
    outline_color: str = "#111111",
    position: str = "bottom",
    margin_v: int = 96,
) -> SubtitleStyleTemplate:
    return SubtitleStyleTemplate.model_validate(
        {
            "template_id": template_id,
            "display_name": template_id,
            "font_family": "PingFang SC",
            "font_size": font_size,
            "primary_color": primary_color,
            "outline_color": outline_color,
            "outline_width": 3,
            "position": position,
            "margin_v": margin_v,
        }
    )


def _subtitle(
    timeline_clip_id: str,
    text: str,
    start: int,
    end: int,
    style_template_id: str,
) -> SubtitleClip:
    return SubtitleClip.model_validate(
        _subtitle_payload(timeline_clip_id, text, start, end, style_template_id)
    )


def _subtitle_payload(
    timeline_clip_id: str,
    text: str,
    start: int,
    end: int,
    style_template_id: str,
) -> dict[str, object]:
    return {
        "timeline_clip_id": timeline_clip_id,
        "track_id": "subtitles",
        "text": text,
        "timeline_start_frame": start,
        "timeline_end_frame": end,
        "style_template_id": style_template_id,
        "binding": {"kind": "manual"},
        "safe_area_check": "ok",
    }


def _timeline(subtitles: list[dict[str, object]] | None = None) -> TimelineState:
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
                    "clips": [_media_clip("tc_1", "asset_1", 0, 60)],
                },
                {"track_id": "visual_overlay", "track_type": "visual_overlay", "clips": []},
                {"track_id": "original_audio", "track_type": "audio", "clips": []},
                {"track_id": "voiceover", "track_type": "audio", "clips": []},
                {"track_id": "bgm", "track_type": "audio", "clips": []},
                {"track_id": "subtitles", "track_type": "text", "clips": subtitles or []},
            ],
        }
    )


def _media_clip(
    timeline_clip_id: str,
    asset_id: str,
    start: int,
    end: int,
    *,
    track_id: str = "visual_base",
) -> dict[str, object]:
    return {
        "timeline_clip_id": timeline_clip_id,
        "track_id": track_id,
        "asset_id": asset_id,
        "clip_id": f"clip_{asset_id}",
        "role": "b_roll",
        "timeline_start_frame": start,
        "timeline_end_frame": end,
        "source_start_frame": 0,
        "source_end_frame": end - start,
    }


def _sources(tmp_path: Path) -> dict[str, MediaSource]:
    return {"asset_1": MediaSource("asset_1", tmp_path / "asset_1.mp4", "hash_asset_1")}


def _command_sources(tmp_path: Path) -> dict[str, MediaSource]:
    return {
        "asset_base": MediaSource("asset_base", tmp_path / "base.mp4", "hash_base"),
        "asset_overlay": MediaSource("asset_overlay", tmp_path / "overlay.mp4", "hash_overlay"),
    }


def _segment_with_subtitles(
    *,
    overlays: tuple[SegmentClip, ...] = (),
) -> TimelineSegment:
    return TimelineSegment(
        segment_id="seg_0001_0_60",
        timeline_start_frame=0,
        timeline_end_frame=60,
        fps=30,
        base=SegmentClip(
            timeline_clip_id="base",
            track_id="visual_base",
            asset_id="asset_base",
            clip_id="clip_base",
            timeline_start_frame=0,
            timeline_end_frame=60,
            source_start_frame=0,
            source_end_frame=60,
            playback_rate=1.0,
            effect_chain=(),
        ),
        overlays=overlays,
        subtitles=(_subtitle("sub_1", "字幕", 0, 60, "clean_bottom"),),
    )


def _make_color_fixture(path: Path) -> None:
    subprocess.run(
        [
            "ffmpeg",
            "-y",
            "-f",
            "lavfi",
            "-i",
            "color=c=blue:s=160x160:r=30:d=2",
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
