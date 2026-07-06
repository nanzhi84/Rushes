from __future__ import annotations

from pathlib import Path

from agent_harness.loop import _load_project_artifact_stats, _load_project_audio_assets
from contracts.case import CaseState
from storage import schema
from storage.db import create_workspace_engine

NOW = "2026-07-05T00:00:00+00:00"


def _case_state() -> CaseState:
    return CaseState.model_validate(
        {
            "case_id": "case_1",
            "project_id": "project_1",
            "name": "Case",
            "brief": {"goal": "make a video", "confirmed_facts": []},
            "selected_asset_ids": [],
            "disabled_asset_ids": [],
            "scratch_memory": {},
        }
    )


def _insert_asset(
    connection: object,
    *,
    asset_id: str,
    kind: str,
    filename: str,
    mtime: int,
    usable: bool = True,
) -> None:
    connection.execute(  # type: ignore[attr-defined]
        schema.assets.insert().values(
            asset_id=asset_id,
            storage_mode="reference",
            object_hash=None,
            reference_path=f"/tmp/{asset_id}",
            kind=kind,
            source="upload",
            filename=filename,
            hash=f"{asset_id}_hash",
            mtime=mtime,
            size=1,
            probe=None,
            proxy_object_hash=None,
            ingest_status="imported",
            annotation_status="pending",
            annotation_pass="none",
            index_status="none",
            usable=usable,
            failure=None,
        )
    )
    connection.execute(  # type: ignore[attr-defined]
        schema.project_asset_links.insert().values(
            project_id="project_1",
            asset_id=asset_id,
            enabled=True,
            linked_at=NOW,
            note="",
        )
    )


def _seed(connection: object) -> None:
    connection.execute(  # type: ignore[attr-defined]
        schema.projects.insert().values(
            project_id="project_1",
            name="Project",
            status="active",
            defaults="{}",
            created_at=NOW,
            updated_at=NOW,
        )
    )
    _insert_asset(connection, asset_id="video_1", kind="video", filename="clip.mp4", mtime=10)
    _insert_asset(connection, asset_id="audio_old", kind="audio", filename="旧音频.m4a", mtime=20)
    _insert_asset(connection, asset_id="audio_new", kind="audio", filename="新音频.m4a", mtime=30)


def test_audio_kind_assets_flow_into_audio_assets_and_voiceover_ids(tmp_path: Path) -> None:
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)
        _seed(connection)

    case_state = _case_state()
    with engine.connect() as connection:
        audio_assets = _load_project_audio_assets(connection, case_state)
        stats = _load_project_artifact_stats(connection, case_state)

    # kind=="audio" 素材进入 project_audio_assets，按 mtime 倒序（新在前）。
    assert [asset.asset_id for asset in audio_assets] == ["audio_new", "audio_old"]
    # voiceover_asset_ids 收敛为全部 usable audio 素材（不含 video）。
    assert stats.voiceover_asset_ids == frozenset({"audio_new", "audio_old"})


def test_non_audio_and_unusable_assets_excluded(tmp_path: Path) -> None:
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)
        _seed(connection)
        _insert_asset(
            connection,
            asset_id="audio_broken",
            kind="audio",
            filename="坏的.m4a",
            mtime=40,
            usable=False,
        )

    case_state = _case_state()
    with engine.connect() as connection:
        audio_assets = _load_project_audio_assets(connection, case_state)
        stats = _load_project_artifact_stats(connection, case_state)

    audio_ids = {asset.asset_id for asset in audio_assets}
    assert "video_1" not in audio_ids
    assert "audio_broken" not in audio_ids
    assert "video_1" not in stats.voiceover_asset_ids
    assert "audio_broken" not in stats.voiceover_asset_ids
