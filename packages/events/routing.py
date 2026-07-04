"""Pure SSE routing predicates derived from PRD §4.5."""

from __future__ import annotations

from typing import Any

from contracts.events import DomainEventBase

SSE_SUPPRESSED_EVENTS: frozenset[str] = frozenset(
    {"PolicyRefusal", "ProviderCallRecorded", "ContextCompacted"}
)
MEMORY_EVENTS: frozenset[str] = frozenset(
    {"MemoryCandidateExtracted", "MemoryCandidateDiscarded", "MemorySaved"}
)
JOB_EVENTS: frozenset[str] = frozenset(
    {"JobEnqueued", "JobProgress", "JobSucceeded", "JobFailed", "JobCancelled"}
)


def should_push_sse(event: DomainEventBase) -> bool:
    return event.event not in SSE_SUPPRESSED_EVENTS


def routes_to_case(event: DomainEventBase, case_id: str) -> bool:
    """Case endpoint predicate: case_id or requested_by_case_id must match."""

    if event.event in SSE_SUPPRESSED_EVENTS:
        return False
    event_case_id = _string_attr(event, "case_id")
    if event_case_id == case_id:
        return True
    requested_by_case_id = _requested_by_case_id(event)
    return requested_by_case_id == case_id


def routes_to_workspace(event: DomainEventBase) -> bool:
    """Workspace endpoint predicate, including §4.5 special cases."""

    if event.event in SSE_SUPPRESSED_EVENTS:
        return False
    if event.event == "TurnEnded":
        return False
    if event.event in {"CapabilityDegraded", "SecurityRefusal"}:
        return True
    if _string_attr(event, "case_id") is not None or _requested_by_case_id(event) is not None:
        return True
    if _string_attr(event, "project_id") is not None or _string_attr(event, "asset_id") is not None:
        return True
    if event.event in MEMORY_EVENTS:
        return True
    if event.event in JOB_EVENTS:
        return True
    return _string_attr(event, "scope_type") == "workspace"


def _requested_by_case_id(event: DomainEventBase) -> str | None:
    direct = _string_attr(event, "requested_by_case_id")
    if direct is not None:
        return direct
    payload = event.payload
    value = payload.get("requested_by_case_id")
    return value if isinstance(value, str) else None


def _string_attr(event: DomainEventBase, name: str) -> str | None:
    value: Any = getattr(event, name, None)
    return value if isinstance(value, str) else None
