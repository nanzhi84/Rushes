"""Resolve user-time patch anchors to current frame targets."""

from __future__ import annotations

import math
from collections.abc import Mapping, Sequence
from contextlib import nullcontext
from dataclasses import dataclass, field
from typing import Any

from sqlalchemy import select
from sqlalchemy.engine import Connection, Engine

from contracts.draft import DraftState
from contracts.patch import (
    AddBgmOp,
    AdjustGainOp,
    AllRange,
    DeleteRangeOp,
    EditSubtitleTextOp,
    GenerateSubtitlesOp,
    InsertClipOp,
    PatchTimeRange,
    RemoveTrackClipsOp,
    ReorderBlocksOp,
    ReplaceClipOp,
    ResolvedRange,
    SetPlaybackRateOp,
    SetSubtitleStyleOp,
    TimelinePatchRequest,
    TrimClipOp,
)
from contracts.subtitle import SubtitleClip
from contracts.timeline import TimelineMediaClip, TimelineState, TimelineTrack
from storage import schema

from .version_store import TimelineVersionRecord, get_timeline_version, list_timeline_versions

TimelineClip = TimelineMediaClip | SubtitleClip


class AnchorResolutionError(ValueError):
    """Raised when a patch anchor cannot be resolved from stored state."""

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


class AnchorConflict(AnchorResolutionError):
    """Raised when a resolved anchor target was edited before the current version."""


@dataclass(frozen=True, slots=True)
class AnchorResolution:
    request: TimelinePatchRequest
    anchor_version: int
    anchor_preview_id: str | None
    current_version: int
    anchor_timeline: TimelineState
    current_timeline: TimelineState
    anchor_range: ResolvedRange
    current_range: ResolvedRange
    affected_clip_ids: tuple[str, ...] = ()
    metadata: Mapping[str, Any] = field(default_factory=dict)


def resolve_anchor(
    engine: Engine | Connection,
    draft_state: DraftState,
    request: TimelinePatchRequest,
) -> AnchorResolution:
    """Resolve request seconds against the viewed anchor, then map to current version."""

    if request.draft_id != draft_state.draft_id:
        raise AnchorResolutionError(
            "anchor.draft_mismatch",
            "patch request draft_id does not match the active draft",
            details={"request_draft_id": request.draft_id, "draft_id": draft_state.draft_id},
        )
    if draft_state.timeline_current_version is None:
        raise AnchorResolutionError("anchor.timeline_missing", "current timeline is required")

    with _connection_context(engine) as connection:
        anchor_version, anchor_preview_id = _resolve_reference(connection, draft_state, request)
        anchor_record = get_timeline_version(connection, draft_state.draft_id, anchor_version)
        current_record = get_timeline_version(
            connection,
            draft_state.draft_id,
            draft_state.timeline_current_version,
        )
        if anchor_record is None:
            raise AnchorResolutionError(
                "anchor.version_missing",
                f"anchor timeline v{anchor_version} was not found",
                details={"anchor_version": anchor_version},
            )
        if current_record is None:
            raise AnchorResolutionError(
                "anchor.current_missing",
                f"current timeline v{draft_state.timeline_current_version} was not found",
                details={"current_version": draft_state.timeline_current_version},
            )

        anchor_range = _resolve_on_timeline(anchor_record.timeline, request)
        current_range = anchor_range
        if anchor_record.version != current_record.version:
            chain = _version_chain(
                connection,
                draft_id=draft_state.draft_id,
                anchor_version=anchor_record.version,
                current_version=current_record.version,
            )
            current_range = _map_range_through_chain(anchor_range, chain)

    return AnchorResolution(
        request=request,
        anchor_version=anchor_record.version,
        anchor_preview_id=anchor_preview_id,
        current_version=current_record.version,
        anchor_timeline=anchor_record.timeline,
        current_timeline=current_record.timeline,
        anchor_range=anchor_range,
        current_range=current_range,
        affected_clip_ids=tuple(current_range.affected_clip_ids),
        metadata={
            "mapping_strategy": "adjacent_timeline_document_diff",
            "reference_timeline_version": anchor_record.version,
            "reference_preview_id": anchor_preview_id,
            "current_timeline_version": current_record.version,
        },
    )


