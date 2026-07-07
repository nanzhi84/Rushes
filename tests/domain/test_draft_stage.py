from contracts.draft import DraftState
from domain.draft_stage import derive_stage


def _draft_state(**overrides: object) -> DraftState:
    data = {
        "draft_id": "draft_1",
        "name": "草稿",
        "brief": {"goal": "make a video", "confirmed_facts": []},
        "scratch_memory": {},
    }
    data.update(overrides)
    return DraftState.model_validate(data)


def test_stage_fixed_order_exporting_wins_over_refining() -> None:
    draft_state = _draft_state(
        rough_cut_approved=True,
        export_current_id=None,
        running_jobs=[
            {
                "job_id": "job_export",
                "kind": "render.final_mp4",
                "status": "running",
                "progress": 0.2,
            }
        ],
    )

    assert derive_stage(draft_state) == "exporting"


def test_stage_drafting_precedes_briefing_when_audio_plan_exists() -> None:
    draft_state = _draft_state(
        brief={"goal": "", "confirmed_facts": []},
        audio_plan={"mode": "rough_cut", "source_asset_ids": ["asset_1"]},
        rough_cut_approved=False,
    )

    assert derive_stage(draft_state) == "drafting"


def test_stage_refining_and_briefing_defaults() -> None:
    assert derive_stage(_draft_state(rough_cut_approved=True, export_current_id=None)) == "refining"
    assert derive_stage(_draft_state()) == "briefing"


def test_exporting_signals_from_scratch_memory_and_running_jobs() -> None:
    approved = {
        "audio_plan": {"mode": "tts"},
        "rough_cut_approved": True,
        "rough_cut_approved_version": 1,
    }
    by_flag = _draft_state(**approved, scratch_memory={"export_decision_answered": True})
    assert derive_stage(by_flag) == "exporting"

    by_confirm = _draft_state(**approved, scratch_memory={"export_confirmed": True})
    assert derive_stage(by_confirm) == "exporting"

    by_job = _draft_state(
        **approved,
        running_jobs=[{"job_id": "job_1", "kind": "render_final", "status": "running"}],
    )
    assert derive_stage(by_job) == "exporting"
