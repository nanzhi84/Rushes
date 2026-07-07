from pathlib import Path

from sqlalchemy.engine import Connection

from storage import schema
from storage.data_migrations import apply_data_migrations
from storage.db import create_workspace_engine
from storage.workspace_paths import WorkspacePaths

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


# annotations/annotation_clip_projection/candidate_packs 已从 schema 移除（Task 7）；
# 老库仍带着它们，这里用裸 DDL 复原，验证迁移的守卫式 DELETE + DROP 链路。
_LEGACY_ANNOTATIONS_DDL = (
    "CREATE TABLE annotations ("
    "annotation_id TEXT PRIMARY KEY, "
    "asset_id TEXT NOT NULL REFERENCES assets(asset_id), "
    "schema TEXT NOT NULL, status TEXT NOT NULL, document_json TEXT NOT NULL, "
    "created_at TEXT NOT NULL, updated_at TEXT NOT NULL)"
)
_LEGACY_CLIP_PROJECTION_DDL = (
    "CREATE TABLE annotation_clip_projection ("
    "clip_id TEXT PRIMARY KEY, "
    "annotation_id TEXT NOT NULL REFERENCES annotations(annotation_id), "
    "asset_id TEXT NOT NULL REFERENCES assets(asset_id), "
    "start_frame INTEGER NOT NULL, end_frame INTEGER NOT NULL, role TEXT NOT NULL, "
    "summary TEXT NOT NULL, keywords_json TEXT NOT NULL, quality_score REAL, "
    "usable BOOLEAN NOT NULL, embedding BLOB)"
)
_LEGACY_CANDIDATE_PACKS_DDL = (
    "CREATE TABLE candidate_packs ("
    "candidate_pack_id TEXT PRIMARY KEY, "
    "case_id TEXT NOT NULL REFERENCES cases(case_id), "
    "slots TEXT NOT NULL, created_at TEXT NOT NULL)"
)


def _create_legacy_annotation_tables(connection: Connection) -> None:
    connection.exec_driver_sql(_LEGACY_ANNOTATIONS_DDL)
    connection.exec_driver_sql(_LEGACY_CLIP_PROJECTION_DDL)
    connection.exec_driver_sql(_LEGACY_CANDIDATE_PACKS_DDL)


