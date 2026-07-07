"""MaterialSummary persistence repository (Spec C §C3)."""

from __future__ import annotations

from collections.abc import Iterable
from typing import Any

from sqlalchemy import select
from sqlalchemy.engine import Connection

from storage import schema

from ._json import decode_json_columns, encode_json_columns

JSON_COLUMNS = {"summary_json"}


class MaterialSummariesRepository:
    def __init__(self, connection: Connection) -> None:
        self._connection = connection

    def insert(self, values: dict[str, Any]) -> None:
        self._connection.execute(
            schema.material_summaries.insert().values(**encode_json_columns(values, JSON_COLUMNS))
        )

    def latest_ready(self, asset_id: str) -> dict[str, Any] | None:
        """该素材版本号最高的 ``ready`` 摘要；没有则 None。"""

        row = self._connection.execute(
            select(schema.material_summaries)
            .where(schema.material_summaries.c.asset_id == asset_id)
            .where(schema.material_summaries.c.status == "ready")
            .order_by(schema.material_summaries.c.version.desc())
            .limit(1)
        ).first()
        if row is None:
            return None
        return decode_json_columns(dict(row._mapping), JSON_COLUMNS)

    def list_latest_for_assets(self, asset_ids: Iterable[str]) -> dict[str, dict[str, Any]]:
        """批量版 :meth:`latest_ready`：每个素材取版本号最高的 ``ready`` 摘要。

        返回按 asset_id 索引的字典，仅含已有 ready 摘要的素材。
        """

        ids = list(asset_ids)
        if not ids:
            return {}
        rows = self._connection.execute(
            select(schema.material_summaries)
            .where(schema.material_summaries.c.asset_id.in_(ids))
            .where(schema.material_summaries.c.status == "ready")
            .order_by(
                schema.material_summaries.c.asset_id,
                schema.material_summaries.c.version,
            )
        ).all()
        # version 升序遍历：同一 asset 后写覆盖前写，最终留下版本号最高的 ready 摘要。
        latest: dict[str, dict[str, Any]] = {}
        for row in rows:
            decoded = decode_json_columns(dict(row._mapping), JSON_COLUMNS)
            latest[str(decoded["asset_id"])] = decoded
        return latest
