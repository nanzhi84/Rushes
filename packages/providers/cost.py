"""Simple usage-to-cost hook."""

from __future__ import annotations

from typing import Any

TOKEN_INPUT_RATE = 0.000001
TOKEN_OUTPUT_RATE = 0.000003
AUDIO_SECOND_RATE = 0.00002


def estimate_cost(usage: dict[str, Any]) -> float | None:
    total = 0.0
    saw_known_unit = False
    input_tokens = usage.get("input_tokens") or usage.get("prompt_tokens")
    output_tokens = usage.get("output_tokens") or usage.get("completion_tokens")
    audio_seconds = usage.get("audio_seconds") or usage.get("duration_seconds")
    if isinstance(input_tokens, int | float):
        total += float(input_tokens) * TOKEN_INPUT_RATE
        saw_known_unit = True
    if isinstance(output_tokens, int | float):
        total += float(output_tokens) * TOKEN_OUTPUT_RATE
        saw_known_unit = True
    if isinstance(audio_seconds, int | float):
        total += float(audio_seconds) * AUDIO_SECOND_RATE
        saw_known_unit = True
    return round(total, 8) if saw_known_unit else None
