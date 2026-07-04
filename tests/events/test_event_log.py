from pathlib import Path

import pytest
from pydantic import ValidationError

from events.event_log import append_domain_event, deserialize_event
from storage import schema
from storage.db import begin_immediate, create_workspace_engine
from storage.repositories.event_log import EventLogRepository


def test_domain_event_append_and_cursor_readback_order(tmp_path: Path) -> None:
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)

    with begin_immediate(engine) as connection:
        repository = EventLogRepository(connection)
        first = append_domain_event(
            repository,
            {"event": "MemorySaved", "memory_id": "memory_1"},
            created_at="2026-07-04T00:00:00+00:00",
        )
        second = append_domain_event(
            repository,
            {"event": "ContextCompacted", "compaction_id": "compact_1"},
            created_at="2026-07-04T00:00:01+00:00",
        )
        rows = repository.read_after(0)

    assert [row.event_id for row in rows] == [first, second]
    assert [deserialize_event(row).event for row in rows] == ["MemorySaved", "ContextCompacted"]


def test_illegal_event_is_rejected_before_event_log_insert(tmp_path: Path) -> None:
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)

    with begin_immediate(engine) as connection:
        repository = EventLogRepository(connection)
        with pytest.raises(ValidationError):
            append_domain_event(
                repository,
                {"event": "NotARealEvent"},
                created_at="2026-07-04T00:00:00+00:00",
            )
        assert repository.read_after(0) == []
