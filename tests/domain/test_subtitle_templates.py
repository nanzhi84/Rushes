from __future__ import annotations

import pytest

from domain.subtitle_templates import get_subtitle_template, list_subtitle_templates


def test_subtitle_template_registry_is_bounded_and_readable() -> None:
    templates = list_subtitle_templates()

    assert 1 <= len(templates) <= 10
    assert {template.template_id for template in templates} >= {
        "clean_bottom",
        "bold_yellow",
        "minimal_top",
        "karaoke_center",
        "news_bar",
        "soft_shadow",
    }
    assert all(template.display_name for template in templates)


def test_get_subtitle_template_unknown_id_raises_key_error() -> None:
    with pytest.raises(KeyError):
        get_subtitle_template("unknown_template")
