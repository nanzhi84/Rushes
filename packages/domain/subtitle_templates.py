"""Built-in subtitle style templates for postprocess Human Gate decisions."""

from __future__ import annotations

from types import MappingProxyType

from contracts.subtitle import SubtitleStyleTemplate

_SUBTITLE_TEMPLATES: tuple[SubtitleStyleTemplate, ...] = (
    SubtitleStyleTemplate(
        template_id="clean_bottom",
        display_name="白字黑边下方",
        font_family="PingFang SC",
        font_size=42,
        primary_color="#FFFFFF",
        outline_color="#111111",
        outline_width=3,
        position="bottom",
        margin_v=96,
    ),
    SubtitleStyleTemplate(
        template_id="bold_yellow",
        display_name="醒目黄字",
        font_family="PingFang SC Semibold",
        font_size=46,
        primary_color="#FFD84D",
        outline_color="#121212",
        outline_width=4,
        position="bottom",
        margin_v=104,
    ),
    SubtitleStyleTemplate(
        template_id="minimal_top",
        display_name="顶部极简",
        font_family="PingFang SC",
        font_size=34,
        primary_color="#F8F8F8",
        outline_color="#202020",
        outline_width=2,
        position="top",
        margin_v=72,
    ),
    SubtitleStyleTemplate(
        template_id="karaoke_center",
        display_name="居中卡拉 OK",
        font_family="PingFang SC Semibold",
        font_size=48,
        primary_color="#FFFFFF",
        outline_color="#2457FF",
        outline_width=3,
        position="center",
        margin_v=0,
    ),
    SubtitleStyleTemplate(
        template_id="news_bar",
        display_name="新闻条字幕",
        font_family="Noto Sans CJK SC",
        font_size=38,
        primary_color="#FFFFFF",
        outline_color="#0F172A",
        outline_width=5,
        position="bottom",
        margin_v=80,
    ),
    SubtitleStyleTemplate(
        template_id="soft_shadow",
        display_name="柔和阴影",
        font_family="PingFang SC",
        font_size=40,
        primary_color="#FFF7E8",
        outline_color="#38312A",
        outline_width=2,
        position="bottom",
        margin_v=112,
    ),
)
_SUBTITLE_TEMPLATE_BY_ID = MappingProxyType(
    {template.template_id: template for template in _SUBTITLE_TEMPLATES}
)


def list_subtitle_templates() -> list[SubtitleStyleTemplate]:
    return list(_SUBTITLE_TEMPLATES)


def get_subtitle_template(template_id: str) -> SubtitleStyleTemplate:
    return _SUBTITLE_TEMPLATE_BY_ID[template_id]
