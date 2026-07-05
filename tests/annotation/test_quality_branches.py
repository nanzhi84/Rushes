"""Quality helper branch coverage: severity runs, duck-typed clips, flow score."""

from __future__ import annotations

import pytest

from annotation.quality import (
    CleanSpan,
    _clip_range,
    _events_from_flagged_frames,
    _severity,
    clean_spans,
)
from annotation.shot_split import Shot
from contracts.annotation import QualityEvent
from contracts.events import DomainEventBase  # noqa: F401  (import sanity)


def test_events_from_flagged_frames_splits_on_severity_change_and_gap() -> None:
    shot = Shot(shot_id="shot_1", start_frame=0, end_frame=300)
    flagged = [(10, "soft"), (12, "soft"), (14, "hard"), (100, "hard")]
    ranges = _events_from_flagged_frames(flagged, shot=shot, stride=2, padding=2)

    severities = [severity for severity, _, _ in ranges]
    assert severities == ["soft", "hard", "hard"]
    for _, start, end in ranges:
        assert 0 <= start < end <= 300


def test_severity_thresholds_both_directions() -> None:
    assert _severity(10.0, hard_threshold=20.0, soft_threshold=50.0, lower_is_worse=True) == "hard"
    assert _severity(30.0, hard_threshold=20.0, soft_threshold=50.0, lower_is_worse=True) == "soft"
    assert _severity(80.0, hard_threshold=20.0, soft_threshold=50.0, lower_is_worse=True) is None
    assert _severity(9.0, hard_threshold=8.0, soft_threshold=5.0, lower_is_worse=False) == "hard"


def test_clip_range_duck_typing_and_rejection() -> None:
    shot = Shot(shot_id="shot_x", start_frame=5, end_frame=50)
    assert _clip_range(shot) == ("shot_x", 5, 50)

    class DuckClip:
        clip_id = "duck_1"
        source_start_frame = 0
        source_end_frame = 10

    assert _clip_range(DuckClip()) == ("duck_1", 0, 10)

    with pytest.raises(TypeError):
        _clip_range(object())


def test_clean_spans_subtracts_hard_and_keeps_soft() -> None:
    shot = Shot(shot_id="shot_1", start_frame=0, end_frame=100)
    events = [
        QualityEvent(event_id="q1", kind="blur", severity="hard", start_frame=20, end_frame=40),
        QualityEvent(event_id="q2", kind="shake", severity="soft", start_frame=60, end_frame=70),
    ]
    spans = clean_spans([shot], events)

    intervals = [(span.start_frame, span.end_frame) for span in spans]
    assert (0, 20) in intervals and (40, 100) in intervals
    assert all(isinstance(span, CleanSpan) for span in spans)
