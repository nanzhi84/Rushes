"""Pure domain reducers and policy-facing helpers."""

from .draft_stage import DraftStage, derive_stage
from .preconditions import (
    PRECONDITION_REGISTRY,
    DraftArtifactStats,
    PreconditionContext,
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
    "DraftArtifactStats",
    "DraftStage",
    "PreconditionContext",
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
