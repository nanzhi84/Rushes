"""SQLite engine and write-transaction helpers.

Rushes uses synchronous SQLAlchemy 2 with ``sqlite+pysqlite``. Every connection
gets the PRAGMAs required by PRD §14.3: WAL, NORMAL sync, 5s busy timeout, and
foreign keys enabled.

Write discipline: open one connection per task/worker, use ``BEGIN IMMEDIATE``
for short write transactions, commit claim operations immediately, and perform
long-running work outside the transaction. WAL prevents readers from blocking
the writer; it does not turn SQLite into a multi-writer database.
"""

from __future__ import annotations

from collections.abc import Iterator
from contextlib import contextmanager
from pathlib import Path
from typing import Any

from sqlalchemy import create_engine, event
from sqlalchemy.engine import Connection, Engine

from .workspace_paths import WorkspacePaths


def create_workspace_engine(
    workspace_or_db_path: str | Path | WorkspacePaths,
    *,
    echo: bool = False,
) -> Engine:
    """Create a synchronous SQLite engine for a workspace root or rushes.db path."""

    if isinstance(workspace_or_db_path, WorkspacePaths):
        paths = workspace_or_db_path.initialize()
        db_path = paths.db_path
    else:
        candidate = Path(workspace_or_db_path).expanduser().resolve(strict=False)
        if candidate.suffix == ".db":
            candidate.parent.mkdir(parents=True, exist_ok=True)
            db_path = candidate
        else:
            db_path = WorkspacePaths.from_root(candidate).initialize().db_path

    engine = create_engine(f"sqlite+pysqlite:///{db_path}", echo=echo, future=True)

    @event.listens_for(engine, "connect")
    def _set_sqlite_pragmas(dbapi_connection: Any, _connection_record: Any) -> None:
        cursor = dbapi_connection.cursor()
        try:
            cursor.execute("PRAGMA journal_mode=WAL")
            cursor.execute("PRAGMA synchronous=NORMAL")
            cursor.execute("PRAGMA busy_timeout=5000")
            cursor.execute("PRAGMA foreign_keys=ON")
        finally:
            cursor.close()

    return engine


@contextmanager
def begin_immediate(engine: Engine) -> Iterator[Connection]:
    """Open a short ``BEGIN IMMEDIATE`` write transaction on a fresh connection."""

    with engine.connect() as connection:
        connection.exec_driver_sql("BEGIN IMMEDIATE")
        try:
            yield connection
        except BaseException:
            connection.rollback()
            raise
        else:
            connection.commit()
