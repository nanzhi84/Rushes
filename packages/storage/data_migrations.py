"""Idempotent data migrations applied at workspace startup."""

from __future__ import annotations

from sqlalchemy.engine import Connection

# 待删资产集合：subtitle_template 一律删除；default_library 中未被任何
# timeline_versions.document_json 引用的一并删除（被引用的稍后转正为 upload/audio）。
_DOOMED_ASSET_IDS = (
    "SELECT asset_id FROM assets WHERE kind='subtitle_template' "
    "OR (source='default_library' AND NOT EXISTS ("
    "SELECT 1 FROM timeline_versions t "
    "WHERE t.document_json LIKE '%' || assets.asset_id || '%'))"
)

_DOOMED_CLIP_IDS = (
    f"SELECT clip_id FROM annotation_clip_projection WHERE asset_id IN ({_DOOMED_ASSET_IDS})"
)


def apply_data_migrations(connection: Connection) -> None:
    """Apply idempotent raw-SQL fixups to a workspace database.

    Safe to run on every startup: each statement is a no-op once the stored
    data already matches the current contracts (§ AssetKind/AssetSource).
    """

    _collapse_asset_kinds(connection)


def _collapse_asset_kinds(connection: Connection) -> None:
    connection.exec_driver_sql("UPDATE assets SET kind='audio' WHERE kind IN ('bgm','voiceover')")
    # PRAGMA foreign_keys=ON 下删资产必须先清依赖行（FK 子表 → 父表顺序）：
    # clip_fts 检索行 → signal 投影 → clip 投影 → annotations → transcripts → jobs
    # → project_asset_links → assets。clip_fts/signal 依赖 clip 投影定位，须先删。
    connection.exec_driver_sql(f"DELETE FROM clip_fts WHERE clip_id IN ({_DOOMED_CLIP_IDS})")
    connection.exec_driver_sql(
        f"DELETE FROM annotation_signal_projection WHERE clip_id IN ({_DOOMED_CLIP_IDS})"
    )
    connection.exec_driver_sql(
        f"DELETE FROM annotation_clip_projection WHERE asset_id IN ({_DOOMED_ASSET_IDS})"
    )
    connection.exec_driver_sql(f"DELETE FROM annotations WHERE asset_id IN ({_DOOMED_ASSET_IDS})")
    connection.exec_driver_sql(f"DELETE FROM transcripts WHERE asset_id IN ({_DOOMED_ASSET_IDS})")
    connection.exec_driver_sql(f"DELETE FROM jobs WHERE asset_id IN ({_DOOMED_ASSET_IDS})")
    connection.exec_driver_sql(
        f"DELETE FROM project_asset_links WHERE asset_id IN ({_DOOMED_ASSET_IDS})"
    )
    connection.exec_driver_sql(f"DELETE FROM assets WHERE asset_id IN ({_DOOMED_ASSET_IDS})")
    # 幸存的 default_library 资产此时必然被 timeline 引用：转正为用户音频素材
    connection.exec_driver_sql(
        "UPDATE assets SET source='upload', kind='audio' WHERE source='default_library'"
    )
