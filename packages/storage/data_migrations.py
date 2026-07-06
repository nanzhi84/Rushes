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
    _ensure_message_kind_column(connection)
    _ensure_asset_understanding_columns(connection)


def _table_exists(connection: Connection, name: str) -> bool:
    """该库是否存在名为 name 的表（含 fts5 虚拟表，均登记在 sqlite_master）。"""

    row = connection.exec_driver_sql(
        "SELECT 1 FROM sqlite_master WHERE type='table' AND name=?",
        (name,),
    ).first()
    return row is not None


def _ensure_message_kind_column(connection: Connection) -> None:
    """老库的 messages 表补 kind 列，并把 user 行回填为 kind='user'。"""

    columns = {row[1] for row in connection.exec_driver_sql("PRAGMA table_info(messages)").all()}
    if "kind" in columns:
        return
    connection.exec_driver_sql("ALTER TABLE messages ADD COLUMN kind TEXT NOT NULL DEFAULT 'reply'")
    connection.exec_driver_sql("UPDATE messages SET kind='user' WHERE role='user'")


def _ensure_asset_understanding_columns(connection: Connection) -> None:
    """老库的 assets 表补 Spec C 的三列：缩略图哈希 / 便宜索引 JSON / 理解状态。

    SQLite 的 ALTER TABLE ADD COLUMN 不支持带外键约束，迁移里加普通列即可；
    schema 定义带 thumbnail_object_hash→objects.hash 的外键，供新库 create_all 使用。
    每列都先用 PRAGMA table_info 守卫，可在每次启动重复执行。
    """

    columns = {row[1] for row in connection.exec_driver_sql("PRAGMA table_info(assets)").all()}
    if "thumbnail_object_hash" not in columns:
        connection.exec_driver_sql("ALTER TABLE assets ADD COLUMN thumbnail_object_hash TEXT")
    if "index_json" not in columns:
        connection.exec_driver_sql("ALTER TABLE assets ADD COLUMN index_json TEXT")
    if "understanding_status" not in columns:
        connection.exec_driver_sql(
            "ALTER TABLE assets ADD COLUMN understanding_status TEXT NOT NULL DEFAULT 'none'"
        )


def _collapse_asset_kinds(connection: Connection) -> None:
    connection.exec_driver_sql("UPDATE assets SET kind='audio' WHERE kind IN ('bgm','voiceover')")
    # PRAGMA foreign_keys=ON 下删资产必须先清依赖行（FK 子表 → 父表顺序）：
    # clip_fts 检索行 → signal 投影 → clip 投影 → annotations → transcripts → jobs
    # → project_asset_links → assets。clip_fts/signal 依赖 clip 投影定位，须先删。
    # annotation 表族在后续任务里会被删除；此处按表存在性守卫，兼容删表后的新库。
    # _DOOMED_CLIP_IDS 子查询依赖 annotation_clip_projection，该表不在则涉及它的语句一并跳过。
    clip_projection_present = _table_exists(connection, "annotation_clip_projection")
    if clip_projection_present and _table_exists(connection, "clip_fts"):
        connection.exec_driver_sql(f"DELETE FROM clip_fts WHERE clip_id IN ({_DOOMED_CLIP_IDS})")
    if clip_projection_present and _table_exists(connection, "annotation_signal_projection"):
        connection.exec_driver_sql(
            f"DELETE FROM annotation_signal_projection WHERE clip_id IN ({_DOOMED_CLIP_IDS})"
        )
    if clip_projection_present:
        connection.exec_driver_sql(
            f"DELETE FROM annotation_clip_projection WHERE asset_id IN ({_DOOMED_ASSET_IDS})"
        )
    if _table_exists(connection, "annotations"):
        connection.exec_driver_sql(
            f"DELETE FROM annotations WHERE asset_id IN ({_DOOMED_ASSET_IDS})"
        )
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
