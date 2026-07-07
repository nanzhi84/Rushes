"""Named artifact preconditions shared by PolicyGate and ContextBuilder."""

from __future__ import annotations

from collections.abc import Callable, Iterable, Sequence

from pydantic import BaseModel, ConfigDict, Field

from contracts.case import AudioMode, CaseState
from contracts.project import ProjectState

PreconditionFn = Callable[["PreconditionContext"], bool]


class UnknownPreconditionError(ValueError):
    """Raised when a ToolSpec references an unknown precondition name."""


class ProjectArtifactStats(BaseModel):
    """Lightweight project/case artifact facts needed by precondition predicates."""

    model_config = ConfigDict(extra="forbid")

    usable_asset_count: int = 0
    usable_asset_ids: frozenset[str] = Field(default_factory=frozenset)
    asset_ids_with_audio: frozenset[str] = Field(default_factory=frozenset)
    transcript_asset_ids: frozenset[str] = Field(default_factory=frozenset)
    transcript_with_vad_asset_ids: frozenset[str] = Field(default_factory=frozenset)
    transcript_ids: frozenset[str] = Field(default_factory=frozenset)
    transcript_ids_with_vad: frozenset[str] = Field(default_factory=frozenset)
    voiceover_asset_ids: frozenset[str] = Field(default_factory=frozenset)


class ProjectAudioAsset(BaseModel):
    """Small immutable view of project audio assets for Human Gate options."""

    model_config = ConfigDict(extra="forbid", frozen=True)

    asset_id: str
    filename: str


class PreconditionContext(BaseModel):
    """Pure input model for all precondition predicates."""

    model_config = ConfigDict(extra="forbid", arbitrary_types_allowed=True)

    case_state: CaseState | None = None
    project_state: ProjectState | None = None
    project_artifacts: ProjectArtifactStats = Field(default_factory=ProjectArtifactStats)
    project_audio_assets: tuple[ProjectAudioAsset, ...] = Field(default_factory=tuple)


def active_case(context: PreconditionContext) -> bool:
    return context.case_state is not None and context.case_state.status == "active"


def usable_asset_exists(context: PreconditionContext) -> bool:
    stats = context.project_artifacts
    case_state = context.case_state
    if stats.usable_asset_ids:
        disabled = (
            frozenset(case_state.disabled_asset_ids) if case_state is not None else frozenset()
        )
        return bool(stats.usable_asset_ids - disabled)
    return stats.usable_asset_count > 0


def audio_plan_confirmed(context: PreconditionContext) -> bool:
    return context.case_state is not None and context.case_state.audio_plan is not None


def audio_mode_in(*modes: str | AudioMode) -> PreconditionFn:
    """Build a predicate checking CaseState.audio_plan.mode against allowed modes."""

    allowed = frozenset(str(mode) for mode in modes)

    def predicate(context: PreconditionContext) -> bool:
        case_state = context.case_state
        if case_state is None or case_state.audio_plan is None:
            return False
        return str(case_state.audio_plan.mode) in allowed

    return predicate


def audio_source_has_audio(context: PreconditionContext) -> bool:
    case_state = context.case_state
    if case_state is None or case_state.audio_plan is None:
        return False
    stats = context.project_artifacts
    source_ids = frozenset(case_state.audio_plan.source_asset_ids)
    if source_ids:
        return bool(source_ids & stats.asset_ids_with_audio)
    selected_ids = frozenset(case_state.selected_asset_ids)
    if selected_ids:
        return bool(selected_ids & stats.asset_ids_with_audio)
    return bool(stats.asset_ids_with_audio)


def transcript_with_vad_exists(context: PreconditionContext) -> bool:
    case_state = context.case_state
    if case_state is None or case_state.audio_plan is None:
        return False
    stats = context.project_artifacts
    transcript_id = case_state.audio_plan.transcript_id
    if transcript_id is not None and transcript_id in stats.transcript_ids_with_vad:
        return True
    source_ids = frozenset(case_state.audio_plan.source_asset_ids)
    if source_ids:
        return bool(source_ids & stats.transcript_with_vad_asset_ids)
    return bool(stats.transcript_with_vad_asset_ids)


