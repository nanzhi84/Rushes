from __future__ import annotations

from pathlib import Path

from agent_harness.loop import _load_draft_artifact_stats, _load_draft_audio_assets
from contracts.draft import DraftState
from storage import schema
from storage.db import create_workspace_engine

NOW = "2026-07-05T00:00:00+00:00"


def _draft_state() -> DraftState:
    return DraftState.model_validate(
        {
            "draft_id": "draft_1",
            "name": "Draft",
            "brief": {"goal": "make a video", "confirmed_facts": []},
            "defaults": {"aspect_ratio": "9:16", "fps": 30},
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
            usable=usable,
            failure=None,
        )
    )
    connection.execute(  # type: ignore[attr-defined]
        schema.draft_asset_links.insert().values(
            draft_id="draft_1",
            asset_id=asset_id,
            linked_at=NOW,
            note="",
        )
    )


def _seed(connection: object) -> None:
    connection.execute(  # type: ignore[attr-defined]
        schema.drafts.insert().values(
            draft_id="draft_1",
            name="Draft",
            state_version=0,
            status="active",
            defaults="{}",
            running_jobs="[]",
            brief='{"goal": "test", "confirmed_facts": []}',
            timeline_validated=False,
            rough_cut_approved=False,
            scratch_memory="{}",
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

    draft_state = _draft_state()
    with engine.connect() as connection:
        audio_assets = _load_draft_audio_assets(connection, draft_state)
        stats = _load_draft_artifact_stats(connection, draft_state)

    # kind=="audio" 素材进入 draft_audio_assets，按 mtime 倒序（新在前）。
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

    draft_state = _draft_state()
    with engine.connect() as connection:
        audio_assets = _load_draft_audio_assets(connection, draft_state)
        stats = _load_draft_artifact_stats(connection, draft_state)

    audio_ids = {asset.asset_id for asset in audio_assets}
    assert "video_1" not in audio_ids
    assert "audio_broken" not in audio_ids
    assert "video_1" not in stats.voiceover_asset_ids
    assert "audio_broken" not in stats.voiceover_asset_ids
