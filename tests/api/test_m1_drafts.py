"""M1 草稿 REST：CRUD、剪映式日期名、全局去重导入、聚合封面。"""

from __future__ import annotations

from datetime import datetime
from pathlib import Path
from typing import Any

from apps.api.main import create_app
from fastapi import FastAPI
from fastapi.testclient import TestClient
from sqlalchemy import func, select
from sqlalchemy.engine import Engine

from agent_harness.reducer import apply
from contracts.events import AssetImported, AssetIndexReady, AssetLinked, DraftCreated
from storage import schema
from storage.db import begin_immediate
from storage.repositories import DraftsRepository, EventLogRepository
from storage.repositories._json import dump_json
from tools import build_default_tool_registry

TOKEN = "test-token"
BASE_URL = "http://127.0.0.1:8000"
AUTH = {"Authorization": f"Bearer {TOKEN}"}


def test_create_draft_generates_lunar_style_date_name_and_dedupes(tmp_path: Path) -> None:
    app = _app(tmp_path)
    client = _client(app)
    now = datetime.now().astimezone()
    base = f"{now.month}月{now.day}日"

    first = client.post("/api/drafts", headers=AUTH, json={})
    second = client.post("/api/drafts", headers=AUTH, json={})
    third = client.post("/api/drafts", headers=AUTH, json={})

    assert first.status_code == 201
    assert first.json()["draft"]["name"] == base
    assert second.json()["draft"]["name"] == f"{base} (2)"
    assert third.json()["draft"]["name"] == f"{base} (3)"
    # 响应含完整草稿详情（含 defaults 从 workspace 拷贝 + 时间戳）。
    detail = first.json()["draft"]
    assert detail["defaults"]["aspect_ratio"] == "9:16"
    assert detail["defaults"]["fps"] == 30
    assert detail["status"] == "active"
    assert detail["created_at"] and detail["updated_at"]


def test_create_draft_accepts_explicit_name(tmp_path: Path) -> None:
    app = _app(tmp_path)
    client = _client(app)

    created = client.post("/api/drafts", headers=AUTH, json={"name": "种草短片"})
    assert created.status_code == 201
    assert created.json()["draft"]["name"] == "种草短片"

    fetched = client.get(f"/api/drafts/{created.json()['draft']['draft_id']}", headers=AUTH)
    assert fetched.status_code == 200
    assert fetched.json()["draft"]["name"] == "种草短片"


def test_draft_rename_delete_copy_are_rest_only_and_reduce_correctly(tmp_path: Path) -> None:
    registry = build_default_tool_registry()
    assert registry.get("draft.rename") is None
    assert registry.get("draft.delete") is None
    assert registry.get("draft.copy") is None

    app = _app(tmp_path)
    engine = _engine(app)
    _seed_draft(engine, "draft_1", "草稿")
    client = _client(app)

    renamed = client.patch("/api/drafts/draft_1", headers=AUTH, json={"name": "改名后的草稿"})
    copied = client.post(
        "/api/drafts/draft_1/copy",
        headers=AUTH,
        json={"draft_id": "draft_2", "name": "副本草稿"},
    )
    trashed = client.request(
        "DELETE",
        "/api/drafts/draft_1",
        headers=AUTH,
        json={"confirm": True},
    )

    assert renamed.status_code == 200
    assert copied.status_code == 201
    assert trashed.status_code == 200
    assert {"DraftRenamed", "DraftCopied", "DraftTrashed"} <= set(_event_types(app))
    with engine.connect() as connection:
        source = DraftsRepository(connection).get("draft_1")
        copied_draft = DraftsRepository(connection).get("draft_2")
    assert source is not None
    assert source["name"] == "改名后的草稿"
    assert source["status"] == "trashed"
    assert copied_draft is not None
    assert copied_draft["name"] == "副本草稿"
    assert copied_draft["state_version"] == 0


def test_delete_draft_requires_confirmation(tmp_path: Path) -> None:
    app = _app(tmp_path)
    _seed_draft(_engine(app), "draft_1", "草稿")

    unconfirmed = _client(app).request("DELETE", "/api/drafts/draft_1", headers=AUTH, json={})

    assert unconfirmed.status_code == 409
    assert unconfirmed.json()["detail"]["reason"] == "confirmation_required"


