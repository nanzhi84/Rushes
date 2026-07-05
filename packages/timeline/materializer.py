"""Materialize LLM candidate selections into frame-accurate TimelineState."""

from __future__ import annotations

from collections.abc import Mapping, Sequence
from contextlib import nullcontext
from dataclasses import dataclass
from typing import Any

from sqlalchemy import select
from sqlalchemy.engine import Connection, Engine

from contracts.candidate import Candidate, CandidatePack, CandidateSlot
from contracts.case import CaseState, CutPlanSlot
from contracts.timeline import TimelineMediaClip, TimelineState, TimelineTrack
from storage import schema
from storage.repositories._json import load_json


class MaterializationError(ValueError):
    """Raised when semantic selections cannot be materialized safely."""


@dataclass(frozen=True, slots=True)
class _ClipProjection:
    clip_id: str
    annotation_id: str
    asset_id: str
    start_frame: int
    end_frame: int
    role: str
    summary: str
    probe: Mapping[str, Any] | None
    annotation_document_json: str
    asset_kind: str


@dataclass(frozen=True, slots=True)
class _SelectedSpan:
    source_start_frame: int
    source_end_frame: int
    timeline_duration_frames: int
    warning: dict[str, Any] | None = None


def materialize_from_selection(
    engine: Engine | Connection,
    case_state: CaseState,
    pack: CandidatePack,
    selections: Sequence[Mapping[str, str]],
) -> TimelineState:
    """Convert selected candidate IDs into a six-track TimelineState.

    The LLM-facing surface is intentionally limited to slot_id and candidate_id.
    Source frames, timeline frames, fps conversion, clamping, and hard-event
    avoidance all happen at this boundary.
    """

    if pack.case_id != case_state.case_id:
        raise MaterializationError("candidate pack does not belong to the active case")
    if case_state.cut_plan is None:
        raise MaterializationError("cut_plan is required to materialize a timeline")

    selected_by_slot = _selection_map(selections)
    slot_by_id = {slot.slot_id: slot for slot in case_state.cut_plan.slots}
    missing_slots = [slot.slot_id for slot in pack.slots if slot.slot_id not in selected_by_slot]
    if missing_slots:
        raise MaterializationError("missing selection for slot(s): " + ", ".join(missing_slots))

    with _connection_context(engine) as connection:
        project_fps = _project_fps(connection, case_state.project_id)
        selected_candidates = _resolve_candidates(pack, selected_by_slot)
        clip_rows = _clip_projection_rows(
            connection,
            [candidate.clip_id for candidate in selected_candidates.values()],
        )
        version = (case_state.timeline_current_version or 0) + 1
        timeline_id = f"{case_state.case_id}:v{version}"
        visual_clips: list[TimelineMediaClip] = []
        voiceover_clips: list[TimelineMediaClip] = []
        checks: list[dict[str, Any]] = []
        cursor = 0
        for index, pack_slot in enumerate(pack.slots, start=1):
            candidate = selected_candidates[pack_slot.slot_id]
            clip_row = clip_rows.get(candidate.clip_id)
            if clip_row is None:
                raise MaterializationError(f"selected clip not found: {candidate.clip_id}")
            cut_slot = slot_by_id.get(pack_slot.slot_id)
            if cut_slot is None:
                raise MaterializationError(f"cut_plan slot not found: {pack_slot.slot_id}")
            span = _select_clean_source_span(
                clip_row,
                cut_slot,
                project_fps=project_fps,
            )
            if span.warning is not None:
                checks.append(span.warning)
            timeline_start = cursor
            timeline_end = cursor + span.timeline_duration_frames
            visual_clips.append(
                TimelineMediaClip(
                    timeline_clip_id=_timeline_clip_id("visual", index, pack_slot.slot_id),
                    track_id="visual_base",
                    asset_id=clip_row.asset_id,
                    clip_id=clip_row.clip_id,
                    role=_timeline_role(clip_row.role),
                    timeline_start_frame=timeline_start,
                    timeline_end_frame=timeline_end,
                    source_start_frame=span.source_start_frame,
                    source_end_frame=span.source_end_frame,
                    parent_block_id=pack_slot.slot_id,
                    effects=[
                        {
                            "kind": "source_summary",
                            "summary": clip_row.summary,
                            "candidate_id": candidate.candidate_id,
                        }
                    ],
                )
            )
            voiceover = _voiceover_clip_for_slot(
                connection,
                case_state,
                cut_slot,
                slot_index=index,
                slot_id=pack_slot.slot_id,
                timeline_start_frame=timeline_start,
                timeline_end_frame=timeline_end,
                project_fps=project_fps,
            )
            if voiceover is not None:
                voiceover_clips.append(voiceover)
            cursor = timeline_end

    return TimelineState(
        timeline_id=timeline_id,
        case_id=case_state.case_id,
        version=version,
        fps=project_fps,
        duration_frames=cursor,
        tracks=_tracks(visual_clips=visual_clips, voiceover_clips=voiceover_clips),
        parent_version=case_state.timeline_current_version,
        validation_report={"valid": True, "checks": checks},
    )


