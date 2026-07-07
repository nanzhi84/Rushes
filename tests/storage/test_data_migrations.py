from pathlib import Path

from storage import schema
from storage.data_migrations import apply_data_migrations
from storage.db import create_workspace_engine


def test_apply_data_migrations_is_noop_and_idempotent(tmp_path: Path) -> None:
    """删库重建后无历史迁移：启动期钩子应干净跑通，且可重复执行不改变结构。"""

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