def test_list_drafts_returns_active_only_with_cover_and_counts(tmp_path: Path) -> None:
    app = _app(tmp_path)
    engine = _engine(app)
    _seed_draft(engine, "draft_1", "草稿一")
    _seed_draft(engine, "draft_trashed", "已删", status="trashed")
    # draft_1 链接 5 个 thumbnail-ready 素材，linked_at 递增。
    with begin_immediate(engine) as connection:
        for index in range(5):
            _seed_thumb_asset(
                connection,
                draft_id="draft_1",
                asset_id=f"asset_{index}",
                linked_at=f"2026-07-07T00:00:0{index}+00:00",
            )

    response = _client(app).get("/api/drafts", headers=AUTH)

    assert response.status_code == 200
    drafts = response.json()["drafts"]
    # 只返回 active 草稿。
    assert [draft["draft_id"] for draft in drafts] == ["draft_1"]
    item = drafts[0]
    assert item["material_count"] == 5
    # 封面 ≤4，按导入时间倒序（asset_4 最新）。
    assert item["cover_asset_ids"] == ["asset_4", "asset_3", "asset_2", "asset_1"]


def test_import_local_same_draft_reimport_is_duplicate(tmp_path: Path) -> None:
    app = _app(tmp_path)
    engine = _engine(app)
    _seed_draft(engine, "draft_1", "草稿")
    media = _media_file(tmp_path, b"clip-bytes")
    client = _client(app)

    first = client.post(
        "/api/drafts/draft_1/materials/import-local",
        headers=AUTH,
        json={"path": str(media)},
    )
    assert first.status_code == 200
    assert len(first.json()["asset_ids"]) == 1
    assert first.json()["duplicates"] == []

    second = client.post(
        "/api/drafts/draft_1/materials/import-local",
        headers=AUTH,
        json={"path": str(media)},
    )
    assert second.status_code == 200
    # 同草稿重复导入 → duplicates，无新建 asset。
    assert second.json()["asset_ids"] == []
    assert second.json()["duplicates"] == [media.name]
    assert second.json()["event_ids"] == []


def test_import_local_second_draft_links_without_new_job(tmp_path: Path) -> None:
    app = _app(tmp_path)
    engine = _engine(app)
    _seed_draft(engine, "draft_a", "草稿A")
    _seed_draft(engine, "draft_b", "草稿B")
    media = _media_file(tmp_path, b"shared-clip")
    client = _client(app)

    first = client.post(
        "/api/drafts/draft_a/materials/import-local",
        headers=AUTH,
        json={"path": str(media)},
    )
    assert first.status_code == 200
    asset_id = first.json()["asset_ids"][0]
    # 模拟 worker 完成：代理与索引产物就位 → 第二草稿导入应 0 新 job。
    with begin_immediate(engine) as connection:
        connection.execute(
            schema.objects.insert().values(
                hash="proxyhash",
                rel_path="objects/proxyhash.mp4",
                size=5,
                created_at="2026-07-07T00:00:00+00:00",
            )
        )
        connection.execute(
            schema.assets.update()
            .where(schema.assets.c.asset_id == asset_id)
            .values(proxy_object_hash="proxyhash", index_json=dump_json({"shots": []}))
        )
    jobs_before = _job_count(engine)

    second = client.post(
        "/api/drafts/draft_b/materials/import-local",
        headers=AUTH,
        json={"path": str(media)},
    )

    assert second.status_code == 200
    body = second.json()
    # 全局命中且本草稿未链 → 秒建链（不是 duplicate），0 新 job。
    assert body["duplicates"] == []
    assert body["asset_ids"] == [asset_id]
    assert _job_count(engine) == jobs_before
    assert "AssetLinked" in _event_types(app)
    # 链接确实落到 draft_b。
    with engine.connect() as connection:
        link = connection.execute(
            select(func.count())
            .select_from(schema.draft_asset_links)
            .where(schema.draft_asset_links.c.draft_id == "draft_b")
            .where(schema.draft_asset_links.c.asset_id == asset_id)
        ).scalar_one()
    assert link == 1


def test_import_local_second_draft_requeues_missing_proxy_via_merge(tmp_path: Path) -> None:
    app = _app(tmp_path)
    engine = _engine(app)
    _seed_draft(engine, "draft_a", "草稿A")
    _seed_draft(engine, "draft_b", "草稿B")
    media = _media_file(tmp_path, b"unready-clip")
    client = _client(app)

    first = client.post(
        "/api/drafts/draft_a/materials/import-local",
        headers=AUTH,
        json={"path": str(media)},
    )
    asset_id = first.json()["asset_ids"][0]
    jobs_before = _job_count(engine)  # proxy 尚未完成

    second = client.post(
        "/api/drafts/draft_b/materials/import-local",
        headers=AUTH,
        json={"path": str(media)},
    )

    assert second.status_code == 200
    assert second.json()["asset_ids"] == [asset_id]
    # 缺 proxy 产物 → 补队；但同幂等键 merge，不产生新 job 行。
    assert _job_count(engine) == jobs_before


