import pytest

from contracts.draft import DraftState
from domain.preconditions import (
    DraftArtifactStats,
    PreconditionContext,
    UnknownPreconditionError,
    evaluate_precondition,
)


def _draft_state(**overrides: object) -> DraftState:
    data = {
        "draft_id": "draft_1",
        "name": "草稿",
        "brief": {"goal": "make a video", "confirmed_facts": []},
        "content_plan": {"outline": ["hook"]},
        "audio_plan": {
            "mode": "rough_cut",
            "source_asset_ids": ["asset_voice"],
            "voiceover_asset_id": "asset_vo",
            "transcript_id": "tr_voice",
        },
        "cut_plan": {"schema": "CutPlan.v1", "slots": [], "total_target_duration_sec": 30},
        "timeline_current_version": 3,
        "timeline_validated": True,
        "preview_current_id": "preview_3",
        "rough_cut_approved": True,
        "rough_cut_approved_version": 3,
        "scratch_memory": {},
    }
    data.update(overrides)
    return DraftState.model_validate(data)


def test_all_prd_preconditions_have_true_and_false_scenarios() -> None:
    true_context = PreconditionContext(
        draft_state=_draft_state(),
        draft_artifacts=DraftArtifactStats(
            usable_asset_count=2,
            usable_asset_ids=frozenset({"asset_voice", "asset_broll"}),
            asset_ids_with_audio=frozenset({"asset_voice"}),
            transcript_asset_ids=frozenset({"asset_voice"}),
            transcript_with_vad_asset_ids=frozenset({"asset_voice"}),
            transcript_ids=frozenset({"tr_voice"}),
            transcript_ids_with_vad=frozenset({"tr_voice"}),
            voiceover_asset_ids=frozenset({"asset_vo"}),
            preview_count=1,
        ),
    )
    false_context = PreconditionContext(
        draft_state=_draft_state(
            status="trashed",
            content_plan=None,
            audio_plan=None,
            cut_plan=None,
            timeline_current_version=None,
            timeline_validated=False,
            preview_current_id=None,
            rough_cut_approved=False,
        ),
        draft_artifacts=DraftArtifactStats(),
    )

    names = [
        "active_draft",
        "usable_asset_exists",
        "audio_plan_confirmed",
        "audio_source_has_audio",
        "transcript_with_vad_exists",
        "content_plan_exists",
        "cut_plan_exists",
        "timeline_exists",
        "timeline_validated",
        "rough_cut_approved",
        "preview_for_current_version_exists",
        "any_preview_exists",
        "voiceover_asset_exists",
    ]

    for name in names:
        assert evaluate_precondition(name, true_context), name
        assert not evaluate_precondition(name, false_context), name


def test_any_preview_exists_survives_timeline_version_change() -> None:
    context = PreconditionContext(
        draft_state=_draft_state(
            timeline_current_version=4,
            preview_current_id=None,
        ),
        draft_artifacts=DraftArtifactStats(preview_count=1),
    )

    assert evaluate_precondition("any_preview_exists", context)
    assert not evaluate_precondition("preview_for_current_version_exists", context)


def test_usable_asset_exists_falls_back_to_count() -> None:
    # usable_asset_ids 未展开时按 usable_asset_count 兜底（无 enabled/disabled 维度）。
    only_count = PreconditionContext(
        draft_state=_draft_state(),
        draft_artifacts=DraftArtifactStats(usable_asset_count=1),
    )
    assert evaluate_precondition("usable_asset_exists", only_count)


def test_audio_mode_in_factory_and_unknown_names() -> None:
    context = PreconditionContext(draft_state=_draft_state())

    assert evaluate_precondition("audio_mode_in(keep_original, rough_cut)", context)
    assert not evaluate_precondition("audio_mode_in(tts)", context)
    with pytest.raises(UnknownPreconditionError):
        evaluate_precondition("does_not_exist", context)
