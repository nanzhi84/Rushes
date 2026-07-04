"""Pure domain reducers and policy-facing helpers."""

from .case_stage import CaseStage, derive_stage
from .preconditions import (
    PRECONDITION_REGISTRY,
    PreconditionContext,
    ProjectArtifactStats,
    UnknownPreconditionError,
    assert_known_preconditions,
    audio_mode_in,
    evaluate_precondition,
    evaluate_preconditions,
    get_precondition,
    known_precondition_names,
    register_precondition,
)

__all__ = [
    "PRECONDITION_REGISTRY",
    "CaseStage",
    "PreconditionContext",
    "ProjectArtifactStats",
    "UnknownPreconditionError",
    "assert_known_preconditions",
    "audio_mode_in",
    "derive_stage",
    "evaluate_precondition",
    "evaluate_preconditions",
    "get_precondition",
    "known_precondition_names",
    "register_precondition",
]
