"""ToolSpec declarations for the Rushes tool registry."""

from __future__ import annotations

from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field

from contracts.asset import AssetKind, StorageMode
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
    InsertCandidateOp,
    RemoveTrackClipsOp,
    ReorderBlocksOp,
    ReplaceClipOp,
    SetPlaybackRateOp,
    SetSubtitleStyleOp,
    TrimClipOp,
)
from contracts.tool import PatchOpSpec, ToolSpec

from .registry import PatchOpRegistry, ToolHandler, ToolRegistry


class RespondInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    message: str
    message_id: str | None = None


class RefuseInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    reason: str
    message: str | None = None
    message_id: str | None = None


class FinishTurnInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    reason: str | None = None


class DecisionAnswerInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    decision_id: str
    answer: DecisionAnswer


class AskUserInput(BaseModel):
    model_config = ConfigDict(extra="forbid", populate_by_name=True)

    question: str
    scope_type: DecisionScopeType = "case"
    project_id: str | None = None
    case_id: str | None = None
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
    scope_type: DecisionScopeType = "case"
    project_id: str | None = None
    case_id: str | None = None
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


class ProjectCreateInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    project_id: str | None = None
    name: str = "Untitled Project"
    defaults: dict[str, Any] = Field(default_factory=dict)


class ProjectRenameInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    project_id: str | None = None
    name: str


class ProjectDeleteInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    project_id: str | None = None


class ProjectCopyInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    source_project_id: str | None = None
    project_id: str | None = None
    name: str | None = None


class ProjectCreateCaseInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    project_id: str | None = None
    case_id: str | None = None
    name: str = "Untitled Case"
    goal: str | None = None
    brief: dict[str, Any] = Field(default_factory=dict)


class ProjectMoveCaseInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    case_id: str | None = None
    target_project_id: str


class ProjectCloseCaseInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    case_id: str | None = None


class ProjectListTreeInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    include_trashed: bool = True


class AssetUploadCompleteInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    project_id: str | None = None
    asset_id: str | None = None
    path: str
    filename: str | None = None
    kind: AssetKind = AssetKind.VIDEO


class AssetImportLocalFileInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    project_id: str | None = None
    asset_id: str | None = None
    path: str
    storage_mode: StorageMode = StorageMode.REFERENCE
    kind: AssetKind = AssetKind.VIDEO


class AssetImportUrlInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    project_id: str | None = None
    asset_id: str | None = None
    url: str
    filename: str | None = None
    kind: AssetKind = AssetKind.VIDEO
    max_bytes: int | None = None


class AssetLinkInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    project_id: str | None = None
    asset_id: str
    enabled: bool = True
    note: str = ""


class AssetUnlinkInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    project_id: str | None = None
    asset_id: str


class AssetSelectForCaseInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    case_id: str | None = None
    asset_id: str


class AssetDisableForCaseInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    case_id: str | None = None
    asset_id: str


class AssetListProjectInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    project_id: str | None = None
    include_disabled: bool = True


class AssetListCaseScopeInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    case_id: str | None = None


class AnnotationEnqueueInput(BaseModel):
    model_config = ConfigDict(extra="forbid", populate_by_name=True)

    asset_id: str
    pass_: Literal["cheap", "deep"] = Field(default="cheap", alias="pass")
    project_id: str | None = None


class AnnotationStatusInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    project_id: str | None = None


class AnnotationRetryInput(BaseModel):
    model_config = ConfigDict(extra="forbid", populate_by_name=True)

    asset_id: str
    pass_: Literal["cheap", "deep"] | None = Field(default=None, alias="pass")
    project_id: str | None = None


class AnnotationInspectInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    asset_id: str
    project_id: str | None = None


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