def _resolve_reference(
    connection: Connection,
    draft_state: DraftState,
    request: TimelinePatchRequest,
) -> tuple[int, str | None]:
    reference = request.reference
    preview_id = reference.preview_id or draft_state.last_viewed_preview_id
    preview_version: int | None = None
    if preview_id is not None:
        preview_version = _preview_timeline_version(connection, draft_state.draft_id, preview_id)
        if preview_version is None and reference.preview_id is not None:
            raise AnchorResolutionError(
                "anchor.preview_missing",
                f"preview was not found for this draft: {reference.preview_id}",
                details={"preview_id": reference.preview_id},
            )
    if reference.timeline_version is not None:
        if preview_version is not None and reference.timeline_version != preview_version:
            raise AnchorConflict(
                "anchor.preview_version_mismatch",
                "reference preview does not point at the requested timeline version",
                details={
                    "preview_id": preview_id,
                    "preview_timeline_version": preview_version,
                    "reference_timeline_version": reference.timeline_version,
                },
            )
        return reference.timeline_version, preview_id
    if preview_version is not None:
        return preview_version, preview_id
    if draft_state.timeline_current_version is None:
        raise AnchorResolutionError("anchor.timeline_missing", "current timeline is required")
    return draft_state.timeline_current_version, None


def _preview_timeline_version(
    connection: Connection,
    draft_id: str,
    preview_id: str,
) -> int | None:
    row = connection.execute(
        select(schema.previews.c.timeline_version)
        .where(schema.previews.c.draft_id == draft_id)
        .where(schema.previews.c.preview_id == preview_id)
    ).first()
    if row is None:
        return None
    return int(row._mapping["timeline_version"])


def _resolve_on_timeline(
    timeline: TimelineState,
    request: TimelinePatchRequest,
) -> ResolvedRange:
    op = request.op
    if isinstance(op, DeleteRangeOp):
        start, end = _seconds_to_frame_range(op.time_range_sec, fps=timeline.fps)
        track_ids = _tracks_for_delete_scope(op.scope)
        return _resolved_range(timeline, start, end, track_ids=track_ids)
    if isinstance(op, InsertClipOp):
        track_id = op.track_id or "visual_base"
        frame = (
            _range_end(timeline)
            if op.position_s is None
            else _second_to_frame(op.position_s, fps=timeline.fps)
        )
        return _resolved_range(timeline, frame, frame + 1, track_ids=(track_id,))
    if isinstance(op, TrimClipOp):
        clip = _find_clip(timeline, op.timeline_clip_id)
        if clip is None:
            raise AnchorResolutionError(
                "anchor.clip_missing",
                f"timeline clip not found: {op.timeline_clip_id}",
                details={"timeline_clip_id": op.timeline_clip_id},
            )
        delta = max(1, abs(_seconds_delta_to_frames(op.delta_sec, fps=timeline.fps)))
        if op.edge == "head":
            start = _clip_start(clip)
            return ResolvedRange(
                start_frame=start,
                end_frame=min(_clip_end(clip), start + delta),
                affected_clip_ids=[op.timeline_clip_id],
            )
        end = _clip_end(clip)
        return ResolvedRange(
            start_frame=max(_clip_start(clip), end - delta),
            end_frame=end,
            affected_clip_ids=[op.timeline_clip_id],
        )
    if isinstance(op, ReplaceClipOp | EditSubtitleTextOp | SetPlaybackRateOp):
        clip_id = op.timeline_clip_id
        clip = _find_clip(timeline, clip_id)
        if clip is None:
            raise AnchorResolutionError(
                "anchor.clip_missing",
                f"timeline clip not found: {clip_id}",
                details={"timeline_clip_id": clip_id},
            )
        return ResolvedRange(
            start_frame=_clip_start(clip),
            end_frame=_clip_end(clip),
            affected_clip_ids=[clip_id],
        )
    if isinstance(op, ReorderBlocksOp):
        clips = _clips_for_blocks(timeline, op.block_id_order)
        return _resolved_from_clips(timeline, clips)
    if isinstance(op, GenerateSubtitlesOp):
        if isinstance(op.range, PatchTimeRange):
            start, end = _seconds_to_frame_range(op.range.time_range_sec, fps=timeline.fps)
            return _resolved_range(timeline, start, end, track_ids=(op.source,))
        return _resolved_range(timeline, 0, _range_end(timeline), track_ids=(op.source,))
    if isinstance(op, SetSubtitleStyleOp):
        if isinstance(op.range, AllRange):
            return _resolved_range(timeline, 0, _range_end(timeline), track_ids=("subtitles",))
        clips = [
            clip
            for clip in _track(timeline, "subtitles").clips
            if clip.timeline_clip_id in set(op.range.clip_ids)
        ]
        return _resolved_from_clips(timeline, clips)
    if isinstance(op, RemoveTrackClipsOp):
        if isinstance(op.range, PatchTimeRange):
            start, end = _seconds_to_frame_range(op.range.time_range_sec, fps=timeline.fps)
            return _resolved_range(timeline, start, end, track_ids=(op.track_id,))
        return _resolved_range(timeline, 0, _range_end(timeline), track_ids=(op.track_id,))
    if isinstance(op, AddBgmOp):
        return _resolved_range(timeline, 0, _range_end(timeline), track_ids=("bgm",))
    if isinstance(op, AdjustGainOp):
        return _resolved_range(timeline, 0, _range_end(timeline), track_ids=(op.track_id,))
    raise AnchorResolutionError("anchor.unsupported_op", "unsupported patch op")


