"""Apply resolved timeline patches to TimelineState documents."""

from __future__ import annotations

import hashlib
import json
import math
from collections.abc import Mapping, Sequence
from contextlib import nullcontext
from dataclasses import dataclass, field
from datetime import UTC, datetime
from typing import Any, Literal, cast
from uuid import uuid4

from sqlalchemy import select
from sqlalchemy.engine import Connection, Engine

from contracts.draft import DraftState
from contracts.events import (
    TimelineValidated,
    TimelineValidationFailed,
    TimelineVersionCreated,
)
from contracts.patch import (
    AddBgmOp,
    AdjustGainOp,
    AllRange,
    DeleteRangeOp,
    EditSubtitleTextOp,
    GenerateSubtitlesOp,
    InsertClipOp,
    RemoveTrackClipsOp,
    ReorderBlocksOp,
    ReplaceClipOp,
    ResolvedRange,
    ResolvedTimelinePatch,
    SetPlaybackRateOp,
    SetSubtitleStyleOp,
    TimelinePatchRequest,
    TrimClipOp,
)
from contracts.subtitle import SubtitleBinding, SubtitleClip
from contracts.timeline import (
    TimelineMediaClip,
    TimelineState,
    TimelineTrack,
    TimelineValidationReport,
)
from storage import schema
from storage.repositories._json import load_json

from .anchor import AnchorConflict, AnchorResolution, AnchorResolutionError, resolve_anchor
from .validator import validate_timeline
from .version_store import list_timeline_versions, store_timeline_version

TimelineClip = TimelineMediaClip | SubtitleClip
PatchStatus = Literal["succeeded", "conflict", "failed"]


class PatchApplyError(ValueError):
    """Raised when a resolved patch cannot be applied safely."""

    def __init__(
        self,
        code: str,
        message: str,
        *,
        details: Mapping[str, Any] | None = None,
    ) -> None:
        super().__init__(message)
        self.code = code
        self.details = dict(details or {})


@dataclass(frozen=True, slots=True)
class PatchOutcome:
    status: PatchStatus
    timeline: TimelineState | None = None
    resolved_patch: ResolvedTimelinePatch | None = None
    validation_report: TimelineValidationReport | None = None
    changed_track_ids: tuple[str, ...] = ()
    events: tuple[dict[str, Any], ...] = ()
    conflict: AnchorConflict | None = None
    error: PatchApplyError | AnchorResolutionError | None = None
    metadata: Mapping[str, Any] = field(default_factory=dict)


@dataclass(frozen=True, slots=True)
class _ClipMaterial:
    asset_id: str
    source_start_frame: int
    source_end_frame: int
    role: str
    asset_kind: str


@dataclass(frozen=True, slots=True)
class _UtterancePlacement:
    utterance_id: str
    text: str
    timeline_start_frame: int
    timeline_end_frame: int


def apply_patch(
    engine: Engine | Connection,
    draft_state: DraftState,
    request: TimelinePatchRequest,
    *,
    created_at: str | None = None,
) -> PatchOutcome:
    """Resolve and apply one TimelinePatchRequest, storing the produced version."""

    try:
        resolution = resolve_anchor(engine, draft_state, request)
    except AnchorConflict as exc:
        return PatchOutcome(
            status="conflict",
            conflict=exc,
            error=exc,
            metadata={"conflict": exc.details, "code": exc.code},
        )
    except AnchorResolutionError as exc:
        return PatchOutcome(
            status="failed",
            error=exc,
            metadata={"error": exc.details, "code": exc.code},
        )

    patch_id = _patch_id(request)
    with _connection_context(engine) as connection:
        try:
            patched = _apply_resolved_op(connection, draft_state, resolution, patch_id)
        except PatchApplyError as exc:
            return PatchOutcome(
                status="failed",
                error=exc,
                metadata={"error": exc.details, "code": exc.code},
            )
        new_version = _next_version(connection, draft_state.draft_id)
        patched = patched.model_copy(
            update={
                "timeline_id": f"{draft_state.draft_id}:v{new_version}",
                "version": new_version,
                "parent_version": resolution.current_version,
                "created_by_patch_id": patch_id,
            },
            deep=True,
        )
        report = validate_timeline(connection, draft_state, patched)
        patched = patched.model_copy(update={"validation_report": report}, deep=True)
        changed_track_ids = _changed_track_ids(resolution.current_timeline, patched)
        resolved_patch = ResolvedTimelinePatch(
            patch_id=patch_id,
            request_ref=request,
            resolved=resolution.current_range,
            produced_timeline_version=patched.version,
            metadata={
                **dict(resolution.metadata),
                "anchor_resolved": resolution.anchor_range.model_dump(mode="json"),
                "current_resolved": resolution.current_range.model_dump(mode="json"),
                "changed_track_ids": list(changed_track_ids),
            },
        )
        store_timeline_version(connection, patched, created_at=created_at)

    created_event = TimelineVersionCreated(
        draft_id=draft_state.draft_id,
        timeline_version=patched.version,
        parent_version=patched.parent_version,
        patch_id=patch_id,
        payload={
            "timeline_id": patched.timeline_id,
            "timeline_version": patched.version,
            "parent_version": patched.parent_version,
            "patch_id": patch_id,
            "timeline": patched.model_dump(mode="json"),
            "validation_report": report.model_dump(mode="json"),
            "changed_track_ids": list(changed_track_ids),
            "resolved_patch": resolved_patch.model_dump(mode="json", by_alias=True),
            "created_at": created_at or _now_iso(),
        },
    )
    validation_event = _validation_event(draft_state, patched.version, report)
    return PatchOutcome(
        status="succeeded" if report.valid else "failed",
        timeline=patched,
        resolved_patch=resolved_patch,
        validation_report=report,
        changed_track_ids=changed_track_ids,
        events=(
            created_event.model_dump(mode="json"),
            validation_event.model_dump(mode="json"),
        ),
        metadata={"mapping_strategy": resolution.metadata.get("mapping_strategy")},
    )


