"""asset.import_local_file / import_url / list_assets 的纯 handler 测试（单级草稿模型）。"""

from __future__ import annotations

from collections.abc import Callable
from pathlib import Path
from typing import Any

import pytest
from sqlalchemy.engine import Connection

from contracts.asset import StorageMode
from contracts.draft import DraftState
from contracts.tool_result import ToolResult
from storage import schema
from storage.db import create_workspace_engine
from storage.repositories._json import dump_json
from storage.workspace_paths import WorkspacePaths
from tools import ToolExecutionContext
from tools.asset import import_local_file, import_url, list_assets
from tools.specs import (
    AssetImportLocalFileInput,
    AssetImportUrlInput,
    AssetListAssetsInput,
)

NOW = "2026-07-05T00:00:00+00:00"
Handler = Callable[[Any, ToolExecutionContext], ToolResult]
_MISSING = object()


def _draft_state() -> DraftState:
    return DraftState.model_validate(
        {
            "draft_id": "draft_1",
            "name": "Draft",
            "brief": {"goal": "test"},
        }
    )


def _context(
    *,
    paths: WorkspacePaths | None = None,
    connection: Connection | None = None,
    draft_state: DraftState | None | object = _MISSING,
) -> ToolExecutionContext:
    metadata: dict[str, object] = {}
    if paths is not None:
        metadata["workspace_paths"] = paths
    return ToolExecutionContext(
        tool_call_id="tc_1",
        turn_id="turn_1",
        draft_state=_draft_state() if draft_state is _MISSING else draft_state,  # type: ignore[arg-type]
        readonly_connection=connection,
        metadata=metadata,
    )


def test_import_local_file_defaults_to_reference_and_queues_proxy(tmp_path: Path) -> None:
    source = tmp_path / "local.mp4"
    source.write_bytes(b"local")

    result = import_local_file(AssetImportLocalFileInput(path=str(source)), _context())

    assert result.status == "succeeded"
    assert [event["event"] for event in result.events] == [
        "AssetImported",
        "AssetLinked",
        "JobEnqueued",
        "JobEnqueued",
    ]
    # poster 先于 proxy 入队：缩略图/时长秒出，不必等 proxy 转码。
    assert result.events[2]["payload"]["kind"] == "poster"
    assert result.events[3]["payload"]["kind"] == "proxy"
    assert result.events[0]["payload"]["storage_mode"] == "reference"
    assert result.events[0]["payload"]["reference_path"] == str(source.resolve())
    assert result.events[0]["payload"]["object_hash"] is None
    assert result.events[1]["draft_id"] == "draft_1"
    assert result.data["draft_id"] == "draft_1"


def test_import_local_file_copy_mode_copies_to_object_store(tmp_path: Path) -> None:
    paths = WorkspacePaths.from_root(tmp_path / "workspace").initialize()
    source = tmp_path / "clip.mp4"
    source.write_bytes(b"copy-bytes")

    result = import_local_file(
        AssetImportLocalFileInput(path=str(source), storage_mode=StorageMode.COPY),
        _context(paths=paths),
    )

    assert result.status == "succeeded"
    assert result.events[0]["payload"]["storage_mode"] == "copy"
    object_hash = result.events[0]["payload"]["object_hash"]
    assert paths.object_path(object_hash).read_bytes() == b"copy-bytes"
    assert result.events[0]["payload"]["reference_path"] is None


def test_import_local_file_records_rel_dir_on_link(tmp_path: Path) -> None:
    source = tmp_path / "local.mp4"
    source.write_bytes(b"local")

    result = import_local_file(
        AssetImportLocalFileInput(path=str(source), rel_dir="clips/set1"),
        _context(),
    )

    assert result.status == "succeeded"
    assert result.events[1]["event"] == "AssetLinked"
    assert result.events[1]["payload"]["rel_dir"] == "clips/set1"