def _version_chain(
    connection: Connection,
    *,
    draft_id: str,
    anchor_version: int,
    current_version: int,
) -> list[tuple[TimelineVersionRecord, TimelineVersionRecord]]:
    if anchor_version == current_version:
        return []
    records = {record.version: record for record in list_timeline_versions(connection, draft_id)}
    if anchor_version not in records or current_version not in records:
        raise AnchorResolutionError(
            "anchor.version_chain_missing",
            "timeline version chain cannot be loaded",
            details={"anchor_version": anchor_version, "current_version": current_version},
        )
    reverse_pairs: list[tuple[TimelineVersionRecord, TimelineVersionRecord]] = []
    child_version = current_version
    while child_version != anchor_version:
        child = records[child_version]
        parent_version = child.parent_version
        if parent_version is None or parent_version not in records:
            raise AnchorConflict(
                "anchor.version_chain_broken",
                "current timeline is not a descendant of the anchor version",
                details={
                    "anchor_version": anchor_version,
                    "current_version": current_version,
                    "child_version": child_version,
                    "parent_version": parent_version,
                },
            )
        parent = records[parent_version]
        reverse_pairs.append((parent, child))
        child_version = parent.version
    return list(reversed(reverse_pairs))


def _map_range_through_chain(
    initial: ResolvedRange,
    chain: Sequence[tuple[TimelineVersionRecord, TimelineVersionRecord]],
) -> ResolvedRange:
    current = initial
    for previous, next_record in chain:
        current = _map_range_step(previous.timeline, next_record.timeline, current)
    return current


