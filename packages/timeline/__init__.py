"""Frame-level timeline materialization and validation."""

from .anchor import AnchorConflict, AnchorResolution, AnchorResolutionError, resolve_anchor
from .materializer import MaterializationError, materialize_from_selection
from .patch_apply import PatchApplyError, PatchOutcome, apply_patch
from .summary import render_timeline_summary
from .validator import (
    TimelineValidationContext,
    build_timeline_invariant_hook,
    validate_timeline,
    validate_timeline_invariants,
)
from .version_store import (
    TimelineVersionRecord,
    get_timeline_version,
    list_timeline_versions,
    restore_timeline_version,
    store_timeline_version,
    update_timeline_validation_report,
)

__all__ = [
    "AnchorConflict",
    "AnchorResolution",
    "AnchorResolutionError",
    "MaterializationError",
    "PatchApplyError",
    "PatchOutcome",
    "TimelineValidationContext",
    "TimelineVersionRecord",
    "apply_patch",
    "build_timeline_invariant_hook",
    "get_timeline_version",
    "list_timeline_versions",
    "materialize_from_selection",
    "render_timeline_summary",
    "resolve_anchor",
    "restore_timeline_version",
    "store_timeline_version",
    "update_timeline_validation_report",
    "validate_timeline",
    "validate_timeline_invariants",
]
