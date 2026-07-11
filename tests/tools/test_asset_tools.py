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
    # REFERENCE 导入不在同步路径整文件哈希：poster、hash、proxy 三个 job 依次入队。
    assert [event["event"] for event in result.events] == [
        "AssetImported",
        "AssetLinked",
        "JobEnqueued",
        "JobEnqueued",
        "JobEnqueued",
    ]
    assert result.events[2]["payload"]["kind"] == "poster"
    assert result.events[3]["payload"]["kind"] == "hash"
    assert result.events[4]["payload"]["kind"] == "proxy"
    assert result.events[3]["payload"]["idempotency_key"] == f"asset:{result.data['asset_id']}:hash"
    # hash 未就绪：先发 pending 占位（size+mtime），真 sha256 交后台 hash job 补算。
    stat = source.stat()
    assert result.events[0]["payload"]["hash"] == f"pending:{stat.st_size}:{stat.st_mtime_ns}"
    assert result.data["hash"] == f"pending:{stat.st_size}:{stat.st_mtime_ns}"
    assert result.events[0]["payload"]["storage_mode"] == "reference"
    assert result.events[0]["payload"]["reference_path"] == str(source.resolve())
    assert result.events[0]["payload"]["object_hash"] is None
    assert result.events[1]["draft_id"] == "draft_1"
    assert result.data["draft_id"] == "draft_1"


def test_import_local_file_playable_enqueues_index_not_proxy(tmp_path: Path, monkeypatch) -> None:
    import tools.asset.handlers as handlers

    # 可播格式：跳 proxy，改为直接入队 index（index 原本挂在 proxy handler 末尾）。
    monkeypatch.setattr(handlers, "asset_needs_proxy", lambda *a, **k: False)
    source = tmp_path / "playable.mp4"
    source.write_bytes(b"playable")

    result = import_local_file(AssetImportLocalFileInput(path=str(source)), _context())

    assert result.status == "succeeded"
    assert [event["event"] for event in result.events] == [
        "AssetImported",
        "AssetLinked",
        "JobEnqueued",
        "JobEnqueued",
        "JobEnqueued",
    ]
    assert result.events[2]["payload"]["kind"] == "poster"
    assert result.events[3]["payload"]["kind"] == "hash"
    assert result.events[4]["payload"]["kind"] == "index"


def test_import_local_file_copy_mode_copies_to_object_store(tmp_path: Path) -> None:
    import hashlib

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
    # COPY 的 hash 直接沿用 put_file 的真 sha256，且不入 hash job（put_file 已整文件哈希过一遍）。
    expected_digest = hashlib.sha256(b"copy-bytes").hexdigest()
    assert result.events[0]["payload"]["hash"] == expected_digest
    assert result.data["hash"] == expected_digest
    assert "hash" not in [event["payload"].get("kind") for event in result.events[2:]]


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


def test_list_assets_returns_l0_manifest_rows(tmp_path: Path) -> None:
    engine = create_workspace_engine(tmp_path / "workspace")
    with engine.begin() as connection:
        schema.create_all(connection)
        _seed_draft(connection)
        _seed_asset(
            connection,
            "asset_a",
            kind="video",
            filename="a.mp4",
            probe={
                "duration_sec": 12.5,
                "fps": 29.97,
                "width": 1920,
                "height": 1080,
                "has_audio": True,
            },
            thumbnail_object_hash="thumb_a",
            understanding_status="ready",
            ingest_status="indexed",
        )
        _seed_asset(connection, "asset_b", kind="image", filename="b.png")
        _seed_link(connection, "asset_a", rel_dir="clips/set1")
        _seed_link(connection, "asset_b", rel_dir=None)
        _seed_ready_summary(connection, "asset_a")

    with engine.connect() as connection:
        result = list_assets(AssetListAssetsInput(), _context(connection=connection))

    assert result.status == "succeeded"
    assert result.data["draft_id"] == "draft_1"
    assert result.data["total"] == 2
    assert result.data["next_after"] is None
    assets = {item["asset_id"]: item for item in result.data["assets"]}
    assert assets["asset_a"] == {
        "asset_id": "asset_a",
        "filename": "a.mp4",
        "kind": "video",
        "rel_dir": "clips/set1",
        "duration_sec": 12.5,
        "fps": 29.97,
        "width": 1920,
        "height": 1080,
        "orientation": "landscape",
        "has_audio": True,
        "usable": True,
        "ingest_status": "indexed",
        "understanding_status": "ready",
        "has_summary": True,
        "thumbnail_ready": True,
    }
    # probe 缺失：时长/宽高/朝向/有音轨全部降级为 None，缩略图未就绪。
    assert assets["asset_b"]["duration_sec"] is None
    assert assets["asset_b"]["width"] is None
    assert assets["asset_b"]["orientation"] is None
    assert assets["asset_b"]["has_audio"] is None
    assert assets["asset_b"]["thumbnail_ready"] is False
    assert assets["asset_b"]["understanding_status"] == "none"


