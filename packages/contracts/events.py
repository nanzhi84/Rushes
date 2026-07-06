"""DomainEvent discriminated union contracts."""

from typing import Annotated, Any, ClassVar, Literal

from pydantic import BaseModel, ConfigDict, Field

type VersionMode = Literal["strict", "merge"]
type Actor = Literal["user", "agent", "job", "system"]
type DecisionScopeType = Literal["workspace", "project", "case"]


class DomainEventBase(BaseModel):
    model_config = ConfigDict(extra="forbid")

    event: str
    actor: Actor = "agent"
    project_id: str | None = None
    case_id: str | None = None
    base_version: int | None = None
    payload: dict[str, Any] = Field(default_factory=dict)
    created_at: str | None = None

    version_mode: ClassVar[VersionMode] = "merge"
    merge_key: ClassVar[tuple[str, ...]] = ()

    @classmethod
    def reducer_version_mode(cls, scope_type: DecisionScopeType | None = None) -> VersionMode:
        return cls.version_mode


class ProjectCreated(DomainEventBase):
    event: Literal["ProjectCreated"] = "ProjectCreated"
    project_id: str
    name: str | None = None
    version_mode: ClassVar[VersionMode] = "merge"
    merge_key: ClassVar[tuple[str, ...]] = ("project_id",)


class ProjectRenamed(DomainEventBase):
    event: Literal["ProjectRenamed"] = "ProjectRenamed"
    project_id: str
    name: str
    version_mode: ClassVar[VersionMode] = "merge"
    merge_key: ClassVar[tuple[str, ...]] = ("project_id",)


class ProjectTrashed(DomainEventBase):
    event: Literal["ProjectTrashed"] = "ProjectTrashed"
    project_id: str
    version_mode: ClassVar[VersionMode] = "merge"
    merge_key: ClassVar[tuple[str, ...]] = ("project_id",)


class ProjectCopied(DomainEventBase):
    event: Literal["ProjectCopied"] = "ProjectCopied"
    project_id: str
    source_project_id: str | None = None
    version_mode: ClassVar[VersionMode] = "merge"
    merge_key: ClassVar[tuple[str, ...]] = ("project_id",)


class CaseCreated(DomainEventBase):
    event: Literal["CaseCreated"] = "CaseCreated"
    case_id: str
    project_id: str
    version_mode: ClassVar[VersionMode] = "merge"
    merge_key: ClassVar[tuple[str, ...]] = ("case_id",)


class CaseRenamed(DomainEventBase):
    event: Literal["CaseRenamed"] = "CaseRenamed"
    case_id: str
    name: str
    version_mode: ClassVar[VersionMode] = "merge"
    merge_key: ClassVar[tuple[str, ...]] = ("case_id",)


class CaseCopied(DomainEventBase):
    event: Literal["CaseCopied"] = "CaseCopied"
    case_id: str
    source_case_id: str | None = None
    version_mode: ClassVar[VersionMode] = "merge"
    merge_key: ClassVar[tuple[str, ...]] = ("case_id",)


class CaseMoved(DomainEventBase):
    event: Literal["CaseMoved"] = "CaseMoved"
    case_id: str
    source_project_id: str | None = None
    target_project_id: str | None = None
    version_mode: ClassVar[VersionMode] = "merge"
    merge_key: ClassVar[tuple[str, ...]] = ("case_id",)


class CaseClosed(DomainEventBase):
    event: Literal["CaseClosed"] = "CaseClosed"
    case_id: str
    version_mode: ClassVar[VersionMode] = "merge"
    merge_key: ClassVar[tuple[str, ...]] = ("case_id",)


class CaseTrashed(DomainEventBase):
    event: Literal["CaseTrashed"] = "CaseTrashed"
    case_id: str
    version_mode: ClassVar[VersionMode] = "merge"
    merge_key: ClassVar[tuple[str, ...]] = ("case_id",)


class AssetImported(DomainEventBase):
    event: Literal["AssetImported"] = "AssetImported"
    asset_id: str
    job_id: str | None = None
    version_mode: ClassVar[VersionMode] = "merge"
    merge_key: ClassVar[tuple[str, ...]] = ("asset_id", "job_id")


class AssetProbed(DomainEventBase):
    event: Literal["AssetProbed"] = "AssetProbed"
    asset_id: str
    job_id: str | None = None
    version_mode: ClassVar[VersionMode] = "merge"
    merge_key: ClassVar[tuple[str, ...]] = ("asset_id",)


