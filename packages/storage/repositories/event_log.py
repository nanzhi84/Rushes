"""EventLog persistence repository."""

from __future__ import annotations

from collections.abc import Callable
from dataclasses import dataclass
from typing import Any

from sqlalchemy import select
from sqlalchemy.engine import Connection

from storage import schema

from ._json import dump_json, load_json


@dataclass(frozen=True, slots=True)
class EventLogRow:
    event_id: int
    event_type: str
    actor: str
    project_id: str | None
    case_id: str | None
    payload_json: dict[str, Any]
    state_version: int | None
    created_at: str


class EventLogRepository:
    def __init__(self, connection: Connection) -> None:
        self._connection = connection

    def append(
        self,
        *,
        event_type: str,
        actor: str,
        project_id: str | None,
        case_id: str | None,
        payload_json: dict[str, Any],
        state_version: int | None,
        created_at: str,
    ) -> int:
        result = self._connection.execute(
            schema.event_log.insert().values(
                event_type=event_type,
                actor=actor,
                project_id=project_id,
                case_id=case_id,
                payload_json=dump_json(payload_json),
                state_version=state_version,
                created_at=created_at,
            )
        )
        primary_key = result.inserted_primary_key
        inserted_id = primary_key[0] if primary_key is not None else None
        if not isinstance(inserted_id, int):
            raise TypeError("event_log.event_id must be an integer primary key")
        return inserted_id

    def read_after(
        self,
        cursor: int,
        *,
        limit: int = 100,
        predicate: Callable[[EventLogRow], bool] | None = None,
    ) -> list[EventLogRow]:
        rows = self._connection.execute(
            select(schema.event_log)
            .where(schema.event_log.c.event_id > cursor)
            .order_by(schema.event_log.c.event_id)
            .limit(limit)
        ).all()
        decoded = [self._decode_row(dict(row._mapping)) for row in rows]
        if predicate is None:
            return decoded
        return [row for row in decoded if predicate(row)]

    def _decode_row(self, values: dict[str, Any]) -> EventLogRow:
        payload = load_json(values["payload_json"])
        if not isinstance(payload, dict):
            raise TypeError("event_log.payload_json must decode to an object")
        return EventLogRow(
            event_id=int(values["event_id"]),
            event_type=str(values["event_type"]),
            actor=str(values["actor"]),
            project_id=values["project_id"],
            case_id=values["case_id"],
            payload_json=payload,
            state_version=values["state_version"],
            created_at=str(values["created_at"]),
        )
