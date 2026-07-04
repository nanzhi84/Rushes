"""Provider call persistence repository."""

from __future__ import annotations

from typing import Any

from sqlalchemy import select
from sqlalchemy.engine import Connection

from storage import schema

from ._json import decode_json_columns, encode_json_columns
from ._rows import row_to_dict

JSON_COLUMNS = {"usage_json"}


class ProviderCallsRepository:
    def __init__(self, connection: Connection) -> None:
        self._connection = connection

    def insert(self, values: dict[str, Any]) -> None:
        self._connection.execute(
            schema.provider_calls.insert().values(**encode_json_columns(values, JSON_COLUMNS))
        )

    def get(self, call_id: str) -> dict[str, Any] | None:
        row = self._connection.execute(
            select(schema.provider_calls).where(schema.provider_calls.c.call_id == call_id)
        ).first()
        result = row_to_dict(row)
        if result is None:
            return None
        return decode_json_columns(result, JSON_COLUMNS)

    def list_for_case(self, case_id: str) -> list[dict[str, Any]]:
        rows = self._connection.execute(
            select(schema.provider_calls).where(schema.provider_calls.c.case_id == case_id)
        ).all()
        return [decode_json_columns(dict(row._mapping), JSON_COLUMNS) for row in rows]