def _map_range_step(
    previous: TimelineState,
    next_timeline: TimelineState,
    resolved: ResolvedRange,
) -> ResolvedRange:
    if not resolved.affected_clip_ids:
        return _map_empty_target_step(previous, next_timeline, resolved)

    previous_clips = _clip_index(previous)
    next_clips = _clip_index(next_timeline)
    mapped_pieces: list[tuple[int, int]] = []
    for clip_id in resolved.affected_clip_ids:
        previous_clip = previous_clips.get(clip_id)
        next_clip = next_clips.get(clip_id)
        if previous_clip is None or next_clip is None:
            raise AnchorConflict(
                "anchor.target_clip_missing",
                "anchor target clip was removed before the current version",
                details={"timeline_clip_id": clip_id},
            )
        if not _same_clip_payload(previous_clip, next_clip):
            raise AnchorConflict(
                "anchor.target_clip_changed",
                "anchor target clip was modified before the current version",
                details={"timeline_clip_id": clip_id},
            )
        overlap_start = max(resolved.start_frame, _clip_start(previous_clip))
        overlap_end = min(resolved.end_frame, _clip_end(previous_clip))
        if overlap_start >= overlap_end:
            continue
        mapped_start = _clip_start(next_clip) + (overlap_start - _clip_start(previous_clip))
        mapped_end = _clip_start(next_clip) + (overlap_end - _clip_start(previous_clip))
        mapped_pieces.append((mapped_start, mapped_end))

    if not mapped_pieces:
        raise AnchorConflict(
            "anchor.target_range_changed",
            "anchor target range no longer maps to the current version",
            details={
                "start_frame": resolved.start_frame,
                "end_frame": resolved.end_frame,
                "affected_clip_ids": list(resolved.affected_clip_ids),
            },
        )
    start = min(piece[0] for piece in mapped_pieces)
    end = max(piece[1] for piece in mapped_pieces)
    if start >= end:
        raise AnchorConflict(
            "anchor.target_range_empty",
            "anchor target range collapsed before the current version",
            details={"start_frame": start, "end_frame": end},
        )
    return ResolvedRange(
        start_frame=start,
        end_frame=end,
        affected_clip_ids=list(resolved.affected_clip_ids),
    )


def _map_empty_target_step(
    previous: TimelineState,
    next_timeline: TimelineState,
    resolved: ResolvedRange,
) -> ResolvedRange:
    if previous.duration_frames == next_timeline.duration_frames:
        return resolved
    previous_anchor = _visual_clip_at_or_after(previous, resolved.start_frame)
    next_anchor = (
        None
        if previous_anchor is None
        else _clip_index(next_timeline).get(previous_anchor.timeline_clip_id)
    )
    if (
        previous_anchor is None
        or next_anchor is None
        or not _same_clip_payload(
            previous_anchor,
            next_anchor,
        )
    ):
        raise AnchorConflict(
            "anchor.empty_target_ambiguous",
            "anchor point cannot be mapped after timeline duration changed",
            details={
                "start_frame": resolved.start_frame,
                "previous_duration_frames": previous.duration_frames,
                "next_duration_frames": next_timeline.duration_frames,
            },
        )
    shift = _clip_start(next_anchor) - _clip_start(previous_anchor)
    return ResolvedRange(
        start_frame=resolved.start_frame + shift,
        end_frame=resolved.end_frame + shift,
        affected_clip_ids=[],
    )


def _resolved_range(
    timeline: TimelineState,
    start: int,
    end: int,
    *,
    track_ids: Sequence[str],
) -> ResolvedRange:
    start = max(0, start)
    end = max(start + 1, end)
    affected = [
        clip.timeline_clip_id
        for track_id in track_ids
        for clip in _track(timeline, track_id).clips
        if _overlaps(_clip_start(clip), _clip_end(clip), start, end)
    ]
    return ResolvedRange(start_frame=start, end_frame=end, affected_clip_ids=sorted(affected))


def _resolved_from_clips(
    timeline: TimelineState,
    clips: Sequence[TimelineClip],
) -> ResolvedRange:
    if not clips:
        return ResolvedRange(start_frame=0, end_frame=_range_end(timeline), affected_clip_ids=[])
    return ResolvedRange(
        start_frame=min(_clip_start(clip) for clip in clips),
        end_frame=max(_clip_end(clip) for clip in clips),
        affected_clip_ids=sorted(clip.timeline_clip_id for clip in clips),
    )


