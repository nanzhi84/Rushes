"""Idempotent data migrations applied at workspace startup."""

from __future__ import annotations

from sqlalchemy.engine import Connection


def apply_data_migrations(connection: Connection) -> None:
    """Apply idempotent raw-SQL fixups to a workspace database.

    Safe to run on every startup: each statement is a no-op once the stored
    data already matches the current contracts (§ AssetKind/AssetSource).
    """

    _collapse_asset_kinds(connection)


def _collapse_asset_kinds(connection: Connection) -> None:
    connection.exec_driver_sql("UPDATE assets SET kind='audio' WHERE kind IN ('bgm','voiceover')")
    connection.exec_driver_sql(
        "DELETE FROM project_asset_links WHERE asset_id IN "
        "(SELECT asset_id FROM assets WHERE kind='subtitle_template')"
    )
    connection.exec_driver_sql("DELETE FROM assets WHERE kind='subtitle_template'")
    connection.exec_driver_sql(
        "UPDATE assets SET source='upload', kind='audio' WHERE source='default_library' "
        "AND EXISTS (SELECT 1 FROM timeline_versions t "
        "WHERE t.document_json LIKE '%' || assets.asset_id || '%')"
    )
    connection.exec_driver_sql(
        "DELETE FROM project_asset_links WHERE asset_id IN "
        "(SELECT asset_id FROM assets WHERE source='default_library')"
    )
    connection.exec_driver_sql("DELETE FROM assets WHERE source='default_library'")