def tool_specs() -> tuple[ToolSpec, ...]:
    return (
        ToolSpec(
            name="respond",
            namespace="builtin",
            version="1",
            input_model=RespondInput,
            result_model=None,
            handler_ref="tools.builtin.respond",
            allowed_scopes=["case_agent_console"],
            requires_artifacts=[],
            requires_active_project=False,
            requires_active_case=False,
            side_effects=[],
            emits_events=["TurnEnded"],
            description="Append an assistant message and end the current turn.",
        ),
        ToolSpec(
            name="refuse",
            namespace="builtin",
            version="1",
            input_model=RefuseInput,
            result_model=None,
            handler_ref="tools.builtin.refuse",
            allowed_scopes=["case_agent_console"],
            requires_artifacts=[],
            requires_active_project=False,
            requires_active_case=False,
            side_effects=[],
            emits_events=["TurnEnded"],
            description="Return an out-of-scope refusal and end the current turn.",
        ),
        ToolSpec(
            name="finish_turn",
            namespace="builtin",
            version="1",
            input_model=FinishTurnInput,
            result_model=None,
            handler_ref="tools.builtin.finish_turn",
            allowed_scopes=["case_agent_console"],
            requires_artifacts=[],
            requires_active_project=False,
            requires_active_case=False,
            side_effects=[],
            emits_events=["TurnEnded"],
            description="End the current turn without adding an assistant message.",
        ),
        ToolSpec(
            name="decision.answer",
            namespace="decision",
            version="1",
            input_model=DecisionAnswerInput,
            result_model=None,
            handler_ref="tools.builtin.decision_answer",
            allowed_scopes=["case_agent_console"],
            requires_artifacts=[],
            requires_active_project=False,
            requires_active_case=False,
            side_effects=["case"],
            emits_events=["DecisionAnswered"],
            description="Convert a structured user answer into a DecisionAnswered event.",
        ),
        ToolSpec(
            name="interaction.ask_user",
            namespace="interaction",
            version="1",
            input_model=AskUserInput,
            result_model=None,
            handler_ref="tools.interaction.ask_user",
            allowed_scopes=["case_agent_console"],
            requires_artifacts=[],
            requires_active_project=False,
            requires_active_case=False,
            side_effects=["case"],
            emits_events=["DecisionCreated"],
            description="Ask an open or multiple-choice question through a Decision.",
        ),
        ToolSpec(
            name="interaction.confirm_action",
            namespace="interaction",
            version="1",
            input_model=ConfirmActionInput,
            result_model=None,
            handler_ref="tools.interaction.confirm_action",
            allowed_scopes=["case_agent_console"],
            requires_artifacts=[],
            requires_active_project=False,
            requires_active_case=False,
            side_effects=["case"],
            emits_events=["DecisionCreated"],
            description="Create a confirmation Decision for an action.",
        ),
        ToolSpec(
            name="interaction.show_progress",
            namespace="interaction",
            version="1",
            input_model=ShowProgressInput,
            result_model=None,
            handler_ref="tools.interaction.show_progress",
            allowed_scopes=["case_agent_console"],
            requires_artifacts=[],
            requires_active_project=False,
            requires_active_case=False,
            side_effects=[],
            emits_events=[],
            description="Emit a frontend-renderable progress interaction.",
        ),
        ToolSpec(
            name="interaction.show_preview",
            namespace="interaction",
            version="1",
            input_model=ShowPreviewInput,
            result_model=None,
            handler_ref="tools.interaction.show_preview",
            allowed_scopes=["case_agent_console"],
            requires_artifacts=[],
            requires_active_project=False,
            requires_active_case=False,
            side_effects=[],
            emits_events=[],
            description="Emit a frontend-renderable preview interaction.",
        ),
        ToolSpec(
            name="interaction.show_timeline",
            namespace="interaction",
            version="1",
            input_model=ShowTimelineInput,
            result_model=None,
            handler_ref="tools.interaction.show_timeline",
            allowed_scopes=["case_agent_console"],
            requires_artifacts=[],
            requires_active_project=False,
            requires_active_case=False,
            side_effects=[],
            emits_events=[],
            description="Emit a frontend-renderable timeline summary.",
        ),
        ToolSpec(
            name="interaction.show_error",
            namespace="interaction",
            version="1",
            input_model=ShowErrorInput,
            result_model=None,
            handler_ref="tools.interaction.show_error",
            allowed_scopes=["case_agent_console"],
            requires_artifacts=[],
            requires_active_project=False,
            requires_active_case=False,
            side_effects=[],
            emits_events=[],
            description="Emit a frontend-renderable structured error.",
        ),
        ToolSpec(
            name="project.create",
            namespace="project",
            version="1",
            input_model=ProjectCreateInput,
            result_model=None,
            handler_ref="tools.project.create",
            allowed_scopes=["case_agent_console", "project_page"],
            requires_artifacts=[],
            requires_active_project=False,
            requires_active_case=False,
            side_effects=["project"],
            idempotency_key_fields=["project_id"],
            emits_events=["ProjectCreated"],
            description="Create a new Project.",
        ),
        ToolSpec(
            name="project.rename",
            namespace="project",
            version="1",
            input_model=ProjectRenameInput,
            result_model=None,
            handler_ref="tools.project.rename",
            allowed_scopes=["case_agent_console", "project_page"],
            requires_artifacts=[],
            requires_active_project=True,
            requires_active_case=False,
            side_effects=["project"],
            idempotency_key_fields=["project_id", "name"],
            emits_events=["ProjectRenamed"],
            description="Rename the active Project.",
        ),
        ToolSpec(
            name="project.delete",
            namespace="project",
            version="1",
            input_model=ProjectDeleteInput,
            result_model=None,
            handler_ref="tools.project.delete",
            allowed_scopes=["case_agent_console", "project_page"],
            requires_artifacts=[],
            requires_active_project=True,
            requires_active_case=False,
            requires_confirmation=True,
            confirmation_decision_type="destructive_project_action",
            side_effects=["project"],
            idempotency_key_fields=["project_id"],
            emits_events=["ProjectTrashed"],
            description="Soft-delete the active Project after confirmation.",
        ),
        ToolSpec(
            name="project.copy",
            namespace="project",
            version="1",
            input_model=ProjectCopyInput,
            result_model=None,
            handler_ref="tools.project.copy",
            allowed_scopes=["case_agent_console", "project_page"],
            requires_artifacts=[],
            requires_active_project=True,
            requires_active_case=False,
            side_effects=["project"],
            idempotency_key_fields=["source_project_id", "project_id"],
            emits_events=["ProjectCopied"],
            description="Copy the active Project and its asset links without copying Cases.",
        ),
        ToolSpec(
            name="project.create_case",
            namespace="project",
            version="1",
            input_model=ProjectCreateCaseInput,
            result_model=None,
            handler_ref="tools.project.create_case",
            allowed_scopes=["case_agent_console", "project_page"],
            requires_artifacts=[],
            requires_active_project=True,
            requires_active_case=False,
            side_effects=["project", "case"],
            idempotency_key_fields=["project_id", "case_id"],
            emits_events=["CaseCreated"],
            description="Create a Case in the active Project.",
        ),
        ToolSpec(
            name="project.move_case",
            namespace="project",
            version="1",
            input_model=ProjectMoveCaseInput,
            result_model=None,
            handler_ref="tools.project.move_case",
            allowed_scopes=["case_agent_console", "project_page"],
            requires_artifacts=[],
            requires_active_project=True,
            requires_active_case=True,
            requires_confirmation=True,
            confirmation_decision_type="destructive_project_action",
            side_effects=["project", "case", "asset"],
            idempotency_key_fields=["case_id", "target_project_id"],
            emits_events=["CaseMoved", "AssetLinked"],
            description="Move the active Case to another Project after confirmation.",
        ),
        ToolSpec(
            name="project.close_case",
            namespace="project",
            version="1",
            input_model=ProjectCloseCaseInput,
            result_model=None,
            handler_ref="tools.project.close_case",
            allowed_scopes=["case_agent_console", "project_page"],
            requires_artifacts=[],
            requires_active_project=True,
            requires_active_case=True,
            side_effects=["case"],
            idempotency_key_fields=["case_id"],
            emits_events=["CaseClosed"],
            description="Close the active Case without deleting it.",
        ),
        ToolSpec(
            name="project.list_tree",
            namespace="project",
            version="1",
            input_model=ProjectListTreeInput,
            result_model=None,
            handler_ref="tools.project.list_tree",
            allowed_scopes=["case_agent_console", "project_page"],
            requires_artifacts=[],
            requires_active_project=False,
            requires_active_case=False,
            side_effects=[],
            emits_events=[],
            description="Return the Project/Case two-level tree.",
        ),
        ToolSpec(
            name="asset.upload_complete",
            namespace="asset",
            version="1",
            input_model=AssetUploadCompleteInput,
            result_model=None,
            handler_ref="tools.asset.upload_complete",
            allowed_scopes=["project_page"],
            requires_artifacts=[],
            requires_active_project=True,
            requires_active_case=False,
            side_effects=["asset", "object_store", "job"],
            idempotency_key_fields=["project_id", "path"],
            emits_events=["AssetImported", "AssetLinked", "JobEnqueued"],
            exposure="harness_only",
            description="Complete an uploaded file into a copy-mode asset and enqueue probe/proxy.",
        ),
        ToolSpec(
            name="asset.import_local_file",
            namespace="asset",
            version="1",
            input_model=AssetImportLocalFileInput,
            result_model=None,
            handler_ref="tools.asset.import_local_file",
            allowed_scopes=["project_page"],
            requires_artifacts=[],
            requires_active_project=True,
            requires_active_case=False,
            side_effects=["asset", "object_store", "job"],
            idempotency_key_fields=["project_id", "path", "storage_mode"],
            emits_events=["AssetImported", "AssetLinked", "JobEnqueued"],
            exposure="harness_only",
            description="Import a local media file, defaulting to reference storage.",
        ),
        ToolSpec(
            name="asset.import_url",
            namespace="asset",
            version="1",
            input_model=AssetImportUrlInput,
            result_model=None,
            handler_ref="tools.asset.import_url",
            allowed_scopes=["case_agent_console", "project_page"],
            requires_artifacts=[],
            requires_active_project=True,
            requires_active_case=False,
            requires_confirmation=True,
            confirmation_decision_type="url_import",
            side_effects=["asset", "object_store", "job"],
            idempotency_key_fields=["project_id", "url"],
            emits_events=["JobEnqueued", "AssetImported"],
            is_long_running=True,
            description="Import one explicitly confirmed URL as a project-level job.",
        ),
        ToolSpec(
            name="asset.link_to_project",
            namespace="asset",
            version="1",
            input_model=AssetLinkInput,
            result_model=None,
            handler_ref="tools.asset.link_to_project",
            allowed_scopes=["case_agent_console", "project_page"],
            requires_artifacts=[],
            requires_active_project=True,
            requires_active_case=False,
            side_effects=["asset"],
            idempotency_key_fields=["project_id", "asset_id"],
            emits_events=["AssetLinked"],
            description="Link an existing asset into the active Project.",
        ),
        ToolSpec(
            name="asset.unlink_from_project",
            namespace="asset",
            version="1",
            input_model=AssetUnlinkInput,
            result_model=None,
            handler_ref="tools.asset.unlink_from_project",
            allowed_scopes=["case_agent_console", "project_page"],
            requires_artifacts=[],
            requires_active_project=True,
            requires_active_case=False,
            side_effects=["asset"],
            idempotency_key_fields=["project_id", "asset_id"],
            emits_events=["AssetUnlinked"],
            description="Unlink an asset from the active Project.",
        ),
        ToolSpec(
            name="asset.select_for_case",
            namespace="asset",
            version="1",
            input_model=AssetSelectForCaseInput,
            result_model=None,
            handler_ref="tools.asset.select_for_case",
            allowed_scopes=["case_agent_console"],
            requires_artifacts=[],
            requires_active_project=True,
            requires_active_case=True,
            side_effects=["case"],
            idempotency_key_fields=["case_id", "asset_id"],
            emits_events=["CaseAssetScopeChanged"],
            description=(
                "Select a project asset for the active Case without mutating the asset pool."
            ),
        ),
        ToolSpec(
            name="asset.disable_for_case",
            namespace="asset",
            version="1",
            input_model=AssetDisableForCaseInput,
            result_model=None,
            handler_ref="tools.asset.disable_for_case",
            allowed_scopes=["case_agent_console"],
            requires_artifacts=[],
            requires_active_project=True,
            requires_active_case=True,
            side_effects=["case"],
            idempotency_key_fields=["case_id", "asset_id"],
            emits_events=["CaseAssetScopeChanged"],
            description=(
                "Disable a project asset for the active Case without mutating the asset pool."
            ),
        ),
        ToolSpec(
            name="asset.list_project_assets",
            namespace="asset",
            version="1",
            input_model=AssetListProjectInput,
            result_model=None,
            handler_ref="tools.asset.list_project_assets",
            allowed_scopes=["case_agent_console", "project_page"],
            requires_artifacts=[],
            requires_active_project=True,
            requires_active_case=False,
            side_effects=[],
            emits_events=[],
            description="List assets linked to the active Project.",
        ),
        ToolSpec(
            name="asset.list_case_scope",
            namespace="asset",
            version="1",
            input_model=AssetListCaseScopeInput,
            result_model=None,
            handler_ref="tools.asset.list_case_scope",
            allowed_scopes=["case_agent_console"],
            requires_artifacts=[],
            requires_active_project=True,
            requires_active_case=True,
            side_effects=[],
            emits_events=[],
            description="List selected and disabled asset IDs for the active Case.",
        ),
        ToolSpec(
            name="audio.inspect_sources",
            namespace="audio",
            version="1",
            input_model=AudioInspectSourcesInput,
            result_model=None,
            handler_ref="tools.audio.inspect_sources",
            allowed_scopes=["case_agent_console"],
            requires_artifacts=["usable_asset_exists"],
            requires_active_project=True,
            requires_active_case=True,
            side_effects=["asset"],
            emits_events=["AssetProbed", "CapabilityDegraded"],
            description="Inspect case assets for audio tracks and local speech/silence segments.",
        ),
        ToolSpec(
            name="audio.asr_original",
            namespace="audio",
            version="1",
            input_model=AudioAsrOriginalInput,
            result_model=None,
            handler_ref="tools.audio.asr_original",
            allowed_scopes=["case_agent_console"],
            requires_artifacts=[
                "audio_mode_in(keep_original,rough_cut)",
                "audio_source_has_audio",
            ],
            requires_active_project=True,
            requires_active_case=True,
            side_effects=["job"],
            idempotency_key_fields=["asset_id", "provider_id"],
            emits_events=["JobEnqueued"],
            is_long_running=True,
            description="Queue raw-preserving ASR for the selected original audio asset.",
        ),
        ToolSpec(
            name="audio.rough_cut_speech",
            namespace="audio",
            version="1",
            input_model=AudioRoughCutSpeechInput,
            result_model=None,
            handler_ref="tools.audio.rough_cut_speech",
            allowed_scopes=["case_agent_console"],
            requires_artifacts=[
                "audio_mode_in(rough_cut)",
                "transcript_with_vad_exists",
            ],
            requires_active_project=True,
            requires_active_case=True,
            side_effects=["case"],
            emits_events=["DecisionCreated", "CapabilityDegraded", "ProviderCallRecorded"],
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
            allowed_scopes=["case_agent_console"],
            requires_artifacts=[
                "audio_mode_in(tts)",
                "content_plan_exists",
            ],
            requires_active_project=True,
            requires_active_case=True,
            side_effects=["job"],
            idempotency_key_fields=["provider_id", "asr_provider_id", "voice_type"],
            emits_events=["JobEnqueued"],
            is_long_running=True,
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
            allowed_scopes=["case_agent_console"],
            requires_artifacts=[
                "audio_mode_in(uploaded_voiceover)",
                "voiceover_asset_exists",
            ],
            requires_active_project=True,
            requires_active_case=True,
            side_effects=["job"],
            idempotency_key_fields=["asset_id", "provider_id", "script_text"],
            emits_events=["JobEnqueued"],
            is_long_running=True,
            description=(
                "Queue uploaded voiceover ASR and local DP alignment against the user script."
            ),
        ),
        ToolSpec(
            name="annotation.enqueue",
            namespace="annotation",
            version="1",
            input_model=AnnotationEnqueueInput,
            result_model=None,
            handler_ref="tools.annotation.enqueue",
            allowed_scopes=["case_agent_console", "project_page"],
            requires_artifacts=[],
            requires_active_project=True,
            requires_active_case=False,
            side_effects=["job"],
            idempotency_key_fields=["project_id", "asset_id", "pass"],
            emits_events=["JobEnqueued"],
            is_long_running=True,
            description="Queue cheap or deep annotation for a project asset.",
        ),
        ToolSpec(
            name="annotation.status",
            namespace="annotation",
            version="1",
            input_model=AnnotationStatusInput,
            result_model=None,
            handler_ref="tools.annotation.status",
            allowed_scopes=["case_agent_console", "project_page"],
            requires_artifacts=[],
            requires_active_project=True,
            requires_active_case=False,
            side_effects=[],
            emits_events=[],
            description="Read annotation status for assets in the active Project.",
        ),
        ToolSpec(
            name="annotation.retry",
            namespace="annotation",
            version="1",
            input_model=AnnotationRetryInput,
            result_model=None,
            handler_ref="tools.annotation.retry",
            allowed_scopes=["case_agent_console", "project_page"],
            requires_artifacts=[],
            requires_active_project=True,
            requires_active_case=False,
            side_effects=["job"],
            idempotency_key_fields=["project_id", "asset_id", "pass"],
            emits_events=["JobEnqueued"],
            is_long_running=True,
            description="Requeue a failed annotation job.",
        ),
        ToolSpec(
            name="annotation.inspect",
            namespace="annotation",
            version="1",
            input_model=AnnotationInspectInput,
            result_model=None,
            handler_ref="tools.annotation.inspect",
            allowed_scopes=["case_agent_console", "project_page"],
            requires_artifacts=[],
            requires_active_project=True,
            requires_active_case=False,
            side_effects=[],
            emits_events=[],
            description="Inspect usable spans, failure details, and quality events for an asset.",
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
                description="Replace an existing timeline clip with a candidate.",
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
                kind="insert_candidate",
                params_model=InsertCandidateOp,
                ripple_semantics="ripple",
                description="Insert a candidate at a user-referenced position.",
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
    from .annotation import (
        enqueue as annotation_enqueue,
    )
    from .annotation import (
        inspect as annotation_inspect,
    )
    from .annotation import (
        retry as annotation_retry,
    )
    from .annotation import (
        status as annotation_status,
    )
    from .asset import (
        disable_for_case,
        import_local_file,
        import_url,
        link_to_project,
        list_case_scope,
        list_project_assets,
        select_for_case,
        unlink_from_project,
        upload_complete,
    )
    from .audio import align_uploaded_voiceover as audio_align_uploaded_voiceover
    from .audio import asr_original as audio_asr_original
    from .audio import generate_tts as audio_generate_tts
    from .audio import inspect_sources as audio_inspect_sources
    from .audio import rough_cut_speech as audio_rough_cut_speech
    from .builtin import decision_answer, finish_turn, refuse, respond
    from .interaction import (
        ask_user,
        confirm_action,
        show_error,
        show_preview,
        show_progress,
        show_timeline,
    )
    from .project import (
        close_case,
        copy,
        create,
        create_case,
        delete,
        list_tree,
        move_case,
        rename,
    )

    handlers: dict[str, ToolHandler] = {
        "respond": respond,
        "refuse": refuse,
        "finish_turn": finish_turn,
        "decision.answer": decision_answer,
        "interaction.ask_user": ask_user,
        "interaction.confirm_action": confirm_action,
        "interaction.show_progress": show_progress,
        "interaction.show_preview": show_preview,
        "interaction.show_timeline": show_timeline,
        "interaction.show_error": show_error,
        "project.create": create,
        "project.rename": rename,
        "project.delete": delete,
        "project.copy": copy,
        "project.create_case": create_case,
        "project.move_case": move_case,
        "project.close_case": close_case,
        "project.list_tree": list_tree,
        "asset.upload_complete": upload_complete,
        "asset.import_local_file": import_local_file,
        "asset.import_url": import_url,
        "asset.link_to_project": link_to_project,
        "asset.unlink_from_project": unlink_from_project,
        "asset.select_for_case": select_for_case,
        "asset.disable_for_case": disable_for_case,
        "asset.list_project_assets": list_project_assets,
        "asset.list_case_scope": list_case_scope,
        "audio.inspect_sources": audio_inspect_sources,
        "audio.asr_original": audio_asr_original,
        "audio.rough_cut_speech": audio_rough_cut_speech,
        "audio.generate_tts": audio_generate_tts,
        "audio.align_uploaded_voiceover": audio_align_uploaded_voiceover,
        "annotation.enqueue": annotation_enqueue,
        "annotation.status": annotation_status,
        "annotation.retry": annotation_retry,
        "annotation.inspect": annotation_inspect,
    }
    registry = ToolRegistry()
    for spec in tool_specs():
        registry.register(spec, handlers[spec.name])
    return registry
