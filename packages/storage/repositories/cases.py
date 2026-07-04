"""Case persistence repository with state_version optimistic locking."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any

from sqlalchemy import select, update
from sqlalchemy.engine import Connection

from storage import schema

from ._json import decode_json_columns, encode_json_columns
from ._rows import row_to_dict

JSON_COLUMNS = {
    "running_jobs",
    "last_error",
    "brief",
    "content_plan",
    "audio_plan",
    "cut_plan",
    "postprocess_plan",
    "selected_asset_ids",
    "disabled_asset_ids",
    "scratch_memory",
}


@dataclass(frozen=True, slots=True)
class CaseUpdateConflict:
    case_id: str
    expected_state_version: int


class CasesRepository:
    def __init__(self, connection: Connection) -> None:
        self._connection = connection

    def insert(self, values: dict[str, Any]) -> None:
        self._connection.execute(
            schema.cases.insert().values(**encode_json_columns(values, JSON_COLUMNS))
        )

    def get(self, case_id: str) -> dict[str, Any] | None:
        row = self._connection.execute(
            select(schema.cases).where(schema.cases.c.case_id == case_id)
        ).first()
        result = row_to_dict(row)
        if result is None:
            return None
        return decode_json_columns(result, JSON_COLUMNS)

    def update_with_state_version(
        self,
        case_id: str,
        expected_state_version: int,
        values: dict[str, Any],
    ) -> CaseUpdateConflict | None:
        encoded_values = encode_json_columns(values, JSON_COLUMNS)
        encoded_values["state_version"] = expected_state_version + 1
        result = self._connection.execute(
            update(schema.cases)
            .where(schema.cases.c.case_id == case_id)
            .where(schema.cases.c.state_version == expected_state_version)
            .values(**encoded_values)
        )
        if result.rowcount == 1:
            return None
        return CaseUpdateConflict(case_id=case_id, expected_state_version=expected_state_version)
