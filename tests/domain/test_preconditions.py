import pytest

from contracts.case import CaseState
from domain.preconditions import (
    PreconditionContext,
    ProjectArtifactStats,
    UnknownPreconditionError,
    evaluate_precondition,
)


def _case_state(**overrides: object) -> CaseState:
    data = {
        "case_id": "case_1",
        "project_id": "project_1",
        "name": "Case",
        "brief": {"goal": "make a video", "confirmed_facts": []},
        "content_plan": {"outline": ["hook"]},
        "audio_plan": {
            "mode": "rough_cut",
            "source_asset_ids": ["asset_voice"],
            "voiceover_asset_id": "asset_vo",
            "transcript_id": "tr_voice",
        },
        "cut_plan": {"schema": "CutPlan.v1", "slots": [], "total_target_duration_sec": 30},
        "candidate_pack_id": "pack_1",
        "timeline_current_version": 3,
        "timeline_validated": True,
        "preview_current_id": "preview_3",
        "rough_cut_approved": True,
        "rough_cut_approved_version": 3,
        "selected_asset_ids": [],
        "disabled_asset_ids": [],
        "scratch_memory": {},
    }
    data.update(overrides)
    return CaseState.model_validate(data)


def test_all_prd_preconditions_have_true_and_false_cases() -> None:
    true_context = PreconditionContext(
        case_state=_case_state(),
        project_artifacts=ProjectArtifactStats(
            usable_asset_count=2,
            usable_asset_ids=frozenset({"asset_voice", "asset_broll"}),
            asset_ids_with_audio=frozenset({"asset_voice"}),
            transcript_asset_ids=frozenset({"asset_voice"}),
            transcript_with_vad_asset_ids=frozenset({"asset_voice"}),
            transcript_ids=frozenset({"tr_voice"}),
            transcript_ids_with_vad=frozenset({"tr_voice"}),
            voiceover_asset_ids=frozenset({"asset_vo"}),
            candidate_pack_valid=True,
        ),
    )
    false_context = PreconditionContext(
        case_state=_case_state(
            status="closed",
            content_plan=None,
            audio_plan=None,
            cut_plan=None,
            candidate_pack_id=None,
            timeline_current_version=None,
            timeline_validated=False,
            preview_current_id=None,
            rough_cut_approved=False,
        ),
        project_artifacts=ProjectArtifactStats(candidate_pack_valid=False),
    )

    names = [
        "active_case",
        "usable_asset_exists",
        "audio_plan_confirmed",
        "transcript_with_vad_exists",
        "content_plan_exists",
        "cut_plan_exists",
        "candidate_pack_exists",
        "timeline_exists",
        "timeline_validated",
        "rough_cut_approved",
        "preview_for_current_version_exists",
        "voiceover_asset_exists",
    ]

    for name in names:
        assert evaluate_precondition(name, true_context), name
        assert not evaluate_precondition(name, false_context), name


def test_audio_mode_in_factory_and_unknown_names() -> None:
    context = PreconditionContext(case_state=_case_state())

    assert evaluate_precondition("audio_mode_in(keep_original, rough_cut)", context)
    assert not evaluate_precondition("audio_mode_in(tts)", context)
    with pytest.raises(UnknownPreconditionError):
        evaluate_precondition("does_not_exist", context)
