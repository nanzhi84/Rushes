"""ToolSpec declarations for the Rushes tool registry."""

from __future__ import annotations

from typing import Literal

from pydantic import BaseModel, ConfigDict, Field, field_validator, model_validator

from contracts.asset import AssetKind, AssetListAssetsResult, StorageMode
from contracts.decision import (
    DecisionAnswer,
    DecisionOption,
    DecisionScopeType,
    DecisionType,
    PendingToolCall,
)
from contracts.patch import (
    AddBgmOp,
    AdjustGainOp,
    DeleteRangeOp,
    EditSubtitleTextOp,
    GenerateSubtitlesOp,
    InsertClipOp,
    RemoveTrackClipsOp,
    ReorderBlocksOp,
    ReplaceClipOp,
    SetPlaybackRateOp,
    SetSubtitleStyleOp,
    TimelinePatchRequest,
    TrimClipOp,
)
from contracts.preview_inspection import PreviewInspectionResult
from contracts.tool import PatchOpSpec, ToolSpec
from contracts.understanding import UnderstandMaterialsResult

from .registry import PatchOpRegistry, ToolHandler, ToolRegistry


class DecisionAnswerInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    decision_id: str
    answer: DecisionAnswer


class AskUserInput(BaseModel):
    model_config = ConfigDict(extra="forbid", populate_by_name=True)

    question: str
    scope_type: DecisionScopeType = "draft"
    draft_id: str | None = None
    decision_id: str | None = None
    decision_type: DecisionType = Field(default="generic", alias="type")
    options: list[DecisionOption] = Field(default_factory=list)
    allow_free_text: bool = True
    blocking: bool = True
    reduce_target: Literal["brief.confirmed_facts", "scratch_memory"] | None = None
    metadata: dict[str, object] = Field(default_factory=dict)


class ConfirmActionInput(BaseModel):
    model_config = ConfigDict(extra="forbid", populate_by_name=True)

    question: str
    scope_type: DecisionScopeType = "draft"
    draft_id: str | None = None
    decision_id: str | None = None
    decision_type: DecisionType = Field(default="generic", alias="type")
    options: list[DecisionOption] = Field(default_factory=list)
    pending_tool_call: PendingToolCall | None = None
    blocking: bool = True
    metadata: dict[str, object] = Field(default_factory=dict)


class ShowProgressInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    title: str
    body: str | None = None
    progress: float | None = None
    metadata: dict[str, object] = Field(default_factory=dict)


class ShowPreviewInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    preview_id: str
    title: str = "预览已生成"
    body: str | None = None
    media_ref: str | None = None
    metadata: dict[str, object] = Field(default_factory=dict)


class ShowTimelineInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    timeline_summary: str
    title: str = "时间线"
    body: str | None = None
    metadata: dict[str, object] = Field(default_factory=dict)


class ShowErrorInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    title: str
    body: str
    error_code: str
    retryable: bool = False
    metadata: dict[str, object] = Field(default_factory=dict)


class AssetImportLocalFileInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    asset_id: str | None = None
    path: str
    storage_mode: StorageMode = StorageMode.REFERENCE
    kind: AssetKind = AssetKind.VIDEO
    # 文件夹导入时相对所选根目录的子路径（含所选目录名），素材面板按它分组展示。
    rel_dir: str | None = None


class AssetImportUrlInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    asset_id: str | None = None
    url: str
    filename: str | None = None
    kind: AssetKind = AssetKind.VIDEO
    max_bytes: int | None = None


class AssetListAssetsInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    kind: Literal["video", "audio", "image", "font"] | None = None
    has_audio: bool | None = None
    rel_dir: str | None = None
    ingest_status: (
        Literal["imported", "probing", "probed", "proxying", "indexed", "ready", "failed"] | None
    ) = None
    only_usable: bool | None = None
    limit: int | None = Field(default=None, ge=1, le=200)
    # keyset 分页游标：按 asset_id 升序，返回 asset_id 严格大于 after 的行。
    after: str | None = None


class AudioInspectSourcesInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    asset_ids: list[str] = Field(default_factory=list)


class AudioAsrOriginalInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    asset_id: str | None = None
    provider_id: str | None = None


class AudioRoughCutSpeechInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    asset_id: str | None = None
    transcript_id: str | None = None
    llm_provider_id: str | None = None
    filler_words: list[str] = Field(default_factory=list)
    pause_threshold_ms: int = Field(default=600, gt=0)
    repeat_similarity_threshold: float = Field(default=0.88, ge=0.0, le=1.0)


class AudioGenerateTtsInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    provider_id: str | None = None
    asr_provider_id: str | None = None
    voice_type: str | None = None


class AudioAlignUploadedVoiceoverInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    script_text: str
    asset_id: str | None = None
    provider_id: str | None = None


class ContentCreatePlanInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    storyline_hint: str | None = None
    target_duration_sec: float | None = Field(default=None, gt=0)
    slot_count: int | None = Field(default=None, ge=1)


class ContentRevisePlanInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    revision_hint: str


class ComposeInitialClip(BaseModel):
    model_config = ConfigDict(extra="forbid")

    asset_id: str
    source_start_s: float
    source_end_s: float
    role: Literal["a_roll", "b_roll", "image"]

    @model_validator(mode="after")
    def validate_source_range(self) -> ComposeInitialClip:
        if self.source_start_s < 0:
            raise ValueError("source_start_s must be non-negative")
        if self.source_start_s >= self.source_end_s:
            raise ValueError("source_start_s must be < source_end_s")
        return self


class ComposeInitialInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    clips: list[ComposeInitialClip] = Field(min_length=1)
    voiceover_asset_id: str | None = None


class TimelineValidateInput(BaseModel):
    model_config = ConfigDict(extra="forbid")


class TimelineInspectInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    version: int | None = None


class TimelineRestoreVersionInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    source_version: int


class RenderPreviewInput(BaseModel):
    model_config = ConfigDict(extra="forbid")


class RenderInspectPreviewInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    preview_id: str
    checks: list[Literal["streams", "decode", "black", "freeze", "silence", "loudness"]] | None = (
        None
    )

    @field_validator("checks")
    @classmethod
    def _dedupe_checks(cls, value: list[str] | None) -> list[str] | None:
        return None if value is None else list(dict.fromkeys(value))


class RenderFinalMp4Input(BaseModel):
    model_config = ConfigDict(extra="forbid")


class RenderStatusInput(BaseModel):
    model_config = ConfigDict(extra="forbid")


class MemoryExtractFromDraftInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    summary_hint: str | None = None


class MemoryAskScopeInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    candidate_id: str


class MemorySaveInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    candidate_id: str


class MemorySearchRelevantInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    query: str
    limit: int = Field(default=5, ge=1, le=5)


class UnderstandMaterialsInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    asset_ids: list[str] = Field(min_length=1)
    depth: Literal["scan", "deep"] = "deep"
    focus: str | None = None
    max_steps_per_asset: int | None = Field(default=None, ge=1, le=12)

    @model_validator(mode="after")
    def validate_depth_options(self) -> UnderstandMaterialsInput:
        if self.depth == "scan" and self.max_steps_per_asset is not None:
            raise ValueError("depth=scan 不支持 max_steps_per_asset")
        return self


