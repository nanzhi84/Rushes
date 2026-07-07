"""Draft persistence repository with state_version optimistic locking."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any

from sqlalchemy import select, update
from sqlalchemy.engine import Connection

from storage import schema

from ._json import decode_json_columns, encode_json_columns
from ._rows import row_to_dict

JSON_COLUMNS = {
    "defaults",
    "running_jobs",
    "last_error",
    "brief",
    "content_plan",
    "audio_plan",
    "cut_plan",
    "postprocess_plan",
    "scratch_memory",
}


@dataclass(frozen=True, slots=True)
class DraftUpdateConflict:
    draft_id: str
    expected_state_version: int


class DraftsRepository:
    def __init__(self, connection: Connection) -> None:
        self._connection = connection

    def insert(self, values: dict[str, Any]) -> None:
        self._connection.execute(
            schema.drafts.insert().values(**encode_json_columns(values, JSON_COLUMNS))
        )

    def get(self, draft_id: str) -> dict[str, Any] | None:
        row = self._connection.execute(
            select(schema.drafts).where(schema.drafts.c.draft_id == draft_id)
        ).first()
        result = row_to_dict(row)
        if result is None:
            return None
        return decode_json_columns(result, JSON_COLUMNS)

    def update_with_state_version(
        self,
        draft_id: str,
        expected_state_version: int,
        values: dict[str, Any],
    ) -> DraftUpdateConflict | None:
        encoded_values = encode_json_columns(values, JSON_COLUMNS)
        encoded_values["state_version"] = expected_state_version + 1
        result = self._connection.execute(
            update(schema.drafts)
            .where(schema.drafts.c.draft_id == draft_id)
            .where(schema.drafts.c.state_version == expected_state_version)
            .values(**encoded_values)
        )
        if result.rowcount == 1:
            return None
        return DraftUpdateConflict(draft_id=draft_id, expected_state_version=expected_state_version)