class ProxyGenerated(DomainEventBase):
    event: Literal["ProxyGenerated"] = "ProxyGenerated"
    asset_id: str
    job_id: str | None = None
    version_mode: ClassVar[VersionMode] = "merge"
    merge_key: ClassVar[tuple[str, ...]] = ("asset_id",)


class AnnotationCompleted(DomainEventBase):
    event: Literal["AnnotationCompleted"] = "AnnotationCompleted"
    asset_id: str
    job_id: str | None = None
    annotation_id: str | None = None
    version_mode: ClassVar[VersionMode] = "merge"
    merge_key: ClassVar[tuple[str, ...]] = ("asset_id", "job_id")


class AnnotationFailed(DomainEventBase):
    event: Literal["AnnotationFailed"] = "AnnotationFailed"
    asset_id: str
    job_id: str | None = None
    version_mode: ClassVar[VersionMode] = "merge"
    merge_key: ClassVar[tuple[str, ...]] = ("asset_id", "job_id")


class AssetInvalidated(DomainEventBase):
    event: Literal["AssetInvalidated"] = "AssetInvalidated"
    asset_id: str
    job_id: str | None = None
    version_mode: ClassVar[VersionMode] = "merge"
    merge_key: ClassVar[tuple[str, ...]] = ("asset_id", "job_id")


class AssetIndexReady(DomainEventBase):
    event: Literal["AssetIndexReady"] = "AssetIndexReady"
    asset_id: str
    version_mode: ClassVar[VersionMode] = "merge"
    merge_key: ClassVar[tuple[str, ...]] = ()


class AssetIndexFailed(DomainEventBase):
    event: Literal["AssetIndexFailed"] = "AssetIndexFailed"
    asset_id: str
    version_mode: ClassVar[VersionMode] = "merge"
    merge_key: ClassVar[tuple[str, ...]] = ()


class MaterialUnderstandingStarted(DomainEventBase):
    event: Literal["MaterialUnderstandingStarted"] = "MaterialUnderstandingStarted"
    asset_id: str
    version_mode: ClassVar[VersionMode] = "merge"
    merge_key: ClassVar[tuple[str, ...]] = ()


class MaterialUnderstandingCompleted(DomainEventBase):
    event: Literal["MaterialUnderstandingCompleted"] = "MaterialUnderstandingCompleted"
    asset_id: str
    version_mode: ClassVar[VersionMode] = "merge"
    merge_key: ClassVar[tuple[str, ...]] = ()


class MaterialUnderstandingFailed(DomainEventBase):
    event: Literal["MaterialUnderstandingFailed"] = "MaterialUnderstandingFailed"
    asset_id: str
    version_mode: ClassVar[VersionMode] = "merge"
    merge_key: ClassVar[tuple[str, ...]] = ()


class AssetLinked(DomainEventBase):
    event: Literal["AssetLinked"] = "AssetLinked"
    project_id: str
    asset_id: str
    version_mode: ClassVar[VersionMode] = "merge"
    merge_key: ClassVar[tuple[str, ...]] = ("project_id", "asset_id")


class AssetUnlinked(DomainEventBase):
    event: Literal["AssetUnlinked"] = "AssetUnlinked"
    project_id: str
    asset_id: str
    version_mode: ClassVar[VersionMode] = "merge"
    merge_key: ClassVar[tuple[str, ...]] = ("project_id", "asset_id")


class CaseAssetScopeChanged(DomainEventBase):
    event: Literal["CaseAssetScopeChanged"] = "CaseAssetScopeChanged"
    case_id: str
    version_mode: ClassVar[VersionMode] = "strict"
    merge_key: ClassVar[tuple[str, ...]] = ()


class DecisionEventBase(DomainEventBase):
    decision_id: str
    scope_type: DecisionScopeType
    version_mode: ClassVar[VersionMode] = "strict"
    merge_key: ClassVar[tuple[str, ...]] = ("decision_id",)
    version_mode_by_scope: ClassVar[dict[DecisionScopeType, VersionMode]] = {
        "case": "strict",
        "project": "merge",
        "workspace": "merge",
    }

    @classmethod
    def reducer_version_mode(cls, scope_type: DecisionScopeType | None = None) -> VersionMode:
        if scope_type is None:
            return cls.version_mode
        return cls.version_mode_by_scope[scope_type]


class DecisionCreated(DecisionEventBase):
    event: Literal["DecisionCreated"] = "DecisionCreated"


class DecisionAnswered(DecisionEventBase):
    event: Literal["DecisionAnswered"] = "DecisionAnswered"


class DecisionCancelled(DecisionEventBase):
    event: Literal["DecisionCancelled"] = "DecisionCancelled"


