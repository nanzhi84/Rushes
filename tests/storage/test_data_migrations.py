from pathlib import Path

from storage import schema
from storage.data_migrations import apply_data_migrations
from storage.db import create_workspace_engine


def _material_summary_columns(connection: object) -> set[str]:
    return {
        str(row[1])
        for row in connection.exec_driver_sql(  # type: ignore[attr-defined]
            "PRAGMA table_info(material_summaries)"
        ).all()
    }


def test_apply_data_migrations_is_idempotent(tmp_path: Path) -> None:
    """启动期钩子应干净跑通，可重复执行不改变结构。"""

    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)

        def _table_names() -> set[str]:
            return {
                str(row[0])
                for row in connection.exec_driver_sql(
                    "SELECT name FROM sqlite_master WHERE type IN ('table', 'view')"
                ).all()
            }

        before = _table_names()
        apply_data_migrations(connection)
        apply_data_migrations(connection)

        assert _table_names() == before
        assert set(schema.ALL_TABLE_NAMES).issubset(before)
        # 新库经 create_all 已带缓存两列，迁移对其恒 no-op。
        assert {"fingerprint", "prompt_version"} <= _material_summary_columns(connection)


def test_migration_adds_material_summary_cache_columns_to_legacy_table(tmp_path: Path) -> None:
    """存量库缺 fingerprint/prompt_version 时，迁移 ALTER 补上（存在性守卫）。"""

    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        # 建一张缺缓存两列的旧版 material_summaries。
        connection.exec_driver_sql(
            "CREATE TABLE material_summaries ("
            "summary_id TEXT PRIMARY KEY, asset_id TEXT NOT NULL, version INTEGER NOT NULL, "
            "focus TEXT, status TEXT NOT NULL, summary_json TEXT NOT NULL, model TEXT, "
            "created_at TEXT NOT NULL)"
        )
        assert "fingerprint" not in _material_summary_columns(connection)

        apply_data_migrations(connection)
        assert {"fingerprint", "prompt_version"} <= _material_summary_columns(connection)

        # 再跑一次不报错（幂等）。
        apply_data_migrations(connection)
        assert {"fingerprint", "prompt_version"} <= _material_summary_columns(connection)
