"""Timeline version persistence repository."""

from __future__ import annotations

from typing import Any

from sqlalchemy import select
from sqlalchemy.engine import Connection

from storage import schema

from ._json import decode_json_columns, encode_json_columns
from ._rows import row_to_dict

JSON_COLUMNS = {"document_json", "validation_report"}


class TimelineVersionsRepository:
    def __init__(self, connection: Connection) -> None:
        self._connection = connection

    def insert(self, values: dict[str, Any]) -> None:
        self._connection.execute(
            schema.timeline_versions.insert().values(**encode_json_columns(values, JSON_COLUMNS))
        )

    def get_by_case_version(self, case_id: str, version: int) -> dict[str, Any] | None:
        row = self._connection.execute(
            select(schema.timeline_versions)
            .where(schema.timeline_versions.c.case_id == case_id)
            .where(schema.timeline_versions.c.version == version)
        ).first()
        result = row_to_dict(row)
        if result is None:
            return None
        return decode_json_columns(result, JSON_COLUMNS)

    def list_for_case(self, case_id: str) -> list[dict[str, Any]]:
        rows = self._connection.execute(
            select(schema.timeline_versions)
            .where(schema.timeline_versions.c.case_id == case_id)
            .order_by(schema.timeline_versions.c.version)
        ).all()
        return [decode_json_columns(dict(row._mapping), JSON_COLUMNS) for row in rows]
