from pydantic import TypeAdapter

from contracts.events import EVENT_CLASSES, EVENT_UNION, event_registry

EVENT_PAYLOADS: dict[str, dict[str, object]] = {
    "ProjectCreated": {"event": "ProjectCreated", "project_id": "project_001"},
    "ProjectRenamed": {"event": "ProjectRenamed", "project_id": "project_001", "name": "New"},
    "ProjectTrashed": {"event": "ProjectTrashed", "project_id": "project_001"},
    "ProjectCopied": {
        "event": "ProjectCopied",
        "project_id": "project_002",
        "source_project_id": "project_001",
    },
    "CaseCreated": {"event": "CaseCreated", "project_id": "project_001", "case_id": "case_001"},
    "CaseRenamed": {"event": "CaseRenamed", "case_id": "case_001", "name": "New"},
    "CaseCopied": {"event": "CaseCopied", "case_id": "case_002", "source_case_id": "case_001"},
    "CaseMoved": {
        "event": "CaseMoved",
        "case_id": "case_001",
        "source_project_id": "project_001",
        "target_project_id": "project_002",
    },
    "CaseClosed": {"event": "CaseClosed", "case_id": "case_001"},
    "CaseTrashed": {"event": "CaseTrashed", "case_id": "case_001"},
    "AssetImported": {"event": "AssetImported", "asset_id": "asset_001", "job_id": "job_001"},
    "AssetProbed": {"event": "AssetProbed", "asset_id": "asset_001", "job_id": "job_001"},
    "ProxyGenerated": {"event": "ProxyGenerated", "asset_id": "asset_001", "job_id": "job_001"},
    "AnnotationCompleted": {
        "event": "AnnotationCompleted",
        "asset_id": "asset_001",
        "job_id": "job_001",
        "annotation_id": "ann_001",
    },
    "AnnotationFailed": {
        "event": "AnnotationFailed",
        "asset_id": "asset_001",
        "job_id": "job_001",
    },
    "AssetInvalidated": {
        "event": "AssetInvalidated",
        "asset_id": "asset_001",
        "job_id": "job_001",
    },
    "AssetLinked": {"event": "AssetLinked", "project_id": "project_001", "asset_id": "asset_001"},
    "AssetUnlinked": {
        "event": "AssetUnlinked",
        "project_id": "project_001",
        "asset_id": "asset_001",
    },
    "CaseAssetScopeChanged": {
        "event": "CaseAssetScopeChanged",
        "case_id": "case_001",
        "base_version": 1,
    },
    "DecisionCreated": {
        "event": "DecisionCreated",
        "decision_id": "dec_001",
        "scope_type": "case",
        "case_id": "case_001",
        "base_version": 1,
    },
    "DecisionAnswered": {
        "event": "DecisionAnswered",
        "decision_id": "dec_001",
        "scope_type": "case",
        "case_id": "case_001",
        "base_version": 1,
    },
    "DecisionCancelled": {
        "event": "DecisionCancelled",
        "decision_id": "dec_001",
        "scope_type": "case",
        "case_id": "case_001",
        "base_version": 1,
    },
    "BriefUpdated": {"event": "BriefUpdated", "case_id": "case_001", "base_version": 1},
    "ContentPlanUpdated": {
        "event": "ContentPlanUpdated",
        "case_id": "case_001",
        "base_version": 1,
    },
    "AudioPlanUpdated": {"event": "AudioPlanUpdated", "case_id": "case_001", "base_version": 1},
    "CutPlanUpdated": {"event": "CutPlanUpdated", "case_id": "case_001", "base_version": 1},
    "PostprocessPlanUpdated": {
        "event": "PostprocessPlanUpdated",
        "case_id": "case_001",
        "base_version": 1,
    },
    "CandidatePackCreated": {
        "event": "CandidatePackCreated",
        "case_id": "case_001",
        "candidate_pack_id": "cand_001",
        "base_version": 1,
    },
    "TimelineVersionCreated": {
        "event": "TimelineVersionCreated",
        "case_id": "case_001",
        "timeline_version": 2,
        "patch_id": "patch_001",
        "parent_version": 1,
        "base_version": 1,
    },
    "TimelineVersionRestored": {
        "event": "TimelineVersionRestored",
        "case_id": "case_001",
        "timeline_version": 1,
        "base_version": 2,
    },
    "TimelineValidated": {
        "event": "TimelineValidated",
        "case_id": "case_001",
        "timeline_version": 2,
        "base_version": 2,
    },
    "TimelineValidationFailed": {
        "event": "TimelineValidationFailed",
        "case_id": "case_001",
        "timeline_version": 2,
        "base_version": 2,
    },
    "PreviewRendered": {
        "event": "PreviewRendered",
        "case_id": "case_001",
        "timeline_version": 2,
        "artifact_id": "prev_001",
    },
    "PreviewViewed": {"event": "PreviewViewed", "case_id": "case_001", "preview_id": "prev_001"},
    "ExportCompleted": {
        "event": "ExportCompleted",
        "case_id": "case_001",
        "timeline_version": 2,
        "artifact_id": "exp_001",
    },
    "MemoryCandidateExtracted": {
        "event": "MemoryCandidateExtracted",
        "candidate_id": "memcand_001",
        "case_id": "case_001",
    },
    "MemoryCandidateDiscarded": {
        "event": "MemoryCandidateDiscarded",
        "candidate_id": "memcand_001",
        "case_id": "case_001",
    },
    "MemorySaved": {
        "event": "MemorySaved",
        "memory_id": "mem_001",
        "candidate_id": "memcand_001",
    },
    "JobEnqueued": {
        "event": "JobEnqueued",
        "job_id": "job_001",
        "requested_by_case_id": "case_001",
    },
    "JobProgress": {
        "event": "JobProgress",
        "job_id": "job_001",
        "requested_by_case_id": "case_001",
        "progress": 0.5,
    },
    "JobSucceeded": {
        "event": "JobSucceeded",
        "job_id": "job_001",
        "requested_by_case_id": "case_001",
    },
    "JobFailed": {
        "event": "JobFailed",
        "job_id": "job_001",
        "requested_by_case_id": "case_001",
    },
    "JobCancelled": {
        "event": "JobCancelled",
        "job_id": "job_001",
        "requested_by_case_id": "case_001",
    },
    "PolicyRefusal": {"event": "PolicyRefusal", "refusal_id": "refusal_001"},
    "ProviderCallRecorded": {
        "event": "ProviderCallRecorded",
        "provider_call_id": "pc_001",
    },
    "ContextCompacted": {"event": "ContextCompacted", "compaction_id": "compact_001"},
    "TurnEnded": {"event": "TurnEnded", "turn_id": "turn_001", "case_id": "case_001"},
    "CapabilityDegraded": {
        "event": "CapabilityDegraded",
        "degradation_id": "deg_001",
        "capability": "asr.transcribe",
        "provider_id": "aliyun",
        "reason": "timeout",
        "fallback": "volcengine",
    },
    "SecurityRefusal": {
        "event": "SecurityRefusal",
        "security_refusal_id": "sec_001",
        "route": "/api/fs",
        "path": "/etc/passwd",
        "reason": "outside roots",
    },
}


