"""Assemble frame-accurate TimelineState directly from summary-level clips.

The agentic material-understanding path replaces the offline candidate-pack
retrieval loop: the agent picks clips as ``asset_id`` + source second interval +
timeline role, and this module turns that summary-level plan into a six-track
``TimelineState`` (fps / second→frame conversion, source clamping, contiguous
primary track). All frame-level detail stays behind this boundary.
"""

from __future__ import annotations

from collections.abc import Mapping, Sequence
from contextlib import nullcontext
from dataclasses import dataclass
from typing import Any

from sqlalchemy import select
from sqlalchemy.engine import Connection, Engine

from contracts.case import CaseState
from contracts.timeline import TimelineMediaClip, TimelineState, TimelineTrack
from storage import schema
from storage.repositories._json import load_json

_VISUAL_ROLES = ("a_roll", "b_roll", "image")


class MaterializationError(ValueError):
    """Raised when summary-level clips cannot be materialized safely."""


@dataclass(frozen=True, slots=True)
class _ClipSpec:
    asset_id: str
    source_start_s: float
    source_end_s: float
    role: str


@dataclass(frozen=True, slots=True)
class _AssetRow:
    kind: str
    probe: Mapping[str, Any] | None


def materialize_from_clips(
    engine: Engine | Connection,
    case_state: CaseState,
    clips: Sequence[Mapping[str, Any]],
    *,
    voiceover_asset_id: str | None = None,
) -> TimelineState:
    """Build a six-track TimelineState from summary-level clip selections.

    Each entry in ``clips`` gives ``asset_id`` + ``source_start_s`` /
    ``source_end_s`` + ``role`` (``a_roll`` / ``b_roll`` / ``image``). Visual
    clips are laid contiguously on ``visual_base``; when ``voiceover_asset_id``
    is provided one voiceover clip is stretched across the whole timeline.
    """

    specs = [_parse_clip(clip) for clip in clips]
    with _connection_context(engine) as connection:
        project_fps = _project_fps(connection, case_state.project_id)
        version = (case_state.timeline_current_version or 0) + 1
        timeline_id = f"{case_state.case_id}:v{version}"
        visual_clips: list[TimelineMediaClip] = []
        cursor = 0
        for index, spec in enumerate(specs, start=1):
            asset = _asset_row(connection, spec.asset_id)
            source_fps = _source_fps(asset.probe, default=float(project_fps))
            frame_count = _asset_total_frames(asset.probe, source_fps=source_fps)
            duration_frames = max(1, round((spec.source_end_s - spec.source_start_s) * project_fps))
            source_start, source_end = _source_frame_span(
                spec,
                asset,
                source_fps=source_fps,
                frame_count=frame_count,
            )
            timeline_start = cursor
            timeline_end = cursor + duration_frames
            visual_clips.append(
                TimelineMediaClip(
                    timeline_clip_id=_timeline_clip_id("visual", index),
                    track_id="visual_base",
                    asset_id=spec.asset_id,
                    clip_id=None,
                    role=spec.role,
                    timeline_start_frame=timeline_start,
                    timeline_end_frame=timeline_end,
                    source_start_frame=source_start,
                    source_end_frame=source_end,
                    parent_block_id=f"block_{index:03d}",
                )
            )
            cursor = timeline_end
        voiceover_clips = _voiceover_clips(
            connection,
            voiceover_asset_id,
            duration_frames=cursor,
            project_fps=project_fps,
        )

    return TimelineState(
        timeline_id=timeline_id,
        case_id=case_state.case_id,
        version=version,
        fps=project_fps,
        duration_frames=cursor,
        tracks=_tracks(visual_clips=visual_clips, voiceover_clips=voiceover_clips),
        parent_version=case_state.timeline_current_version,
        validation_report={"valid": True, "checks": []},
    )


def _parse_clip(clip: Mapping[str, Any]) -> _ClipSpec:
    asset_id = clip.get("asset_id")
    role = clip.get("role")
    source_start_s = clip.get("source_start_s")
    source_end_s = clip.get("source_end_s")
    if not isinstance(asset_id, str) or not asset_id:
        raise MaterializationError("each clip requires an asset_id")
    if role not in _VISUAL_ROLES:
        raise MaterializationError(f"unsupported clip role: {role!r}")
    if not isinstance(source_start_s, int | float) or isinstance(source_start_s, bool):
        raise MaterializationError(f"clip {asset_id} requires numeric source_start_s")
    if not isinstance(source_end_s, int | float) or isinstance(source_end_s, bool):
        raise MaterializationError(f"clip {asset_id} requires numeric source_end_s")
    if float(source_start_s) < 0:
        raise MaterializationError(f"clip {asset_id} source_start_s must be non-negative")
    if float(source_start_s) >= float(source_end_s):
        raise MaterializationError(f"clip {asset_id} requires source_start_s < source_end_s")
    return _ClipSpec(
        asset_id=asset_id,
        source_start_s=float(source_start_s),
        source_end_s=float(source_end_s),
        role=role,
    )


