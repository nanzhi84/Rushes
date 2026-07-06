from pathlib import Path

from storage import schema
from storage.db import begin_immediate, create_workspace_engine
from storage.repositories import MaterialSummariesRepository

NOW = "2026-07-06T00:00:00+00:00"


def _prepare_workspace(tmp_path: Path) -> None:
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)


def _insert_asset(connection: object, asset_id: str) -> None:
    connection.execute(  # type: ignore[attr-defined]
        schema.assets.insert().values(
            asset_id=asset_id,
            storage_mode="reference",
            kind="video",
            source="upload",
            hash=f"hash-{asset_id}",
            size=0,
            ingest_status="indexed",
            annotation_status="pending",
            annotation_pass="none",
            index_status="none",
            usable=True,
        )
    )


def _summary_values(
    summary_id: str,
    asset_id: str,
    version: int,
    *,
    status: str,
    focus: str | None = None,
) -> dict[str, object]:
    return {
        "summary_id": summary_id,
        "asset_id": asset_id,
        "version": version,
        "focus": focus,
        "status": status,
        "summary_json": {"overall": f"summary v{version}", "segments": []},
        "model": "qwen-vl-max",
        "created_at": NOW,
    }


def test_insert_and_latest_ready_returns_highest_ready_version(tmp_path: Path) -> None:
    _prepare_workspace(tmp_path)
    engine = create_workspace_engine(tmp_path)
    with begin_immediate(engine) as connection:
        _insert_asset(connection, "asset_1")
        repo = MaterialSummariesRepository(connection)
        repo.insert(_summary_values("s1", "asset_1", 1, status="ready"))
        repo.insert(_summary_values("s2", "asset_1", 2, status="ready", focus="产品卖点"))
        repo.insert(_summary_values("s3", "asset_1", 3, status="running"))

        latest = repo.latest_ready("asset_1")

    assert latest is not None
    assert latest["version"] == 2
    assert latest["focus"] == "产品卖点"
    # summary_json 作为 JSON 列往返解码为原 dict
    assert latest["summary_json"] == {"overall": "summary v2", "segments": []}


def test_latest_ready_returns_none_without_ready_summary(tmp_path: Path) -> None:
    _prepare_workspace(tmp_path)
    engine = create_workspace_engine(tmp_path)
    with begin_immediate(engine) as connection:
        _insert_asset(connection, "asset_1")
        repo = MaterialSummariesRepository(connection)
        repo.insert(_summary_values("s1", "asset_1", 1, status="running"))
        repo.insert(_summary_values("s2", "asset_1", 2, status="failed"))

        assert repo.latest_ready("asset_1") is None
        assert repo.latest_ready("missing") is None


def test_list_latest_for_assets_picks_latest_ready_per_asset(tmp_path: Path) -> None:
    _prepare_workspace(tmp_path)
    engine = create_workspace_engine(tmp_path)
    with begin_immediate(engine) as connection:
        for asset_id in ("asset_1", "asset_2", "asset_3"):
            _insert_asset(connection, asset_id)
        repo = MaterialSummariesRepository(connection)
        repo.insert(_summary_values("a1v1", "asset_1", 1, status="ready"))
        repo.insert(_summary_values("a1v2", "asset_1", 2, status="ready"))
        repo.insert(_summary_values("a2v1", "asset_2", 1, status="ready"))
        # asset_3 只有非 ready 摘要，不应出现在结果里
        repo.insert(_summary_values("a3v1", "asset_3", 1, status="running"))

        result = repo.list_latest_for_assets(["asset_1", "asset_2", "asset_3"])

    assert set(result) == {"asset_1", "asset_2"}
    assert result["asset_1"]["version"] == 2
    assert result["asset_2"]["version"] == 1


def test_list_latest_for_assets_empty_input_returns_empty(tmp_path: Path) -> None:
    _prepare_workspace(tmp_path)
    engine = create_workspace_engine(tmp_path)
    with begin_immediate(engine) as connection:
        repo = MaterialSummariesRepository(connection)
        assert repo.list_latest_for_assets([]) == {}


def test_mark_status_flips_summary_status(tmp_path: Path) -> None:
    _prepare_workspace(tmp_path)
    engine = create_workspace_engine(tmp_path)
    with begin_immediate(engine) as connection:
        _insert_asset(connection, "asset_1")
        repo = MaterialSummariesRepository(connection)
        repo.insert(_summary_values("s1", "asset_1", 1, status="running"))
        assert repo.latest_ready("asset_1") is None

        repo.mark_status("s1", "ready")

        latest = repo.latest_ready("asset_1")
    assert latest is not None
    assert latest["status"] == "ready"