class BriefUpdated(DomainEventBase):
    event: Literal["BriefUpdated"] = "BriefUpdated"
    case_id: str
    version_mode: ClassVar[VersionMode] = "strict"
    merge_key: ClassVar[tuple[str, ...]] = ()


class ContentPlanUpdated(DomainEventBase):
    event: Literal["ContentPlanUpdated"] = "ContentPlanUpdated"
    case_id: str
    version_mode: ClassVar[VersionMode] = "strict"
    merge_key: ClassVar[tuple[str, ...]] = ()


class AudioPlanUpdated(DomainEventBase):
    event: Literal["AudioPlanUpdated"] = "AudioPlanUpdated"
    case_id: str
    version_mode: ClassVar[VersionMode] = "strict"
    merge_key: ClassVar[tuple[str, ...]] = ()


class CutPlanUpdated(DomainEventBase):
    event: Literal["CutPlanUpdated"] = "CutPlanUpdated"
    case_id: str
    version_mode: ClassVar[VersionMode] = "strict"
    merge_key: ClassVar[tuple[str, ...]] = ()


class PostprocessPlanUpdated(DomainEventBase):
    event: Literal["PostprocessPlanUpdated"] = "PostprocessPlanUpdated"
    case_id: str
    version_mode: ClassVar[VersionMode] = "strict"
    merge_key: ClassVar[tuple[str, ...]] = ()


class CandidatePackCreated(DomainEventBase):
    event: Literal["CandidatePackCreated"] = "CandidatePackCreated"
    case_id: str
    candidate_pack_id: str | None = None
    version_mode: ClassVar[VersionMode] = "strict"
    merge_key: ClassVar[tuple[str, ...]] = ()


class TimelineVersionCreated(DomainEventBase):
    event: Literal["TimelineVersionCreated"] = "TimelineVersionCreated"
    case_id: str
    timeline_version: int | None = None
    patch_id: str | None = None
    parent_version: int | None = None
    version_mode: ClassVar[VersionMode] = "strict"
    merge_key: ClassVar[tuple[str, ...]] = ()


class TimelineVersionRestored(DomainEventBase):
    event: Literal["TimelineVersionRestored"] = "TimelineVersionRestored"
    case_id: str
    timeline_version: int | None = None
    version_mode: ClassVar[VersionMode] = "strict"
    merge_key: ClassVar[tuple[str, ...]] = ()


class TimelineValidated(DomainEventBase):
    event: Literal["TimelineValidated"] = "TimelineValidated"
    case_id: str
    timeline_version: int | None = None
    version_mode: ClassVar[VersionMode] = "strict"
    merge_key: ClassVar[tuple[str, ...]] = ()


class TimelineValidationFailed(DomainEventBase):
    event: Literal["TimelineValidationFailed"] = "TimelineValidationFailed"
    case_id: str
    timeline_version: int | None = None
    version_mode: ClassVar[VersionMode] = "strict"
    merge_key: ClassVar[tuple[str, ...]] = ()


class PreviewRendered(DomainEventBase):
    event: Literal["PreviewRendered"] = "PreviewRendered"
    case_id: str
    timeline_version: int
    artifact_id: str
    version_mode: ClassVar[VersionMode] = "merge"
    merge_key: ClassVar[tuple[str, ...]] = ("timeline_version", "artifact_id")


class PreviewViewed(DomainEventBase):
    event: Literal["PreviewViewed"] = "PreviewViewed"
    case_id: str
    preview_id: str
    version_mode: ClassVar[VersionMode] = "merge"
    merge_key: ClassVar[tuple[str, ...]] = ("preview_id",)


class ExportCompleted(DomainEventBase):
    event: Literal["ExportCompleted"] = "ExportCompleted"
    case_id: str
    timeline_version: int
    artifact_id: str
    version_mode: ClassVar[VersionMode] = "merge"
    merge_key: ClassVar[tuple[str, ...]] = ("timeline_version", "artifact_id")


class MemoryCandidateExtracted(DomainEventBase):
    event: Literal["MemoryCandidateExtracted"] = "MemoryCandidateExtracted"
    candidate_id: str
    case_id: str | None = None
    version_mode: ClassVar[VersionMode] = "merge"
    merge_key: ClassVar[tuple[str, ...]] = ("candidate_id",)


class MemoryCandidateDiscarded(DomainEventBase):
    event: Literal["MemoryCandidateDiscarded"] = "MemoryCandidateDiscarded"
    candidate_id: str
    case_id: str | None = None
    version_mode: ClassVar[VersionMode] = "merge"
    merge_key: ClassVar[tuple[str, ...]] = ("candidate_id",)