def _apply_resolved_op(
    connection: Connection,
    draft_state: DraftState,
    resolution: AnchorResolution,
    patch_id: str,
) -> TimelineState:
    timeline = resolution.current_timeline.model_copy(deep=True)
    op = resolution.request.op
    if isinstance(op, DeleteRangeOp):
        return _apply_delete_range(timeline, op, resolution.current_range, patch_id)
    if isinstance(op, ReplaceClipOp):
        return _apply_replace_clip(connection, draft_state, timeline, op)
    if isinstance(op, ReorderBlocksOp):
        return _apply_reorder_blocks(timeline, op)
    if isinstance(op, TrimClipOp):
        return _apply_trim_clip(connection, timeline, op)
    if isinstance(op, InsertClipOp):
        return _apply_insert_clip(
            connection,
            draft_state,
            timeline,
            op,
            resolution.current_range,
        )
    if isinstance(op, GenerateSubtitlesOp):
        return _apply_generate_subtitles(
            connection,
            draft_state,
            timeline,
            op,
            resolution.current_range,
        )
    if isinstance(op, SetSubtitleStyleOp):
        return _apply_set_subtitle_style(timeline, op)
    if isinstance(op, EditSubtitleTextOp):
        return _apply_edit_subtitle_text(timeline, op)
    if isinstance(op, RemoveTrackClipsOp):
        return _apply_remove_track_clips(timeline, op, resolution.current_range, patch_id)
    if isinstance(op, AddBgmOp):
        return _apply_add_bgm(connection, timeline, op)
    if isinstance(op, AdjustGainOp):
        return _apply_adjust_gain(timeline, op)
    if isinstance(op, SetPlaybackRateOp):
        return _apply_set_playback_rate(timeline, op)
    raise PatchApplyError("patch.unsupported_op", "unsupported patch op")


def _apply_delete_range(
    timeline: TimelineState,
    op: DeleteRangeOp,
    resolved: ResolvedRange,
    patch_id: str,
) -> TimelineState:
    track_ids = set(_tracks_for_delete_scope(op.scope))
    start, end = _clamped_range(resolved, timeline.duration_frames)
    delete_len = max(0, end - start)
    tracks = _tracks_by_id(timeline)
    for track_id in track_ids:
        track = tracks[track_id]
        track.clips = _delete_from_clips(
            track.clips,
            start,
            end,
            ripple=op.ripple,
            patch_id=patch_id,
        )
    audio_track_ids = {"voiceover", "original_audio"} & track_ids
    if audio_track_ids and "subtitles" not in track_ids:
        tracks["subtitles"].clips = _remove_bound_subtitles_in_range(
            tracks["subtitles"].clips,
            audio_track_ids=audio_track_ids,
            start=start,
            end=end,
        )
    duration = timeline.duration_frames
    if op.ripple and "visual_base" in track_ids:
        duration = max(0, timeline.duration_frames - delete_len)
    result = _timeline_from_tracks(timeline, tracks, duration_frames=duration)
    return _sync_bound_subtitles(result)


def _apply_replace_clip(
    connection: Connection,
    draft_state: DraftState,
    timeline: TimelineState,
    op: ReplaceClipOp,
) -> TimelineState:
    material = _clip_material(
        connection,
        draft_state,
        asset_id=op.asset_id,
        source_start_s=op.source_start_s,
        source_end_s=op.source_end_s,
        role=op.role,
        fps=timeline.fps,
    )
    tracks = _tracks_by_id(timeline)
    found = False
    for track in tracks.values():
        next_clips: list[TimelineClip] = []
        for clip in track.clips:
            if clip.timeline_clip_id != op.timeline_clip_id:
                next_clips.append(clip)
                continue
            if not isinstance(clip, TimelineMediaClip):
                raise PatchApplyError(
                    "patch.replace.non_media_clip",
                    "replace_clip only supports media timeline clips",
                    details={"timeline_clip_id": op.timeline_clip_id},
                )
            next_clips.append(_clip_from_material(clip, material))
            found = True
        track.clips = next_clips
    if not found:
        raise PatchApplyError(
            "patch.replace.clip_missing",
            "replace_clip target was not found",
            details={"timeline_clip_id": op.timeline_clip_id},
        )
    return _timeline_from_tracks(timeline, tracks)


def _apply_reorder_blocks(timeline: TimelineState, op: ReorderBlocksOp) -> TimelineState:
    tracks = _tracks_by_id(timeline)
    visual_blocks = _visual_blocks(timeline)
    requested = list(op.block_id_order)
    if set(requested) != set(visual_blocks):
        raise PatchApplyError(
            "patch.reorder.block_order_mismatch",
            "reorder_blocks must include each existing visual block exactly once",
            details={"expected": sorted(visual_blocks), "received": requested},
        )
    new_starts: dict[str, int] = {}
    cursor = 0
    for block_id in requested:
        new_starts[block_id] = cursor
        cursor += visual_blocks[block_id]

    for track in tracks.values():
        moved: list[TimelineClip] = []
        for clip in track.clips:
            clip_block_id = _block_id_for_clip(timeline, clip)
            if clip_block_id is None:
                moved.append(clip)
                continue
            block_id = clip_block_id
            old_start = _block_start(timeline, block_id)
            delta = new_starts[block_id] - old_start
            moved.append(_shift_clip(clip, delta))
        track.clips = sorted(moved, key=_clip_sort_key)
    result = _timeline_from_tracks(timeline, tracks, duration_frames=cursor)
    return _sync_bound_subtitles(result)


