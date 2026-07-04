import pytest

import domain.decision_effects as effects
from contracts.case import CaseState
from contracts.decision import Decision, DecisionAnswer
from domain.decision_effects import (
    decision_effects_registry,
    pending_tool_call_status_after_answer,
    pending_tool_call_will_replay,
    reduce_decision_answer,
)


def _case_state(**overrides: object) -> CaseState:
    data = {
        "case_id": "case_1",
        "project_id": "project_1",
        "name": "Case",
        "state_version": 3,
        "brief": {"goal": "make a cut", "confirmed_facts": []},
        "timeline_current_version": 7,
        "timeline_validated": True,
        "rough_cut_approved": True,
        "rough_cut_approved_version": 6,
        "selected_asset_ids": [],
        "disabled_asset_ids": [],
        "scratch_memory": {},
    }
    data.update(overrides)
    return CaseState.model_validate(data)


def _decision(decision_type: str, **overrides: object) -> Decision:
    data = {
        "decision_id": f"dec_{decision_type}",
        "scope_type": "case",
        "project_id": "project_1",
        "case_id": "case_1",
        "type": decision_type,
        "question": "?",
        "blocking": True,
    }
    data.update(overrides)
    return Decision.model_validate(data)


def _answer(**overrides: object) -> DecisionAnswer:
    data = {"option_id": "yes", "answered_via": "button", "payload": {}}
    data.update(overrides)
    return DecisionAnswer.model_validate(data)


def test_registry_covers_all_eleven_decision_types() -> None:
    assert set(decision_effects_registry) == {
        "audio_mode",
        "approve_content_plan",
        "approve_speech_cut",
        "approve_rough_cut",
        "subtitle",
        "bgm",
        "export",
        "memory_scope",
        "destructive_project_action",
        "url_import",
        "generic",
    }


def test_audio_mode_reduces_to_audio_plan_and_event() -> None:
    result = reduce_decision_answer(
        _case_state(),
        _decision("audio_mode"),
        _answer(option_id="rough_cut"),
    )

    assert result.state_patch["audio_plan"]["mode"] == "rough_cut"
    assert [event.event for event in result.followup_events] == ["AudioPlanUpdated"]


def test_approve_content_plan_marks_plan_approved() -> None:
    result = reduce_decision_answer(
        _case_state(content_plan={"outline": ["a"]}),
        _decision("approve_content_plan"),
        _answer(),
    )

    assert result.state_patch["content_plan"]["status"] == "approved"
    assert result.followup_events[0].event == "ContentPlanUpdated"


def test_approve_speech_cut_writes_removed_ranges_without_rough_cut_change() -> None:
    result = reduce_decision_answer(
        _case_state(),
        _decision("approve_speech_cut"),
        _answer(
            payload={
                "removed_ranges": [
                    {"start_ms": 1000, "end_ms": 1500, "kind": "filler", "source": "rough_cut"}
                ]
            }
        ),
    )

    assert result.state_patch["cut_plan"]["removed_ranges"][0]["start_ms"] == 1000
    assert "rough_cut_approved" not in result.state_patch
    assert result.followup_events[0].event == "CutPlanUpdated"
    assert result.followups[0].kind == "enqueue_delete_range_patches"


def test_approve_rough_cut_sets_bool_and_bound_version_without_events() -> None:
    result = reduce_decision_answer(
        _case_state(rough_cut_approved=False),
        _decision("approve_rough_cut"),
        _answer(payload={"timeline_version": 4}),
    )

    assert result.state_patch == {"rough_cut_approved": True, "rough_cut_approved_version": 4}
    assert result.followup_events == ()


def test_subtitle_rejection_disables_subtitle_plan() -> None:
    result = reduce_decision_answer(
        _case_state(),
        _decision("subtitle"),
        _answer(option_id="skip", payload={"enabled": False}),
    )

    assert result.state_patch["postprocess_plan"]["subtitle"] == {
        "enabled": False,
        "style_template_id": None,
    }
    assert result.followup_events[0].event == "PostprocessPlanUpdated"


def test_bgm_acceptance_reduces_to_bgm_plan() -> None:
    result = reduce_decision_answer(
        _case_state(),
        _decision("bgm"),
        _answer(payload={"enabled": True, "asset_id": "asset_bgm", "gain_db": -8.0, "duck": True}),
    )

    assert result.state_patch["postprocess_plan"]["bgm"] == {
        "enabled": True,
        "asset_id": "asset_bgm",
        "gain_db": -8.0,
        "duck": True,
    }


def test_export_destructive_and_url_import_only_return_replay_followup() -> None:
    pending_tool_call = {
        "tool_name": "render.final_mp4",
        "arguments": {"case_id": "case_1"},
        "idempotency_key": "idem",
        "argument_fingerprint": "fp",
    }

    for decision_type in ("export", "destructive_project_action", "url_import"):
        result = reduce_decision_answer(
            _case_state(),
            _decision(
                decision_type,
                pending_tool_call=pending_tool_call,
                pending_tool_call_status="pending",
            ),
            _answer(option_id="approve"),
        )

        assert result.state_patch == {}
        assert result.followup_events == ()
        assert result.followups[0].kind == "replay_pending_tool_call"


def test_memory_scope_returns_memory_save_followup_only() -> None:
    result = reduce_decision_answer(
        _case_state(),
        _decision("memory_scope"),
        _answer(payload={"candidate_id": "memcand_1", "scope": "project"}),
    )

    assert result.state_patch == {}
    assert result.followup_events == ()
    assert result.followups[0].kind == "enqueue_memory_save"
    assert result.followups[0].payload["scope"] == "project"