class MemorySaved(DomainEventBase):
    event: Literal["MemorySaved"] = "MemorySaved"
    memory_id: str
    candidate_id: str | None = None
    version_mode: ClassVar[VersionMode] = "merge"
    merge_key: ClassVar[tuple[str, ...]] = ("memory_id",)


class JobEnqueued(DomainEventBase):
    event: Literal["JobEnqueued"] = "JobEnqueued"
    job_id: str
    requested_by_case_id: str | None = None
    version_mode: ClassVar[VersionMode] = "merge"
    merge_key: ClassVar[tuple[str, ...]] = ("job_id",)


class JobProgress(DomainEventBase):
    event: Literal["JobProgress"] = "JobProgress"
    job_id: str
    requested_by_case_id: str | None = None
    progress: float | None = None
    version_mode: ClassVar[VersionMode] = "merge"
    merge_key: ClassVar[tuple[str, ...]] = ("job_id", "progress")


class JobSucceeded(DomainEventBase):
    event: Literal["JobSucceeded"] = "JobSucceeded"
    job_id: str
    requested_by_case_id: str | None = None
    version_mode: ClassVar[VersionMode] = "merge"
    merge_key: ClassVar[tuple[str, ...]] = ("job_id",)


class JobFailed(DomainEventBase):
    event: Literal["JobFailed"] = "JobFailed"
    job_id: str
    requested_by_case_id: str | None = None
    version_mode: ClassVar[VersionMode] = "merge"
    merge_key: ClassVar[tuple[str, ...]] = ("job_id",)


class JobCancelled(DomainEventBase):
    event: Literal["JobCancelled"] = "JobCancelled"
    job_id: str
    requested_by_case_id: str | None = None
    version_mode: ClassVar[VersionMode] = "merge"
    merge_key: ClassVar[tuple[str, ...]] = ("job_id",)


class PolicyRefusal(DomainEventBase):
    event: Literal["PolicyRefusal"] = "PolicyRefusal"
    refusal_id: str
    version_mode: ClassVar[VersionMode] = "merge"
    merge_key: ClassVar[tuple[str, ...]] = ("refusal_id",)


class ProviderCallRecorded(DomainEventBase):
    event: Literal["ProviderCallRecorded"] = "ProviderCallRecorded"
    provider_call_id: str
    version_mode: ClassVar[VersionMode] = "merge"
    merge_key: ClassVar[tuple[str, ...]] = ("provider_call_id",)


class ContextCompacted(DomainEventBase):
    event: Literal["ContextCompacted"] = "ContextCompacted"
    compaction_id: str
    version_mode: ClassVar[VersionMode] = "merge"
    merge_key: ClassVar[tuple[str, ...]] = ("compaction_id",)


class TurnEnded(DomainEventBase):
    event: Literal["TurnEnded"] = "TurnEnded"
    turn_id: str
    case_id: str
    version_mode: ClassVar[VersionMode] = "merge"
    merge_key: ClassVar[tuple[str, ...]] = ("turn_id",)


class CapabilityDegraded(DomainEventBase):
    event: Literal["CapabilityDegraded"] = "CapabilityDegraded"
    degradation_id: str
    capability: str
    provider_id: str | None = None
    reason: str
    fallback: str | None = None
    version_mode: ClassVar[VersionMode] = "merge"
    merge_key: ClassVar[tuple[str, ...]] = ("degradation_id",)


class SecurityRefusal(DomainEventBase):
    event: Literal["SecurityRefusal"] = "SecurityRefusal"
    security_refusal_id: str
    route: str
    path: str | None = None
    origin: str | None = None
    reason: str
    actor: Actor = "system"
    version_mode: ClassVar[VersionMode] = "merge"
    merge_key: ClassVar[tuple[str, ...]] = ("security_refusal_id",)


