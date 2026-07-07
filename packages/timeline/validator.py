"""TimelineState structural invariants from PRD §10.2."""

from __future__ import annotations

from collections.abc import Callable, Mapping, Sequence
from contextlib import nullcontext
from dataclasses import dataclass
from typing import Any

from sqlalchemy import select
from sqlalchemy.engine import Connection, Engine

from contracts.case import CaseState
from contracts.subtitle import SubtitleClip
from contracts.timeline import TimelineMediaClip, TimelineState, TimelineValidationReport
from storage import schema
from storage.repositories._json import load_json

TimelineInvariantHook = Callable[[Connection, CaseState, TimelineState], Sequence[str]]


@dataclass(frozen=True, slots=True)
class TimelineValidationContext:
    connection: Connection
    case_state: CaseState
    project_fps: int


def validate_timeline(
    engine: Engine | Connection,
    case_state: CaseState,
    timeline: TimelineState,
) -> TimelineValidationReport:
    """Validate all PRD §10.2 invariants and return a structured report."""

    with _connection_context(engine) as connection:
        context = TimelineValidationContext(
            connection=connection,
            case_state=case_state,
            project_fps=_project_fps(connection, case_state.project_id),
        )
        checks = _existing_warnings(timeline)
        checks.extend(_validate_identity(context, timeline))
        checks.extend(_validate_primary_visual_coverage(timeline))
        checks.extend(_validate_asset_references(context, timeline))
        checks.extend(_validate_source_ranges(context, timeline))
        checks.extend(_validate_audio_and_subtitle_bindings(timeline))
        checks.extend(_validate_fps_and_ranges(context, timeline))
    return TimelineValidationReport(
        valid=not any(check.get("severity") == "error" for check in checks),
        checks=checks,
    )


def validate_timeline_invariants(
    connection: Connection,
    case_state: CaseState,
    timeline: TimelineState,
) -> Sequence[str]:
    """Reducer hook adapter that keeps agent_harness independent from timeline."""

    report = validate_timeline(connection, case_state, timeline)
    return tuple(
        f"{check.get('code')}: {check.get('message')}"
        for check in report.checks
        if check.get("severity") == "error"
    )


def build_timeline_invariant_hook() -> TimelineInvariantHook:
    return validate_timeline_invariants


def _existing_warnings(timeline: TimelineState) -> list[dict[str, Any]]:
    if timeline.validation_report is None:
        return []
    return [
        dict(check)
        for check in timeline.validation_report.checks
        if isinstance(check, dict) and check.get("severity") == "warning"
    ]


def _validate_identity(
    context: TimelineValidationContext,
    timeline: TimelineState,
) -> list[dict[str, Any]]:
    checks: list[dict[str, Any]] = []
    if timeline.case_id != context.case_state.case_id:
        checks.append(
            _error(
                "timeline.identity.case_mismatch",
                "timeline case_id must match the active case",
                timeline_case_id=timeline.case_id,
                case_id=context.case_state.case_id,
            )
        )
    if timeline.duration_frames < 0:
        checks.append(
            _error(
                "timeline.duration.negative",
                "timeline duration_frames must be non-negative",
                duration_frames=timeline.duration_frames,
            )
        )
    return checks


def _validate_primary_visual_coverage(timeline: TimelineState) -> list[dict[str, Any]]:
    track = _track(timeline, "visual_base")
    clips = sorted(_media_clips(track), key=lambda clip: clip.timeline_start_frame)
    checks: list[dict[str, Any]] = []
    cursor = 0
    for clip in clips:
        if clip.timeline_start_frame > cursor:
            checks.append(
                _error(
                    "timeline.primary_visual.gap",
                    f"primary visual has gap [{cursor},{clip.timeline_start_frame})",
                    start_frame=cursor,
                    end_frame=clip.timeline_start_frame,
                )
            )
        if clip.timeline_start_frame < cursor:
            checks.append(
                _error(
                    "timeline.primary_visual.overlap",
                    (f"primary visual overlaps before {cursor}: {clip.timeline_clip_id}"),
                    timeline_clip_id=clip.timeline_clip_id,
                    overlap_start_frame=clip.timeline_start_frame,
                    expected_start_frame=cursor,
                )
            )
        cursor = max(cursor, clip.timeline_end_frame)
    if cursor < timeline.duration_frames:
        checks.append(
            _error(
                "timeline.primary_visual.gap",
                f"primary visual has gap [{cursor},{timeline.duration_frames})",
                start_frame=cursor,
                end_frame=timeline.duration_frames,
            )
        )
    if cursor > timeline.duration_frames:
        checks.append(
            _error(
                "timeline.primary_visual.overrun",
                "primary visual extends beyond timeline duration",
                visual_end_frame=cursor,
                duration_frames=timeline.duration_frames,
            )
        )
    return checks