def test_list_assets_filters_rel_dir_and_ingest_status_and_validates_result(
    tmp_path: Path,
) -> None:
    from tools.specs import build_default_tool_registry

    engine = create_workspace_engine(tmp_path / "workspace")
    with engine.begin() as connection:
        schema.create_all(connection)
        _seed_draft(connection)
        _seed_asset(connection, "asset_a", kind="video", ingest_status="indexed")
        _seed_asset(connection, "asset_b", kind="video", ingest_status="failed")
        _seed_asset(connection, "asset_c", kind="video", ingest_status="indexed")
        _seed_asset(connection, "asset_d", kind="audio", ingest_status="probed")
        _seed_link(connection, "asset_a", rel_dir="clips/set1")
        _seed_link(connection, "asset_b", rel_dir="clips/set1")
        _seed_link(connection, "asset_c", rel_dir="clips/set2")
        _seed_link(connection, "asset_d", rel_dir="clips/set1")

    with engine.connect() as connection:
        result = list_assets(
            AssetListAssetsInput(rel_dir="clips/set1", ingest_status="indexed"),
            _context(connection=connection),
        )

    assert [item["asset_id"] for item in result.data["assets"]] == ["asset_a"]
    with engine.connect() as connection:
        probed = list_assets(
            AssetListAssetsInput(ingest_status="probed"),
            _context(connection=connection),
        )
    assert [item["asset_id"] for item in probed.data["assets"]] == ["asset_d"]
    spec = build_default_tool_registry().require("asset.list_assets").spec
    assert spec.result_model is not None
    spec.result_model.model_validate(result.data)


def test_list_assets_filters_by_kind_audio_and_usable(tmp_path: Path) -> None:
    engine = create_workspace_engine(tmp_path / "workspace")
    with engine.begin() as connection:
        schema.create_all(connection)
        _seed_draft(connection)
        _seed_asset(
            connection,
            "asset_a",
            kind="video",
            probe={"duration_sec": 3.0, "width": 100, "height": 100, "has_audio": True},
        )
        _seed_asset(
            connection,
            "asset_b",
            kind="video",
            probe={"duration_sec": 3.0, "width": 100, "height": 100, "has_audio": False},
        )
        _seed_asset(connection, "asset_c", kind="audio")
        _seed_asset(connection, "asset_d", kind="video", usable=False)
        for asset_id in ("asset_a", "asset_b", "asset_c", "asset_d"):
            _seed_link(connection, asset_id, rel_dir=None)

    with engine.connect() as connection:
        by_kind = list_assets(AssetListAssetsInput(kind="video"), _context(connection=connection))
        with_audio = list_assets(
            AssetListAssetsInput(kind="video", has_audio=True),
            _context(connection=connection),
        )
        usable_only = list_assets(
            AssetListAssetsInput(only_usable=True), _context(connection=connection)
        )

    assert {item["asset_id"] for item in by_kind.data["assets"]} == {
        "asset_a",
        "asset_b",
        "asset_d",
    }
    assert {item["asset_id"] for item in with_audio.data["assets"]} == {"asset_a"}
    assert with_audio.data["total"] == 1
    assert "asset_d" not in {item["asset_id"] for item in usable_only.data["assets"]}