def tool_specs() -> tuple[ToolSpec, ...]:
    return (
        ToolSpec(
            name="decision.answer",
            namespace="decision",
            version="1",
            input_model=DecisionAnswerInput,
            result_model=None,
            handler_ref="tools.builtin.decision_answer",
            allowed_scopes=["draft_editor"],
            requires_artifacts=[],
            requires_active_draft=False,
            side_effects=["draft"],
            emits_events=["DecisionAnswered"],
            cost_tier="cheap",
            description="Convert a structured user answer into a DecisionAnswered event.",
        ),
        ToolSpec(
            name="interaction.ask_user",
            namespace="interaction",
            version="1",
            input_model=AskUserInput,
            result_model=None,
            handler_ref="tools.interaction.ask_user",
            allowed_scopes=["draft_editor"],
            requires_artifacts=[],
            requires_active_draft=False,
            side_effects=["draft"],
            emits_events=["DecisionCreated"],
            cost_tier="cheap",
            description="Ask an open or multiple-choice question through a Decision.",
        ),
        ToolSpec(
            name="interaction.confirm_action",
            namespace="interaction",
            version="1",
            input_model=ConfirmActionInput,
            result_model=None,
            handler_ref="tools.interaction.confirm_action",
            allowed_scopes=["draft_editor"],
            requires_artifacts=[],
            requires_active_draft=False,
            side_effects=["draft"],
            emits_events=["DecisionCreated"],
            cost_tier="cheap",
            description="Create a confirmation Decision for an action.",
        ),
        ToolSpec(
            name="interaction.show_progress",
            namespace="interaction",
            version="1",
            input_model=ShowProgressInput,
            result_model=None,
            handler_ref="tools.interaction.show_progress",
            allowed_scopes=["draft_editor"],
            requires_artifacts=[],
            requires_active_draft=False,
            side_effects=[],
            emits_events=[],
            cost_tier="cheap",
            description="Emit a frontend-renderable progress interaction.",
        ),
        ToolSpec(
            name="interaction.show_preview",
            namespace="interaction",
            version="1",
            input_model=ShowPreviewInput,
            result_model=None,
            handler_ref="tools.interaction.show_preview",
            allowed_scopes=["draft_editor"],
            requires_artifacts=[],
            requires_active_draft=False,
            side_effects=[],
            emits_events=[],
            cost_tier="cheap",
            description="Emit a frontend-renderable preview interaction.",
        ),
        ToolSpec(
            name="interaction.show_timeline",
            namespace="interaction",
            version="1",
            input_model=ShowTimelineInput,
            result_model=None,
            handler_ref="tools.interaction.show_timeline",
            allowed_scopes=["draft_editor"],
            requires_artifacts=[],
            requires_active_draft=False,
            side_effects=[],
            emits_events=[],
            cost_tier="cheap",
            description="Emit a frontend-renderable timeline summary.",
        ),
        ToolSpec(
            name="interaction.show_error",
            namespace="interaction",
            version="1",
            input_model=ShowErrorInput,
            result_model=None,
            handler_ref="tools.interaction.show_error",
            allowed_scopes=["draft_editor"],
            requires_artifacts=[],
            requires_active_draft=False,
            side_effects=[],
            emits_events=[],
            cost_tier="cheap",
            description="Emit a frontend-renderable structured error.",
        ),
        ToolSpec(
            name="asset.import_local_file",
            namespace="asset",
            version="1",
            input_model=AssetImportLocalFileInput,
            result_model=None,
            handler_ref="tools.asset.import_local_file",
            allowed_scopes=["draft_editor"],
            requires_artifacts=[],
            requires_active_draft=True,
            side_effects=["asset", "object_store", "job"],
            idempotency_key_fields=["path", "storage_mode"],
            emits_events=["AssetImported", "AssetLinked", "JobEnqueued"],
            exposure="harness_only",
            cost_tier="expensive",
            description="Import a local media file into the active draft, defaulting to reference.",
        ),
        ToolSpec(
            name="asset.import_url",
            namespace="asset",
            version="1",
            input_model=AssetImportUrlInput,
            result_model=None,
            handler_ref="tools.asset.import_url",
            allowed_scopes=["draft_editor"],
            requires_artifacts=[],
            requires_active_draft=True,
            requires_confirmation=True,
            confirmation_decision_type="url_import",
            side_effects=["asset", "object_store", "job"],
            idempotency_key_fields=["url"],
            emits_events=["JobEnqueued", "AssetImported"],
            is_long_running=True,
            cost_tier="expensive",
            description="Import one explicitly confirmed URL and link it to the active draft.",
        ),
        ToolSpec(
            name="asset.list_assets",
            namespace="asset",
            version="1",
            input_model=AssetListAssetsInput,
            result_model=AssetListAssetsResult,
            handler_ref="tools.asset.list_assets",
            allowed_scopes=["draft_editor"],
            requires_artifacts=[],
            requires_active_draft=True,
            side_effects=[],
            emits_events=[],
            cost_tier="free",
            description=(
                "List assets linked to the active draft with optional kind/has_audio/only_usable "
                "filters and keyset pagination (limit + after)."
            ),
        ),
        ToolSpec(
            name="audio.inspect_sources",
            namespace="audio",
            version="1",
            input_model=AudioInspectSourcesInput,
            result_model=None,
            handler_ref="tools.audio.inspect_sources",
            allowed_scopes=["draft_editor"],
            requires_artifacts=["usable_asset_exists"],
            requires_active_draft=True,
            side_effects=["asset"],
            emits_events=["AssetProbed", "CapabilityDegraded"],
            cost_tier="cheap",
            description="Inspect draft assets for audio tracks and local speech/silence segments.",
        ),
        ToolSpec(
            name="audio.asr_original",
            namespace="audio",
            version="1",
            input_model=AudioAsrOriginalInput,
            result_model=None,
            handler_ref="tools.audio.asr_original",
            allowed_scopes=["draft_editor"],
            requires_artifacts=[
                "audio_mode_in(keep_original,rough_cut)",
                "audio_source_has_audio",
            ],
            requires_active_draft=True,
            side_effects=["job"],
            idempotency_key_fields=["asset_id", "provider_id"],
            emits_events=["JobEnqueued"],
            is_long_running=True,
            cost_tier="expensive",
            description="Queue raw-preserving ASR for the selected original audio asset.",
        ),
        ToolSpec(
            name="audio.rough_cut_speech",
            namespace="audio",
            version="1",
            input_model=AudioRoughCutSpeechInput,
            result_model=None,
            handler_ref="tools.audio.rough_cut_speech",
            allowed_scopes=["draft_editor"],
            requires_artifacts=[
                "audio_mode_in(rough_cut)",
                "transcript_with_vad_exists",
            ],
            requires_active_draft=True,
            side_effects=["draft"],
            emits_events=["DecisionCreated", "CapabilityDegraded", "ProviderCallRecorded"],
            cost_tier="expensive",
            description=(
                "Create an approve_speech_cut decision from rule and semantic rough-cut candidates."
            ),
        ),
        ToolSpec(
            name="audio.generate_tts",
            namespace="audio",
            version="1",
            input_model=AudioGenerateTtsInput,
            result_model=None,
            handler_ref="tools.audio.generate_tts",
            allowed_scopes=["draft_editor"],
            requires_artifacts=[
                "audio_mode_in(tts)",
                "content_plan_exists",
            ],
            requires_active_draft=True,
            side_effects=["job"],
            idempotency_key_fields=["provider_id", "asr_provider_id", "voice_type"],
            emits_events=["JobEnqueued"],
            is_long_running=True,
            cost_tier="expensive",
            description=(
                "Queue Volcengine TTS synthesis plus ASR timestamp fallback cut-plan "
                "materialization."
            ),
        ),
        ToolSpec(
            name="audio.align_uploaded_voiceover",
            namespace="audio",
            version="1",
            input_model=AudioAlignUploadedVoiceoverInput,
            result_model=None,
            handler_ref="tools.audio.align_uploaded_voiceover",
            allowed_scopes=["draft_editor"],
            requires_artifacts=[
                "audio_mode_in(uploaded_voiceover)",
                "voiceover_asset_exists",
            ],
            requires_active_draft=True,
            side_effects=["job"],
            idempotency_key_fields=["asset_id", "provider_id", "script_text"],
            emits_events=["JobEnqueued"],
            is_long_running=True,
            cost_tier="expensive",
            description=(
                "Queue uploaded voiceover ASR and local DP alignment against the user script."
            ),
        ),
        ToolSpec(
            name="content.create_plan",
            namespace="content",
            version="1",
            input_model=ContentCreatePlanInput,
            result_model=None,
            handler_ref="tools.content.create_plan",
            allowed_scopes=["draft_editor"],
            requires_artifacts=[],
            requires_active_draft=True,
            side_effects=["draft"],
            emits_events=["ContentPlanUpdated", "CutPlanUpdated"],
            cost_tier="cheap",
            description="Create a content plan and, for silent mode, a visual cut plan.",
        ),
        ToolSpec(
            name="content.revise_plan",
            namespace="content",
            version="1",
            input_model=ContentRevisePlanInput,
            result_model=None,
            handler_ref="tools.content.revise_plan",
            allowed_scopes=["draft_editor"],
            requires_artifacts=[],
            requires_active_draft=True,
            side_effects=["draft"],
            emits_events=["ContentPlanUpdated", "CutPlanUpdated"],
            cost_tier="cheap",
            description="Revise the existing content plan and matching silent-mode cut plan.",
        ),
        ToolSpec(
            name="understand.materials",
            namespace="understand",
            version="1",
            input_model=UnderstandMaterialsInput,
            result_model=UnderstandMaterialsResult,
            handler_ref="tools.understand.materials",
            allowed_scopes=["draft_editor"],
            requires_artifacts=[],
            requires_active_draft=True,
            requires_confirmation=False,
            side_effects=["asset"],
            emits_events=[
                "MaterialUnderstandingStarted",
                "MaterialUnderstandingCompleted",
                "MaterialUnderstandingFailed",
            ],
            cost_tier="expensive",
            cost_note="depth=scan 便宜 / depth=deep 昂贵",
            description=(
                "Scan many visual assets cheaply or deeply inspect selected assets "
                "with timestamped evidence."
            ),
        ),
        ToolSpec(
            name="timeline.compose_initial",
            namespace="timeline",
            version="1",
            input_model=ComposeInitialInput,
            result_model=None,
            handler_ref="tools.timeline_tools.compose_initial",
            allowed_scopes=["draft_editor"],
            requires_artifacts=[
                "active_draft",
                "audio_plan_confirmed",
                "usable_asset_exists",
            ],
            requires_active_draft=True,
            side_effects=["timeline", "draft"],
            emits_events=[
                "TimelineVersionCreated",
                "TimelineValidated",
                "TimelineValidationFailed",
            ],
            cost_tier="cheap",
            description="Assemble timeline v1 directly from summary-level clip selections.",
        ),
        ToolSpec(
            name="timeline.apply_patch",
            namespace="timeline",
            version="1",
            input_model=TimelinePatchRequest,
            result_model=None,
            handler_ref="tools.timeline_tools.apply_patch",
            allowed_scopes=["draft_editor"],
            requires_artifacts=["timeline_exists"],
            requires_active_draft=True,
            side_effects=["timeline", "draft"],
            emits_events=[
                "TimelineVersionCreated",
                "TimelineValidated",
                "TimelineValidationFailed",
                "DecisionCreated",
            ],
            cost_tier="cheap",
            description="Apply a TimelinePatchRequest against the viewed timeline anchor.",
        ),
        ToolSpec(
            name="timeline.validate",
            namespace="timeline",
            version="1",
            input_model=TimelineValidateInput,
            result_model=None,
            handler_ref="tools.timeline_tools.validate",
            allowed_scopes=["draft_editor"],
            requires_artifacts=["timeline_exists"],
            requires_active_draft=True,
            side_effects=["timeline", "draft"],
            emits_events=["TimelineValidated", "TimelineValidationFailed"],
            cost_tier="cheap",
            description="Validate the current timeline version against PRD §10.2 invariants.",
        ),
        ToolSpec(
            name="timeline.inspect",
            namespace="timeline",
            version="1",
            input_model=TimelineInspectInput,
            result_model=None,
            handler_ref="tools.timeline_tools.inspect",
            allowed_scopes=["draft_editor"],
            requires_artifacts=["timeline_exists"],
            requires_active_draft=True,
            side_effects=[],
            emits_events=[],
            cost_tier="free",
            description="Return a prompt-safe summary for the current timeline version.",
        ),
        ToolSpec(
            name="timeline.restore_version",
            namespace="timeline",
            version="1",
            input_model=TimelineRestoreVersionInput,
            result_model=None,
            handler_ref="tools.timeline_tools.restore_version",
            allowed_scopes=["draft_editor"],
            requires_artifacts=["timeline_exists"],
            requires_active_draft=True,
            side_effects=["timeline", "draft"],
            emits_events=["TimelineVersionRestored"],
            cost_tier="cheap",
            description="Restore an old timeline as a new version record.",
        ),
        ToolSpec(
            name="render.preview",
            namespace="render",
            version="1",
            input_model=RenderPreviewInput,
            result_model=None,
            handler_ref="tools.render_tools.preview",
            allowed_scopes=["draft_editor"],
            requires_artifacts=["timeline_validated"],
            requires_active_draft=True,
            side_effects=["job", "object_store"],
            idempotency_key_fields=["timeline_version"],
            emits_events=["JobEnqueued"],
            is_long_running=True,
            cost_tier="expensive",
            description="Queue a cached preview render for the current validated timeline.",
        ),
        ToolSpec(
            name="render.inspect_preview",
            namespace="render",
            version="1",
            input_model=RenderInspectPreviewInput,
            result_model=PreviewInspectionResult,
            handler_ref="tools.render_tools.inspect_preview",
            allowed_scopes=["draft_editor"],
            requires_artifacts=["any_preview_exists"],
            requires_active_draft=True,
            side_effects=[],
            emits_events=[],
            cost_tier="expensive",
            description=(
                "Inspect a rendered preview's pixels, audio, streams, and advisory visual quality."
            ),
        ),
        ToolSpec(
            name="render.final_mp4",
            namespace="render",
            version="1",
            input_model=RenderFinalMp4Input,
            result_model=None,
            handler_ref="tools.render_tools.final_mp4",
            allowed_scopes=["draft_editor"],
            requires_artifacts=["timeline_validated", "preview_for_current_version_exists"],
            requires_active_draft=True,
            requires_confirmation=True,
            confirmation_decision_type="export",
            side_effects=["job", "object_store"],
            idempotency_key_fields=["timeline_version"],
            emits_events=["JobEnqueued"],
            is_long_running=True,
            cost_tier="expensive",
            description="Queue a confirmed final MP4 export for the current validated timeline.",
        ),
        ToolSpec(
            name="render.status",
            namespace="render",
            version="1",
            input_model=RenderStatusInput,
            result_model=None,
            handler_ref="tools.render_tools.status",
            allowed_scopes=["draft_editor"],
            requires_artifacts=["timeline_exists"],
            requires_active_draft=True,
            side_effects=[],
            emits_events=[],
            cost_tier="free",
            description="Read current preview/export artifacts and running render jobs.",
        ),
        ToolSpec(
            name="memory.extract_from_draft",
            namespace="memory",
            version="1",
            input_model=MemoryExtractFromDraftInput,
            result_model=None,
            handler_ref="tools.memory_tools.extract_from_draft",
            allowed_scopes=["draft_editor"],
            requires_artifacts=[],
            requires_active_draft=True,
            side_effects=["memory"],
            emits_events=[
                "MemoryCandidateExtracted",
                "CapabilityDegraded",
                "ProviderCallRecorded",
            ],
            cost_tier="expensive",
            description="Extract one pending long-term memory candidate from the current draft.",
        ),
        ToolSpec(
            name="memory.ask_scope",
            namespace="memory",
            version="1",
            input_model=MemoryAskScopeInput,
            result_model=None,
            handler_ref="tools.memory_tools.ask_scope",
            allowed_scopes=["draft_editor"],
            requires_artifacts=[],
            requires_active_draft=True,
            side_effects=["draft"],
            idempotency_key_fields=["candidate_id"],
            emits_events=["DecisionCreated"],
            cost_tier="cheap",
            description="Ask the user whether to save a memory candidate as a user memory.",
        ),
        ToolSpec(
            name="memory.save",
            namespace="memory",
            version="1",
            input_model=MemorySaveInput,
            result_model=None,
            handler_ref="tools.memory_tools.save",
            allowed_scopes=["draft_editor"],
            requires_artifacts=[],
            requires_active_draft=True,
            side_effects=["memory"],
            idempotency_key_fields=["candidate_id"],
            emits_events=["MemorySaved"],
            exposure="harness_only",
            cost_tier="cheap",
            description="Persist an approved memory candidate after memory_scope is answered.",
        ),
        ToolSpec(
            name="memory.search_relevant",
            namespace="memory",
            version="1",
            input_model=MemorySearchRelevantInput,
            result_model=None,
            handler_ref="tools.memory_tools.search_relevant",
            allowed_scopes=["draft_editor"],
            requires_artifacts=[],
            requires_active_draft=True,
            side_effects=[],
            emits_events=[],
            cost_tier="free",
            description="Search user memories for relevant summaries.",
        ),
    )