def _apply_trim_clip(
    connection: Connection,
    timeline: TimelineState,
    op: TrimClipOp,
) -> TimelineState:
    frames = _seconds_delta_to_frames(op.delta_sec, fps=timeline.fps)
    if frames == 0:
        return timeline
    tracks = _tracks_by_id(timeline)
    target = _find_media_clip(timeline, op.timeline_clip_id)
    if target is None:
        raise PatchApplyError(
            "patch.trim.clip_missing",
            "trim_clip target was not found",
            details={"timeline_clip_id": op.timeline_clip_id},
        )
    adjusted = _trimmed_media_clip(connection, target, edge=op.edge, delta_frames=frames)
    duration_delta = _clip_duration(adjusted) - _clip_duration(target)
    for track in tracks.values():
        updated: list[TimelineClip] = []
        for clip in track.clips:
            if clip.timeline_clip_id == op.timeline_clip_id:
                updated.append(adjusted)
            elif clip.timeline_start_frame >= target.timeline_end_frame and duration_delta != 0:
                updated.append(_shift_clip(clip, duration_delta))
            else:
                updated.append(clip)
        track.clips = sorted(updated, key=_clip_sort_key)
    duration = timeline.duration_frames
    if target.track_id == "visual_base":
        duration = max(0, timeline.duration_frames + duration_delta)
    result = _timeline_from_tracks(timeline, tracks, duration_frames=duration)
    return _sync_bound_subtitles(result)


def _apply_insert_clip(
    connection: Connection,
    draft_state: DraftState,
    timeline: TimelineState,
    op: InsertClipOp,
    resolved: ResolvedRange,
) -> TimelineState:
    material = _clip_material(
        connection,
        draft_state,
        asset_id=op.asset_id,
        source_start_s=op.source_start_s,
        source_end_s=op.source_end_s,
        role=op.role,
        fps=timeline.fps,
    )
    track_id = op.track_id or "visual_base"
    position = min(max(0, resolved.start_frame), timeline.duration_frames)
    duration = max(1, round((op.source_end_s - op.source_start_s) * timeline.fps))
    inserted = TimelineMediaClip(
        timeline_clip_id=_new_clip_id(timeline, "tc_insert"),
        track_id=track_id,
        asset_id=material.asset_id,
        clip_id=None,
        role=material.role,
        timeline_start_frame=position,
        timeline_end_frame=position + duration,
        source_start_frame=material.source_start_frame,
        source_end_frame=min(material.source_end_frame, material.source_start_frame + duration),
    )
    tracks = _tracks_by_id(timeline)
    if track_id != "visual_base":
        # Overlays sit on top of the primary track: no ripple, no duration change.
        tracks[track_id].clips = sorted([*tracks[track_id].clips, inserted], key=_clip_sort_key)
        return _sync_bound_subtitles(_timeline_from_tracks(timeline, tracks))
    for other_id, track in tracks.items():
        if other_id == "visual_base":
            track.clips = _insert_visual_clip(track.clips, inserted, position, duration)
        else:
            track.clips = [
                _shift_clip(clip, duration) if clip.timeline_start_frame >= position else clip
                for clip in track.clips
            ]
    result = _timeline_from_tracks(
        timeline,
        tracks,
        duration_frames=timeline.duration_frames + duration,
    )
    return _sync_bound_subtitles(result)


def _apply_generate_subtitles(
    connection: Connection,
    draft_state: DraftState,
    timeline: TimelineState,
    op: GenerateSubtitlesOp,
    resolved: ResolvedRange,
) -> TimelineState:
    del draft_state
    tracks = _tracks_by_id(timeline)
    existing = [
        clip
        for clip in tracks["subtitles"].clips
        if not (
            isinstance(clip, SubtitleClip)
            and clip.binding.kind == op.source
            and _overlaps(
                clip.timeline_start_frame,
                clip.timeline_end_frame,
                resolved.start_frame,
                resolved.end_frame,
            )
        )
    ]
    generated: list[SubtitleClip] = []
    existing_ids = {clip.timeline_clip_id for clip in _all_clips(timeline)}
    for audio_clip in _media_clips(tracks[op.source]):
        for placement in _utterance_placements(connection, audio_clip):
            if not _overlaps(
                placement.timeline_start_frame,
                placement.timeline_end_frame,
                resolved.start_frame,
                resolved.end_frame,
            ):
                continue
            generated.append(
                SubtitleClip(
                    timeline_clip_id=_next_clip_id(existing_ids, "sub"),
                    text=placement.text,
                    timeline_start_frame=max(placement.timeline_start_frame, resolved.start_frame),
                    timeline_end_frame=min(placement.timeline_end_frame, resolved.end_frame),
                    style_template_id=op.style_template_id,
                    binding=SubtitleBinding(kind=op.source, utterance_id=placement.utterance_id),
                    safe_area_check="ok",
                )
            )
    tracks["subtitles"].clips = sorted([*existing, *generated], key=_clip_sort_key)
    return _timeline_from_tracks(timeline, tracks)