def _validate_asset_references(
    context: TimelineValidationContext,
    timeline: TimelineState,
) -> list[dict[str, Any]]:
    checks: list[dict[str, Any]] = []
    disabled = set(context.case_state.disabled_asset_ids)
    for asset_id in sorted(_asset_ids(timeline)):
        row = _asset_reference_row(context.connection, context.case_state.project_id, asset_id)
        if row is None:
            checks.append(
                _error(
                    "timeline.asset_reference.missing_or_unlinked",
                    "timeline references an asset that is not linked to this project",
                    asset_id=asset_id,
                )
            )
            continue
        if not bool(row.get("link_enabled")):
            checks.append(
                _error(
                    "timeline.asset_reference.unlinked",
                    "timeline references a disabled project asset link",
                    asset_id=asset_id,
                )
            )
        if not bool(row.get("usable")):
            checks.append(
                _error(
                    "timeline.asset_reference.unusable",
                    "timeline references an unusable asset",
                    asset_id=asset_id,
                )
            )
        if asset_id in disabled:
            checks.append(
                _error(
                    "timeline.asset_reference.case_disabled",
                    "timeline references an asset disabled for this case",
                    asset_id=asset_id,
                )
            )
    return checks


def _validate_source_ranges(
    context: TimelineValidationContext,
    timeline: TimelineState,
) -> list[dict[str, Any]]:
    checks: list[dict[str, Any]] = []
    probe_by_asset = _asset_probe_rows(context.connection, _asset_ids(timeline))
    for clip in _all_media_clips(timeline):
        probe = probe_by_asset.get(clip.asset_id)
        source_fps = _source_fps(probe, default=float(timeline.fps))
        frame_count = _asset_total_frames(probe, source_fps=source_fps)
        if frame_count is not None and (
            clip.source_start_frame < 0 or clip.source_end_frame > frame_count
        ):
            checks.append(
                _error(
                    "timeline.source_range.out_of_bounds",
                    "clip source range is outside the asset frame count",
                    timeline_clip_id=clip.timeline_clip_id,
                    asset_id=clip.asset_id,
                    source_start_frame=clip.source_start_frame,
                    source_end_frame=clip.source_end_frame,
                    frame_count=frame_count,
                )
            )
    return checks


def _validate_audio_and_subtitle_bindings(timeline: TimelineState) -> list[dict[str, Any]]:
    checks: list[dict[str, Any]] = []
    for track_id in ("original_audio", "voiceover", "bgm"):
        for clip in _media_clips(_track(timeline, track_id)):
            if clip.timeline_start_frame < 0 or clip.timeline_end_frame > timeline.duration_frames:
                checks.append(
                    _error(
                        "timeline.audio.out_of_bounds",
                        "audio clip extends outside the timeline duration",
                        timeline_clip_id=clip.timeline_clip_id,
                        track_id=track_id,
                        timeline_start_frame=clip.timeline_start_frame,
                        timeline_end_frame=clip.timeline_end_frame,
                        duration_frames=timeline.duration_frames,
                    )
                )
    for subtitle in _subtitle_clips(timeline):
        if (
            subtitle.timeline_start_frame < 0
            or subtitle.timeline_end_frame > timeline.duration_frames
        ):
            checks.append(
                _error(
                    "timeline.subtitle.out_of_bounds",
                    "subtitle clip extends outside the timeline duration",
                    timeline_clip_id=subtitle.timeline_clip_id,
                    timeline_start_frame=subtitle.timeline_start_frame,
                    timeline_end_frame=subtitle.timeline_end_frame,
                    duration_frames=timeline.duration_frames,
                )
            )
        if not _subtitle_binding_target_exists(timeline, subtitle):
            checks.append(
                _error(
                    "timeline.subtitle.binding_missing",
                    "subtitle binding target does not exist",
                    timeline_clip_id=subtitle.timeline_clip_id,
                    binding=subtitle.binding.model_dump(mode="json"),
                )
            )
    return checks


