"""Pure-function branch coverage for concat mixing helpers."""

from __future__ import annotations

from media.concat import _loudnorm_filter, _mix_bus
from media.segment_render import RenderProfile


def _profile() -> RenderProfile:
    from media.preview import PREVIEW_PROFILE

    return PREVIEW_PROFILE


def test_mix_bus_variants() -> None:
    parts: list[str] = []
    assert _mix_bus(parts, [], "speech") is None

    single = _mix_bus(parts, ["[a1]"], "speech")
    assert single == "[speech]"
    assert parts[-1] == "[a1]anull[speech]"

    multi = _mix_bus(parts, ["[a1]", "[a2]", "[a3]"], "bgm")
    assert multi == "[bgm]"
    assert "amix=inputs=3" in parts[-1]


def test_loudnorm_filter_measured_and_fallbacks() -> None:
    profile = _profile()

    first_pass = _loudnorm_filter(profile, None, "json")
    assert "print_format=json" in first_pass and "linear" not in first_pass

    incomplete = _loudnorm_filter(profile, {"input_i": "-20.0"}, "summary")
    assert "linear" not in incomplete

    measured = {
        "input_i": "-20.0",
        "input_tp": "-3.0",
        "input_lra": "9.0",
        "input_thresh": "-30.0",
        "target_offset": "0.3",
    }
    second_pass = _loudnorm_filter(profile, measured, "summary")
    assert "measured_I=-20.0" in second_pass
    assert "linear=true" in second_pass