def _apply_set_subtitle_style(timeline: TimelineState, op: SetSubtitleStyleOp) -> TimelineState:
    tracks = _tracks_by_id(timeline)
    target_ids = None if isinstance(op.range, AllRange) else set(op.range.clip_ids)
    tracks["subtitles"].clips = [
        clip.model_copy(update={"style_template_id": op.style_template_id})
        if isinstance(clip, SubtitleClip)
        and (target_ids is None or clip.timeline_clip_id in target_ids)
        else clip
        for clip in tracks["subtitles"].clips
    ]
    return _timeline_from_tracks(timeline, tracks)


def _apply_edit_subtitle_text(timeline: TimelineState, op: EditSubtitleTextOp) -> TimelineState:
    tracks = _tracks_by_id(timeline)
    found = False
    updated: list[TimelineClip] = []
    for clip in tracks["subtitles"].clips:
        if isinstance(clip, SubtitleClip) and clip.timeline_clip_id == op.timeline_clip_id:
            updated.append(clip.model_copy(update={"text": op.text}))
            found = True
        else:
            updated.append(clip)
    if not found:
        raise PatchApplyError(
            "patch.subtitle.clip_missing",
            "subtitle clip was not found",
            details={"timeline_clip_id": op.timeline_clip_id},
        )
    tracks["subtitles"].clips = updated
    return _timeline_from_tracks(timeline, tracks)


def _apply_remove_track_clips(
    timeline: TimelineState,
    op: RemoveTrackClipsOp,
    resolved: ResolvedRange,
    patch_id: str,
) -> TimelineState:
    tracks = _tracks_by_id(timeline)
    if isinstance(op.range, AllRange):
        tracks[op.track_id].clips = []
    else:
        tracks[op.track_id].clips = _delete_from_clips(
            tracks[op.track_id].clips,
            resolved.start_frame,
            resolved.end_frame,
            ripple=True,
            patch_id=patch_id,
        )
    duration = timeline.duration_frames
    if op.track_id == "visual_base":
        duration = _visual_duration(tracks["visual_base"].clips)
    result = _timeline_from_tracks(timeline, tracks, duration_frames=duration)
    return _sync_bound_subtitles(result)


def _apply_add_bgm(connection: Connection, timeline: TimelineState, op: AddBgmOp) -> TimelineState:
    _assert_asset_usable(connection, op.asset_id)
    source_end = _asset_frame_count(connection, op.asset_id, default_fps=timeline.fps)
    tracks = _tracks_by_id(timeline)
    tracks["bgm"].clips = [
        TimelineMediaClip(
            timeline_clip_id=_new_clip_id(timeline, "bgm"),
            track_id="bgm",
            asset_id=op.asset_id,
            clip_id=None,
            role="bgm",
            timeline_start_frame=0,
            timeline_end_frame=max(1, timeline.duration_frames),
            source_start_frame=0,
            source_end_frame=max(1, source_end),
            gain_db=op.gain_db,
            effects=[{"kind": "duck", "enabled": op.duck}],
        )
    ]
    return _timeline_from_tracks(timeline, tracks)


def _apply_adjust_gain(timeline: TimelineState, op: AdjustGainOp) -> TimelineState:
    tracks = _tracks_by_id(timeline)
    tracks[op.track_id].clips = [
        clip.model_copy(update={"gain_db": op.gain_db})
        if isinstance(clip, TimelineMediaClip)
        else clip
        for clip in tracks[op.track_id].clips
    ]
    return _timeline_from_tracks(timeline, tracks)


def _apply_set_playback_rate(timeline: TimelineState, op: SetPlaybackRateOp) -> TimelineState:
    if op.rate <= 0:
        raise PatchApplyError(
            "patch.playback_rate.invalid",
            "playback rate must be positive",
            details={"rate": op.rate},
        )
    tracks = _tracks_by_id(timeline)
    found = False
    for track in tracks.values():
        updated: list[TimelineClip] = []
        for clip in track.clips:
            if isinstance(clip, TimelineMediaClip) and clip.timeline_clip_id == op.timeline_clip_id:
                updated.append(clip.model_copy(update={"playback_rate": op.rate}))
                found = True
            else:
                updated.append(clip)
        track.clips = updated
    if not found:
        raise PatchApplyError(
            "patch.playback_rate.clip_missing",
            "set_playback_rate target was not found",
            details={"timeline_clip_id": op.timeline_clip_id},
        )
    return _timeline_from_tracks(timeline, tracks)


def _delete_from_clips(
    clips: Sequence[TimelineClip],
    start: int,
    end: int,
    *,
    ripple: bool,
    patch_id: str,
) -> list[TimelineClip]:
    delete_len = max(0, end - start)
    result: list[TimelineClip] = []
    for clip in clips:
        if clip.timeline_end_frame <= start:
            result.append(clip)
            continue
        if clip.timeline_start_frame >= end:
            result.append(_shift_clip(clip, -delete_len) if ripple else clip)
            continue
        if isinstance(clip, SubtitleClip):
            continue
        result.extend(_delete_from_media_clip(clip, start, end, ripple=ripple, patch_id=patch_id))
    return sorted(result, key=_clip_sort_key)