def test_list_assets_keyset_pagination(tmp_path: Path) -> None:
    engine = create_workspace_engine(tmp_path / "workspace")
    with engine.begin() as connection:
        schema.create_all(connection)
        _seed_draft(connection)
        for index in range(5):
            asset_id = f"asset_{index}"
            _seed_asset(connection, asset_id, kind="video")
            _seed_link(connection, asset_id, rel_dir=None)

    with engine.connect() as connection:
        page1 = list_assets(AssetListAssetsInput(limit=2), _context(connection=connection))
        page2 = list_assets(
            AssetListAssetsInput(limit=2, after=page1.data["next_after"]),
            _context(connection=connection),
        )
        page3 = list_assets(
            AssetListAssetsInput(limit=2, after=page2.data["next_after"]),
            _context(connection=connection),
        )

    assert [item["asset_id"] for item in page1.data["assets"]] == ["asset_0", "asset_1"]
    assert page1.data["total"] == 5
    assert page1.data["next_after"] == "asset_1"
    assert [item["asset_id"] for item in page2.data["assets"]] == ["asset_2", "asset_3"]
    assert page2.data["next_after"] == "asset_3"
    # 末页不足 limit：next_after 归 None，表示没有更多。
    assert [item["asset_id"] for item in page3.data["assets"]] == ["asset_4"]
    assert page3.data["next_after"] is None


def test_list_assets_pagination_with_has_audio_filter(tmp_path: Path) -> None:
    engine = create_workspace_engine(tmp_path / "workspace")
    with engine.begin() as connection:
        schema.create_all(connection)
        _seed_draft(connection)
        # asset_0 / asset_2 有音轨，asset_1 / asset_3 无——has_audio 过滤走内存路径。
        for index in range(4):
            asset_id = f"asset_{index}"
            _seed_asset(
                connection,
                asset_id,
                kind="video",
                probe={"duration_sec": 1.0, "width": 10, "height": 10, "has_audio": index % 2 == 0},
            )
            _seed_link(connection, asset_id, rel_dir=None)

    with engine.connect() as connection:
        page1 = list_assets(
            AssetListAssetsInput(has_audio=True, limit=1), _context(connection=connection)
        )
        page2 = list_assets(
            AssetListAssetsInput(has_audio=True, limit=1, after=page1.data["next_after"]),
            _context(connection=connection),
        )

    # total 是过滤后全集（2），分页语义（next_after）与 SQL 下推路径一致。
    assert [item["asset_id"] for item in page1.data["assets"]] == ["asset_0"]
    assert page1.data["total"] == 2
    assert page1.data["next_after"] == "asset_0"
    assert [item["asset_id"] for item in page2.data["assets"]] == ["asset_2"]
    assert page2.data["total"] == 2
    assert page2.data["next_after"] is None


def test_list_assets_orientation_from_probe(tmp_path: Path) -> None:
    engine = create_workspace_engine(tmp_path / "workspace")
    with engine.begin() as connection:
        schema.create_all(connection)
        _seed_draft(connection)
        _seed_asset(
            connection,
            "asset_p",
            kind="video",
            probe={"duration_sec": 1.0, "width": 720, "height": 1280, "has_audio": False},
        )
        _seed_asset(
            connection,
            "asset_s",
            kind="video",
            probe={"duration_sec": 1.0, "width": 512, "height": 512, "has_audio": False},
        )
        _seed_link(connection, "asset_p", rel_dir=None)
        _seed_link(connection, "asset_s", rel_dir=None)

    with engine.connect() as connection:
        result = list_assets(AssetListAssetsInput(), _context(connection=connection))

    orientation = {item["asset_id"]: item["orientation"] for item in result.data["assets"]}
    assert orientation == {"asset_p": "portrait", "asset_s": "square"}


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


def _seed_asset(
    connection: Connection,
    asset_id: str,
    *,
    kind: str,
    probe: dict[str, Any] | None = None,
    thumbnail_object_hash: str | None = None,
    understanding_status: str = "none",
    ingest_status: str = "imported",
    usable: bool = True,
    filename: str | None = None,
) -> None:
    if thumbnail_object_hash is not None:
        connection.execute(
            schema.objects.insert().values(
                hash=thumbnail_object_hash,
                rel_path=f"objects/{thumbnail_object_hash}",
                size=1,
                created_at=NOW,
            )
        )
    connection.execute(
        schema.assets.insert().values(
            asset_id=asset_id,
            storage_mode="reference",
            object_hash=None,
            reference_path=f"/tmp/{asset_id}",
            kind=kind,
            source="local_path",
            filename=filename if filename is not None else f"{asset_id}",
            hash=f"hash_{asset_id}",
            mtime=1,
            size=1,
            probe=None if probe is None else dump_json(probe),
            proxy_object_hash=None,
            ingest_status=ingest_status,
            usable=usable,
            failure=None,
            thumbnail_object_hash=thumbnail_object_hash,
            understanding_status=understanding_status,
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
