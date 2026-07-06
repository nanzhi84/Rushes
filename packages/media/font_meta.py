"""Read font family/style metadata from the OpenType ``name`` table."""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path

# OpenType name IDs (see the name-table spec).
_NAME_FAMILY = 1
_NAME_SUBFAMILY = 2
_NAME_FULL = 4
_NAME_TYPO_FAMILY = 16
_NAME_TYPO_SUBFAMILY = 17


class FontMetaError(RuntimeError):
    """Raised when a font file cannot be parsed for name metadata."""


@dataclass(frozen=True, slots=True)
class FontMeta:
    family: str
    style: str
    full_name: str


def read_font_meta(font_path: str | Path) -> FontMeta:
    """Return family/style/full-name from ``font_path`` (TTF/OTF/TTC head font)."""

    path = Path(font_path).expanduser().resolve(strict=True)
    try:
        from fontTools.ttLib import TTFont, TTLibError
    except ImportError as exc:  # pragma: no cover - fonttools is a hard dependency
        raise FontMetaError("fonttools is required for font metadata") from exc

    try:
        font = TTFont(str(path), lazy=True, fontNumber=0)
    except (TTLibError, OSError, ValueError) as exc:
        raise FontMetaError(f"cannot open font: {path}") from exc
    try:
        name_table = font.get("name")
        if name_table is None:
            raise FontMetaError(f"font has no name table: {path}")
        family = _first_name(name_table, _NAME_TYPO_FAMILY, _NAME_FAMILY)
        style = _first_name(name_table, _NAME_TYPO_SUBFAMILY, _NAME_SUBFAMILY)
        full_name = _first_name(name_table, _NAME_FULL) or _join(family, style)
    finally:
        font.close()
    if not family:
        raise FontMetaError(f"font has no family name: {path}")
    return FontMeta(family=family, style=style or "Regular", full_name=full_name)


def _first_name(name_table: object, *name_ids: int) -> str:
    for name_id in name_ids:
        value = name_table.getDebugName(name_id)  # type: ignore[attr-defined]
        if isinstance(value, str) and value.strip():
            return value.strip()
    return ""


def _join(family: str, style: str) -> str:
    return " ".join(part for part in (family, style) if part).strip()
