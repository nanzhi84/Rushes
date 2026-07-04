"""AgentTrace persistence for loop observability."""

from __future__ import annotations

from datetime import UTC, datetime
from typing import Any, Literal

from sqlalchemy import func, select
from sqlalchemy.engine import Engine

from storage import schema
from storage.db import begin_immediate
from storage.repositories._json import dump_json

AgentTraceKind = Literal["context", "action", "gate", "tool_result", "events"]


class TraceRecorder:
    """Append seq-ordered trace rows for one case turn."""

    def __init__(
        self,
        *,
        engine: Engine,
        case_id: str,
        turn_id: str,
        created_at: str | None = None,
    ) -> None:
        self._engine = engine
        self._case_id = case_id
        self._turn_id = turn_id
        self._created_at = created_at
        self._seq = self._load_next_seq() - 1

    def record(self, kind: AgentTraceKind, payload: dict[str, Any]) -> int:
        self._seq += 1
        created_at = self._created_at or _now_iso()
        with begin_immediate(self._engine) as connection:
            connection.execute(
                schema.agent_traces.insert().values(
                    trace_id=f"trace_{self._turn_id}_{self._seq}",
                    turn_id=self._turn_id,
                    case_id=self._case_id,
                    seq=self._seq,
                    kind=kind,
                    payload_json=dump_json(payload),
                    created_at=created_at,
                )
            )
        return self._seq

    def _load_next_seq(self) -> int:
        with self._engine.connect() as connection:
            value = connection.execute(
                select(func.max(schema.agent_traces.c.seq))
                .where(schema.agent_traces.c.case_id == self._case_id)
                .where(schema.agent_traces.c.turn_id == self._turn_id)
            ).scalar_one()
        if value is None:
            return 0
        return int(value) + 1


class NullTraceRecorder:
    """TraceRecorder-compatible no-op used by narrow unit tests."""

    def record(self, kind: AgentTraceKind, payload: dict[str, Any]) -> int:
        del kind, payload
        return -1


def _now_iso() -> str:
    return datetime.now(UTC).isoformat()