def _delete_from_media_clip(
    clip: TimelineMediaClip,
    start: int,
    end: int,
    *,
    ripple: bool,
    patch_id: str,
) -> list[TimelineMediaClip]:
    overlap_start = max(start, clip.timeline_start_frame)
    overlap_end = min(end, clip.timeline_end_frame)
    if overlap_start >= overlap_end:
        return [clip]
    pieces: list[TimelineMediaClip] = []
    if clip.timeline_start_frame < overlap_start:
        pieces.append(
            clip.model_copy(
                update={
                    "timeline_end_frame": overlap_start,
                    "source_end_frame": _source_frame_at(clip, overlap_start),
                }
            )
        )
    if overlap_end < clip.timeline_end_frame:
        # 尾块存在时必有 overlap_end == end，ripple 下老位置 p>=end 的内容统一
        # 移到 p - (end - start)。必须用完整区间长度：区间左越 clip 头时
        # overlap 只是区间的一部分，按 overlap 长度平移会少移
        # (overlap_start - start) 帧、与后续整体平移的 clip 重叠。
        deleted = end - start if ripple else 0
        right_start = overlap_end - deleted
        right_end = clip.timeline_end_frame - deleted
        pieces.append(
            clip.model_copy(
                update={
                    "timeline_clip_id": f"{clip.timeline_clip_id}_r{patch_id[-6:]}",
                    "timeline_start_frame": right_start,
                    "timeline_end_frame": right_end,
                    "source_start_frame": _source_frame_at(clip, overlap_end),
                }
            )
        )
    return [piece for piece in pieces if piece.timeline_start_frame < piece.timeline_end_frame]


def _remove_bound_subtitles_in_range(
    clips: Sequence[TimelineClip],
    *,
    audio_track_ids: set[str],
    start: int,
    end: int,
) -> list[TimelineClip]:
    kept: list[TimelineClip] = []
    for clip in clips:
        if (
            isinstance(clip, SubtitleClip)
            and clip.binding.kind in audio_track_ids
            and _overlaps(clip.timeline_start_frame, clip.timeline_end_frame, start, end)
        ):
            continue
        kept.append(clip)
    return kept


def _trimmed_media_clip(
    connection: Connection,
    clip: TimelineMediaClip,
    *,
    edge: Literal["head", "tail"],
    delta_frames: int,
) -> TimelineMediaClip:
    duration = _clip_duration(clip)
    if edge == "head":
        new_source_start = clip.source_start_frame + delta_frames
        new_duration = duration - delta_frames
        updates = {
            "source_start_frame": new_source_start,
            "timeline_end_frame": clip.timeline_start_frame + new_duration,
        }
    else:
        new_source_end = clip.source_end_frame - delta_frames
        new_duration = duration - delta_frames
        updates = {
            "source_end_frame": new_source_end,
            "timeline_end_frame": clip.timeline_start_frame + new_duration,
        }
    trimmed = clip.model_copy(update=updates)
    if trimmed.timeline_start_frame >= trimmed.timeline_end_frame:
        raise PatchApplyError(
            "patch.trim.empty_clip",
            "trim_clip would remove the entire clip",
            details={"timeline_clip_id": clip.timeline_clip_id},
        )
    if trimmed.source_start_frame < 0 or trimmed.source_start_frame >= trimmed.source_end_frame:
        raise PatchApplyError(
            "patch.trim.invalid_source_range",
            "trim_clip would create an invalid source range",
            details={
                "timeline_clip_id": clip.timeline_clip_id,
                "source_start_frame": trimmed.source_start_frame,
                "source_end_frame": trimmed.source_end_frame,
            },
        )
    frame_count = _asset_frame_count(connection, trimmed.asset_id, default_fps=30)
    if trimmed.source_end_frame > frame_count:
        raise PatchApplyError(
            "patch.trim.source_out_of_bounds",
            "trim_clip would extend past the source asset",
            details={"timeline_clip_id": clip.timeline_clip_id, "frame_count": frame_count},
        )
    return trimmed


def _insert_visual_clip(
    clips: Sequence[TimelineClip],
    inserted: TimelineMediaClip,
    position: int,
    duration: int,
) -> list[TimelineClip]:
    result: list[TimelineClip] = []
    for clip in clips:
        if not isinstance(clip, TimelineMediaClip):
            result.append(clip)
            continue
        if clip.timeline_end_frame <= position:
            result.append(clip)
        elif clip.timeline_start_frame >= position:
            result.append(_shift_clip(clip, duration))
        else:
            left = clip.model_copy(
                update={
                    "timeline_end_frame": position,
                    "source_end_frame": _source_frame_at(clip, position),
                }
            )
            right = clip.model_copy(
                update={
                    "timeline_clip_id": f"{clip.timeline_clip_id}_after_insert",
                    "timeline_start_frame": position + duration,
                    "timeline_end_frame": clip.timeline_end_frame + duration,
                    "source_start_frame": _source_frame_at(clip, position),
                }
            )
            result.extend([left, right])
    result.append(inserted)
    return sorted(result, key=_clip_sort_key)


def _sync_bound_subtitles(timeline: TimelineState) -> TimelineState:
    tracks = _tracks_by_id(timeline)
    synced: list[TimelineClip] = []
    for clip in tracks["subtitles"].clips:
        if not isinstance(clip, SubtitleClip) or clip.binding.kind == "manual":
            synced.append(clip)
            continue
        placement = _subtitle_binding_placement(timeline, clip.binding)
        if placement is None:
            continue
        synced.append(
            clip.model_copy(
                update={
                    "timeline_start_frame": placement[0],
                    "timeline_end_frame": placement[1],
                }
            )
        )
    tracks["subtitles"].clips = sorted(synced, key=_clip_sort_key)
    return _timeline_from_tracks(timeline, tracks)


