"""JSON serialization helpers for TEXT-backed JSON columns."""

from __future__ import annotations

import json
from typing import Any


def dump_json(value: Any) -> str:
    return json.dumps(value, ensure_ascii=False, separators=(",", ":"), sort_keys=True)


def load_json(value: str | None) -> Any:
    if value is None:
        return None
    return json.loads(value)


def encode_json_columns(values: dict[str, Any], json_columns: set[str]) -> dict[str, Any]:
    encoded = dict(values)
    for column in json_columns:
        if column in encoded:
            value = encoded[column]
            encoded[column] = None if value is None else dump_json(value)
    return encoded


def decode_json_columns(values: dict[str, Any], json_columns: set[str]) -> dict[str, Any]:
    decoded = dict(values)
    for column in json_columns:
        if column in decoded:
            decoded[column] = load_json(decoded[column])
    return decoded