EVENT_CLASSES: tuple[type[DomainEventBase], ...] = (
    ProjectCreated,
    ProjectRenamed,
    ProjectTrashed,
    ProjectCopied,
    CaseCreated,
    CaseRenamed,
    CaseCopied,
    CaseMoved,
    CaseClosed,
    CaseTrashed,
    AssetImported,
    AssetProbed,
    ProxyGenerated,
    AnnotationCompleted,
    AnnotationFailed,
    AssetInvalidated,
    AssetIndexReady,
    AssetIndexFailed,
    MaterialUnderstandingStarted,
    MaterialUnderstandingCompleted,
    MaterialUnderstandingFailed,
    AssetLinked,
    AssetUnlinked,
    CaseAssetScopeChanged,
    DecisionCreated,
    DecisionAnswered,
    DecisionCancelled,
    BriefUpdated,
    ContentPlanUpdated,
    AudioPlanUpdated,
    CutPlanUpdated,
    PostprocessPlanUpdated,
    CandidatePackCreated,
    TimelineVersionCreated,
    TimelineVersionRestored,
    TimelineValidated,
    TimelineValidationFailed,
    PreviewRendered,
    PreviewViewed,
    ExportCompleted,
    MemoryCandidateExtracted,
    MemoryCandidateDiscarded,
    MemorySaved,
    JobEnqueued,
    JobProgress,
    JobSucceeded,
    JobFailed,
    JobCancelled,
    PolicyRefusal,
    ProviderCallRecorded,
    ContextCompacted,
    TurnEnded,
    CapabilityDegraded,
    SecurityRefusal,
)

type EventName = Literal[
    "ProjectCreated",
    "ProjectRenamed",
    "ProjectTrashed",
    "ProjectCopied",
    "CaseCreated",
    "CaseRenamed",
    "CaseCopied",
    "CaseMoved",
    "CaseClosed",
    "CaseTrashed",
    "AssetImported",
    "AssetProbed",
    "ProxyGenerated",
    "AnnotationCompleted",
    "AnnotationFailed",
    "AssetInvalidated",
    "AssetIndexReady",
    "AssetIndexFailed",
    "MaterialUnderstandingStarted",
    "MaterialUnderstandingCompleted",
    "MaterialUnderstandingFailed",
    "AssetLinked",
    "AssetUnlinked",
    "CaseAssetScopeChanged",
    "DecisionCreated",
    "DecisionAnswered",
    "DecisionCancelled",
    "BriefUpdated",
    "ContentPlanUpdated",
    "AudioPlanUpdated",
    "CutPlanUpdated",
    "PostprocessPlanUpdated",
    "CandidatePackCreated",
    "TimelineVersionCreated",
    "TimelineVersionRestored",
    "TimelineValidated",
    "TimelineValidationFailed",
    "PreviewRendered",
    "PreviewViewed",
    "ExportCompleted",
    "MemoryCandidateExtracted",
    "MemoryCandidateDiscarded",
    "MemorySaved",
    "JobEnqueued",
    "JobProgress",
    "JobSucceeded",
    "JobFailed",
    "JobCancelled",
    "PolicyRefusal",
    "ProviderCallRecorded",
    "ContextCompacted",
    "TurnEnded",
    "CapabilityDegraded",
    "SecurityRefusal",
]

type EVENT_UNION = Annotated[
    ProjectCreated
    | ProjectRenamed
    | ProjectTrashed
    | ProjectCopied
    | CaseCreated
    | CaseRenamed
    | CaseCopied
    | CaseMoved
    | CaseClosed
    | CaseTrashed
    | AssetImported
    | AssetProbed
    | ProxyGenerated
    | AnnotationCompleted
    | AnnotationFailed
    | AssetInvalidated
    | AssetIndexReady
    | AssetIndexFailed
    | MaterialUnderstandingStarted
    | MaterialUnderstandingCompleted
    | MaterialUnderstandingFailed
    | AssetLinked
    | AssetUnlinked
    | CaseAssetScopeChanged
    | DecisionCreated
    | DecisionAnswered
    | DecisionCancelled
    | BriefUpdated
    | ContentPlanUpdated
    | AudioPlanUpdated
    | CutPlanUpdated
    | PostprocessPlanUpdated
    | CandidatePackCreated
    | TimelineVersionCreated
    | TimelineVersionRestored
    | TimelineValidated
    | TimelineValidationFailed
    | PreviewRendered
    | PreviewViewed
    | ExportCompleted
    | MemoryCandidateExtracted
    | MemoryCandidateDiscarded
    | MemorySaved
    | JobEnqueued
    | JobProgress
    | JobSucceeded
    | JobFailed
    | JobCancelled
    | PolicyRefusal
    | ProviderCallRecorded
    | ContextCompacted
    | TurnEnded
    | CapabilityDegraded
    | SecurityRefusal,
    Field(discriminator="event"),
]
type DomainEvent = EVENT_UNION


def event_registry() -> dict[str, type[DomainEventBase]]:
    registry: dict[str, type[DomainEventBase]] = {}
    for event_class in EVENT_CLASSES:
        event_name = event_class.model_fields["event"].default
        if not isinstance(event_name, str):
            raise TypeError(f"{event_class.__name__} must declare a string event default")
        registry[event_name] = event_class
    return registry