def _subtitle_binding_placement(
    timeline: TimelineState,
    binding: SubtitleBinding,
) -> tuple[int, int] | None:
    track = _tracks_by_id(timeline).get(binding.kind)
    if track is None:
        return None
    for clip in _media_clips(track):
        utterance_ids = _clip_utterance_ids(clip)
        if binding.utterance_id is None or binding.utterance_id in utterance_ids:
            return clip.timeline_start_frame, clip.timeline_end_frame
    return None


def _utterance_placements(
    connection: Connection,
    audio_clip: TimelineMediaClip,
) -> list[_UtterancePlacement]:
    utterances = _utterances_for_audio_clip(connection, audio_clip)
    if not utterances:
        return []
    source_fps = _asset_fps(connection, audio_clip.asset_id, default_fps=30)
    source_duration = max(1, audio_clip.source_end_frame - audio_clip.source_start_frame)
    timeline_duration = max(1, audio_clip.timeline_end_frame - audio_clip.timeline_start_frame)
    placements: list[_UtterancePlacement] = []
    wanted = _clip_utterance_ids(audio_clip)
    for utterance in utterances:
        utterance_id = _str_value(utterance.get("utterance_id"))
        text = _str_value(utterance.get("text")) or ""
        start_ms = _int_value(utterance.get("start_ms"))
        end_ms = _int_value(utterance.get("end_ms"))
        if utterance_id is None or start_ms is None or end_ms is None or start_ms >= end_ms:
            continue
        if wanted and utterance_id not in wanted:
            continue
        source_start = round(start_ms / 1000 * source_fps)
        source_end = round(end_ms / 1000 * source_fps)
        overlap_start = max(source_start, audio_clip.source_start_frame)
        overlap_end = min(source_end, audio_clip.source_end_frame)
        if overlap_start >= overlap_end:
            continue
        timeline_start = audio_clip.timeline_start_frame + round(
            ((overlap_start - audio_clip.source_start_frame) / source_duration) * timeline_duration
        )
        timeline_end = audio_clip.timeline_start_frame + round(
            ((overlap_end - audio_clip.source_start_frame) / source_duration) * timeline_duration
        )
        if timeline_start >= timeline_end:
            timeline_end = timeline_start + 1
        placements.append(
            _UtterancePlacement(
                utterance_id=utterance_id,
                text=text,
                timeline_start_frame=timeline_start,
                timeline_end_frame=timeline_end,
            )
        )
    return placements


def _utterances_for_audio_clip(
    connection: Connection,
    audio_clip: TimelineMediaClip,
) -> list[Mapping[str, Any]]:
    transcript_ids = _clip_transcript_ids(audio_clip)
    rows: Sequence[Any]
    if transcript_ids:
        rows = connection.execute(
            select(schema.transcripts.c.utterances)
            .where(schema.transcripts.c.asset_id == audio_clip.asset_id)
            .where(schema.transcripts.c.transcript_id.in_(list(transcript_ids)))
        ).all()
    else:
        rows = connection.execute(
            select(schema.transcripts.c.utterances)
            .where(schema.transcripts.c.asset_id == audio_clip.asset_id)
            .order_by(schema.transcripts.c.transcript_id)
        ).all()
    utterances: list[Mapping[str, Any]] = []
    for row in rows:
        raw = load_json(str(row._mapping["utterances"]))
        if isinstance(raw, list):
            parsed = [item for item in raw if isinstance(item, Mapping)]
            utterances.extend(cast(list[Mapping[str, Any]], parsed))
    return utterances


def _clip_material(
    connection: Connection,
    draft_state: DraftState,
    *,
    asset_id: str,
    source_start_s: float,
    source_end_s: float,
    role: str,
    fps: int,
) -> _ClipMaterial:
    _assert_asset_usable(connection, asset_id, draft_state=draft_state)
    asset_kind = _asset_kind(connection, asset_id)
    if role == "image" or asset_kind == "image":
        return _ClipMaterial(
            asset_id=asset_id,
            source_start_frame=0,
            source_end_frame=1,
            role=role,
            asset_kind=asset_kind,
        )
    source_fps = _asset_fps(connection, asset_id, default_fps=fps)
    frame_count = _asset_frame_count(connection, asset_id, default_fps=fps)
    source_start = max(0, round(source_start_s * source_fps))
    source_end = max(source_start + 1, round(source_end_s * source_fps))
    source_end = min(source_end, frame_count)
    source_start = min(source_start, source_end - 1)
    if source_start < 0 or source_start >= source_end:
        raise PatchApplyError(
            "patch.clip.invalid_source_range",
            "clip has no usable source frames",
            details={"asset_id": asset_id},
        )
    return _ClipMaterial(
        asset_id=asset_id,
        source_start_frame=source_start,
        source_end_frame=source_end,
        role=role,
        asset_kind=asset_kind,
    )


def _clip_from_material(
    existing: TimelineMediaClip,
    material: _ClipMaterial,
) -> TimelineMediaClip:
    duration = _clip_duration(existing)
    source_end = min(material.source_end_frame, material.source_start_frame + max(1, duration))
    return existing.model_copy(
        update={
            "asset_id": material.asset_id,
            "clip_id": None,
            "role": material.role,
            "source_start_frame": material.source_start_frame,
            "source_end_frame": source_end,
            "effects": [],
        }
    )


def _asset_kind(connection: Connection, asset_id: str) -> str:
    row = connection.execute(
        select(schema.assets.c.kind).where(schema.assets.c.asset_id == asset_id)
    ).first()
    if row is None:
        raise PatchApplyError(
            "patch.asset_unavailable",
            "asset is missing, disabled, or unusable",
            details={"asset_id": asset_id},
        )
    return str(row._mapping["kind"])


