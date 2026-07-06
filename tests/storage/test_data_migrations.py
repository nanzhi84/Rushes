from pathlib import Path

from sqlalchemy.engine import Connection

from storage import schema
from storage.data_migrations import apply_data_migrations
from storage.db import create_workspace_engine

# asset_id 之间互不为子串，避免 timeline document_json LIKE 误匹配
ASSET_BGM = "asset-legacy-bgm"
ASSET_VOICEOVER = "asset-legacy-voiceover"
ASSET_SUBTITLE = "asset-legacy-subtitle"
ASSET_LIB_REFERENCED = "asset-lib-referenced"
ASSET_LIB_UNREFERENCED = "asset-lib-unreferenced"
ASSET_VIDEO = "asset-plain-video"


def _insert_asset(connection: Connection, asset_id: str, *, kind: str, source: str) -> None:
    connection.execute(
        schema.assets.insert().values(
            asset_id=asset_id,
            storage_mode="reference",
            kind=kind,
            source=source,
            hash=f"hash-{asset_id}",
            size=0,
            ingest_status="ready",
            annotation_status="pending",
            annotation_pass="none",
            index_status="pending",
            usable=True,
        )
    )


def _insert_link(connection: Connection, asset_id: str) -> None:
    connection.execute(
        schema.project_asset_links.insert().values(
            project_id="p1",
            asset_id=asset_id,
            enabled=True,
            linked_at="t",
            note="",
        )
    )


def _seed_legacy_workspace(connection: Connection) -> None:
    schema.create_all(connection)
    connection.execute(
        schema.projects.insert().values(
            project_id="p1",
            name="P1",
            status="active",
            defaults="{}",
            created_at="t",
            updated_at="t",
        )
    )
    connection.execute(
        schema.cases.insert().values(
            case_id="c1",
            project_id="p1",
            name="C1",
            status="idle",
            running_jobs="[]",
            brief="{}",
            selected_asset_ids="[]",
            disabled_asset_ids="[]",
            scratch_memory="{}",
        )
    )
    _insert_asset(connection, ASSET_BGM, kind="bgm", source="upload")
    _insert_asset(connection, ASSET_VOICEOVER, kind="voiceover", source="upload")
    _insert_asset(connection, ASSET_SUBTITLE, kind="subtitle_template", source="upload")
    _insert_asset(connection, ASSET_LIB_REFERENCED, kind="bgm", source="default_library")
    _insert_asset(connection, ASSET_LIB_UNREFERENCED, kind="voiceover", source="default_library")
    _insert_asset(connection, ASSET_VIDEO, kind="video", source="upload")
    for asset_id in (ASSET_SUBTITLE, ASSET_LIB_REFERENCED, ASSET_LIB_UNREFERENCED, ASSET_VIDEO):
        _insert_link(connection, asset_id)
    connection.execute(
        schema.timeline_versions.insert().values(
            timeline_id="tl1",
            case_id="c1",
            version=1,
            document_json=f'{{"clips":[{{"asset_id":"{ASSET_LIB_REFERENCED}"}}]}}',
            created_at="t",
        )
    )


def _asset_row(connection: Connection, asset_id: str) -> dict[str, object] | None:
    row = (
        connection.execute(schema.assets.select().where(schema.assets.c.asset_id == asset_id))
        .mappings()
        .one_or_none()
    )
    return dict(row) if row is not None else None


def _link_exists(connection: Connection, asset_id: str) -> bool:
    return (
        connection.execute(
            schema.project_asset_links.select().where(
                schema.project_asset_links.c.asset_id == asset_id
            )
        ).first()
        is not None
    )


def _snapshot(connection: Connection) -> tuple[list[tuple[object, ...]], list[tuple[object, ...]]]:
    assets = [
        tuple(row)
        for row in connection.execute(
            schema.assets.select().order_by(schema.assets.c.asset_id)
        ).all()
    ]
    links = [
        tuple(row)
        for row in connection.execute(
            schema.project_asset_links.select().order_by(schema.project_asset_links.c.asset_id)
        ).all()
    ]
    return assets, links


def test_collapse_legacy_asset_kinds(tmp_path: Path) -> None:
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        _seed_legacy_workspace(connection)

        apply_data_migrations(connection)

        # bgm / voiceover 收敛为 audio，source 不变
        bgm = _asset_row(connection, ASSET_BGM)
        assert bgm is not None and bgm["kind"] == "audio" and bgm["source"] == "upload"
        voiceover = _asset_row(connection, ASSET_VOICEOVER)
        assert voiceover is not None and voiceover["kind"] == "audio"

        # subtitle_template 资产及其 link 一并删除
        assert _asset_row(connection, ASSET_SUBTITLE) is None
        assert _link_exists(connection, ASSET_SUBTITLE) is False

        # 被 timeline 引用的 default_library → source=upload / kind=audio，link 保留
        referenced = _asset_row(connection, ASSET_LIB_REFERENCED)
        assert referenced is not None
        assert referenced["source"] == "upload"
        assert referenced["kind"] == "audio"
        assert _link_exists(connection, ASSET_LIB_REFERENCED) is True

        # 未被引用的 default_library 资产及其 link 删除
        assert _asset_row(connection, ASSET_LIB_UNREFERENCED) is None
        assert _link_exists(connection, ASSET_LIB_UNREFERENCED) is False

        # 普通视频素材保持不变
        video = _asset_row(connection, ASSET_VIDEO)
        assert video is not None and video["kind"] == "video" and video["source"] == "upload"
        assert _link_exists(connection, ASSET_VIDEO) is True


def test_apply_data_migrations_is_idempotent(tmp_path: Path) -> None:
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        _seed_legacy_workspace(connection)

        apply_data_migrations(connection)
        first = _snapshot(connection)

        apply_data_migrations(connection)
        second = _snapshot(connection)

        assert first == second
