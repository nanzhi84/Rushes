"""Project persistence repository."""

from __future__ import annotations

from typing import Any

from sqlalchemy import select, update
from sqlalchemy.engine import Connection

from storage import schema

from ._json import decode_json_columns, encode_json_columns
from ._rows import row_to_dict

JSON_COLUMNS = {"defaults"}


class ProjectsRepository:
    def __init__(self, connection: Connection) -> None:
        self._connection = connection

    def insert(self, values: dict[str, Any]) -> None:
        self._connection.execute(
            schema.projects.insert().values(**encode_json_columns(values, JSON_COLUMNS))
        )

    def get(self, project_id: str) -> dict[str, Any] | None:
        row = self._connection.execute(
            select(schema.projects).where(schema.projects.c.project_id == project_id)
        ).first()
        result = row_to_dict(row)
        if result is None:
            return None
        return decode_json_columns(result, JSON_COLUMNS)

    def update_fields(self, project_id: str, values: dict[str, Any]) -> bool:
        result = self._connection.execute(
            update(schema.projects)
            .where(schema.projects.c.project_id == project_id)
            .values(**encode_json_columns(values, JSON_COLUMNS))
        )
        return result.rowcount == 1