def _selection_map(selections: Sequence[Mapping[str, str]]) -> dict[str, str]:
    selected: dict[str, str] = {}
    for selection in selections:
        slot_id = selection.get("slot_id")
        candidate_id = selection.get("candidate_id")
        if not isinstance(slot_id, str) or not isinstance(candidate_id, str):
            raise MaterializationError("each selection requires slot_id and candidate_id")
        if slot_id in selected:
            raise MaterializationError(f"duplicate selection for slot: {slot_id}")
        selected[slot_id] = candidate_id
    return selected


def _resolve_candidates(
    pack: CandidatePack,
    selected_by_slot: Mapping[str, str],
) -> dict[str, Candidate]:
    resolved: dict[str, Candidate] = {}
    for slot in pack.slots:
        selected_candidate_id = selected_by_slot[slot.slot_id]
        candidate = _candidate_for_slot(slot, selected_candidate_id)
        if candidate is None:
            raise MaterializationError(
                f"candidate {selected_candidate_id} is not in slot {slot.slot_id}"
            )
        resolved[slot.slot_id] = candidate
    unknown_slots = sorted(set(selected_by_slot) - {slot.slot_id for slot in pack.slots})
    if unknown_slots:
        raise MaterializationError(
            "selection references unknown slot(s): " + ", ".join(unknown_slots)
        )
    return resolved


def _candidate_for_slot(slot: CandidateSlot, candidate_id: str) -> Candidate | None:
    for candidate in slot.candidates:
        if candidate.candidate_id == candidate_id:
            return candidate
    return None


def _clip_projection_rows(
    connection: Connection,
    clip_ids: Sequence[str],
) -> dict[str, _ClipProjection]:
    if not clip_ids:
        return {}
    rows = connection.execute(
        select(
            schema.annotation_clip_projection.c.clip_id,
            schema.annotation_clip_projection.c.annotation_id,
            schema.annotation_clip_projection.c.asset_id,
            schema.annotation_clip_projection.c.start_frame,
            schema.annotation_clip_projection.c.end_frame,
            schema.annotation_clip_projection.c.role,
            schema.annotation_clip_projection.c.summary,
            schema.assets.c.probe,
            schema.assets.c.kind,
            schema.annotations_table.c.document_json,
        )
        .select_from(
            schema.annotation_clip_projection.join(
                schema.assets,
                schema.assets.c.asset_id == schema.annotation_clip_projection.c.asset_id,
            ).join(
                schema.annotations_table,
                schema.annotations_table.c.annotation_id
                == schema.annotation_clip_projection.c.annotation_id,
            )
        )
        .where(schema.annotation_clip_projection.c.clip_id.in_(list(clip_ids)))
    ).all()
    result: dict[str, _ClipProjection] = {}
    for row in rows:
        values = row._mapping
        probe = _probe_payload(values["probe"])
        clip_id = str(values["clip_id"])
        result[clip_id] = _ClipProjection(
            clip_id=clip_id,
            annotation_id=str(values["annotation_id"]),
            asset_id=str(values["asset_id"]),
            start_frame=int(values["start_frame"]),
            end_frame=int(values["end_frame"]),
            role=str(values["role"]),
            summary=str(values["summary"]),
            probe=probe,
            annotation_document_json=str(values["document_json"]),
            asset_kind=str(values["kind"]),
        )
    return result


