from pydantic import TypeAdapter

from contracts.events import EVENT_CLASSES, EVENT_UNION, event_registry

EVENT_PAYLOADS: dict[str, dict[str, object]] = {
    "DraftCreated": {"event": "DraftCreated", "draft_id": "draft_001"},
    "DraftRenamed": {"event": "DraftRenamed", "draft_id": "draft_001", "name": "New"},
    "DraftCopied": {
        "event": "DraftCopied",
        "draft_id": "draft_002",
        "source_draft_id": "draft_001",
    },
    "DraftTrashed": {"event": "DraftTrashed", "draft_id": "draft_001"},
    "AssetImported": {"event": "AssetImported", "asset_id": "asset_001", "job_id": "job_001"},
    "AssetProbed": {"event": "AssetProbed", "asset_id": "asset_001", "job_id": "job_001"},
    "ProxyGenerated": {"event": "ProxyGenerated", "asset_id": "asset_001", "job_id": "job_001"},
    "AssetInvalidated": {
        "event": "AssetInvalidated",
        "asset_id": "asset_001",
        "job_id": "job_001",
    },
    "AssetIndexReady": {
        "event": "AssetIndexReady",
        "asset_id": "asset_001",
        "payload": {"index_json": {"shots": []}, "thumbnail_object_hash": "thumb_001"},
    },
    "AssetIndexFailed": {
        "event": "AssetIndexFailed",
        "asset_id": "asset_001",
        "payload": {"failure": {"message": "index failed"}},
    },
    "MaterialUnderstandingStarted": {
        "event": "MaterialUnderstandingStarted",
        "asset_id": "asset_001",
        "payload": {"version": 1},
    },
    "MaterialUnderstandingCompleted": {
        "event": "MaterialUnderstandingCompleted",
        "asset_id": "asset_001",
        "payload": {"summary_id": "sum_001", "version": 1},
    },
    "MaterialUnderstandingFailed": {
        "event": "MaterialUnderstandingFailed",
        "asset_id": "asset_001",
        "payload": {"failure": {"message": "understand timeout"}},
    },
    "AssetLinked": {"event": "AssetLinked", "draft_id": "draft_001", "asset_id": "asset_001"},
    "AssetUnlinked": {
        "event": "AssetUnlinked",
        "draft_id": "draft_001",
        "asset_id": "asset_001",
    },
    "DecisionCreated": {
        "event": "DecisionCreated",
        "decision_id": "dec_001",
        "scope_type": "draft",
        "draft_id": "draft_001",
        "base_version": 1,
    },
    "DecisionAnswered": {
        "event": "DecisionAnswered",
        "decision_id": "dec_001",
        "scope_type": "draft",
        "draft_id": "draft_001",
        "base_version": 1,
    },
    "DecisionCancelled": {
        "event": "DecisionCancelled",
        "decision_id": "dec_001",
        "scope_type": "draft",
        "draft_id": "draft_001",
        "base_version": 1,
    },
    "BriefUpdated": {"event": "BriefUpdated", "draft_id": "draft_001", "base_version": 1},
    "ContentPlanUpdated": {
        "event": "ContentPlanUpdated",
        "draft_id": "draft_001",
        "base_version": 1,
    },
    "AudioPlanUpdated": {"event": "AudioPlanUpdated", "draft_id": "draft_001", "base_version": 1},
    "CutPlanUpdated": {"event": "CutPlanUpdated", "draft_id": "draft_001", "base_version": 1},
    "PostprocessPlanUpdated": {
        "event": "PostprocessPlanUpdated",
        "draft_id": "draft_001",
        "base_version": 1,
    },
    "TimelineVersionCreated": {
        "event": "TimelineVersionCreated",
        "draft_id": "draft_001",
        "timeline_version": 2,
        "patch_id": "patch_001",
        "parent_version": 1,
        "base_version": 1,
    },
    "TimelineVersionRestored": {
        "event": "TimelineVersionRestored",
        "draft_id": "draft_001",
        "timeline_version": 1,
        "base_version": 2,
    },
    "TimelineValidated": {
        "event": "TimelineValidated",
        "draft_id": "draft_001",
        "timeline_version": 2,
        "base_version": 2,
    },
    "TimelineValidationFailed": {
        "event": "TimelineValidationFailed",
        "draft_id": "draft_001",
        "timeline_version": 2,
        "base_version": 2,
    },
    "PreviewRendered": {
        "event": "PreviewRendered",
        "draft_id": "draft_001",
        "timeline_version": 2,
        "artifact_id": "prev_001",
    },
    "PreviewViewed": {"event": "PreviewViewed", "draft_id": "draft_001", "preview_id": "prev_001"},
    "ExportCompleted": {
        "event": "ExportCompleted",
        "draft_id": "draft_001",
        "timeline_version": 2,
        "artifact_id": "exp_001",
    },
    "MemoryCandidateExtracted": {
        "event": "MemoryCandidateExtracted",
        "candidate_id": "memcand_001",
        "draft_id": "draft_001",
    },
    "MemoryCandidateDiscarded": {
        "event": "MemoryCandidateDiscarded",
        "candidate_id": "memcand_001",
        "draft_id": "draft_001",
    },
    "MemorySaved": {
        "event": "MemorySaved",
        "memory_id": "mem_001",
        "candidate_id": "memcand_001",
    },
    "JobEnqueued": {
        "event": "JobEnqueued",
        "job_id": "job_001",
        "requested_by_draft_id": "draft_001",
    },
    "JobProgress": {
        "event": "JobProgress",
        "job_id": "job_001",
        "requested_by_draft_id": "draft_001",
        "progress": 0.5,
    },
    "JobSucceeded": {
        "event": "JobSucceeded",
        "job_id": "job_001",
        "requested_by_draft_id": "draft_001",
    },
    "JobFailed": {
        "event": "JobFailed",
        "job_id": "job_001",
        "requested_by_draft_id": "draft_001",
    },
    "JobCancelled": {
        "event": "JobCancelled",
        "job_id": "job_001",
        "requested_by_draft_id": "draft_001",
    },
    "PolicyRefusal": {"event": "PolicyRefusal", "refusal_id": "refusal_001"},
    "ProviderCallRecorded": {
        "event": "ProviderCallRecorded",
        "provider_call_id": "pc_001",
    },
    "ContextCompacted": {"event": "ContextCompacted", "compaction_id": "compact_001"},
    "TurnEnded": {"event": "TurnEnded", "turn_id": "turn_001", "draft_id": "draft_001"},
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
    "DraftCreated": "merge",
    "DraftRenamed": "merge",
    "DraftCopied": "merge",
    "DraftTrashed": "merge",
    "AssetImported": "merge",
    "AssetProbed": "merge",
    "ProxyGenerated": "merge",
    "AssetInvalidated": "merge",
    "AssetIndexReady": "merge",
    "AssetIndexFailed": "merge",
    "MaterialUnderstandingStarted": "merge",
    "MaterialUnderstandingCompleted": "merge",
    "MaterialUnderstandingFailed": "merge",
    "AssetLinked": "merge",
    "AssetUnlinked": "merge",
    "DecisionCreated": "strict",
    "DecisionAnswered": "strict",
    "DecisionCancelled": "strict",
    "BriefUpdated": "strict",
    "ContentPlanUpdated": "strict",
    "AudioPlanUpdated": "strict",
    "CutPlanUpdated": "strict",
    "PostprocessPlanUpdated": "strict",
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
    assert len(registry) == 44
    assert len(EVENT_CLASSES) == 44


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

    assert registry["DraftCreated"].merge_key == ("draft_id",)
    assert registry["AssetLinked"].merge_key == ("draft_id", "asset_id")
    assert registry["PreviewRendered"].merge_key == ("timeline_version", "artifact_id")
    assert registry["JobSucceeded"].merge_key == ("job_id",)
    assert registry["PolicyRefusal"].merge_key == ("refusal_id",)


def test_decision_events_are_scope_sensitive_for_reducer() -> None:
    registry = event_registry()
    for name in ("DecisionCreated", "DecisionAnswered", "DecisionCancelled"):
        event_class = registry[name]
        assert event_class.reducer_version_mode("draft") == "strict"
        assert event_class.reducer_version_mode("workspace") == "merge"
