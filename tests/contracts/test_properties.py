from hypothesis import given
from hypothesis import strategies as st

from contracts import AudioMode, AudioPlan, CutPlanSlot, TranscriptWord


@given(
    start=st.integers(min_value=0, max_value=10_000),
    width=st.integers(min_value=1, max_value=500),
)
def test_half_open_ms_intervals_accept_positive_width(start: int, width: int) -> None:
    word = TranscriptWord(w="呃", start_ms=start, end_ms=start + width, type="filler")
    assert word.start_ms < word.end_ms


@given(
    start=st.floats(min_value=0.0, max_value=100.0, allow_nan=False, allow_infinity=False),
    width=st.floats(min_value=0.001, max_value=20.0, allow_nan=False, allow_infinity=False),
)
def test_cut_plan_slot_duration_intervals_accept_positive_width(start: float, width: float) -> None:
    slot = CutPlanSlot(
        slot_id="slot",
        brief="brief",
        target_duration_sec=(start, start + width),
        narration_ref=None,
    )
    assert slot.target_duration_sec[0] < slot.target_duration_sec[1]


@given(mode=st.sampled_from([mode.value for mode in AudioMode]))
def test_audio_plan_accepts_only_audio_mode_enum_values(mode: str) -> None:
    plan = AudioPlan.model_validate({"mode": mode})
    assert plan.mode.value == mode