def _select_clean_source_span(
    clip: _ClipProjection,
    slot: CutPlanSlot,
    *,
    project_fps: int,
) -> _SelectedSpan:
    if clip.asset_kind == "image":
        return _image_span(clip, slot, project_fps=project_fps)
    source_fps = _source_fps(clip.probe, default=float(project_fps))
    asset_total_frames = _asset_total_frames(clip.probe, source_fps=source_fps)
    source_start = max(0, clip.start_frame)
    source_end = (
        clip.end_frame if asset_total_frames is None else min(clip.end_frame, asset_total_frames)
    )
    if source_start >= source_end:
        raise MaterializationError(f"clip has no usable source frames: {clip.clip_id}")
    clean_spans = _clean_spans(
        source_start,
        source_end,
        _hard_quality_events(clip.annotation_document_json),
    )
    warning: dict[str, Any] | None = None
    if clean_spans:
        selected_start, selected_end = max(clean_spans, key=lambda item: item[1] - item[0])
    else:
        selected_start, selected_end = source_start, source_end
        warning = _warning(
            "timeline.materialize.no_clean_span",
            "no clean span remains after hard quality events; using clamped clip span",
            slot_id=slot.slot_id,
            clip_id=clip.clip_id,
        )
    min_frames, max_frames = _target_frame_window(slot, project_fps=project_fps)
    clean_project_frames = _source_to_project_frames(
        selected_end - selected_start,
        source_fps=source_fps,
        project_fps=project_fps,
    )
    if clean_project_frames > max_frames:
        timeline_duration = max_frames
        source_needed = max(
            1,
            round((timeline_duration / project_fps) * source_fps),
        )
        selected_end = min(selected_end, selected_start + source_needed)
    else:
        timeline_duration = max(1, clean_project_frames)
        if clean_project_frames < min_frames:
            warning = _warning(
                "timeline.materialize.short_clean_span",
                "clean span is shorter than the slot target; using the full clean span",
                slot_id=slot.slot_id,
                clip_id=clip.clip_id,
                target_min_frames=min_frames,
                actual_frames=clean_project_frames,
            )
    return _SelectedSpan(
        source_start_frame=selected_start,
        source_end_frame=selected_end,
        timeline_duration_frames=timeline_duration,
        warning=warning,
    )


def _image_span(
    clip: _ClipProjection,
    slot: CutPlanSlot,
    *,
    project_fps: int,
) -> _SelectedSpan:
    _min_frames, max_frames = _target_frame_window(slot, project_fps=project_fps)
    source_start = max(0, clip.start_frame)
    source_end = max(source_start + 1, min(clip.end_frame, source_start + 1))
    return _SelectedSpan(
        source_start_frame=source_start,
        source_end_frame=source_end,
        timeline_duration_frames=max_frames,
    )


def _voiceover_clip_for_slot(
    connection: Connection,
    case_state: CaseState,
    slot: CutPlanSlot,
    *,
    slot_index: int,
    slot_id: str,
    timeline_start_frame: int,
    timeline_end_frame: int,
    project_fps: int,
) -> TimelineMediaClip | None:
    if slot.narration_ref is None or case_state.audio_plan is None:
        return None
    voiceover_asset_id = case_state.audio_plan.voiceover_asset_id
    if voiceover_asset_id is None:
        return None
    source_span = _voiceover_source_span(
        connection,
        voiceover_asset_id,
        slot.narration_ref,
        project_fps=project_fps,
    )
    if source_span is None:
        return None
    source_start, source_end = source_span
    return TimelineMediaClip(
        timeline_clip_id=_timeline_clip_id("voiceover", slot_index, slot_id),
        track_id="voiceover",
        asset_id=voiceover_asset_id,
        clip_id=None,
        role="voiceover",
        timeline_start_frame=timeline_start_frame,
        timeline_end_frame=timeline_end_frame,
        source_start_frame=source_start,
        source_end_frame=source_end,
        lock_policy="sync_to_audio",
        parent_block_id=slot_id,
        effects=[
            {
                "kind": "narration_ref",
                "narration_ref": dict(slot.narration_ref),
            }
        ],
    )


def _voiceover_source_span(
    connection: Connection,
    asset_id: str,
    narration_ref: Mapping[str, Any],
    *,
    project_fps: int,
) -> tuple[int, int] | None:
    start_ms, end_ms = _narration_ms_range(connection, asset_id, narration_ref)
    if start_ms is None or end_ms is None or start_ms >= end_ms:
        return None
    probe = _asset_probe(connection, asset_id)
    source_fps = _source_fps(probe, default=float(project_fps))
    source_start = max(0, round(start_ms / 1000 * source_fps))
    source_end = max(source_start + 1, round(end_ms / 1000 * source_fps))
    total_frames = _asset_total_frames(probe, source_fps=source_fps)
    if total_frames is not None:
        source_end = min(source_end, total_frames)
        if source_start >= source_end:
            return None
    return source_start, source_end