EXPECTED_VERSION_MODES: dict[str, str] = {
    "ProjectCreated": "merge",
    "ProjectRenamed": "merge",
    "ProjectTrashed": "merge",
    "ProjectCopied": "merge",
    "CaseCreated": "merge",
    "CaseRenamed": "merge",
    "CaseCopied": "merge",
    "CaseMoved": "merge",
    "CaseClosed": "merge",
    "CaseTrashed": "merge",
    "AssetImported": "merge",
    "AssetProbed": "merge",
    "ProxyGenerated": "merge",
    "AnnotationCompleted": "merge",
    "AnnotationFailed": "merge",
    "AssetInvalidated": "merge",
    "AssetLinked": "merge",
    "AssetUnlinked": "merge",
    "CaseAssetScopeChanged": "strict",
    "DecisionCreated": "strict",
    "DecisionAnswered": "strict",
    "DecisionCancelled": "strict",
    "BriefUpdated": "strict",
    "ContentPlanUpdated": "strict",
    "AudioPlanUpdated": "strict",
    "CutPlanUpdated": "strict",
    "PostprocessPlanUpdated": "strict",
    "CandidatePackCreated": "strict",
    "TimelineVersionCreated": "strict",
    "TimelineVersionRestored": "strict",
    "TimelineValidated": "strict",
    "TimelineValidationFailed": "strict",
    "PreviewRendered": "merge",
    "PreviewViewed": "merge",
    "ExportCompleted": "merge",
    "MemoryCandidateExtracted": "merge",
    "MemoryCandidateDiscarded": "merge",
    "MemorySaved": "merge",
    "JobEnqueued": "merge",
    "JobProgress": "merge",
    "JobSucceeded": "merge",
    "JobFailed": "merge",
    "JobCancelled": "merge",
    "PolicyRefusal": "merge",
    "ProviderCallRecorded": "merge",
    "ContextCompacted": "merge",
    "TurnEnded": "merge",
    "CapabilityDegraded": "merge",
    "SecurityRefusal": "merge",
}


def test_event_registry_matches_prd_event_table() -> None:
    registry = event_registry()
    assert set(registry) == set(EVENT_PAYLOADS)
    assert len(registry) == 49
    assert len(EVENT_CLASSES) == 49


def test_each_event_discriminator_parses_to_expected_class() -> None:
    adapter = TypeAdapter(EVENT_UNION)
    registry = event_registry()

    for event_name, payload in EVENT_PAYLOADS.items():
        parsed = adapter.validate_python(payload)
        assert type(parsed) is registry[event_name]
        assert parsed.event == event_name


def test_version_modes_and_merge_keys_match_authoritative_table() -> None:
    registry = event_registry()
    for event_name, version_mode in EXPECTED_VERSION_MODES.items():
        assert registry[event_name].version_mode == version_mode

    assert registry["ProjectCreated"].merge_key == ("project_id",)
    assert registry["CaseCreated"].merge_key == ("case_id",)
    assert registry["AssetLinked"].merge_key == ("project_id", "asset_id")
    assert registry["PreviewRendered"].merge_key == ("timeline_version", "artifact_id")
    assert registry["JobSucceeded"].merge_key == ("job_id",)
    assert registry["PolicyRefusal"].merge_key == ("refusal_id",)


def test_decision_events_are_scope_sensitive_for_reducer() -> None:
    registry = event_registry()
    for name in ("DecisionCreated", "DecisionAnswered", "DecisionCancelled"):
        event_class = registry[name]
        assert event_class.reducer_version_mode("case") == "strict"
        assert event_class.reducer_version_mode("project") == "merge"
        assert event_class.reducer_version_mode("workspace") == "merge"