def _insert_asset_dependencies(connection: Connection, asset_id: str) -> None:
    """给资产种上依赖行（含老库仍在的 annotation 表族）。"""

    annotation_id = f"ann-{asset_id}"
    clip_id = f"clip-{asset_id}"
    connection.exec_driver_sql(
        "INSERT INTO annotations "
        "(annotation_id, asset_id, schema, status, document_json, created_at, updated_at) "
        "VALUES (?, ?, 'v1', 'done', '{}', 't', 't')",
        (annotation_id, asset_id),
    )
    connection.exec_driver_sql(
        "INSERT INTO annotation_clip_projection "
        "(clip_id, annotation_id, asset_id, start_frame, end_frame, role, summary, "
        "keywords_json, usable) "
        "VALUES (?, ?, ?, 0, 10, 'b_roll', 's', '[]', 1)",
        (clip_id, annotation_id, asset_id),
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
    # annotation 表族在迁移中整表 DROP，迁移后无法计数；这里只查始终存活的依赖表。
    queries: dict[str, tuple[str, tuple[str, ...]]] = {
        "transcripts": ("SELECT COUNT(*) FROM transcripts WHERE asset_id = ?", (asset_id,)),
        "jobs": ("SELECT COUNT(*) FROM jobs WHERE asset_id = ?", (asset_id,)),
    }
    return {
        name: int(connection.exec_driver_sql(sql, params).scalar_one())
        for name, (sql, params) in queries.items()
    }


def _seed_legacy_workspace(connection: Connection) -> None:
    schema.create_all(connection)
    _create_legacy_annotation_tables(connection)
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

        # 被删资产：本体与全部依赖行一并消失
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


def _insert_transcript_and_job(connection: Connection, asset_id: str) -> None:
    """只种 transcripts/jobs 依赖：这两张表始终存在，不随 annotation 表族被删。"""

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


def _drop_annotation_family_tables(connection: Connection) -> None:
    """模拟后续任务删表后的新库：去掉 annotation 表族与 clip_fts（子表 → 父表顺序）。"""

    connection.exec_driver_sql("DROP TABLE IF EXISTS clip_fts")
    connection.exec_driver_sql("DROP TABLE IF EXISTS annotation_signal_projection")
    connection.exec_driver_sql("DROP TABLE IF EXISTS annotation_clip_projection")
    connection.exec_driver_sql("DROP TABLE IF EXISTS annotations")


def _seed_workspace_without_annotation_tables(connection: Connection) -> None:
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
    # 待删资产与幸存资产都种上 transcripts/jobs 依赖：验证始终存在的表照删不误删
    for asset_id in (ASSET_SUBTITLE, ASSET_LIB_UNREFERENCED, ASSET_VIDEO):
        _insert_transcript_and_job(connection, asset_id)
    connection.execute(
        schema.timeline_versions.insert().values(
            timeline_id="tl1",
            case_id="c1",
            version=1,
            document_json=f'{{"clips":[{{"asset_id":"{ASSET_LIB_REFERENCED}"}}]}}',
            created_at="t",
        )
    )
    # 关键：删掉 annotation 表族，复现「删表后的新库」结构
    _drop_annotation_family_tables(connection)


_SNAPSHOT_TABLES_NO_ANNOTATION = ("assets", "project_asset_links", "transcripts", "jobs")


def _snapshot_no_annotation(connection: Connection) -> dict[str, list[tuple[object, ...]]]:
    return {
        table: [
            tuple(row)
            for row in connection.exec_driver_sql(f"SELECT * FROM {table} ORDER BY 1").all()
        ]
        for table in _SNAPSHOT_TABLES_NO_ANNOTATION
    }


def test_migrations_run_without_annotation_tables(tmp_path: Path) -> None:
    """删表后的新库缺 annotation 表族时，迁移仍应干净跑通且效果不变。"""

    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        _seed_workspace_without_annotation_tables(connection)

        # 不应抛 no such table
        apply_data_migrations(connection)

        # kind 收敛照常生效
        bgm = _asset_row(connection, ASSET_BGM)
        assert bgm is not None and bgm["kind"] == "audio" and bgm["source"] == "upload"
        voiceover = _asset_row(connection, ASSET_VOICEOVER)
        assert voiceover is not None and voiceover["kind"] == "audio"

        # subtitle_template / 未引用的 default_library 资产及其 link/依赖被删
        for asset_id in (ASSET_SUBTITLE, ASSET_LIB_UNREFERENCED):
            assert _asset_row(connection, asset_id) is None
            assert _link_exists(connection, asset_id) is False
            assert (
                connection.exec_driver_sql(
                    "SELECT COUNT(*) FROM transcripts WHERE asset_id = ?", (asset_id,)
                ).scalar_one()
                == 0
            )
            assert (
                connection.exec_driver_sql(
                    "SELECT COUNT(*) FROM jobs WHERE asset_id = ?", (asset_id,)
                ).scalar_one()
                == 0
            )

        # 被 timeline 引用的 default_library → source=upload / kind=audio，link 保留
        referenced = _asset_row(connection, ASSET_LIB_REFERENCED)
        assert referenced is not None
        assert referenced["source"] == "upload"
        assert referenced["kind"] == "audio"
        assert _link_exists(connection, ASSET_LIB_REFERENCED) is True

        # 普通视频素材及其依赖不受影响
        video = _asset_row(connection, ASSET_VIDEO)
        assert video is not None and video["kind"] == "video" and video["source"] == "upload"
        assert _link_exists(connection, ASSET_VIDEO) is True
        assert (
            connection.exec_driver_sql(
                "SELECT COUNT(*) FROM transcripts WHERE asset_id = ?", (ASSET_VIDEO,)
            ).scalar_one()
            == 1
        )

        # 再跑一次仍幂等
        first = _snapshot_no_annotation(connection)
        apply_data_migrations(connection)
        assert _snapshot_no_annotation(connection) == first


def _seed_messages_without_kind(connection: Connection) -> None:
    """模拟老库：messages 表还没有 kind 列。"""

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
    # 重建 messages 表，去掉 kind 列以复现迁移前的老结构
    connection.exec_driver_sql("DROP TABLE messages")
    connection.exec_driver_sql(
        "CREATE TABLE messages ("
        "message_id TEXT PRIMARY KEY, "
        "case_id TEXT NOT NULL REFERENCES cases(case_id), "
        "role TEXT NOT NULL, "
        "content TEXT NOT NULL, "
        "created_at TEXT NOT NULL)"
    )
    connection.exec_driver_sql(
        "INSERT INTO messages (message_id, case_id, role, content, created_at) "
        "VALUES (?, ?, ?, ?, ?)",
        ("m-user", "c1", "user", '"hi"', "t1"),
    )
    connection.exec_driver_sql(
        "INSERT INTO messages (message_id, case_id, role, content, created_at) "
        "VALUES (?, ?, ?, ?, ?)",
        ("m-assistant", "c1", "assistant", '"ok"', "t2"),
    )


def _message_columns(connection: Connection) -> set[str]:
    return {row[1] for row in connection.exec_driver_sql("PRAGMA table_info(messages)").all()}


def _message_kind(connection: Connection, message_id: str) -> str:
    return str(
        connection.exec_driver_sql(
            "SELECT kind FROM messages WHERE message_id = ?", (message_id,)
        ).scalar_one()
    )


_OLD_ASSETS_DDL = (
    "CREATE TABLE assets ("
    "asset_id TEXT PRIMARY KEY, "
    "storage_mode TEXT NOT NULL, "
    "object_hash TEXT, "
    "reference_path TEXT, "
    "kind TEXT NOT NULL, "
    "source TEXT NOT NULL, "
    "filename TEXT NOT NULL DEFAULT '', "
    "hash TEXT NOT NULL, "
    "mtime INTEGER, "
    "size INTEGER NOT NULL, "
    "probe TEXT, "
    "proxy_object_hash TEXT, "
    "ingest_status TEXT NOT NULL, "
    "annotation_status TEXT NOT NULL, "
    "annotation_pass TEXT NOT NULL, "
    "index_status TEXT NOT NULL, "
    "usable BOOLEAN NOT NULL, "
    "failure TEXT)"
)


def _rebuild_assets_without_understanding_columns(db_path: Path) -> None:
    """把 assets 表换成缺 Spec C 三列的老结构，并种一条历史素材行。

    assets 是众多子表的父表，用绕过 engine 的裸 sqlite3 连接关掉外键强制后再重建，
    避免 DROP/CREATE 触发外键校验。
    """

    import sqlite3

    raw = sqlite3.connect(db_path)
    try:
        raw.execute("PRAGMA foreign_keys=OFF")
        raw.execute("DROP TABLE assets")
        raw.execute(_OLD_ASSETS_DDL)
        raw.execute(
            "INSERT INTO assets ("
            "asset_id, storage_mode, kind, source, hash, size, "
            "ingest_status, annotation_status, annotation_pass, index_status, usable) "
            "VALUES (?, 'reference', 'video', 'upload', 'h', 0, "
            "'ready', 'pending', 'none', 'none', 1)",
            (ASSET_VIDEO,),
        )
        raw.commit()
    finally:
        raw.close()


def _asset_columns(connection: Connection) -> set[str]:
    return {row[1] for row in connection.exec_driver_sql("PRAGMA table_info(assets)").all()}


def test_ensure_asset_understanding_columns_adds_and_backfills(tmp_path: Path) -> None:
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)
    db_path = WorkspacePaths.from_root(tmp_path).db_path
    _rebuild_assets_without_understanding_columns(db_path)

    with engine.begin() as connection:
        assert "understanding_status" not in _asset_columns(connection)

        apply_data_migrations(connection)

        columns = _asset_columns(connection)
        assert {"thumbnail_object_hash", "index_json", "understanding_status"} <= columns
        # 历史行回填默认 understanding_status='none'，索引列为空
        row = _asset_row(connection, ASSET_VIDEO)
        assert row is not None
        assert row["understanding_status"] == "none"
        assert row["index_json"] is None
        assert row["thumbnail_object_hash"] is None


