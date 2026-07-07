"""MaterialSummary contracts for agentic material understanding (Spec C §C3)."""

from __future__ import annotations

from typing import Literal

from pydantic import BaseModel, ConfigDict, Field

type SemanticRole = Literal[
    "speech_footage",
    "footage",
    "music",
    "voiceover",
    "ambient",
    "photo",
    "font",
    "other",
]
type SegmentQuality = Literal["good", "usable", "avoid"]


class SummarySpent(BaseModel):
    """理解子代理为产出该摘要花掉的算力账（看帧数 / 转写秒数）。"""

    model_config = ConfigDict(extra="forbid")

    frames_viewed: int = 0
    asr_seconds: float = 0.0


class SummarySegment(BaseModel):
    """带时间戳（秒）的素材分段描述，可直接用于剪辑决策。"""

    model_config = ConfigDict(extra="forbid")

    start_s: float
    end_s: float
    description: str
    transcript: str | None = None
    tags: list[str] = Field(default_factory=list)
    quality: SegmentQuality
    notes: str | None = None


class MaterialSummary(BaseModel):
    """理解子代理产出、落库 material_summaries 的结构化摘要（Spec C §C3 JSON 契约）。"""

    model_config = ConfigDict(extra="forbid")

    asset_id: str
    version: int
    focus: str | None = None
    semantic_role: SemanticRole
    overall: str
    language: str | None = None
    segments: list[SummarySegment] = Field(default_factory=list)
    generated_at: str
    model: str
    spent: SummarySpent = Field(default_factory=SummarySpent)