def _assert_asset_usable(
    connection: Connection,
    asset_id: str,
    *,
    draft_state: DraftState | None = None,
) -> None:
    query = (
        select(schema.assets.c.usable)
        .select_from(
            schema.assets.join(
                schema.draft_asset_links,
                schema.draft_asset_links.c.asset_id == schema.assets.c.asset_id,
            )
        )
        .where(schema.assets.c.asset_id == asset_id)
    )
    if draft_state is not None:
        query = query.where(schema.draft_asset_links.c.draft_id == draft_state.draft_id)
    row = connection.execute(query).first()
    if row is None or not bool(row._mapping["usable"]):
        raise PatchApplyError(
            "patch.asset_unavailable",
            "asset is missing, unlinked, or unusable",
            details={"asset_id": asset_id},
        )


def _tracks_by_id(timeline: TimelineState) -> dict[str, TimelineTrack]:
    return {track.track_id: track.model_copy(deep=True) for track in timeline.tracks}


def _timeline_from_tracks(
    timeline: TimelineState,
    tracks: Mapping[str, TimelineTrack],
    *,
    duration_frames: int | None = None,
) -> TimelineState:
    ordered = [
        tracks["visual_base"],
        tracks["visual_overlay"],
        tracks["original_audio"],
        tracks["voiceover"],
        tracks["bgm"],
        tracks["subtitles"],
    ]
    for track in ordered:
        track.clips = sorted(track.clips, key=_clip_sort_key)
    return timeline.model_copy(
        update={
            "duration_frames": timeline.duration_frames
            if duration_frames is None
            else duration_frames,
            "tracks": ordered,
        },
        deep=True,
    )


def _tracks_for_delete_scope(scope: str) -> tuple[str, ...]:
    if scope == "all_tracks":
        return ("visual_base", "visual_overlay", "original_audio", "voiceover", "bgm", "subtitles")
    if scope == "visual":
        return ("visual_base", "visual_overlay")
    if scope == "audio":
        return ("original_audio", "voiceover", "bgm")
    if scope == "subtitles":
        return ("subtitles",)
    return (scope,)


def _clamped_range(resolved: ResolvedRange, duration_frames: int) -> tuple[int, int]:
    start = min(max(0, resolved.start_frame), max(0, duration_frames))
    end = min(max(start, resolved.end_frame), max(0, duration_frames))
    return start, end


def _find_media_clip(timeline: TimelineState, timeline_clip_id: str) -> TimelineMediaClip | None:
    for clip in _all_clips(timeline):
        if isinstance(clip, TimelineMediaClip) and clip.timeline_clip_id == timeline_clip_id:
            return clip
    return None


def _all_clips(timeline: TimelineState) -> list[TimelineClip]:
    return [clip for track in timeline.tracks for clip in track.clips]


def _media_clips(track: TimelineTrack) -> list[TimelineMediaClip]:
    return [clip for clip in track.clips if isinstance(clip, TimelineMediaClip)]


def _shift_clip(clip: TimelineClip, delta: int) -> TimelineClip:
    if delta == 0:
        return clip
    return clip.model_copy(
        update={
            "timeline_start_frame": clip.timeline_start_frame + delta,
            "timeline_end_frame": clip.timeline_end_frame + delta,
        }
    )


def _source_frame_at(clip: TimelineMediaClip, timeline_frame: int) -> int:
    timeline_offset = timeline_frame - clip.timeline_start_frame
    timeline_duration = max(1, clip.timeline_end_frame - clip.timeline_start_frame)
    source_duration = max(1, clip.source_end_frame - clip.source_start_frame)
    return clip.source_start_frame + round((timeline_offset / timeline_duration) * source_duration)


def _clip_duration(clip: TimelineClip) -> int:
    return clip.timeline_end_frame - clip.timeline_start_frame


def _clip_sort_key(clip: TimelineClip) -> tuple[int, int, str]:
    return (clip.timeline_start_frame, clip.timeline_end_frame, clip.timeline_clip_id)


def _visual_duration(clips: Sequence[TimelineClip]) -> int:
    media = [clip for clip in clips if isinstance(clip, TimelineMediaClip)]
    return max((clip.timeline_end_frame for clip in media), default=0)


def _visual_blocks(timeline: TimelineState) -> dict[str, int]:
    blocks: dict[str, tuple[int, int]] = {}
    visual = _tracks_by_id(timeline)["visual_base"]
    for clip in _media_clips(visual):
        block_id = clip.parent_block_id or clip.timeline_clip_id
        existing = blocks.get(block_id)
        start = clip.timeline_start_frame
        end = clip.timeline_end_frame
        blocks[block_id] = (
            start if existing is None else min(existing[0], start),
            end if existing is None else max(existing[1], end),
        )
    return {block_id: end - start for block_id, (start, end) in blocks.items()}


def _block_start(timeline: TimelineState, block_id: str) -> int:
    starts = [
        clip.timeline_start_frame
        for clip in _all_clips(timeline)
        if (_block_id_for_clip(timeline, clip) == block_id)
    ]
    return min(starts) if starts else 0


def _block_id_for_clip(timeline: TimelineState, clip: TimelineClip) -> str | None:
    if isinstance(clip, TimelineMediaClip) and clip.parent_block_id is not None:
        return clip.parent_block_id
    if clip.track_id == "visual_base":
        return clip.timeline_clip_id
    for visual in _media_clips(_tracks_by_id(timeline)["visual_base"]):
        if _overlaps(
            clip.timeline_start_frame,
            clip.timeline_end_frame,
            visual.timeline_start_frame,
            visual.timeline_end_frame,
        ):
            return visual.parent_block_id or visual.timeline_clip_id
    return None