def patch_op_registry() -> PatchOpRegistry:
    return PatchOpRegistry(
        (
            PatchOpSpec(
                kind="delete_range",
                params_model=DeleteRangeOp,
                ripple_semantics="ripple",
                description="Delete a time range from one or more tracks.",
            ),
            PatchOpSpec(
                kind="replace_clip",
                params_model=ReplaceClipOp,
                ripple_semantics="in_place",
                description="Replace a timeline clip's source with another asset span.",
            ),
            PatchOpSpec(
                kind="reorder_blocks",
                params_model=ReorderBlocksOp,
                ripple_semantics="ripple",
                description="Reorder semantic timeline blocks.",
            ),
            PatchOpSpec(
                kind="trim_clip",
                params_model=TrimClipOp,
                ripple_semantics="ripple",
                description="Trim a clip head or tail by user-referenced seconds.",
            ),
            PatchOpSpec(
                kind="insert_clip",
                params_model=InsertClipOp,
                ripple_semantics="ripple",
                description="Insert an asset span at a user-referenced position.",
            ),
            PatchOpSpec(
                kind="generate_subtitles",
                params_model=GenerateSubtitlesOp,
                requires_confirmation=True,
                confirmation_decision_type="subtitle",
                requires_artifacts=["rough_cut_approved"],
                ripple_semantics="in_place",
                description="Generate subtitle clips from speech timestamps.",
            ),
            PatchOpSpec(
                kind="set_subtitle_style",
                params_model=SetSubtitleStyleOp,
                ripple_semantics="in_place",
                description="Change subtitle styling.",
            ),
            PatchOpSpec(
                kind="edit_subtitle_text",
                params_model=EditSubtitleTextOp,
                ripple_semantics="in_place",
                description="Edit an existing subtitle clip's text.",
            ),
            PatchOpSpec(
                kind="remove_track_clips",
                params_model=RemoveTrackClipsOp,
                ripple_semantics="ripple",
                description="Remove clips from a track.",
            ),
            PatchOpSpec(
                kind="add_bgm",
                params_model=AddBgmOp,
                requires_confirmation=True,
                confirmation_decision_type="bgm",
                requires_artifacts=["rough_cut_approved"],
                ripple_semantics="in_place",
                description="Add a BGM track after BGM confirmation.",
            ),
            PatchOpSpec(
                kind="adjust_gain",
                params_model=AdjustGainOp,
                ripple_semantics="in_place",
                description="Adjust audio track gain.",
            ),
            PatchOpSpec(
                kind="set_playback_rate",
                params_model=SetPlaybackRateOp,
                ripple_semantics="ripple",
                description="Set clip playback rate.",
            ),
        )
    )