def _clips_for_blocks(timeline: TimelineState, block_id_order: Sequence[str]) -> list[TimelineClip]:
    wanted = set(block_id_order)
    clips: list[TimelineClip] = []
    for track in timeline.tracks:
        for clip in track.clips:
            block_id = _clip_parent_block_id(clip) or clip.timeline_clip_id
            if block_id in wanted:
                clips.append(clip)
    return clips


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


def _seconds_to_frame_range(seconds: tuple[float, float], *, fps: int) -> tuple[int, int]:
    start_sec, end_sec = seconds
    start = math.floor(start_sec * fps + 1e-9)
    end = math.ceil(end_sec * fps - 1e-9)
    if start >= end:
        end = start + 1
    return start, end


def _second_to_frame(second: float, *, fps: int) -> int:
    return max(0, round(second * fps))


def _seconds_delta_to_frames(delta: float, *, fps: int) -> int:
    frames = round(delta * fps)
    if frames == 0 and delta != 0:
        return 1 if delta > 0 else -1
    return frames


def _range_end(timeline: TimelineState) -> int:
    return max(1, timeline.duration_frames)


def _track(timeline: TimelineState, track_id: str) -> TimelineTrack:
    for track in timeline.tracks:
        if track.track_id == track_id:
            return track
    raise AnchorResolutionError(
        "anchor.track_missing",
        f"timeline track not found: {track_id}",
        details={"track_id": track_id},
    )


def _find_clip(timeline: TimelineState, timeline_clip_id: str) -> TimelineClip | None:
    return _clip_index(timeline).get(timeline_clip_id)


def _clip_index(timeline: TimelineState) -> dict[str, TimelineClip]:
    return {clip.timeline_clip_id: clip for track in timeline.tracks for clip in track.clips}


def _visual_clip_at_or_after(
    timeline: TimelineState,
    frame: int,
) -> TimelineMediaClip | None:
    clips = [
        clip
        for clip in _track(timeline, "visual_base").clips
        if isinstance(clip, TimelineMediaClip)
    ]
    for clip in sorted(clips, key=lambda item: item.timeline_start_frame):
        if clip.timeline_start_frame <= frame < clip.timeline_end_frame:
            return clip
        if clip.timeline_start_frame >= frame:
            return clip
    return clips[-1] if clips else None


def _same_clip_payload(left: TimelineClip, right: TimelineClip) -> bool:
    if type(left) is not type(right):
        return False
    if isinstance(left, TimelineMediaClip) and isinstance(right, TimelineMediaClip):
        return (
            left.track_id == right.track_id
            and left.asset_id == right.asset_id
            and left.clip_id == right.clip_id
            and left.role == right.role
            and left.source_start_frame == right.source_start_frame
            and left.source_end_frame == right.source_end_frame
            and left.playback_rate == right.playback_rate
            and left.parent_block_id == right.parent_block_id
            and (_clip_end(left) - _clip_start(left)) == (_clip_end(right) - _clip_start(right))
        )
    if isinstance(left, SubtitleClip) and isinstance(right, SubtitleClip):
        return (
            left.track_id == right.track_id
            and left.text == right.text
            and left.style_template_id == right.style_template_id
            and left.binding == right.binding
            and (_clip_end(left) - _clip_start(left)) == (_clip_end(right) - _clip_start(right))
        )
    return False


def _clip_start(clip: TimelineClip) -> int:
    return int(clip.timeline_start_frame)


def _clip_end(clip: TimelineClip) -> int:
    return int(clip.timeline_end_frame)


def _clip_parent_block_id(clip: TimelineClip) -> str | None:
    if isinstance(clip, TimelineMediaClip):
        return clip.parent_block_id
    return None


def _overlaps(left_start: int, left_end: int, right_start: int, right_end: int) -> bool:
    return max(left_start, right_start) < min(left_end, right_end)


def _connection_context(engine: Engine | Connection) -> Any:
    if isinstance(engine, Connection):
        return nullcontext(engine)
    return engine.connect()