def test_ensure_asset_understanding_columns_is_idempotent(tmp_path: Path) -> None:
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)
    db_path = WorkspacePaths.from_root(tmp_path).db_path
    _rebuild_assets_without_understanding_columns(db_path)

    with engine.begin() as connection:
        apply_data_migrations(connection)
        # 列已就位时二次运行不应抛 duplicate column
        apply_data_migrations(connection)
        assert {"thumbnail_object_hash", "index_json", "understanding_status"} <= _asset_columns(
            connection
        )


def test_ensure_message_kind_column_adds_and_backfills(tmp_path: Path) -> None:
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        _seed_messages_without_kind(connection)
        assert "kind" not in _message_columns(connection)

        apply_data_migrations(connection)

        # 迁移后 kind 列存在；user 行回填为 'user'，其余沿用默认 'reply'
        assert "kind" in _message_columns(connection)
        assert _message_kind(connection, "m-user") == "user"
        assert _message_kind(connection, "m-assistant") == "reply"


def test_message_kind_migration_is_idempotent(tmp_path: Path) -> None:
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        _seed_messages_without_kind(connection)

        apply_data_migrations(connection)
        apply_data_migrations(connection)

        assert _message_kind(connection, "m-user") == "user"
        assert _message_kind(connection, "m-assistant") == "reply"


