"""Shared execution context passed to tool handlers."""

from __future__ import annotations

from collections.abc import Mapping
from dataclasses import dataclass, field
from typing import Any

from sqlalchemy.engine import Connection

from contracts.decision import Decision
from contracts.draft import DraftState


@dataclass(frozen=True, slots=True)
class ToolExecutionContext:
    """Read-only harness state and repository access for a single tool call."""

    tool_call_id: str
    turn_id: str
    draft_state: DraftState | None = None
    decisions: tuple[Decision, ...] = ()
    readonly_connection: Connection | None = None
    created_at: str | None = None
    metadata: Mapping[str, Any] = field(default_factory=dict)
