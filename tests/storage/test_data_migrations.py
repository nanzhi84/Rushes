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


def _insert_asset_dependencies(connection: Connection, asset_id: str) -> None:
    """给资产种上所有 FK 指向 assets 的依赖行（含 clip_fts 检索行）。"""

    annotation_id = f"ann-{asset_id}"
    clip_id = f"clip-{asset_id}"
    connection.execute(
        schema.annotations_table.insert().values(
            annotation_id=annotation_id,
            asset_id=asset_id,
            schema="v1",
            status="done",
            document_json="{}",
            created_at="t",
            updated_at="t",
        )
    )
    connection.execute(
        schema.annotation_clip_projection.insert().values(
            clip_id=clip_id,
            annotation_id=annotation_id,
            asset_id=asset_id,
            start_frame=0,
            end_frame=10,
            role="b_roll",
            summary="s",
            keywords_json="[]",
            usable=True,
        )
    )
    connection.execute(
        schema.annotation_signal_projection.insert().values(
            signal_id=f"sig-{asset_id}",
            clip_id=clip_id,
            namespace="ns",
            field="f",
        )
    )
    connection.exec_driver_sql(
        "INSERT INTO clip_fts (clip_id, summary, keywords, retrieval_sentence, ocr_text) "
        "VALUES (?, ?, ?, ?, ?)",
        (clip_id, "s", "k", "r", "o"),
    )
    connection.execute(
        schema.transcripts.insert().values(
            transcript_id=f"tr-{asset_id}",
            asset_id=asset_id,
            provider_id="prov",
            raw_preserved=True,
            utterances="[]",
            vad_segments="[]",
        )
    )
    connection.execute(
        schema.jobs.insert().values(
            job_id=f"job-{asset_id}",
            kind="proxy",
            status="succeeded",
            asset_id=asset_id,
            idempotency_key=f"idem-{asset_id}",
            payload_json="{}",
            attempts=0,
            max_retries=0,
            next_run_at="t",
            created_at="t",
        )
    )


def _dependency_counts(connection: Connection, asset_id: str) -> dict[str, int]:
    clip_id = f"clip-{asset_id}"
    queries: dict[str, tuple[str, tuple[str, ...]]] = {
        "annotations": ("SELECT COUNT(*) FROM annotations WHERE asset_id = ?", (asset_id,)),
        "clips": (
            "SELECT COUNT(*) FROM annotation_clip_projection WHERE asset_id = ?",
            (asset_id,),
        ),
        "signals": (
            "SELECT COUNT(*) FROM annotation_signal_projection WHERE clip_id = ?",
            (clip_id,),
        ),
        "fts": ("SELECT COUNT(*) FROM clip_fts WHERE clip_id = ?", (clip_id,)),
        "transcripts": ("SELECT COUNT(*) FROM transcripts WHERE asset_id = ?", (asset_id,)),
        "jobs": ("SELECT COUNT(*) FROM jobs WHERE asset_id = ?", (asset_id,)),
    }
    return {
        name: int(connection.exec_driver_sql(sql, params).scalar_one())
        for name, (sql, params) in queries.items()
    }


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
    # 待删资产与幸存资产都带上完整依赖行：验证 FK 安全删除且不过删
    for asset_id in (ASSET_SUBTITLE, ASSET_LIB_UNREFERENCED, ASSET_VIDEO):
        _insert_asset_dependencies(connection, asset_id)
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


_SNAPSHOT_TABLES = (
    "assets",
    "project_asset_links",
    "annotations",
    "annotation_clip_projection",
    "annotation_signal_projection",
    "clip_fts",
    "transcripts",
    "jobs",
)


def _snapshot(connection: Connection) -> dict[str, list[tuple[object, ...]]]:
    return {
        table: [
            tuple(row)
            for row in connection.exec_driver_sql(f"SELECT * FROM {table} ORDER BY 1").all()
        ]
        for table in _SNAPSHOT_TABLES
    }


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


def test_deletes_dependent_rows_before_assets(tmp_path: Path) -> None:
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        _seed_legacy_workspace(connection)
        # 生产同款连接：外键强制开启，删资产前必须先清依赖行
        assert connection.exec_driver_sql("PRAGMA foreign_keys").scalar_one() == 1

        apply_data_migrations(connection)

        # 被删资产：本体与全部依赖行（含 clip_fts 检索行）一并消失
        for asset_id in (ASSET_SUBTITLE, ASSET_LIB_UNREFERENCED):
            assert _asset_row(connection, asset_id) is None
            counts = _dependency_counts(connection, asset_id)
            assert counts == dict.fromkeys(counts, 0)

        # 幸存资产：依赖行原样保留，不过删
        survivor_counts = _dependency_counts(connection, ASSET_VIDEO)
        assert survivor_counts == dict.fromkeys(survivor_counts, 1)

        # 再跑一次仍幂等，幸存依赖不受影响
        apply_data_migrations(connection)
        assert _dependency_counts(connection, ASSET_VIDEO) == survivor_counts


def test_apply_data_migrations_is_idempotent(tmp_path: Path) -> None:
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        _seed_legacy_workspace(connection)

        apply_data_migrations(connection)
        first = _snapshot(connection)

        apply_data_migrations(connection)
        second = _snapshot(connection)

        assert first == second