_LEGACY_SIGNAL_DDL = (
    "CREATE TABLE annotation_signal_projection ("
    "signal_id TEXT PRIMARY KEY, "
    "clip_id TEXT NOT NULL REFERENCES annotation_clip_projection(clip_id), "
    "namespace TEXT NOT NULL, field TEXT NOT NULL, "
    "value_text TEXT, value_number REAL, confidence REAL)"
)
_LEGACY_CLIP_FTS_DDL = (
    "CREATE VIRTUAL TABLE clip_fts "
    "USING fts5(clip_id, summary, keywords, retrieval_sentence, ocr_text)"
)


def _rebuild_legacy_offline_db(db_path: Path) -> None:
    """还原「离线标注+检索」时代的老库：assets 带三个已删列，另有 signal 投影与

    clip_fts 两张已删表，并种一个待删的 subtitle_template 资产（连同 annotation /
    clip / signal / fts 依赖），用来触发 collapse 的 FK 安全删除与随后的 DROP。
    """

    import sqlite3

    raw = sqlite3.connect(db_path)
    try:
        raw.execute("PRAGMA foreign_keys=OFF")
        raw.execute("DROP TABLE assets")
        raw.execute(_OLD_ASSETS_DDL)
        # annotation 表族已从 schema 移除，老库仍带着它们：用裸 DDL 复原后再种依赖行
        raw.execute(_LEGACY_ANNOTATIONS_DDL)
        raw.execute(_LEGACY_CLIP_PROJECTION_DDL)
        raw.execute(_LEGACY_CANDIDATE_PACKS_DDL)
        raw.execute(_LEGACY_SIGNAL_DDL)
        raw.execute(_LEGACY_CLIP_FTS_DDL)
        raw.execute(
            "INSERT INTO assets ("
            "asset_id, storage_mode, kind, source, hash, size, "
            "ingest_status, annotation_status, annotation_pass, index_status, usable) "
            "VALUES (?, 'reference', 'subtitle_template', 'upload', 'h', 0, "
            "'ready', 'completed', 'cheap', 'ready', 1)",
            (ASSET_SUBTITLE,),
        )
        raw.execute(
            "INSERT INTO annotations "
            "(annotation_id, asset_id, schema, status, document_json, created_at, updated_at) "
            "VALUES ('ann-doomed', ?, 'v1', 'done', '{}', 't', 't')",
            (ASSET_SUBTITLE,),
        )
        raw.execute(
            "INSERT INTO annotation_clip_projection "
            "(clip_id, annotation_id, asset_id, start_frame, end_frame, role, summary, "
            "keywords_json, usable) "
            "VALUES ('clip-doomed', 'ann-doomed', ?, 0, 10, 'b_roll', 's', '[]', 1)",
            (ASSET_SUBTITLE,),
        )
        raw.execute(
            "INSERT INTO annotation_signal_projection "
            "(signal_id, clip_id, namespace, field) "
            "VALUES ('sig-doomed', 'clip-doomed', 'vision', 'label')"
        )
        raw.execute(
            "INSERT INTO clip_fts (clip_id, summary, keywords, retrieval_sentence, ocr_text) "
            "VALUES ('clip-doomed', 's', '', 's', '')"
        )
        raw.commit()
    finally:
        raw.close()