PATCH_OP_REGISTRY = patch_op_registry()


def build_default_tool_registry() -> ToolRegistry:
    from .asset import import_local_file, import_url, list_assets
    from .audio import align_uploaded_voiceover as audio_align_uploaded_voiceover
    from .audio import asr_original as audio_asr_original
    from .audio import generate_tts as audio_generate_tts
    from .audio import inspect_sources as audio_inspect_sources
    from .audio import rough_cut_speech as audio_rough_cut_speech
    from .builtin import decision_answer
    from .content import create_plan as content_create_plan
    from .content import revise_plan as content_revise_plan
    from .interaction import (
        ask_user,
        confirm_action,
        show_error,
        show_preview,
        show_progress,
        show_timeline,
    )
    from .memory_tools import (
        ask_scope as memory_ask_scope,
    )
    from .memory_tools import (
        extract_from_draft as memory_extract_from_draft,
    )
    from .memory_tools import (
        save as memory_save,
    )
    from .memory_tools import (
        search_relevant as memory_search_relevant,
    )
    from .render_tools import final_mp4 as render_final_mp4
    from .render_tools import inspect_preview as render_inspect_preview
    from .render_tools import preview as render_preview
    from .render_tools import status as render_status
    from .timeline_tools import apply_patch as timeline_apply_patch
    from .timeline_tools import compose_initial as timeline_compose_initial
    from .timeline_tools import inspect as timeline_inspect
    from .timeline_tools import restore_version as timeline_restore_version
    from .timeline_tools import validate as timeline_validate
    from .understand import materials as understand_materials

    handlers: dict[str, ToolHandler] = {
        "decision.answer": decision_answer,
        "interaction.ask_user": ask_user,
        "interaction.confirm_action": confirm_action,
        "interaction.show_progress": show_progress,
        "interaction.show_preview": show_preview,
        "interaction.show_timeline": show_timeline,
        "interaction.show_error": show_error,
        "asset.import_local_file": import_local_file,
        "asset.import_url": import_url,
        "asset.list_assets": list_assets,
        "audio.inspect_sources": audio_inspect_sources,
        "audio.asr_original": audio_asr_original,
        "audio.rough_cut_speech": audio_rough_cut_speech,
        "audio.generate_tts": audio_generate_tts,
        "audio.align_uploaded_voiceover": audio_align_uploaded_voiceover,
        "content.create_plan": content_create_plan,
        "content.revise_plan": content_revise_plan,
        "understand.materials": understand_materials,
        "timeline.compose_initial": timeline_compose_initial,
        "timeline.apply_patch": timeline_apply_patch,
        "timeline.validate": timeline_validate,
        "timeline.inspect": timeline_inspect,
        "timeline.restore_version": timeline_restore_version,
        "render.preview": render_preview,
        "render.inspect_preview": render_inspect_preview,
        "render.final_mp4": render_final_mp4,
        "render.status": render_status,
        "memory.extract_from_draft": memory_extract_from_draft,
        "memory.ask_scope": memory_ask_scope,
        "memory.save": memory_save,
        "memory.search_relevant": memory_search_relevant,
    }
    registry = ToolRegistry()
    for spec in tool_specs():
        registry.register(spec, handlers[spec.name])
    return registry