def test_import_local_second_draft_backfills_proxy_for_indexed_legacy_asset(
    tmp_path: Path,
) -> None:
    app = _app(tmp_path)
    engine = _engine(app)
    _seed_draft(engine, "draft_a", "草稿A")
    _seed_draft(engine, "draft_b", "草稿B")
    media = _media_file(tmp_path, b"legacy-indexed-clip")
    client = _client(app)
    asset_id = "asset_legacy_indexed"
    # 直接构造旧可播性规则留下的终态：索引已完成、没有 proxy，也从未创建 proxy job。
    result = apply(
        (
            AssetImported(
                draft_id="draft_a",
                asset_id=asset_id,
                payload={
                    "storage_mode": "reference",
                    "reference_path": str(media),
                    "kind": "video",
                    "source": "local",
                    "filename": media.name,
                    "hash": "legacy-hash",
                    "mtime": media.stat().st_mtime_ns,
                    "size": media.stat().st_size,
                    "probe": {"duration_sec": 1.0, "has_audio": False},
                    "ingest_status": "imported",
                    "usable": True,
                },
            ),
            AssetLinked(draft_id="draft_a", asset_id=asset_id),
            AssetIndexReady(
                draft_id="draft_a",
                asset_id=asset_id,
                payload={"index_json": {"duration_sec": 1.0}, "ingest_status": "indexed"},
            ),
        ),
        engine=engine,
        base_version=None,
        actor="job",
    )
    assert result.status == "applied"

    second = client.post(
        "/api/drafts/draft_b/materials/import-local",
        headers=AUTH,
        json={"path": str(media)},
    )

    assert second.status_code == 200
    with engine.connect() as connection:
        proxy_jobs = connection.execute(
            select(func.count())
            .select_from(schema.jobs)
            .where(schema.jobs.c.asset_id == asset_id)
            .where(schema.jobs.c.kind == "proxy")
        ).scalar_one()
    assert proxy_jobs == 1


def _app(tmp_path: Path) -> FastAPI:
    return create_app(
        tmp_path / "workspace",
        token=TOKEN,
        fs_roots=[tmp_path],
        startup_port=8000,
    )


def _client(app: FastAPI) -> TestClient:
    return TestClient(app, base_url=BASE_URL)


def _state(app: FastAPI) -> Any:
    return app.state.api_state


def _engine(app: FastAPI) -> Engine:
    return _state(app).engine


def _seed_draft(engine: Engine, draft_id: str, name: str, *, status: str = "active") -> None:
    result = apply(
        (
            DraftCreated(
                draft_id=draft_id,
                payload={"name": name, "brief": {"goal": "test"}, "status": status},
            ),
        ),
        engine=engine,
        base_version=None,
        actor="user",
    )
    assert result.status == "applied"


def _seed_thumb_asset(
    connection: Any,
    *,
    draft_id: str,
    asset_id: str,
    linked_at: str,
) -> None:
    thumb_hash = f"thumb_{asset_id}"
    connection.execute(
        schema.objects.insert().values(
            hash=thumb_hash,
            rel_path=f"objects/{thumb_hash}.jpg",
            size=3,
            created_at=linked_at,
        )
    )
    connection.execute(
        schema.assets.insert().values(
            asset_id=asset_id,
            storage_mode="reference",
            object_hash=None,
            reference_path=f"/media/{asset_id}.mp4",
            kind="video",
            source="local_path",
            filename=f"{asset_id}.mp4",
            hash=f"hash_{asset_id}",
            mtime=1,
            size=10,
            probe=None,
            proxy_object_hash=None,
            ingest_status="imported",
            usable=True,
            failure=None,
            thumbnail_object_hash=thumb_hash,
            index_json=None,
            understanding_status="none",
        )
    )
    connection.execute(
        schema.draft_asset_links.insert().values(
            draft_id=draft_id,
            asset_id=asset_id,
            linked_at=linked_at,
            note="",
            rel_dir=None,
        )
    )


def _media_file(tmp_path: Path, data: bytes) -> Path:
    path = tmp_path / f"clip_{len(data)}.mp4"
    path.write_bytes(data)
    return path


def _job_count(engine: Engine) -> int:
    with engine.connect() as connection:
        return int(connection.execute(select(func.count()).select_from(schema.jobs)).scalar_one())


def _event_types(app: FastAPI) -> list[str]:
    with _engine(app).connect() as connection:
        rows = EventLogRepository(connection).read_after(0, limit=500)
    return [row.event_type for row in rows]
