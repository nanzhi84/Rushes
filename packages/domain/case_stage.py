"""Macro stage derivation for UI and context compaction."""

from __future__ import annotations

from typing import Literal

from contracts.case import CaseState

CaseStage = Literal["briefing", "drafting", "refining", "exporting", "closed"]

_EXPORT_JOB_KINDS = frozenset(
    {
        "export",
        "final_export",
        "render.final_mp4",
        "render_final_mp4",
        "render_final",
    }
)


def derive_stage(case_state: CaseState) -> CaseStage:
    """Derive the macro stage with PRD §5.3's fixed first-match order."""

    if case_state.status == "closed":
        return "closed"
    if _export_decision_answered_without_export(case_state):
        return "exporting"
    if case_state.rough_cut_approved and case_state.export_current_id is None:
        return "refining"
    if case_state.audio_plan is not None and not case_state.rough_cut_approved:
        return "drafting"
    return "briefing"


def _export_decision_answered_without_export(case_state: CaseState) -> bool:
    if case_state.export_current_id is not None:
        return False
    if case_state.scratch_memory.get("export_decision_answered") is True:
        return True
    if case_state.scratch_memory.get("export_confirmed") is True:
        return True
    return any(
        job.status in {"pending", "running"} and job.kind in _EXPORT_JOB_KINDS
        for job in case_state.running_jobs
    )
