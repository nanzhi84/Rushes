from contracts.case import CaseState
from domain.case_stage import derive_stage


def _case_state(**overrides: object) -> CaseState:
    data = {
        "case_id": "case_1",
        "project_id": "project_1",
        "name": "Case",
        "brief": {"goal": "make a video", "confirmed_facts": []},
        "selected_asset_ids": [],
        "disabled_asset_ids": [],
        "scratch_memory": {},
    }
    data.update(overrides)
    return CaseState.model_validate(data)


def test_stage_fixed_order_closed_wins_over_exporting() -> None:
    case_state = _case_state(
        status="closed",
        rough_cut_approved=True,
        export_current_id=None,
        scratch_memory={"export_decision_answered": True},
    )

    assert derive_stage(case_state) == "closed"


def test_stage_fixed_order_exporting_wins_over_refining() -> None:
    case_state = _case_state(
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

    assert derive_stage(case_state) == "exporting"


def test_stage_drafting_precedes_briefing_when_audio_plan_exists() -> None:
    case_state = _case_state(
        brief={"goal": "", "confirmed_facts": []},
        audio_plan={"mode": "rough_cut", "source_asset_ids": ["asset_1"]},
        rough_cut_approved=False,
    )

    assert derive_stage(case_state) == "drafting"


def test_stage_refining_and_briefing_defaults() -> None:
    assert derive_stage(_case_state(rough_cut_approved=True, export_current_id=None)) == "refining"
    assert derive_stage(_case_state()) == "briefing"