def test_import_url_handler_queues_import_url_job() -> None:
    result = import_url(
        AssetImportUrlInput(
            asset_id="asset_url",
            url="https://example.test/clip.mp4",
            filename="clip.mp4",
        ),
        _context(),
    )

    assert result.status == "running"
    assert result.data["asset_id"] == "asset_url"
    assert result.data["draft_id"] == "draft_1"
    assert result.events[0]["event"] == "JobEnqueued"
    assert result.events[0]["payload"]["kind"] == "import_url"
    assert result.events[0]["payload"]["job_payload"]["url"] == "https://example.test/clip.mp4"


def test_list_assets_reports_link_summary_and_usable(tmp_path: Path) -> None:
    engine = create_workspace_engine(tmp_path / "workspace")
    with engine.begin() as connection:
        schema.create_all(connection)
        _seed_draft(connection)
        _seed_asset(connection, "asset_a", kind="video")
        _seed_asset(connection, "asset_b", kind="image")
        _seed_link(connection, "asset_a", rel_dir="clips/set1")
        _seed_link(connection, "asset_b", rel_dir=None)
        # 仅 asset_a 有一条 ready 摘要，asset_b 没有摘要。
        _seed_ready_summary(connection, "asset_a")

    with engine.connect() as connection:
        result = list_assets(AssetListAssetsInput(), _context(connection=connection))

    assert result.status == "succeeded"
    assert result.data["draft_id"] == "draft_1"
    assets = {item["asset_id"]: item for item in result.data["assets"]}
    assert set(assets) == {"asset_a", "asset_b"}
    assert assets["asset_a"] == {
        "asset_id": "asset_a",
        "kind": "video",
        "rel_dir": "clips/set1",
        "usable": True,
        "has_summary": True,
    }
    assert assets["asset_b"] == {
        "asset_id": "asset_b",
        "kind": "image",
        "rel_dir": None,
        "usable": True,
        "has_summary": False,
    }


@pytest.mark.parametrize(
    ("handler", "input_model"),
    [
        (import_local_file, AssetImportLocalFileInput(path="/missing.mp4")),
        (import_url, AssetImportUrlInput(url="https://example.test/clip.mp4")),
        (list_assets, AssetListAssetsInput()),
    ],
)
def test_asset_handlers_fail_cleanly_without_draft(
    handler: Handler,
    input_model: Any,
) -> None:
    result = handler(
        input_model,
        ToolExecutionContext(tool_call_id="tc_1", turn_id="turn_1"),
    )

    assert result.status == "failed"
    assert result.error is not None
    assert result.error.error_code == "missing_draft"
    assert result.events == []


def _seed_draft(connection: Connection) -> None:
    connection.execute(
        schema.drafts.insert().values(
            draft_id="draft_1",
            name="Draft",
            state_version=0,
            status="active",
            defaults=dump_json({"aspect_ratio": "9:16", "fps": 30}),
            running_jobs="[]",
            brief=dump_json({"goal": "test", "confirmed_facts": []}),
            timeline_validated=False,
            rough_cut_approved=False,
            scratch_memory="{}",
            created_at=NOW,
            updated_at=NOW,
        )
    )


def _seed_asset(connection: Connection, asset_id: str, *, kind: str) -> None:
    connection.execute(
        schema.assets.insert().values(
            asset_id=asset_id,
            storage_mode="reference",
            object_hash=None,
            reference_path=f"/tmp/{asset_id}",
            kind=kind,
            source="local_path",
            filename=f"{asset_id}",
            hash=f"hash_{asset_id}",
            mtime=1,
            size=1,
            probe=None,
            proxy_object_hash=None,
            ingest_status="imported",
            usable=True,
            failure=None,
        )
    )


def _seed_link(connection: Connection, asset_id: str, *, rel_dir: str | None) -> None:
    connection.execute(
        schema.draft_asset_links.insert().values(
            draft_id="draft_1",
            asset_id=asset_id,
            linked_at=NOW,
            note="",
            rel_dir=rel_dir,
        )
    )


def _seed_ready_summary(connection: Connection, asset_id: str) -> None:
    connection.execute(
        schema.material_summaries.insert().values(
            summary_id=f"ms_{asset_id}_v1",
            asset_id=asset_id,
            version=1,
            focus=None,
            status="ready",
            summary_json=dump_json({"overall": "一段摘要"}),
            model="qwen-vl-plus",
            created_at=NOW,
        )
    )
