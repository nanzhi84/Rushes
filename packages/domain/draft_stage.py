"""Macro stage derivation for UI and context compaction."""

from __future__ import annotations

from typing import Literal

from contracts.draft import DraftState

DraftStage = Literal["briefing", "drafting", "refining", "exporting"]

_EXPORT_JOB_KINDS = frozenset(
    {
        "export",
        "final_export",
        "render.final_mp4",
        "render_final_mp4",
        "render_final",
    }
)


def derive_stage(draft_state: DraftState) -> DraftStage:
    """Derive the macro stage with PRD §5.3's fixed first-match order.

    草稿无 closed 阶段（只有 active / trashed 两态，trashed 不进编辑器），
    求值顺序固定 exporting → refining → drafting → briefing。
    """

    if _export_decision_answered_without_export(draft_state):
        return "exporting"
    if draft_state.rough_cut_approved and draft_state.export_current_id is None:
        return "refining"
    if draft_state.audio_plan is not None and not draft_state.rough_cut_approved:
        return "drafting"
    return "briefing"


def _export_decision_answered_without_export(draft_state: DraftState) -> bool:
    if draft_state.export_current_id is not None:
        return False
    if draft_state.scratch_memory.get("export_decision_answered") is True:
        return True
    if draft_state.scratch_memory.get("export_confirmed") is True:
        return True
    return any(
        job.status in {"pending", "running"} and job.kind in _EXPORT_JOB_KINDS
        for job in draft_state.running_jobs
    )
