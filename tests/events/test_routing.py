from contracts.events import (
    CapabilityDegraded,
    ContextCompacted,
    JobProgress,
    MemoryCandidateExtracted,
    PolicyRefusal,
    ProjectCreated,
    ProviderCallRecorded,
    SecurityRefusal,
    TurnEnded,
)
from events.routing import routes_to_case, routes_to_workspace, should_push_sse


def test_requested_by_case_id_routes_job_to_case_and_workspace() -> None:
    event = JobProgress(event="JobProgress", job_id="job_1", requested_by_case_id="case_1")

    assert routes_to_case(event, "case_1")
    assert not routes_to_case(event, "case_2")
    assert routes_to_workspace(event)


def test_payload_requested_by_case_id_is_case_route_key() -> None:
    event = ProjectCreated(
        event="ProjectCreated",
        project_id="project_1",
        payload={"requested_by_case_id": "case_1"},
    )

    assert routes_to_case(event, "case_1")
    assert routes_to_workspace(event)


def test_turn_ended_is_case_only() -> None:
    event = TurnEnded(event="TurnEnded", turn_id="turn_1", case_id="case_1")

    assert routes_to_case(event, "case_1")
    assert not routes_to_workspace(event)


def test_record_events_do_not_push_sse() -> None:
    events = [
        PolicyRefusal(event="PolicyRefusal", refusal_id="refusal_1"),
        ProviderCallRecorded(event="ProviderCallRecorded", provider_call_id="provider_call_1"),
        ContextCompacted(event="ContextCompacted", compaction_id="compact_1"),
    ]

    for event in events:
        assert not should_push_sse(event)
        assert not routes_to_case(event, "case_1")
        assert not routes_to_workspace(event)


def test_memory_candidate_routes_workspace_and_case_when_case_id_present() -> None:
    event = MemoryCandidateExtracted(
        event="MemoryCandidateExtracted",
        candidate_id="candidate_1",
        case_id="case_1",
    )

    assert routes_to_case(event, "case_1")
    assert routes_to_workspace(event)


def test_capability_degraded_and_security_refusal_visibility_rules() -> None:
    case_degradation = CapabilityDegraded(
        event="CapabilityDegraded",
        degradation_id="deg_1",
        capability="asr",
        reason="timeout",
        case_id="case_1",
    )
    workspace_degradation = CapabilityDegraded(
        event="CapabilityDegraded",
        degradation_id="deg_2",
        capability="asr",
        reason="timeout",
    )
    security_refusal = SecurityRefusal(
        event="SecurityRefusal",
        security_refusal_id="sec_1",
        route="/api/fs",
        reason="outside roots",
    )

    assert routes_to_case(case_degradation, "case_1")
    assert routes_to_workspace(case_degradation)
    assert not routes_to_case(workspace_degradation, "case_1")
    assert routes_to_workspace(workspace_degradation)
    assert not routes_to_case(security_refusal, "case_1")
    assert routes_to_workspace(security_refusal)
