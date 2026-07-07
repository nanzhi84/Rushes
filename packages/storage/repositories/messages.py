"""Message persistence repository."""

from __future__ import annotations

from typing import Any

from sqlalchemy import select
from sqlalchemy.engine import Connection

from storage import schema

from ._json import decode_json_columns, encode_json_columns

JSON_COLUMNS = {"content"}


class MessagesRepository:
    def __init__(self, connection: Connection) -> None:
        self._connection = connection

    def insert(self, values: dict[str, Any]) -> None:
        self._connection.execute(
            schema.messages.insert().values(**encode_json_columns(values, JSON_COLUMNS))
        )

    def list_for_draft(self, draft_id: str, *, limit: int | None = None) -> list[dict[str, Any]]:
        query = (
            select(schema.messages)
            .where(schema.messages.c.draft_id == draft_id)
            .order_by(schema.messages.c.created_at, schema.messages.c.message_id)
        )
        if limit is not None:
            query = query.limit(limit)
        rows = self._connection.execute(query).all()
        return [decode_json_columns(dict(row._mapping), JSON_COLUMNS) for row in rows]
