"""Job persistence repository implementing PRD §14.3 claim semantics."""

from __future__ import annotations

from typing import Any

from sqlalchemy import select, text, update
from sqlalchemy.engine import Connection

from storage import schema

from ._json import decode_json_columns, encode_json_columns
from ._rows import row_to_dict

JSON_COLUMNS = {"payload_json", "result_json", "error_json"}

CLAIM_SQL = text(
    """
    UPDATE jobs SET status='running', worker_id=:w, started_at=:t, heartbeat_at=:t
    WHERE job_id = (SELECT job_id FROM jobs
                    WHERE status='pending' AND next_run_at <= :t
                    ORDER BY created_at LIMIT 1)
      AND status='pending'
    """
)


class JobsRepository:
    def __init__(self, connection: Connection) -> None:
        self._connection = connection

    def insert(self, values: dict[str, Any]) -> None:
        encoded = encode_json_columns(values, JSON_COLUMNS)
        if encoded.get("next_run_at") is None:
            encoded["next_run_at"] = encoded["created_at"]
        self._connection.execute(schema.jobs.insert().values(**encoded))

    def get(self, job_id: str) -> dict[str, Any] | None:
        row = self._connection.execute(
            select(schema.jobs).where(schema.jobs.c.job_id == job_id)
        ).first()
        result = row_to_dict(row)
        if result is None:
            return None
        return decode_json_columns(result, JSON_COLUMNS)

    def claim_next(self, *, worker_id: str, now: str) -> str | None:
        """Run the exact SQLite claim pattern from PRD §14.3 and check changes()."""

        self._connection.execute(CLAIM_SQL, {"w": worker_id, "t": now})
        claimed = self._connection.execute(text("SELECT changes()")).scalar_one()
        if claimed != 1:
            return None
        job_id = self._connection.execute(
            select(schema.jobs.c.job_id)
            .where(schema.jobs.c.status == "running")
            .where(schema.jobs.c.worker_id == worker_id)
            .where(schema.jobs.c.started_at == now)
            .where(schema.jobs.c.heartbeat_at == now)
            .order_by(schema.jobs.c.created_at)
            .limit(1)
        ).scalar_one()
        return str(job_id)

    def heartbeat(
        self,
        job_id: str,
        *,
        worker_id: str,
        now: str,
        progress: float | None = None,
    ) -> bool:
        values: dict[str, Any] = {"heartbeat_at": now}
        if progress is not None:
            values["progress"] = progress
        result = self._connection.execute(
            update(schema.jobs)
            .where(schema.jobs.c.job_id == job_id)
            .where(schema.jobs.c.worker_id == worker_id)
            .where(schema.jobs.c.status == "running")
            .values(**values)
        )
        return result.rowcount == 1

    def reset_stale_running(self, *, heartbeat_before: str, next_run_at: str) -> int:
        result = self._connection.execute(
            update(schema.jobs)
            .where(schema.jobs.c.status == "running")
            .where(schema.jobs.c.heartbeat_at < heartbeat_before)
            .values(
                status="pending",
                worker_id=None,
                heartbeat_at=None,
                started_at=None,
                next_run_at=next_run_at,
            )
        )
        return int(result.rowcount or 0)

    def finish(
        self,
        job_id: str,
        *,
        status: str,
        finished_at: str,
        result_json: dict[str, Any] | None = None,
        error_json: dict[str, Any] | None = None,
    ) -> bool:
        result_value = (
            None
            if result_json is None
            else encode_json_columns({"result_json": result_json}, JSON_COLUMNS)["result_json"]
        )
        error_value = (
            None
            if error_json is None
            else encode_json_columns({"error_json": error_json}, JSON_COLUMNS)["error_json"]
        )
        result = self._connection.execute(
            update(schema.jobs)
            .where(schema.jobs.c.job_id == job_id)
            .values(
                status=status,
                finished_at=finished_at,
                result_json=result_value,
                error_json=error_value,
            )
        )
        return result.rowcount == 1
