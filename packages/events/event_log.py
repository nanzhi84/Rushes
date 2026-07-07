"""DomainEvent <-> event_log row serialization."""

from __future__ import annotations

from datetime import UTC, datetime
from typing import Any

from pydantic import TypeAdapter

from contracts.events import DomainEvent, DomainEventBase
from storage.repositories.event_log import EventLogRepository, EventLogRow

EVENT_ADAPTER: TypeAdapter[DomainEvent] = TypeAdapter(DomainEvent)


def validate_domain_event(event: DomainEventBase | dict[str, Any]) -> DomainEventBase:
    event_data = event.model_dump(mode="json") if isinstance(event, DomainEventBase) else event
    parsed = EVENT_ADAPTER.validate_python(event_data)
    if not isinstance(parsed, DomainEventBase):
        raise TypeError("contracts DomainEvent must inherit DomainEventBase")
    return parsed


def serialize_event(
    event: DomainEventBase | dict[str, Any],
    *,
    state_version: int | None = None,
    created_at: str | None = None,
) -> dict[str, Any]:
    parsed = validate_domain_event(event)
    payload = parsed.model_dump(mode="json")
    row_created_at = created_at or parsed.created_at or _now_iso()
    payload["created_at"] = row_created_at
    return {
        "event_type": parsed.event,
        "actor": parsed.actor,
        "draft_id": parsed.draft_id,
        "payload_json": payload,
        "state_version": state_version,
        "created_at": row_created_at,
    }


def deserialize_event(row: EventLogRow) -> DomainEventBase:
    payload = dict(row.payload_json)
    payload.setdefault("event", row.event_type)
    payload.setdefault("actor", row.actor)
    payload.setdefault("draft_id", row.draft_id)
    payload.setdefault("created_at", row.created_at)
    return validate_domain_event(payload)


def append_domain_event(
    repository: EventLogRepository,
    event: DomainEventBase | dict[str, Any],
    *,
    state_version: int | None = None,
    created_at: str | None = None,
) -> int:
    values = serialize_event(event, state_version=state_version, created_at=created_at)
    return repository.append(
        event_type=values["event_type"],
        actor=values["actor"],
        draft_id=values["draft_id"],
        payload_json=values["payload_json"],
        state_version=values["state_version"],
        created_at=values["created_at"],
    )


def _now_iso() -> str:
    return datetime.now(UTC).isoformat()