def test_generic_reduces_to_confirmed_fact_or_scratch_memory() -> None:
    fact_result = reduce_decision_answer(
        _case_state(),
        _decision("generic"),
        _answer(payload={"reduce_target": "brief.confirmed_facts", "value": "use fast pacing"}),
    )
    scratch_result = reduce_decision_answer(
        _case_state(),
        _decision("generic"),
        _answer(payload={"reduce_target": "scratch_memory", "key": "tone", "value": "quiet"}),
    )

    assert fact_result.state_patch["brief"]["confirmed_facts"] == ["use fast pacing"]
    assert fact_result.followup_events[0].event == "BriefUpdated"
    assert scratch_result.state_patch["scratch_memory"] == {"tone": "quiet"}
    assert scratch_result.followup_events == ()


def test_missing_decision_effect_validation_raises(monkeypatch: pytest.MonkeyPatch) -> None:
    with pytest.raises(effects.MissingDecisionEffectError):
        effects.validate_decision_type_registered("unknown")

    registry_without_generic = dict(decision_effects_registry)
    registry_without_generic.pop("generic")
    monkeypatch.setattr(effects, "decision_effects_registry", registry_without_generic)

    with pytest.raises(effects.MissingDecisionEffectError, match="generic"):
        effects.validate_all_decision_types_registered()


def test_option_payloads_and_option_id_fallbacks_drive_effects() -> None:
    rough_cut = reduce_decision_answer(
        _case_state(rough_cut_approved=False),
        _decision(
            "approve_rough_cut",
            options=[
                {
                    "option_id": "restore_v8",
                    "label": "Restore v8",
                    "payload": {"timeline_version": 8},
                }
            ],
        ),
        _answer(option_id="restore_v8"),
    )
    subtitle = reduce_decision_answer(
        _case_state(),
        _decision("subtitle"),
        _answer(option_id="subtitle_large"),
    )
    bgm = reduce_decision_answer(
        _case_state(),
        _decision("bgm"),
        _answer(option_id="asset_music"),
    )

    assert rough_cut.state_patch["rough_cut_approved_version"] == 8
    assert subtitle.state_patch["postprocess_plan"]["subtitle"] == {
        "enabled": True,
        "style_template_id": "subtitle_large",
    }
    assert bgm.state_patch["postprocess_plan"]["bgm"]["asset_id"] == "asset_music"


def test_memory_scope_skip_and_invalid_payloads() -> None:
    skipped = reduce_decision_answer(
        _case_state(),
        _decision("memory_scope"),
        _answer(option_id="skip", payload={"candidate_id": "memcand_1"}),
    )

    assert skipped.state_patch == {}
    assert skipped.followups == ()
    with pytest.raises(ValueError, match="candidate_id"):
        reduce_decision_answer(
            _case_state(),
            _decision("memory_scope"),
            _answer(payload={"scope": "project"}),
        )
    with pytest.raises(ValueError, match="scope"):
        reduce_decision_answer(
            _case_state(),
            _decision("memory_scope"),
            _answer(payload={"candidate_id": "memcand_1", "scope": "team"}),
        )


def test_pending_tool_call_rejections_discard_or_skip_followups() -> None:
    pending_tool_call = {
        "tool_name": "render.final_mp4",
        "arguments": {"case_id": "case_1"},
        "idempotency_key": "idem",
        "argument_fingerprint": "fp",
    }
    decision = _decision(
        "export",
        pending_tool_call=pending_tool_call,
        pending_tool_call_status="pending",
    )
    rejection = _answer(option_id="cancel")
    no_pending = _decision("export")

    result = reduce_decision_answer(_case_state(), decision, rejection)

    assert result.followups == ()
    assert pending_tool_call_will_replay(decision, rejection) is False
    assert (
        pending_tool_call_status_after_answer(decision.pending_tool_call, rejection) == "discarded"
    )
    assert pending_tool_call_status_after_answer(no_pending.pending_tool_call, rejection) is None


def test_generic_effect_uses_free_text_option_id_and_default_scratch_key() -> None:
    duplicate_fact = reduce_decision_answer(
        _case_state(brief={"goal": "make a cut", "confirmed_facts": ["existing"]}),
        _decision("generic"),
        _answer(
            free_text="existing",
            payload={"reduce_target": "brief.confirmed_facts"},
        ),
    )
    scratch = reduce_decision_answer(
        _case_state(),
        _decision("generic"),
        _answer(option_id="remember this", payload={"reduce_target": "scratch_memory"}),
    )

    assert duplicate_fact.state_patch["brief"]["confirmed_facts"] == ["existing"]
    assert scratch.state_patch["scratch_memory"] == {"dec_generic": "remember this"}


def test_invalid_decision_answers_raise_value_errors() -> None:
    with pytest.raises(ValueError, match="timeline_version"):
        reduce_decision_answer(
            _case_state(),
            _decision("approve_rough_cut"),
            _answer(),
        )
    with pytest.raises(ValueError, match="removed_ranges"):
        reduce_decision_answer(
            _case_state(),
            _decision("approve_speech_cut"),
            _answer(payload={"removed_ranges": "not a sequence"}),
        )
    with pytest.raises(ValueError, match="reduce_target"):
        reduce_decision_answer(
            _case_state(),
            _decision("generic"),
            _answer(payload={"value": "text"}),
        )
