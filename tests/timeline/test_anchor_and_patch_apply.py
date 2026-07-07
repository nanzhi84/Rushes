from __future__ import annotations

from pathlib import Path
from typing import Any
from uuid import uuid4

import pytest
from hypothesis import HealthCheck, given, settings
from hypothesis import strategies as st
from sqlalchemy.engine import Connection

from contracts.draft import DraftState
from contracts.patch import TimelinePatchRequest
from contracts.timeline import TimelineState
from storage import schema
from storage.db import create_workspace_engine
from storage.repositories._json import dump_json
from timeline import AnchorConflict, apply_patch, resolve_anchor, store_timeline_version
from timeline.validator import validate_timeline

NOW = "2026-07-05T00:00:00+00:00"


def test_resolve_anchor_defaults_to_last_viewed_preview_version(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_project_media(connection)
        store_timeline_version(connection, _timeline(version=1), created_at=NOW)
        _seed_preview(connection, preview_id="prev_1", version=1)

        resolution = resolve_anchor(
            connection,
            _draft_state(timeline_current_version=1, last_viewed_preview_id="prev_1"),
            TimelinePatchRequest.model_validate(
                {
                    "draft_id": "draft_1",
                    "op": {
                        "kind": "delete_range",
                        "time_range_sec": [1.0, 2.0],
                        "scope": "all_tracks",
                        "ripple": True,
                    },
                    "reason": "delete viewed range",
                }
            ),
        )

    assert resolution.anchor_version == 1
    assert resolution.anchor_preview_id == "prev_1"
    assert (resolution.anchor_range.start_frame, resolution.anchor_range.end_frame) == (30, 60)


def test_anchor_mapping_conflict_when_target_clip_changed(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_project_media(connection)
        v1 = _timeline(version=1)
        changed = _timeline(version=2, parent_version=1)
        changed_clip = changed.tracks[0].clips[0]
        changed.tracks[0].clips[0] = changed_clip.model_copy(update={"source_end_frame": 80})
        store_timeline_version(connection, v1, created_at=NOW)
        store_timeline_version(connection, changed, created_at=NOW)
        _seed_preview(connection, preview_id="prev_1", version=1)

        with pytest.raises(AnchorConflict):
            resolve_anchor(
                connection,
                _draft_state(timeline_current_version=2, last_viewed_preview_id="prev_1"),
                _request_delete(1.0, 2.0, version=1, preview_id="prev_1"),
            )


def test_apply_delete_range_from_viewed_v8_maps_to_current_and_syncs_subtitles(
    tmp_path: Path,
) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_project_media(connection)
        store_timeline_version(
            connection,
            _timeline(version=8, duration_frames=270),
            created_at=NOW,
        )
        store_timeline_version(
            connection,
            _timeline(version=9, parent_version=8, duration_frames=270),
            created_at=NOW,
        )
        _seed_preview(connection, preview_id="prev_008", version=8)

        outcome = apply_patch(
            connection,
            _draft_state(timeline_current_version=9, last_viewed_preview_id="prev_008"),
            _request_delete(7.0, 8.4, version=8, preview_id="prev_008"),
            created_at=NOW,
        )

    assert outcome.status == "succeeded"
    assert outcome.timeline is not None
    assert outcome.timeline.version == 10
    assert outcome.timeline.duration_frames == 228
    assert _primary_ranges(outcome.timeline) == [(0, 90), (90, 180), (180, 210), (210, 228)]
    assert {clip.timeline_clip_id for clip in _subtitle_clips(outcome.timeline)} == {
        "sub_1",
        "sub_2",
    }
    assert outcome.resolved_patch is not None
    assert outcome.resolved_patch.resolved.start_frame == 210
    assert outcome.events[0]["event"] == "TimelineVersionCreated"
    assert "resolved_patch" in outcome.events[0]["payload"]


def test_audio_delete_removes_bound_subtitle_and_ripples_later_binding(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_project_media(connection)
        store_timeline_version(connection, _timeline(version=1), created_at=NOW)

        outcome = apply_patch(
            connection,
            _draft_state(timeline_current_version=1),
            _request_delete(3.0, 6.0, scope="voiceover", version=1),
            created_at=NOW,
        )

    assert outcome.status == "succeeded"
    assert outcome.timeline is not None
    subtitles = {clip.timeline_clip_id: clip for clip in _subtitle_clips(outcome.timeline)}
    assert set(subtitles) == {"sub_1", "sub_3"}
    assert (subtitles["sub_3"].timeline_start_frame, subtitles["sub_3"].timeline_end_frame) == (
        90,
        180,
    )


def test_apply_patch_implements_clip_and_direct_edit_ops(tmp_path: Path) -> None:
    cases = [
        (
            "replace_clip",
            {
                "kind": "replace_clip",
                "timeline_clip_id": "tc_v2",
                "asset_id": "asset_new",
                "source_start_s": 0.0,
                "source_end_s": 2.0,
                "role": "b_roll",
            },
            lambda timeline: _visual_clip(timeline, "tc_v2").asset_id == "asset_new",
        ),
        (
            "reorder_blocks",
            {"kind": "reorder_blocks", "block_id_order": ["block_2", "block_1", "block_3"]},
            lambda timeline: _visual_clip(timeline, "tc_v2").timeline_start_frame == 0,
        ),
        (
            "trim_clip",
            {"kind": "trim_clip", "timeline_clip_id": "tc_v1", "edge": "tail", "delta_sec": 0.5},
            lambda timeline: timeline.duration_frames == 255,
        ),
        (
            "insert_clip",
            {
                "kind": "insert_clip",
                "asset_id": "asset_new",
                "source_start_s": 0.0,
                "source_end_s": 2.0,
                "role": "b_roll",
                "position_s": 3.0,
            },
            lambda timeline: timeline.duration_frames == 330,
        ),
        (
            "generate_subtitles",
            {"kind": "generate_subtitles", "source": "voiceover", "style_template_id": "s2"},
            lambda timeline: len(_subtitle_clips(timeline)) == 3,
        ),
        (
            "set_subtitle_style",
            {"kind": "set_subtitle_style", "style_template_id": "s2", "range": {"kind": "all"}},
            lambda timeline: all(
                clip.style_template_id == "s2" for clip in _subtitle_clips(timeline)
            ),
        ),
        (
            "edit_subtitle_text",
            {"kind": "edit_subtitle_text", "timeline_clip_id": "sub_1", "text": "updated"},
            lambda timeline: _subtitle(timeline, "sub_1").text == "updated",
        ),
        (
            "remove_track_clips",
            {"kind": "remove_track_clips", "track_id": "subtitles", "range": {"kind": "all"}},
            lambda timeline: _subtitle_clips(timeline) == [],
        ),
        (
            "add_bgm",
            {"kind": "add_bgm", "asset_id": "asset_bgm", "gain_db": -12.0, "duck": True},
            lambda timeline: len(_track_clips(timeline, "bgm")) == 1,
        ),
        (
            "adjust_gain",
            {"kind": "adjust_gain", "track_id": "voiceover", "gain_db": -3.0},
            lambda timeline: all(
                clip.gain_db == -3.0 for clip in _track_clips(timeline, "voiceover")
            ),
        ),
        (
            "set_playback_rate",
            {"kind": "set_playback_rate", "timeline_clip_id": "tc_v1", "rate": 1.25},
            lambda timeline: _visual_clip(timeline, "tc_v1").playback_rate == 1.25,
        ),
    ]

    for index, (_name, op, assertion) in enumerate(cases, start=1):
        engine = _engine(tmp_path / f"op_{index}")
        with engine.begin() as connection:
            _seed_project_media(connection)
            timeline = _timeline(version=index)
            if op["kind"] == "generate_subtitles":
                timeline = _timeline(version=index, subtitles=False)
            store_timeline_version(connection, timeline, created_at=NOW)
            outcome = apply_patch(
                connection,
                _draft_state(timeline_current_version=index),
                TimelinePatchRequest.model_validate(
                    {
                        "draft_id": "draft_1",
                        "reference": {"timeline_version": index},
                        "op": op,
                        "reason": "test op",
                    }
                ),
                created_at=NOW,
            )

        assert outcome.status == "succeeded"
        assert outcome.timeline is not None
        assert assertion(outcome.timeline)


def test_insert_clip_supports_image_role_on_visual_base(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_project_media(connection)
        _seed_asset(connection, "asset_img", kind="image", probe={})
        store_timeline_version(connection, _timeline(version=1), created_at=NOW)
        outcome = apply_patch(
            connection,
            _draft_state(timeline_current_version=1),
            TimelinePatchRequest.model_validate(
                {
                    "draft_id": "draft_1",
                    "reference": {"timeline_version": 1},
                    "op": {
                        "kind": "insert_clip",
                        "asset_id": "asset_img",
                        "source_start_s": 0.0,
                        "source_end_s": 2.0,
                        "role": "image",
                        "position_s": 0.0,
                    },
                    "reason": "insert image",
                }
            ),
            created_at=NOW,
        )

    assert outcome.status == "succeeded"
    assert outcome.timeline is not None
    inserted = next(
        clip
        for clip in _track_clips(outcome.timeline, "visual_base")
        if clip.asset_id == "asset_img"
    )
    assert inserted.role == "image"
    assert (inserted.source_start_frame, inserted.source_end_frame) == (0, 1)
    assert outcome.timeline.duration_frames == 330


def test_insert_clip_on_visual_overlay_does_not_ripple(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_project_media(connection)
        store_timeline_version(connection, _timeline(version=1), created_at=NOW)
        outcome = apply_patch(
            connection,
            _draft_state(timeline_current_version=1),
            TimelinePatchRequest.model_validate(
                {
                    "draft_id": "draft_1",
                    "reference": {"timeline_version": 1},
                    "op": {
                        "kind": "insert_clip",
                        "asset_id": "asset_new",
                        "source_start_s": 0.0,
                        "source_end_s": 1.0,
                        "role": "b_roll",
                        "track_id": "visual_overlay",
                        "position_s": 1.0,
                    },
                    "reason": "insert overlay",
                }
            ),
            created_at=NOW,
        )

    assert outcome.status == "succeeded"
    assert outcome.timeline is not None
    overlay = _track_clips(outcome.timeline, "visual_overlay")
    assert len(overlay) == 1
    assert overlay[0].asset_id == "asset_new"
    # overlay insert keeps timeline duration and primary track untouched
    assert outcome.timeline.duration_frames == 270


def test_apply_patch_reports_boundary_failures_without_new_timeline(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_project_media(connection)
        store_timeline_version(connection, _timeline(version=1), created_at=NOW)

        outcome = apply_patch(
            connection,
            _draft_state(timeline_current_version=1),
            TimelinePatchRequest.model_validate(
                {
                    "draft_id": "draft_1",
                    "reference": {"timeline_version": 1},
                    "op": {"kind": "set_playback_rate", "timeline_clip_id": "tc_v1", "rate": 0},
                    "reason": "invalid rate",
                }
            ),
            created_at=NOW,
        )

    assert outcome.status == "failed"
    assert outcome.timeline is None
    assert outcome.error is not None
    assert outcome.error.code == "patch.playback_rate.invalid"


@settings(max_examples=20, suppress_health_check=[HealthCheck.function_scoped_fixture])
@given(delete_frames=st.integers(min_value=1, max_value=30))
def test_anchor_mapping_property_preserves_untouched_target_after_prefix_ripple(
    tmp_path: Path,
    delete_frames: int,
) -> None:
    engine = _engine(tmp_path / f"anchor_property_{delete_frames}_{uuid4().hex}")
    with engine.begin() as connection:
        _seed_project_media(connection)
        store_timeline_version(connection, _timeline(version=1), created_at=NOW)
        store_timeline_version(
            connection,
            _timeline_after_prefix_delete(version=2, parent_version=1, deleted=delete_frames),
            created_at=NOW,
        )
        _seed_preview(connection, preview_id="prev_1", version=1)

        resolution = resolve_anchor(
            connection,
            _draft_state(timeline_current_version=2, last_viewed_preview_id="prev_1"),
            _request_delete(7.0, 8.0, version=1, preview_id="prev_1"),
        )

    assert resolution.current_range.start_frame == 210 - delete_frames
    assert resolution.current_range.end_frame == 240 - delete_frames


@settings(max_examples=20, suppress_health_check=[HealthCheck.function_scoped_fixture])
@given(
    first=st.integers(min_value=0, max_value=80),
    width=st.integers(min_value=1, max_value=20),
    second=st.integers(min_value=0, max_value=60),
)
def test_random_delete_patch_chain_preserves_validator_and_version_invariants(
    tmp_path: Path,
    first: int,
    width: int,
    second: int,
) -> None:
    engine = _engine(tmp_path / f"property_{first}_{width}_{second}_{uuid4().hex}")
    with engine.begin() as connection:
        _seed_project_media(connection)
        store_timeline_version(connection, _timeline(version=1, subtitles=False), created_at=NOW)
        draft_state = _draft_state(timeline_current_version=1)
        current_duration = 270
        version = 1
        for start in (first, second):
            if current_duration <= 1:
                break
            start_frame = min(start, current_duration - 1)
            end_frame = min(current_duration, start_frame + width)
            if start_frame >= end_frame:
                continue
            request = _request_delete(
                start_frame / 30,
                end_frame / 30,
                version=version,
                preview_id=None,
            )
            outcome = apply_patch(connection, draft_state, request, created_at=NOW)
            assert outcome.timeline is not None
            assert outcome.timeline.version == version + 1
            report = validate_timeline(connection, draft_state, outcome.timeline)
            assert report.valid
            current_duration = outcome.timeline.duration_frames
            version = outcome.timeline.version
            draft_state = draft_state.model_copy(update={"timeline_current_version": version})


def _engine(tmp_path: Path):
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)
        connection.execute(
            schema.drafts.insert().values(
                draft_id="draft_1",
                name="Draft",
                state_version=0,
                status="active",
                defaults=dump_json({"aspect_ratio": "9:16", "fps": 30}),
                timeline_validated=False,
                rough_cut_approved=True,
                running_jobs="[]",
                brief=dump_json({"goal": "test", "confirmed_facts": []}),
                scratch_memory="{}",
                created_at=NOW,
                updated_at=NOW,
            )
        )
    return engine


def _draft_state(
    *,
    timeline_current_version: int | None,
    last_viewed_preview_id: str | None = None,
) -> DraftState:
    return DraftState.model_validate(
        {
            "draft_id": "draft_1",
            "name": "Draft",
            "brief": {"goal": "test", "confirmed_facts": []},
            "audio_plan": {
                "mode": "tts",
                "voiceover_asset_id": "asset_vo",
                "transcript_id": "tr_vo",
            },
            "cut_plan": {
                "schema": "CutPlan.v1",
                "slots": [
                    {
                        "slot_id": "slot_insert",
                        "brief": "insert",
                        "target_duration_sec": [1.0, 2.0],
                    }
                ],
                "total_target_duration_sec": 2.0,
            },
            "timeline_current_version": timeline_current_version,
            "last_viewed_preview_id": last_viewed_preview_id,
            "rough_cut_approved": True,
            "scratch_memory": {},
        }
    )


def _seed_project_media(connection: Connection) -> None:
    for asset_id in ("asset_1", "asset_2", "asset_3", "asset_new"):
        _seed_asset(connection, asset_id)
    _seed_asset(connection, "asset_vo", kind="audio", probe={"duration_sec": 9.0, "fps": 30})
    _seed_asset(connection, "asset_bgm", kind="audio", probe={"duration_sec": 9.0, "fps": 30})
    connection.execute(
        schema.transcripts.insert().values(
            transcript_id="tr_vo",
            asset_id="asset_vo",
            provider_id="local_fixture",
            raw_preserved=True,
            utterances=dump_json(
                [
                    {"utterance_id": "u1", "text": "one", "start_ms": 0, "end_ms": 3000},
                    {"utterance_id": "u2", "text": "two", "start_ms": 3000, "end_ms": 6000},
                    {"utterance_id": "u3", "text": "three", "start_ms": 6000, "end_ms": 9000},
                ]
            ),
            vad_segments=dump_json([]),
        )
    )


def _seed_asset(
    connection: Connection,
    asset_id: str,
    *,
    kind: str = "video",
    probe: dict[str, Any] | None = None,
) -> None:
    connection.execute(
        schema.assets.insert().values(
            asset_id=asset_id,
            storage_mode="reference",
            object_hash=None,
            reference_path=f"/tmp/{asset_id}.mp4",
            kind=kind,
            source="local_path",
            filename=f"{asset_id}.mp4",
            hash=f"hash_{asset_id}",
            mtime=1,
            size=1,
            probe=dump_json(probe or {"duration_sec": 10.0, "fps": 30.0}),
            proxy_object_hash=None,
            ingest_status="indexed",
            usable=True,
            failure=None,
        )
    )
    connection.execute(
        schema.draft_asset_links.insert().values(
            draft_id="draft_1",
            asset_id=asset_id,
            linked_at=NOW,
            note="",
        )
    )


def _seed_preview(connection: Connection, *, preview_id: str, version: int) -> None:
    object_hash = f"hash_{preview_id}"
    connection.execute(
        schema.objects.insert().values(
            hash=object_hash,
            rel_path=f"objects/{object_hash}",
            size=0,
            created_at=NOW,
        )
    )
    connection.execute(
        schema.previews.insert().values(
            preview_id=preview_id,
            draft_id="draft_1",
            timeline_version=version,
            object_hash=object_hash,
            quality=dump_json({}),
            created_at=NOW,
        )
    )


def _timeline(
    *,
    version: int,
    parent_version: int | None = None,
    duration_frames: int = 270,
    subtitles: bool = True,
) -> TimelineState:
    subtitle_clips = (
        []
        if not subtitles
        else [
            _subtitle_payload("sub_1", 0, 90, "u1"),
            _subtitle_payload("sub_2", 90, 180, "u2"),
            _subtitle_payload("sub_3", 180, 270, "u3"),
        ]
    )
    return TimelineState.model_validate(
        {
            "timeline_id": f"draft_1:v{version}",
            "draft_id": "draft_1",
            "version": version,
            "fps": 30,
            "duration_frames": duration_frames,
            "parent_version": parent_version,
            "tracks": [
                {
                    "track_id": "visual_base",
                    "track_type": "primary_visual",
                    "clips": [
                        _media_payload(
                            "tc_v1",
                            "visual_base",
                            "asset_1",
                            "clip_1",
                            0,
                            90,
                            "block_1",
                        ),
                        _media_payload(
                            "tc_v2",
                            "visual_base",
                            "asset_2",
                            "clip_2",
                            90,
                            180,
                            "block_2",
                        ),
                        _media_payload(
                            "tc_v3",
                            "visual_base",
                            "asset_3",
                            "clip_3",
                            180,
                            270,
                            "block_3",
                        ),
                    ],
                },
                {"track_id": "visual_overlay", "track_type": "visual_overlay", "clips": []},
                {"track_id": "original_audio", "track_type": "audio", "clips": []},
                {
                    "track_id": "voiceover",
                    "track_type": "audio",
                    "clips": [
                        _voice_payload("vo_1", 0, 90, "u1"),
                        _voice_payload("vo_2", 90, 180, "u2"),
                        _voice_payload("vo_3", 180, 270, "u3"),
                    ],
                },
                {"track_id": "bgm", "track_type": "audio", "clips": []},
                {"track_id": "subtitles", "track_type": "text", "clips": subtitle_clips},
            ],
        }
    )


def _timeline_after_prefix_delete(
    *,
    version: int,
    parent_version: int,
    deleted: int,
) -> TimelineState:
    timeline = _timeline(version=version, parent_version=parent_version)
    for track in timeline.tracks:
        shifted = []
        for clip in track.clips:
            if clip.timeline_end_frame <= deleted:
                continue
            if clip.timeline_start_frame < deleted:
                updates = {
                    "timeline_start_frame": 0,
                    "timeline_end_frame": clip.timeline_end_frame - deleted,
                }
                if hasattr(clip, "source_start_frame"):
                    updates["source_start_frame"] = clip.source_start_frame + deleted
                shifted.append(clip.model_copy(update=updates))
            else:
                shifted.append(
                    clip.model_copy(
                        update={
                            "timeline_start_frame": clip.timeline_start_frame - deleted,
                            "timeline_end_frame": clip.timeline_end_frame - deleted,
                        }
                    )
                )
        track.clips = shifted
    return timeline.model_copy(update={"duration_frames": 270 - deleted}, deep=True)


def _media_payload(
    timeline_clip_id: str,
    track_id: str,
    asset_id: str,
    clip_id: str,
    start: int,
    end: int,
    parent_block_id: str,
) -> dict[str, Any]:
    return {
        "timeline_clip_id": timeline_clip_id,
        "track_id": track_id,
        "asset_id": asset_id,
        "clip_id": clip_id,
        "role": "b_roll",
        "timeline_start_frame": start,
        "timeline_end_frame": end,
        "source_start_frame": start,
        "source_end_frame": end,
        "parent_block_id": parent_block_id,
    }


def _voice_payload(
    timeline_clip_id: str,
    start: int,
    end: int,
    utterance_id: str,
) -> dict[str, Any]:
    return {
        "timeline_clip_id": timeline_clip_id,
        "track_id": "voiceover",
        "asset_id": "asset_vo",
        "clip_id": None,
        "role": "voiceover",
        "timeline_start_frame": start,
        "timeline_end_frame": end,
        "source_start_frame": start,
        "source_end_frame": end,
        "lock_policy": "sync_to_audio",
        "effects": [
            {
                "kind": "narration_ref",
                "narration_ref": {"transcript_id": "tr_vo", "utterance_ids": [utterance_id]},
            }
        ],
    }


def _subtitle_payload(
    timeline_clip_id: str,
    start: int,
    end: int,
    utterance_id: str,
) -> dict[str, Any]:
    return {
        "timeline_clip_id": timeline_clip_id,
        "track_id": "subtitles",
        "text": utterance_id,
        "timeline_start_frame": start,
        "timeline_end_frame": end,
        "style_template_id": "s1",
        "binding": {"kind": "voiceover", "utterance_id": utterance_id},
        "safe_area_check": "ok",
    }


def _request_delete(
    start: float,
    end: float,
    *,
    version: int,
    preview_id: str | None = None,
    scope: str = "all_tracks",
) -> TimelinePatchRequest:
    reference: dict[str, Any] = {"timeline_version": version}
    if preview_id is not None:
        reference["preview_id"] = preview_id
    return TimelinePatchRequest.model_validate(
        {
            "draft_id": "draft_1",
            "reference": reference,
            "op": {
                "kind": "delete_range",
                "time_range_sec": [start, end],
                "scope": scope,
                "ripple": True,
            },
            "reason": "delete range",
        }
    )


def _primary_ranges(timeline: TimelineState) -> list[tuple[int, int]]:
    return [
        (clip.timeline_start_frame, clip.timeline_end_frame)
        for clip in _track_clips(timeline, "visual_base")
    ]


def _track_clips(timeline: TimelineState, track_id: str) -> list[Any]:
    for track in timeline.tracks:
        if track.track_id == track_id:
            return list(track.clips)
    raise AssertionError(f"missing track: {track_id}")


def _subtitle_clips(timeline: TimelineState) -> list[Any]:
    return _track_clips(timeline, "subtitles")


def _visual_clip(timeline: TimelineState, timeline_clip_id: str) -> Any:
    for clip in _track_clips(timeline, "visual_base"):
        if clip.timeline_clip_id == timeline_clip_id:
            return clip
    raise AssertionError(f"missing clip: {timeline_clip_id}")


def _subtitle(timeline: TimelineState, timeline_clip_id: str) -> Any:
    for clip in _subtitle_clips(timeline):
        if clip.timeline_clip_id == timeline_clip_id:
            return clip
    raise AssertionError(f"missing subtitle: {timeline_clip_id}")