def _clip_utterance_ids(clip: TimelineMediaClip) -> frozenset[str]:
    ids: set[str] = set()
    for effect in clip.effects:
        if not isinstance(effect, Mapping):
            continue
        narration = effect.get("narration_ref")
        if isinstance(narration, Mapping):
            ids.update(_string_sequence(narration.get("utterance_ids")))
            one = _str_value(narration.get("utterance_id"))
            if one is not None:
                ids.add(one)
        ids.update(_string_sequence(effect.get("utterance_ids")))
        one = _str_value(effect.get("utterance_id"))
        if one is not None:
            ids.add(one)
    return frozenset(ids)


def _clip_transcript_ids(clip: TimelineMediaClip) -> frozenset[str]:
    ids: set[str] = set()
    for effect in clip.effects:
        if not isinstance(effect, Mapping):
            continue
        narration = effect.get("narration_ref")
        if isinstance(narration, Mapping):
            one = _str_value(narration.get("transcript_id"))
            if one is not None:
                ids.add(one)
        one = _str_value(effect.get("transcript_id"))
        if one is not None:
            ids.add(one)
    return frozenset(ids)


def _asset_fps(connection: Connection, asset_id: str, *, default_fps: int) -> float:
    probe = _asset_probe(connection, asset_id)
    if probe is not None:
        fps = probe.get("fps")
        if isinstance(fps, int | float) and fps > 0:
            return float(fps)
    return float(default_fps)


def _asset_frame_count(connection: Connection, asset_id: str, *, default_fps: int) -> int:
    probe = _asset_probe(connection, asset_id)
    if probe is not None:
        for key in ("frame_count", "frames", "duration_frames"):
            value = probe.get(key)
            if isinstance(value, int | float) and value > 0:
                return int(value)
        duration_sec = probe.get("duration_sec")
        if isinstance(duration_sec, int | float) and duration_sec > 0:
            return max(1, round(float(duration_sec) * default_fps))
    return max(1, default_fps)


def _asset_probe(connection: Connection, asset_id: str) -> Mapping[str, Any] | None:
    row = connection.execute(
        select(schema.assets.c.probe).where(schema.assets.c.asset_id == asset_id)
    ).first()
    if row is None:
        return None
    value = row._mapping["probe"]
    if value is None:
        return None
    parsed = load_json(str(value))
    return parsed if isinstance(parsed, Mapping) else None


def _new_clip_id(timeline: TimelineState, prefix: str) -> str:
    existing = {clip.timeline_clip_id for clip in _all_clips(timeline)}
    return _next_clip_id(existing, prefix)


def _next_clip_id(existing: set[str], prefix: str) -> str:
    index = 1
    while f"{prefix}_{index:03d}" in existing:
        index += 1
    clip_id = f"{prefix}_{index:03d}"
    existing.add(clip_id)
    return clip_id


def _seconds_delta_to_frames(delta: float, *, fps: int) -> int:
    frames = math.copysign(max(1, round(abs(delta) * fps)), delta)
    return int(frames)


def _next_version(connection: Connection, draft_id: str) -> int:
    versions = list_timeline_versions(connection, draft_id)
    return max((record.version for record in versions), default=0) + 1


def _changed_track_ids(before: TimelineState, after: TimelineState) -> tuple[str, ...]:
    before_tracks = {track.track_id: track.model_dump(mode="json") for track in before.tracks}
    changed = [
        track.track_id
        for track in after.tracks
        if before_tracks.get(track.track_id) != track.model_dump(mode="json")
    ]
    return tuple(changed)


def _validation_event(
    draft_state: DraftState,
    version: int,
    report: TimelineValidationReport,
) -> TimelineValidated | TimelineValidationFailed:
    payload = {"timeline_version": version, "validation_report": report.model_dump(mode="json")}
    if report.valid:
        return TimelineValidated(
            draft_id=draft_state.draft_id,
            timeline_version=version,
            payload=payload,
        )
    return TimelineValidationFailed(
        draft_id=draft_state.draft_id,
        timeline_version=version,
        payload=payload,
    )


def _patch_id(request: TimelinePatchRequest) -> str:
    raw = json.dumps(request.model_dump(mode="json", by_alias=True), sort_keys=True)
    digest = hashlib.sha256(f"{raw}:{uuid4().hex}".encode()).hexdigest()[:16]
    return f"patch_{digest}"


def _string_sequence(value: Any) -> tuple[str, ...]:
    if not isinstance(value, Sequence) or isinstance(value, str | bytes):
        return ()
    return tuple(str(item) for item in value if isinstance(item, str))


def _str_value(value: Any) -> str | None:
    return value if isinstance(value, str) else None


def _int_value(value: Any) -> int | None:
    if isinstance(value, bool):
        return None
    if isinstance(value, int):
        return value
    if isinstance(value, float) and value.is_integer():
        return int(value)
    return None


def _overlaps(left_start: int, left_end: int, right_start: int, right_end: int) -> bool:
    return max(left_start, right_start) < min(left_end, right_end)


def _now_iso() -> str:
    return datetime.now(UTC).isoformat()


def _connection_context(engine: Engine | Connection) -> Any:
    if isinstance(engine, Connection):
        return nullcontext(engine)
    return engine.connect()
