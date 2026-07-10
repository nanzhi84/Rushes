"""Idempotent data migrations applied at workspace startup.

本次单级草稿模型改版为删库重建（工作区库 0 行、无存量），历史迁移已全部清空。
`apply_data_migrations(connection)` 保留为启动期钩子：每次 API/worker 启动都会跑，
当前是 no-op。以后需要修历史库时，在这里按「存在性守卫 + 已是目标态即 no-op」
的样板追加，确保可重复执行。样板：

    def _ensure_some_column(connection: Connection) -> None:
        columns = {row[1] for row in connection.exec_driver_sql(
            "PRAGMA table_info(some_table)").all()}
        if "some_column" in columns:
            return
        connection.exec_driver_sql("ALTER TABLE some_table ADD COLUMN some_column TEXT")

    def _table_exists(connection: Connection, name: str) -> bool:
        row = connection.exec_driver_sql(
            "SELECT 1 FROM sqlite_master WHERE type='table' AND name=?", (name,)).first()
        return row is not None
"""

from __future__ import annotations

from sqlalchemy.engine import Connection


def apply_data_migrations(connection: Connection) -> None:
    """Apply idempotent raw-SQL fixups to a workspace database.

    每次 API/worker 启动都会跑，靠存在性守卫做到可重复：先查表/查列，已是目标态即 no-op。
    """

    _ensure_material_summary_cache_columns(connection)


def _ensure_material_summary_cache_columns(connection: Connection) -> None:
    """material_summaries 补 fingerprint / prompt_version 两列（Spec C §C3 缓存键强化）。

    新库经 create_all 已带这两列，此处恒 no-op；存量库缺列时 ALTER 补上。
    """

    rows = connection.exec_driver_sql("PRAGMA table_info(material_summaries)").all()
    if not rows:
        return
    columns = {str(row[1]) for row in rows}
    if "fingerprint" not in columns:
        connection.exec_driver_sql("ALTER TABLE material_summaries ADD COLUMN fingerprint TEXT")
    if "prompt_version" not in columns:
        connection.exec_driver_sql("ALTER TABLE material_summaries ADD COLUMN prompt_version TEXT")