def _validate_fps_and_ranges(
    context: TimelineValidationContext,
    timeline: TimelineState,
) -> list[dict[str, Any]]:
    checks: list[dict[str, Any]] = []
    if timeline.fps != context.project_fps:
        checks.append(
            _error(
                "timeline.fps.mismatch",
                "timeline fps must match project defaults",
                timeline_fps=timeline.fps,
                project_fps=context.project_fps,
            )
        )
    for clip in _all_media_clips(timeline):
        if clip.timeline_start_frame < 0 or clip.source_start_frame < 0:
            checks.append(
                _error(
                    "timeline.range.negative",
                    "clip ranges must be non-negative half-open intervals",
                    timeline_clip_id=clip.timeline_clip_id,
                )
            )
        if clip.timeline_start_frame >= clip.timeline_end_frame:
            checks.append(
                _error(
                    "timeline.range.invalid_timeline",
                    "clip timeline range must satisfy start < end",
                    timeline_clip_id=clip.timeline_clip_id,
                )
            )
        if clip.source_start_frame >= clip.source_end_frame:
            checks.append(
                _error(
                    "timeline.range.invalid_source",
                    "clip source range must satisfy start < end",
                    timeline_clip_id=clip.timeline_clip_id,
                )
            )
    for subtitle in _subtitle_clips(timeline):
        if subtitle.timeline_start_frame < 0:
            checks.append(
                _error(
                    "timeline.range.negative",
                    "subtitle ranges must be non-negative half-open intervals",
                    timeline_clip_id=subtitle.timeline_clip_id,
                )
            )
        if subtitle.timeline_start_frame >= subtitle.timeline_end_frame:
            checks.append(
                _error(
                    "timeline.range.invalid_subtitle",
                    "subtitle timeline range must satisfy start < end",
                    timeline_clip_id=subtitle.timeline_clip_id,
                )
            )
    return checks


def _subtitle_binding_target_exists(timeline: TimelineState, subtitle: SubtitleClip) -> bool:
    if subtitle.binding.kind == "manual":
        return True
    target_track_id = "voiceover" if subtitle.binding.kind == "voiceover" else "original_audio"
    target_clips = _media_clips(_track(timeline, target_track_id))
    overlapping = [
        clip
        for clip in target_clips
        if clip.timeline_start_frame < subtitle.timeline_end_frame
        and clip.timeline_end_frame > subtitle.timeline_start_frame
    ]
    if not overlapping:
        return False
    utterance_id = subtitle.binding.utterance_id
    if utterance_id is None:
        return True
    return any(_clip_mentions_utterance(clip, utterance_id) for clip in overlapping) or bool(
        overlapping
    )


def _clip_mentions_utterance(clip: TimelineMediaClip, utterance_id: str) -> bool:
    for effect in clip.effects:
        narration_ref = effect.get("narration_ref")
        if isinstance(narration_ref, Mapping):
            utterance_ids = narration_ref.get("utterance_ids")
            if (
                isinstance(utterance_ids, Sequence)
                and not isinstance(utterance_ids, str | bytes)
                and utterance_id in utterance_ids
            ):
                return True
            if narration_ref.get("utterance_id") == utterance_id:
                return True
        if effect.get("utterance_id") == utterance_id:
            return True
    return False


def _track(timeline: TimelineState, track_id: str) -> Any:
    for track in timeline.tracks:
        if track.track_id == track_id:
            return track
    raise ValueError(f"missing canonical track: {track_id}")


def _media_clips(track: Any) -> list[TimelineMediaClip]:
    return [clip for clip in track.clips if isinstance(clip, TimelineMediaClip)]


def _subtitle_clips(timeline: TimelineState) -> list[SubtitleClip]:
    track = _track(timeline, "subtitles")
    return [clip for clip in track.clips if isinstance(clip, SubtitleClip)]


def _all_media_clips(timeline: TimelineState) -> list[TimelineMediaClip]:
    return [
        clip
        for track in timeline.tracks
        if track.track_id != "subtitles"
        for clip in _media_clips(track)
    ]


def _asset_ids(timeline: TimelineState) -> set[str]:
    return {clip.asset_id for clip in _all_media_clips(timeline)}


def _asset_reference_row(
    connection: Connection,
    project_id: str,
    asset_id: str,
) -> dict[str, Any] | None:
    row = connection.execute(
        select(
            schema.assets.c.asset_id,
            schema.assets.c.usable,
            schema.project_asset_links.c.enabled.label("link_enabled"),
        )
        .select_from(
            schema.assets.outerjoin(
                schema.project_asset_links,
                (schema.project_asset_links.c.asset_id == schema.assets.c.asset_id)
                & (schema.project_asset_links.c.project_id == project_id),
            )
        )
        .where(schema.assets.c.asset_id == asset_id)
    ).first()
    return None if row is None else dict(row._mapping)


def _asset_probe_rows(
    connection: Connection,
    asset_ids: set[str],
) -> dict[str, Mapping[str, Any] | None]:
    if not asset_ids:
        return {}
    rows = connection.execute(
        select(schema.assets.c.asset_id, schema.assets.c.probe).where(
            schema.assets.c.asset_id.in_(asset_ids)
        )
    ).all()
    return {str(row._mapping["asset_id"]): _probe_payload(row._mapping["probe"]) for row in rows}


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


def _error(code: str, message: str, **details: Any) -> dict[str, Any]:
    return {
        "code": code,
        "severity": "error",
        "message": message,
        "details": details,
    }


def _connection_context(engine: Engine | Connection) -> Any:
    if isinstance(engine, Connection):
        return nullcontext(engine)
    return engine.connect()
