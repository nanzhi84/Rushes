"""Timeline version persistence helpers."""

from __future__ import annotations

from collections.abc import Mapping
from contextlib import nullcontext
from dataclasses import dataclass
from datetime import UTC, datetime
from typing import Any

from sqlalchemy import func, select, update
from sqlalchemy.engine import Connection, Engine

from contracts.draft import DraftState
from contracts.timeline import TimelineState, TimelineValidationReport
from storage import schema
from storage.repositories._json import load_json
from storage.repositories.timeline_versions import TimelineVersionsRepository


@dataclass(frozen=True, slots=True)
class TimelineVersionRecord:
    timeline_id: str
    draft_id: str
    version: int
    parent_version: int | None
    created_by_patch_id: str | None
    timeline: TimelineState
    validation_report: TimelineValidationReport | None
    created_at: str


def store_timeline_version(
    engine: Engine | Connection,
    timeline: TimelineState,
    *,
    created_at: str | None = None,
) -> None:
    with _connection_context(engine) as connection:
        if _timeline_exists(connection, timeline.draft_id, timeline.version):
            return
        report = timeline.validation_report
        TimelineVersionsRepository(connection).insert(
            {
                "timeline_id": timeline.timeline_id,
                "draft_id": timeline.draft_id,
                "version": timeline.version,
                "parent_version": timeline.parent_version,
                "created_by_patch_id": timeline.created_by_patch_id,
                "document_json": timeline.model_dump(mode="json"),
                "validation_report": None if report is None else report.model_dump(mode="json"),
                "created_at": created_at or _now_iso(),
            }
        )


def update_timeline_validation_report(
    engine: Engine | Connection,
    *,
    draft_id: str,
    version: int,
    report: TimelineValidationReport,
) -> None:
    with _connection_context(engine) as connection:
        row = connection.execute(
            select(schema.timeline_versions.c.document_json)
            .where(schema.timeline_versions.c.draft_id == draft_id)
            .where(schema.timeline_versions.c.version == version)
        ).first()
        document_json = None
        if row is not None:
            document = load_json(str(row._mapping["document_json"]))
            timeline = TimelineState.model_validate(document).model_copy(
                update={"validation_report": report}
            )
            document_json = timeline.model_dump_json()
        connection.execute(
            update(schema.timeline_versions)
            .where(schema.timeline_versions.c.draft_id == draft_id)
            .where(schema.timeline_versions.c.version == version)
            .values(
                document_json=document_json,
                validation_report=report.model_dump_json(),
            )
        )


def get_timeline_version(
    engine: Engine | Connection,
    draft_id: str,
    version: int,
) -> TimelineVersionRecord | None:
    with _connection_context(engine) as connection:
        row = TimelineVersionsRepository(connection).get_by_draft_version(draft_id, version)
    if row is None:
        return None
    return _record_from_row(row)


def list_timeline_versions(
    engine: Engine | Connection,
    draft_id: str,
) -> list[TimelineVersionRecord]:
    with _connection_context(engine) as connection:
        rows = TimelineVersionsRepository(connection).list_for_draft(draft_id)
    return [_record_from_row(row) for row in rows]


def restore_timeline_version(
    engine: Engine | Connection,
    draft_state: DraftState,
    *,
    source_version: int,
    created_at: str | None = None,
) -> TimelineState:
    with _connection_context(engine) as connection:
        source_row = TimelineVersionsRepository(connection).get_by_draft_version(
            draft_state.draft_id,
            source_version,
        )
        if source_row is None:
            raise KeyError(f"timeline version not found: {source_version}")
        source = _record_from_row(source_row)
        new_version = _next_version(connection, draft_state.draft_id)
        restored = _restored_timeline(
            source.timeline,
            draft_state=draft_state,
            new_version=new_version,
        )
        store_timeline_version(connection, restored, created_at=created_at)
        return restored


def _restored_timeline(
    source: TimelineState,
    *,
    draft_state: DraftState,
    new_version: int,
) -> TimelineState:
    return source.model_copy(
        update={
            "timeline_id": f"{draft_state.draft_id}:v{new_version}",
            "draft_id": draft_state.draft_id,
            "version": new_version,
            "parent_version": draft_state.timeline_current_version,
            "created_by_patch_id": None,
        },
        deep=True,
    )


def _next_version(connection: Connection, draft_id: str) -> int:
    current = connection.execute(
        select(func.max(schema.timeline_versions.c.version)).where(
            schema.timeline_versions.c.draft_id == draft_id
        )
    ).scalar_one_or_none()
    return int(current or 0) + 1


def _timeline_exists(connection: Connection, draft_id: str, version: int) -> bool:
    row = connection.execute(
        select(schema.timeline_versions.c.timeline_id).where(
            schema.timeline_versions.c.draft_id == draft_id,
            schema.timeline_versions.c.version == version,
        )
    ).first()
    return row is not None


def _record_from_row(row: Mapping[str, Any]) -> TimelineVersionRecord:
    timeline = TimelineState.model_validate(row["document_json"])
    raw_report = row.get("validation_report")
    report = None if raw_report is None else TimelineValidationReport.model_validate(raw_report)
    if report is not None:
        timeline = timeline.model_copy(update={"validation_report": report})
    return TimelineVersionRecord(
        timeline_id=str(row["timeline_id"]),
        draft_id=str(row["draft_id"]),
        version=int(row["version"]),
        parent_version=None if row["parent_version"] is None else int(row["parent_version"]),
        created_by_patch_id=(
            None if row["created_by_patch_id"] is None else str(row["created_by_patch_id"])
        ),
        timeline=timeline,
        validation_report=report,
        created_at=str(row["created_at"]),
    )


def _now_iso() -> str:
    return datetime.now(UTC).isoformat()


def _connection_context(engine: Engine | Connection) -> Any:
    if isinstance(engine, Connection):
        return nullcontext(engine)
    return engine.connect()
