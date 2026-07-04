"""Decision persistence repository."""

from __future__ import annotations

from typing import Any

from sqlalchemy import select, update
from sqlalchemy.engine import Connection

from storage import schema

from ._json import decode_json_columns, encode_json_columns
from ._rows import row_to_dict

JSON_COLUMNS = {"options", "answer", "pending_tool_call"}


class DecisionsRepository:
    def __init__(self, connection: Connection) -> None:
        self._connection = connection

    def insert(self, values: dict[str, Any]) -> None:
        self._connection.execute(
            schema.decisions.insert().values(**encode_json_columns(values, JSON_COLUMNS))
        )

    def get(self, decision_id: str) -> dict[str, Any] | None:
        row = self._connection.execute(
            select(schema.decisions).where(schema.decisions.c.decision_id == decision_id)
        ).first()
        result = row_to_dict(row)
        if result is None:
            return None
        return decode_json_columns(result, JSON_COLUMNS)

    def mark_pending_tool_call_replayed(
        self,
        decision_id: str,
        *,
        consumed_at: str,
        replayed_tool_call_id: str,
    ) -> bool:
        """Atomically consume an approved pending tool call exactly once."""

        result = self._connection.execute(
            update(schema.decisions)
            .where(schema.decisions.c.decision_id == decision_id)
            .where(schema.decisions.c.pending_tool_call_status == "approved")
            .values(
                pending_tool_call_status="replayed",
                consumed_at=consumed_at,
                replayed_tool_call_id=replayed_tool_call_id,
            )
        )
        return result.rowcount == 1
