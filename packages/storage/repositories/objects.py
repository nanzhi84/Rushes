"""Object metadata persistence repository."""

from __future__ import annotations

from typing import Any

from sqlalchemy import delete, select
from sqlalchemy.dialects.sqlite import insert as sqlite_insert
from sqlalchemy.engine import Connection

from storage import schema

from ._rows import row_to_dict


class ObjectsRepository:
    def __init__(self, connection: Connection) -> None:
        self._connection = connection

    def upsert(self, *, object_hash: str, rel_path: str, size: int, created_at: str) -> None:
        statement = sqlite_insert(schema.objects).values(
            hash=object_hash,
            rel_path=rel_path,
            size=size,
            created_at=created_at,
        )
        self._connection.execute(
            statement.on_conflict_do_update(
                index_elements=[schema.objects.c.hash],
                set_={"rel_path": rel_path, "size": size},
            )
        )

    def get(self, object_hash: str) -> dict[str, Any] | None:
        row = self._connection.execute(
            select(schema.objects).where(schema.objects.c.hash == object_hash)
        ).first()
        return row_to_dict(row)

    def list_hashes(self) -> set[str]:
        rows = self._connection.execute(select(schema.objects.c.hash)).all()
        return {str(row[0]) for row in rows}

    def delete_hash(self, object_hash: str) -> bool:
        result = self._connection.execute(
            delete(schema.objects).where(schema.objects.c.hash == object_hash)
        )
        return result.rowcount == 1

    def delete_unreferenced(self, referenced_hashes: set[str]) -> int:
        if not referenced_hashes:
            result = self._connection.execute(delete(schema.objects))
            return int(result.rowcount or 0)
        result = self._connection.execute(
            delete(schema.objects).where(schema.objects.c.hash.not_in(referenced_hashes))
        )
        return int(result.rowcount or 0)
