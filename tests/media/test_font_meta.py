from __future__ import annotations

from pathlib import Path

import pytest

from media.font_meta import FontMeta, FontMetaError, read_font_meta


def _build_font(tmp_path: Path, *, family: str = "RushesTest", style: str = "Bold") -> Path:
    from fontTools.fontBuilder import FontBuilder
    from fontTools.pens.ttGlyphPen import TTGlyphPen
    from fontTools.ttLib.tables._g_l_y_f import Glyph

    fb = FontBuilder(1000, isTTF=True)
    fb.setupGlyphOrder([".notdef", "A"])
    fb.setupCharacterMap({0x41: "A"})
    pen = TTGlyphPen(None)
    pen.moveTo((0, 0))
    pen.lineTo((0, 500))
    pen.lineTo((500, 500))
    pen.lineTo((500, 0))
    pen.closePath()
    fb.setupGlyf({".notdef": Glyph(), "A": pen.glyph()})
    fb.setupHorizontalMetrics({".notdef": (500, 0), "A": (500, 0)})
    fb.setupHorizontalHeader(ascent=800, descent=-200)
    fb.setupNameTable({"familyName": family, "styleName": style})
    fb.setupOS2()
    fb.setupPost()
    font_path = tmp_path / "font.ttf"
    fb.save(str(font_path))
    return font_path


def test_read_font_meta_extracts_family_and_style(tmp_path: Path) -> None:
    font_path = _build_font(tmp_path, family="RushesTest", style="Bold")

    meta = read_font_meta(font_path)

    assert isinstance(meta, FontMeta)
    assert meta.family == "RushesTest"
    assert meta.style == "Bold"
    assert "RushesTest" in meta.full_name


def test_read_font_meta_rejects_non_font(tmp_path: Path) -> None:
    junk = tmp_path / "not_a_font.ttf"
    junk.write_bytes(b"this is not a font file")

    with pytest.raises(FontMetaError):
        read_font_meta(junk)


def test_read_font_meta_requires_existing_file(tmp_path: Path) -> None:
    with pytest.raises(FileNotFoundError):
        read_font_meta(tmp_path / "missing.ttf")