def test_legacy_annotation_db_migration_drops_offline_columns_and_tables(tmp_path: Path) -> None:
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)
    db_path = WorkspacePaths.from_root(tmp_path).db_path
    _rebuild_legacy_offline_db(db_path)

    # 迁移前：老库确实带着已删列与已删表
    with engine.begin() as connection:
        legacy_columns = _asset_columns(connection)
        assert {"annotation_status", "annotation_pass", "index_status"} <= legacy_columns
        tables_before = _table_names(connection)
        assert {
            "annotation_signal_projection",
            "clip_fts",
            "annotations",
            "annotation_clip_projection",
            "candidate_packs",
        } <= tables_before

    # 启动即跑迁移，不应崩
    with engine.begin() as connection:
        apply_data_migrations(connection)

    with engine.begin() as connection:
        columns = _asset_columns(connection)
        # 三个离线标注列被真删（否则新代码省略它们的 INSERT 会撞 NOT NULL）
        assert {"annotation_status", "annotation_pass", "index_status"}.isdisjoint(columns)
        tables_after = _table_names(connection)
        # 离线标注/检索表族整体退场：signal/clip_fts 与 annotation 三表全部 DROP
        assert {
            "annotation_signal_projection",
            "clip_fts",
            "annotations",
            "annotation_clip_projection",
            "candidate_packs",
        }.isdisjoint(tables_after)
        # 待删素材连同 clip/signal/fts 依赖被清空，父行也删掉
        assert _asset_row(connection, ASSET_SUBTITLE) is None
        # 关键回归：删列后新代码用精简列 INSERT 素材必须成功
        connection.execute(
            schema.assets.insert().values(
                asset_id="asset-post-migration",
                storage_mode="reference",
                kind="video",
                source="upload",
                hash="h2",
                size=0,
                ingest_status="imported",
                usable=True,
            )
        )
        assert _asset_row(connection, "asset-post-migration") is not None


def test_legacy_case_candidate_pack_column_dropped_before_table(tmp_path: Path) -> None:
    """旧库 cases.candidate_pack_id 外键指向 candidate_packs：迁移须先删列再删表，

    否则删表后新 case 的 INSERT 会在外键校验时撞 no such table。
    """

    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)
    db_path = WorkspacePaths.from_root(tmp_path).db_path

    import sqlite3

    raw = sqlite3.connect(db_path)
    try:
        raw.execute("PRAGMA foreign_keys=OFF")
        raw.execute("DROP TABLE cases")
        raw.execute(_LEGACY_CANDIDATE_PACKS_DDL)
        raw.execute(
            "CREATE TABLE cases (case_id TEXT PRIMARY KEY, "
            "candidate_pack_id TEXT REFERENCES candidate_packs(candidate_pack_id))"
        )
        raw.execute("INSERT INTO cases (case_id, candidate_pack_id) VALUES ('c1', NULL)")
        raw.commit()
    finally:
        raw.close()

    with engine.begin() as connection:
        assert "candidate_pack_id" in {
            row[1] for row in connection.exec_driver_sql("PRAGMA table_info(cases)").all()
        }

        apply_data_migrations(connection)

        cols = {row[1] for row in connection.exec_driver_sql("PRAGMA table_info(cases)").all()}
        assert "candidate_pack_id" not in cols
        assert "candidate_packs" not in _table_names(connection)
        # 关键回归：外键强制开启下，删列+删表后新 case INSERT 不再撞 no such table
        assert connection.exec_driver_sql("PRAGMA foreign_keys").scalar_one() == 1
        connection.exec_driver_sql("INSERT INTO cases (case_id) VALUES ('c2')")
        assert connection.exec_driver_sql("SELECT COUNT(*) FROM cases").scalar_one() == 2


def _table_names(connection: Connection) -> set[str]:
    return {
        str(row[0])
        for row in connection.exec_driver_sql(
            "SELECT name FROM sqlite_master WHERE type IN ('table','view')"
        ).all()
    }
