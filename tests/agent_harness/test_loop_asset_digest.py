from __future__ import annotations

from pathlib import Path

from agent_harness.loop import _load_asset_digest
from contracts.case import CaseState
from storage import schema
from storage.db import create_workspace_engine

NOW = "2026-07-05T00:00:00+00:00"


def _case_state(**overrides: object) -> CaseState:
    data: dict[str, object] = {
        "case_id": "case_1",
        "project_id": "project_1",
        "name": "Case",
        "brief": {"goal": "make a video", "confirmed_facts": []},
        "selected_asset_ids": [],
        "disabled_asset_ids": [],
        "scratch_memory": {},
    }
    data.update(overrides)
    return CaseState.model_validate(data)


def _insert_asset(
    connection: object,
    *,
    asset_id: str,
    kind: str,
    filename: str,
    probe: str | None = None,
    understanding_status: str = "none",
    usable: bool = True,
    enabled: bool = True,
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
            mtime=10,
            size=1,
            probe=probe,
            proxy_object_hash=None,
            ingest_status="imported",
            usable=usable,
            failure=None,
            understanding_status=understanding_status,
        )
    )
    connection.execute(  # type: ignore[attr-defined]
        schema.project_asset_links.insert().values(
            project_id="project_1",
            asset_id=asset_id,
            enabled=enabled,
            linked_at=NOW,
            note="",
        )
    )


def _insert_summary(
    connection: object,
    *,
    asset_id: str,
    version: int,
    semantic_role: str,
    overall: str,
    status: str = "ready",
) -> None:
    from storage.repositories._json import dump_json

    payload = {
        "asset_id": asset_id,
        "version": version,
        "semantic_role": semantic_role,
        "overall": overall,
        "generated_at": NOW,
        "model": "qwen-max",
        "segments": [],
    }
    connection.execute(  # type: ignore[attr-defined]
        schema.material_summaries.insert().values(
            summary_id=f"ms_{asset_id}_v{version}",
            asset_id=asset_id,
            version=version,
            focus=None,
            status=status,
            summary_json=dump_json(payload),
            model="qwen-max",
            created_at=NOW,
        )
    )


def _seed_project(connection: object) -> None:
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


def test_asset_digest_joins_summary_probe_and_status(tmp_path: Path) -> None:
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)
        _seed_project(connection)
        _insert_asset(
            connection,
            asset_id="video_1",
            kind="video",
            filename="口播.mp4",
            probe='{"duration_sec": 12.5, "has_audio": true}',
            understanding_status="ready",
        )
        _insert_asset(
            connection,
            asset_id="video_2",
            kind="video",
            filename="空镜.mp4",
            probe=None,
            understanding_status="none",
        )
        # 同素材两版摘要，latest_ready 取版本号更高者
        _insert_summary(
            connection,
            asset_id="video_1",
            version=1,
            semantic_role="footage",
            overall="旧版摘要",
        )
        _insert_summary(
            connection,
            asset_id="video_1",
            version=2,
            semantic_role="speech_footage",
            overall="主播口播产品卖点",
        )

    with engine.connect() as connection:
        digest = _load_asset_digest(connection, _case_state())

    by_id = {row.asset_id: row for row in digest}
    assert set(by_id) == {"video_1", "video_2"}

    row1 = by_id["video_1"]
    assert row1.filename == "口播.mp4"
    assert row1.kind == "video"
    assert row1.duration_sec == 12.5
    assert row1.understanding_status == "ready"
    assert row1.semantic_role == "speech_footage"  # 取最高版本
    assert row1.overall == "主播口播产品卖点"

    row2 = by_id["video_2"]
    assert row2.duration_sec is None
    assert row2.understanding_status == "none"
    assert row2.semantic_role is None
    assert row2.overall is None


def test_asset_digest_excludes_disabled_and_unlinked(tmp_path: Path) -> None:
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)
        _seed_project(connection)
        _insert_asset(connection, asset_id="keep_1", kind="video", filename="a.mp4")
        _insert_asset(connection, asset_id="case_disabled", kind="video", filename="b.mp4")
        _insert_asset(
            connection,
            asset_id="link_disabled",
            kind="video",
            filename="c.mp4",
            enabled=False,
        )

    case_state = _case_state(disabled_asset_ids=["case_disabled"])
    with engine.connect() as connection:
        digest = _load_asset_digest(connection, case_state)

    assert {row.asset_id for row in digest} == {"keep_1"}


def test_asset_digest_ignores_non_ready_summary(tmp_path: Path) -> None:
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)
        _seed_project(connection)
        _insert_asset(
            connection,
            asset_id="video_1",
            kind="video",
            filename="a.mp4",
            understanding_status="running",
        )
        _insert_summary(
            connection,
            asset_id="video_1",
            version=1,
            semantic_role="footage",
            overall="未完成",
            status="pending",
        )

    with engine.connect() as connection:
        digest = _load_asset_digest(connection, _case_state())

    assert len(digest) == 1
    assert digest[0].semantic_role is None
    assert digest[0].overall is None