def _narration_ms_range(
    connection: Connection,
    asset_id: str,
    narration_ref: Mapping[str, Any],
) -> tuple[int | None, int | None]:
    start_ms = _int_value(narration_ref.get("start_ms"))
    end_ms = _int_value(narration_ref.get("end_ms"))
    if start_ms is not None and end_ms is not None:
        return start_ms, end_ms
    transcript_id = narration_ref.get("transcript_id")
    utterance_ids = narration_ref.get("utterance_ids")
    if (
        not isinstance(transcript_id, str)
        or not isinstance(utterance_ids, Sequence)
        or isinstance(utterance_ids, str | bytes)
    ):
        return None, None
    wanted = {str(item) for item in utterance_ids if isinstance(item, str)}
    if not wanted:
        return None, None
    row = connection.execute(
        select(schema.transcripts.c.utterances)
        .where(schema.transcripts.c.transcript_id == transcript_id)
        .where(schema.transcripts.c.asset_id == asset_id)
    ).first()
    if row is None:
        return None, None
    utterances = load_json(str(row._mapping["utterances"]))
    if not isinstance(utterances, list):
        return None, None
    starts: list[int] = []
    ends: list[int] = []
    for utterance in utterances:
        if not isinstance(utterance, dict) or utterance.get("utterance_id") not in wanted:
            continue
        candidate_start = _int_value(utterance.get("start_ms"))
        candidate_end = _int_value(utterance.get("end_ms"))
        if candidate_start is None or candidate_end is None or candidate_start >= candidate_end:
            continue
        starts.append(candidate_start)
        ends.append(candidate_end)
    if not starts or not ends:
        return None, None
    return min(starts), max(ends)


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


def _clean_spans(
    start_frame: int,
    end_frame: int,
    hard_events: Sequence[tuple[int, int]],
) -> list[tuple[int, int]]:
    spans = [(start_frame, end_frame)]
    for event_start, event_end in sorted(hard_events):
        next_spans: list[tuple[int, int]] = []
        for span_start, span_end in spans:
            overlap_start = max(span_start, event_start)
            overlap_end = min(span_end, event_end)
            if overlap_start >= overlap_end:
                next_spans.append((span_start, span_end))
                continue
            if span_start < overlap_start:
                next_spans.append((span_start, overlap_start))
            if overlap_end < span_end:
                next_spans.append((overlap_end, span_end))
        spans = next_spans
    return [(start, end) for start, end in spans if start < end]


def _hard_quality_events(document_json: str) -> list[tuple[int, int]]:
    document = load_json(document_json)
    if not isinstance(document, dict):
        return []
    events = document.get("quality_events")
    if not isinstance(events, list):
        return []
    hard_events: list[tuple[int, int]] = []
    for event in events:
        if not isinstance(event, dict) or event.get("severity") != "hard":
            continue
        start = _int_value(event.get("start_frame"))
        end = _int_value(event.get("end_frame"))
        if start is not None and end is not None and start < end:
            hard_events.append((start, end))
    return hard_events


def _target_frame_window(slot: CutPlanSlot, *, project_fps: int) -> tuple[int, int]:
    min_sec, max_sec = slot.target_duration_sec
    min_frames = max(1, round(min_sec * project_fps))
    max_frames = max(min_frames, round(max_sec * project_fps))
    return min_frames, max_frames


def _source_to_project_frames(
    source_frames: int,
    *,
    source_fps: float,
    project_fps: int,
) -> int:
    return max(1, round((source_frames / source_fps) * project_fps))


def _timeline_role(role: str) -> str:
    role_map = {
        "a_roll_candidate": "a_roll",
        "b_roll_candidate": "b_roll",
        "image_candidate": "image",
    }
    mapped = role_map.get(role)
    if mapped is None:
        raise MaterializationError(f"unsupported candidate role: {role}")
    return mapped


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


def _int_value(value: Any) -> int | None:
    if isinstance(value, bool):
        return None
    if isinstance(value, int):
        return value
    if isinstance(value, float) and value.is_integer():
        return int(value)
    return None


def _warning(
    code: str,
    message: str,
    **details: Any,
) -> dict[str, Any]:
    return {
        "code": code,
        "severity": "warning",
        "message": message,
        "details": details,
    }


def _timeline_clip_id(prefix: str, index: int, slot_id: str) -> str:
    safe_slot = "".join(char if char.isalnum() or char == "_" else "_" for char in slot_id)
    return f"tc_{prefix}_{index:03d}_{safe_slot}"


def _connection_context(engine: Engine | Connection) -> Any:
    if isinstance(engine, Connection):
        return nullcontext(engine)
    return engine.connect()
