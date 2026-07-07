from __future__ import annotations

from pathlib import Path
from typing import Any

import pytest
from sqlalchemy.engine import Connection

from contracts.case import CaseState
from contracts.timeline import TimelineMediaClip
from storage import schema
from storage.db import create_workspace_engine
from storage.repositories._json import dump_json
from timeline import MaterializationError, materialize_from_clips, validate_timeline
from timeline.materializer import (
    _asset_total_frames,
    _probe_payload,
    _project_fps,
    _source_fps,
)

NOW = "2026-07-05T00:00:00+00:00"


def test_materializer_builds_contiguous_primary_track(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_asset(connection, "asset_1", probe={"duration_sec": 10.0, "fps": 30.0})
        _seed_asset(connection, "asset_2", probe={"duration_sec": 10.0, "fps": 30.0})
    timeline = materialize_from_clips(
        engine,
        _case_state(),
        [
            {"asset_id": "asset_1", "source_start_s": 0.0, "source_end_s": 1.5, "role": "a_roll"},
            {"asset_id": "asset_2", "source_start_s": 2.0, "source_end_s": 4.0, "role": "b_roll"},
        ],
    )

    visual = _media_track(timeline, "visual_base")
    assert [(clip.timeline_start_frame, clip.timeline_end_frame) for clip in visual] == [
        (0, 45),
        (45, 105),
    ]
    assert [clip.role for clip in visual] == ["a_roll", "b_roll"]
    assert timeline.duration_frames == 105
    with engine.connect() as connection:
        assert validate_timeline(connection, _case_state(), timeline).valid


def test_materializer_converts_source_fps_and_clamps_to_frame_count(tmp_path: Path) -> None:
    engine = _engine(tmp_path, fps=30)
    with engine.begin() as connection:
        _seed_asset(connection, "asset_1", probe={"duration_sec": 2.0, "fps": 60.0})
    timeline = materialize_from_clips(
        engine,
        _case_state(),
        [{"asset_id": "asset_1", "source_start_s": 0.5, "source_end_s": 3.0, "role": "b_roll"}],
    )

    clip = _media_track(timeline, "visual_base")[0]
    # source seconds use the asset's 60fps; end clamps to the 120-frame asset.
    assert (clip.source_start_frame, clip.source_end_frame) == (30, 120)
    # timeline duration uses project fps (30): 2.5s -> 75 frames.
    assert clip.timeline_end_frame == 75
    with engine.connect() as connection:
        assert validate_timeline(connection, _case_state(), timeline).valid


def test_materializer_uses_single_source_frame_for_images(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_asset(connection, "asset_img", kind="image", probe={})
    timeline = materialize_from_clips(
        engine,
        _case_state(),
        [{"asset_id": "asset_img", "source_start_s": 0.0, "source_end_s": 2.0, "role": "image"}],
    )

    clip = _media_track(timeline, "visual_base")[0]
    assert clip.role == "image"
    assert (clip.source_start_frame, clip.source_end_frame) == (0, 1)
    assert (clip.timeline_start_frame, clip.timeline_end_frame) == (0, 60)


def test_materializer_lays_voiceover_across_timeline(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_asset(connection, "asset_1", probe={"duration_sec": 10.0, "fps": 30.0})
        _seed_asset(connection, "asset_vo", kind="audio", probe={"fps": 30.0, "frame_count": 300})
    timeline = materialize_from_clips(
        engine,
        _case_state(audio_plan={"mode": "tts", "voiceover_asset_id": "asset_vo"}),
        [{"asset_id": "asset_1", "source_start_s": 0.0, "source_end_s": 2.0, "role": "a_roll"}],
        voiceover_asset_id="asset_vo",
    )

    voiceover = _media_track(timeline, "voiceover")
    assert len(voiceover) == 1
    clip = voiceover[0]
    assert (clip.timeline_start_frame, clip.timeline_end_frame) == (0, timeline.duration_frames)
    assert (clip.source_start_frame, clip.source_end_frame) == (0, 60)
    assert clip.lock_policy == "sync_to_audio"
    with engine.connect() as connection:
        assert validate_timeline(
            connection,
            _case_state(audio_plan={"mode": "tts", "voiceover_asset_id": "asset_vo"}),
            timeline,
        ).valid


def test_materializer_clamps_voiceover_source_to_short_asset(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_asset(connection, "asset_1", probe={"duration_sec": 10.0, "fps": 30.0})
        _seed_asset(connection, "asset_vo", kind="audio", probe={"fps": 30.0, "frame_count": 20})
    timeline = materialize_from_clips(
        engine,
        _case_state(),
        [{"asset_id": "asset_1", "source_start_s": 0.0, "source_end_s": 2.0, "role": "a_roll"}],
        voiceover_asset_id="asset_vo",
    )

    clip = _media_track(timeline, "voiceover")[0]
    assert clip.source_end_frame == 20
    assert clip.timeline_end_frame == 60


def test_materializer_defaults_project_fps_to_30(tmp_path: Path) -> None:
    engine = _engine(tmp_path, fps=0)
    with engine.begin() as connection:
        _seed_asset(connection, "asset_1", probe={"duration_sec": 5.0})
    timeline = materialize_from_clips(
        engine,
        _case_state(),
        [{"asset_id": "asset_1", "source_start_s": 0.0, "source_end_s": 2.0, "role": "b_roll"}],
    )
    assert timeline.fps == 30
    assert timeline.duration_frames == 60


def test_materializer_rejects_invalid_clips(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_asset(connection, "asset_1", probe={"duration_sec": 10.0, "fps": 30.0})

    with pytest.raises(MaterializationError, match="asset not found"):
        materialize_from_clips(
            engine,
            _case_state(),
            [{"asset_id": "ghost", "source_start_s": 0.0, "source_end_s": 1.0, "role": "a_roll"}],
        )
    with pytest.raises(MaterializationError, match="unsupported clip role"):
        materialize_from_clips(
            engine,
            _case_state(),
            [{"asset_id": "asset_1", "source_start_s": 0.0, "source_end_s": 1.0, "role": "hero"}],
        )
    with pytest.raises(MaterializationError, match="source_start_s < source_end_s"):
        materialize_from_clips(
            engine,
            _case_state(),
            [{"asset_id": "asset_1", "source_start_s": 2.0, "source_end_s": 1.0, "role": "a_roll"}],
        )
    with pytest.raises(MaterializationError, match="requires an asset_id"):
        materialize_from_clips(
            engine,
            _case_state(),
            [{"source_start_s": 0.0, "source_end_s": 1.0, "role": "a_roll"}],
        )


def test_materializer_helper_parsing_branches() -> None:
    assert _source_fps(None, default=24.0) == 24.0
    assert _source_fps({"fps": 0}, default=24.0) == 24.0
    assert _asset_total_frames(None, source_fps=24.0) is None
    assert _asset_total_frames({"duration_frames": 42}, source_fps=24.0) == 42
    assert _asset_total_frames({"duration_sec": 0}, source_fps=24.0) is None
    assert _probe_payload(None) is None
    assert _probe_payload({"fps": 30}) == {"fps": 30}
    assert _probe_payload("[]") is None


def test_materializer_helper_defaults_project_fps(tmp_path: Path) -> None:
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)
        assert _project_fps(connection, "missing_project") == 30


def _engine(tmp_path: Path, *, fps: int = 30) -> Any:
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)
        connection.execute(
            schema.projects.insert().values(
                project_id="project_1",
                name="Project",
                status="active",
                defaults=dump_json({"aspect_ratio": "9:16", "fps": fps}),
                created_at=NOW,
                updated_at=NOW,
            )
        )
    return engine


def _case_state(*, audio_plan: dict[str, Any] | None = None) -> CaseState:
    return CaseState.model_validate(
        {
            "case_id": "case_1",
            "project_id": "project_1",
            "name": "Case",
            "brief": {"goal": "test", "confirmed_facts": []},
            "audio_plan": audio_plan or {"mode": "silent"},
            "selected_asset_ids": [],
            "disabled_asset_ids": [],
            "scratch_memory": {},
        }
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
            probe=dump_json(probe if probe is not None else {"duration_sec": 10.0, "fps": 30.0}),
            proxy_object_hash=None,
            ingest_status="indexed",
            usable=True,
            failure=None,
        )
    )
    connection.execute(
        schema.project_asset_links.insert().values(
            project_id="project_1",
            asset_id=asset_id,
            enabled=True,
            linked_at=NOW,
            note="",
        )
    )


def _media_track(timeline: Any, track_id: str) -> list[TimelineMediaClip]:
    for track in timeline.tracks:
        if track.track_id == track_id:
            return [clip for clip in track.clips if isinstance(clip, TimelineMediaClip)]
    raise AssertionError(f"missing track {track_id}")