def _source_frame_span(
    spec: _ClipSpec,
    asset: _AssetRow,
    *,
    source_fps: float,
    frame_count: int | None,
) -> tuple[int, int]:
    if spec.role == "image" or asset.kind == "image":
        return 0, 1
    source_start = max(0, round(spec.source_start_s * source_fps))
    source_end = max(source_start + 1, round(spec.source_end_s * source_fps))
    if frame_count is not None:
        source_end = min(source_end, frame_count)
        source_start = min(source_start, source_end - 1)
    if source_start < 0 or source_start >= source_end:
        raise MaterializationError(f"clip {spec.asset_id} has no usable source frames")
    return source_start, source_end


def _voiceover_clips(
    connection: Connection,
    voiceover_asset_id: str | None,
    *,
    duration_frames: int,
    project_fps: int,
) -> list[TimelineMediaClip]:
    if voiceover_asset_id is None or duration_frames <= 0:
        return []
    probe = _asset_probe(connection, voiceover_asset_id)
    source_fps = _source_fps(probe, default=float(project_fps))
    frame_count = _asset_total_frames(probe, source_fps=source_fps)
    source_end = max(1, round(duration_frames / project_fps * source_fps))
    if frame_count is not None:
        source_end = min(source_end, frame_count)
    if source_end <= 0:
        return []
    return [
        TimelineMediaClip(
            timeline_clip_id="tc_voiceover_001",
            track_id="voiceover",
            asset_id=voiceover_asset_id,
            clip_id=None,
            role="voiceover",
            timeline_start_frame=0,
            timeline_end_frame=duration_frames,
            source_start_frame=0,
            source_end_frame=source_end,
            lock_policy="sync_to_audio",
        )
    ]


def _tracks(
    *,
    visual_clips: list[TimelineMediaClip],
    voiceover_clips: list[TimelineMediaClip],
) -> list[TimelineTrack]:
    return [
        TimelineTrack(track_id="visual_base", track_type="primary_visual", clips=visual_clips),
        TimelineTrack(track_id="visual_overlay", track_type="visual_overlay", clips=[]),
        TimelineTrack(track_id="original_audio", track_type="audio", clips=[]),
        TimelineTrack(track_id="voiceover", track_type="audio", clips=voiceover_clips),
        TimelineTrack(track_id="bgm", track_type="audio", clips=[]),
        TimelineTrack(track_id="subtitles", track_type="text", clips=[]),
    ]


def _asset_row(connection: Connection, asset_id: str) -> _AssetRow:
    row = connection.execute(
        select(schema.assets.c.kind, schema.assets.c.probe).where(
            schema.assets.c.asset_id == asset_id
        )
    ).first()
    if row is None:
        raise MaterializationError(f"asset not found: {asset_id}")
    return _AssetRow(kind=str(row._mapping["kind"]), probe=_probe_payload(row._mapping["probe"]))


def _project_fps(connection: Connection, project_id: str) -> int:
    row = connection.execute(
        select(schema.projects.c.defaults).where(schema.projects.c.project_id == project_id)
    ).first()
    if row is None:
        return 30
    defaults = load_json(str(row._mapping["defaults"]))
    fps = defaults.get("fps") if isinstance(defaults, dict) else None
    if isinstance(fps, int | float) and fps > 0:
        return round(float(fps))
    return 30


def _asset_probe(connection: Connection, asset_id: str) -> Mapping[str, Any] | None:
    row = connection.execute(
        select(schema.assets.c.probe).where(schema.assets.c.asset_id == asset_id)
    ).first()
    if row is None:
        return None
    return _probe_payload(row._mapping["probe"])


def _source_fps(probe: Mapping[str, Any] | None, *, default: float) -> float:
    if probe is not None:
        fps = probe.get("fps")
        if isinstance(fps, int | float) and fps > 0:
            return float(fps)
    return default


def _asset_total_frames(
    probe: Mapping[str, Any] | None,
    *,
    source_fps: float,
) -> int | None:
    if probe is None:
        return None
    for key in ("frame_count", "frames", "duration_frames"):
        value = probe.get(key)
        if isinstance(value, int | float) and value > 0:
            return int(value)
    duration_sec = probe.get("duration_sec")
    if isinstance(duration_sec, int | float) and duration_sec > 0:
        return max(1, round(float(duration_sec) * source_fps))
    return None


def _probe_payload(value: Any) -> Mapping[str, Any] | None:
    if value is None:
        return None
    if isinstance(value, Mapping):
        return value
    parsed = load_json(str(value))
    return parsed if isinstance(parsed, dict) else None


def _timeline_clip_id(prefix: str, index: int) -> str:
    return f"tc_{prefix}_{index:03d}"


def _connection_context(engine: Engine | Connection) -> Any:
    if isinstance(engine, Connection):
        return nullcontext(engine)
    return engine.connect()
