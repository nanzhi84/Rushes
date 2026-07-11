"""MaterialSummary contracts for agentic material understanding (Spec C §C3)."""

from __future__ import annotations

from typing import Literal

from pydantic import BaseModel, ConfigDict, Field, model_validator

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


class ScanFrameUsed(BaseModel):
    """scan 结论实际引用的画面证据。"""

    model_config = ConfigDict(extra="forbid")

    at_sec: float = Field(ge=0)
    source: Literal["poster", "extracted"]


class ScanAssetResult(BaseModel):
    model_config = ConfigDict(extra="forbid")

    asset_id: str
    gist: str
    tags: list[str] = Field(default_factory=list)
    relevance_0_100: float | None = Field(default=None, ge=0, le=100)
    confidence: float = Field(ge=0, le=1)
    frames_used: list[ScanFrameUsed] = Field(min_length=1)


class ScanSkippedAsset(BaseModel):
    model_config = ConfigDict(extra="forbid")

    asset_id: str
    reason: str


class ScanResult(BaseModel):
    model_config = ConfigDict(extra="forbid")

    assets: list[ScanAssetResult] = Field(default_factory=list)
    skipped: list[ScanSkippedAsset] = Field(default_factory=list)


class DeepAssetResult(BaseModel):
    model_config = ConfigDict(extra="forbid")

    asset_id: str
    status: Literal["ready", "failed", "cached"]
    version: int | None = None
    failure_code: str | None = None
    reason: str | None = None
    summary: dict[str, object] | None = None


class DeepResult(BaseModel):
    model_config = ConfigDict(extra="forbid")

    mode: Literal["point_query", "archive"]
    assets: list[DeepAssetResult] = Field(default_factory=list)


class UnderstandMaterialsResult(BaseModel):
    """由 depth 判别的 understand.materials 非空结果契约。"""

    model_config = ConfigDict(extra="forbid")

    depth: Literal["scan", "deep"]
    scan: ScanResult | None = None
    deep: DeepResult | None = None
    # 旧调用方按 asset_id 读取结果的兼容视图；新契约以 deep.assets 为唯一对外结构。
    results: dict[str, DeepAssetResult] = Field(default_factory=dict, exclude=True)
    # harness 无增量 sink 的兼容路径会暂存待写行；校验接受但对外序列化时隐藏。
    material_summary_rows: list[dict[str, object]] = Field(default_factory=list, exclude=True)
    transcript_rows: list[dict[str, object]] = Field(default_factory=list, exclude=True)

    @model_validator(mode="after")
    def validate_depth_payload(self) -> UnderstandMaterialsResult:
        if self.depth == "scan" and (self.scan is None or self.deep is not None):
            raise ValueError("depth=scan requires scan and forbids deep")
        if self.depth == "deep" and (self.deep is None or self.scan is not None):
            raise ValueError("depth=deep requires deep and forbids scan")
        return self