def content_plan_exists(context: PreconditionContext) -> bool:
    return context.case_state is not None and context.case_state.content_plan is not None


def cut_plan_exists(context: PreconditionContext) -> bool:
    return context.case_state is not None and context.case_state.cut_plan is not None


def timeline_exists(context: PreconditionContext) -> bool:
    return (
        context.case_state is not None and context.case_state.timeline_current_version is not None
    )


def timeline_validated(context: PreconditionContext) -> bool:
    return (
        context.case_state is not None
        and context.case_state.timeline_current_version is not None
        and context.case_state.timeline_validated
    )


def rough_cut_approved(context: PreconditionContext) -> bool:
    return context.case_state is not None and context.case_state.rough_cut_approved


def preview_for_current_version_exists(context: PreconditionContext) -> bool:
    return (
        context.case_state is not None
        and context.case_state.timeline_current_version is not None
        and context.case_state.preview_current_id is not None
    )


def voiceover_asset_exists(context: PreconditionContext) -> bool:
    case_state = context.case_state
    if case_state is None or case_state.audio_plan is None:
        return False
    voiceover_asset_id = case_state.audio_plan.voiceover_asset_id
    if voiceover_asset_id is None:
        return False
    return voiceover_asset_id in context.project_artifacts.voiceover_asset_ids


PRECONDITION_REGISTRY: dict[str, PreconditionFn] = {
    "active_case": active_case,
    "usable_asset_exists": usable_asset_exists,
    "audio_plan_confirmed": audio_plan_confirmed,
    "audio_source_has_audio": audio_source_has_audio,
    "transcript_with_vad_exists": transcript_with_vad_exists,
    "content_plan_exists": content_plan_exists,
    "cut_plan_exists": cut_plan_exists,
    "timeline_exists": timeline_exists,
    "timeline_validated": timeline_validated,
    "rough_cut_approved": rough_cut_approved,
    "preview_for_current_version_exists": preview_for_current_version_exists,
    "voiceover_asset_exists": voiceover_asset_exists,
}


def get_precondition(name: str) -> PreconditionFn:
    if name.startswith("audio_mode_in(") and name.endswith(")"):
        modes = _parse_argument_list(name, "audio_mode_in")
        return audio_mode_in(*modes)
    predicate = PRECONDITION_REGISTRY.get(name)
    if predicate is None:
        raise UnknownPreconditionError(f"unknown precondition: {name}")
    return predicate


def evaluate_precondition(name: str, context: PreconditionContext) -> bool:
    return get_precondition(name)(context)


def evaluate_preconditions(names: Sequence[str], context: PreconditionContext) -> bool:
    return all(evaluate_precondition(name, context) for name in names)


def register_precondition(name: str, predicate: PreconditionFn) -> None:
    if name.startswith("audio_mode_in("):
        raise ValueError("audio_mode_in is parameterized and must not be registered by instance")
    PRECONDITION_REGISTRY[name] = predicate


def _parse_argument_list(name: str, factory_name: str) -> tuple[str, ...]:
    prefix = f"{factory_name}("
    if not name.startswith(prefix) or not name.endswith(")"):
        raise UnknownPreconditionError(f"unknown precondition: {name}")
    raw = name[len(prefix) : -1]
    values = tuple(part.strip() for part in raw.split(",") if part.strip())
    if not values:
        raise UnknownPreconditionError(f"{factory_name} requires at least one argument")
    return values


def known_precondition_names() -> tuple[str, ...]:
    return tuple(sorted((*PRECONDITION_REGISTRY, "audio_mode_in(...)")))


def assert_known_preconditions(names: Iterable[str]) -> None:
    for name in names:
        get_precondition(name)
