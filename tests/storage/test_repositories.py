from pathlib import Path

import pytest
from sqlalchemy.exc import IntegrityError

from contracts.transcript import TranscriptDocument, TranscriptUtterance, TranscriptWord, VadSegment
from storage import schema
from storage.db import begin_immediate, create_workspace_engine
from storage.repositories import (
    CasesRepository,
    DecisionsRepository,
    JobsRepository,
    TranscriptsRepository,
)
from storage.repositories.projects import ProjectsRepository

NOW = "2026-07-04T00:00:00+00:00"


def _prepare_workspace(tmp_path: Path) -> Path:
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)
    return tmp_path


def _insert_project_and_case(tmp_path: Path) -> None:
    engine = create_workspace_engine(tmp_path)
    with begin_immediate(engine) as connection:
        ProjectsRepository(connection).insert(
            {
                "project_id": "project_1",
                "name": "Project",
                "status": "active",
                "defaults": {"aspect_ratio": "9:16", "fps": 30},
                "created_at": NOW,
                "updated_at": NOW,
            }
        )
        CasesRepository(connection).insert(
            {
                "case_id": "case_1",
                "project_id": "project_1",
                "name": "Case",
                "state_version": 0,
                "status": "active",
                "pending_decision_id": None,
                "running_jobs": [],
                "last_error": None,
                "brief": {"goal": "test"},
                "content_plan": None,
                "audio_plan": None,
                "cut_plan": None,
                "candidate_pack_id": None,
                "timeline_current_version": None,
                "timeline_validated": False,
                "preview_current_id": None,
                "last_viewed_preview_id": None,
                "rough_cut_approved": False,
                "rough_cut_approved_version": None,
                "postprocess_plan": None,
                "export_current_id": None,
                "selected_asset_ids": [],
                "disabled_asset_ids": [],
                "scratch_memory": {},
            }
        )


def test_case_state_version_optimistic_lock_conflict(tmp_path: Path) -> None:
    workspace = _prepare_workspace(tmp_path)
    _insert_project_and_case(workspace)
    engine = create_workspace_engine(workspace)

    with begin_immediate(engine) as connection:
        repo = CasesRepository(connection)
        assert repo.update_with_state_version("case_1", 0, {"name": "New"}) is None
        conflict = repo.update_with_state_version("case_1", 0, {"name": "Stale"})

    assert conflict is not None
    assert conflict.case_id == "case_1"


def test_decision_pending_tool_call_cas_replays_once(tmp_path: Path) -> None:
    workspace = _prepare_workspace(tmp_path)
    engine = create_workspace_engine(workspace)
    with begin_immediate(engine) as connection:
        repo = DecisionsRepository(connection)
        repo.insert(
            {
                "decision_id": "decision_1",
                "scope_type": "workspace",
                "project_id": None,
                "case_id": None,
                "type": "generic",
                "question": "Run?",
                "options": [],
                "status": "answered",
                "answer": {"answered_via": "button", "option_id": "yes"},
                "pending_tool_call": {
                    "tool_name": "x.y",
                    "arguments": {},
                    "idempotency_key": "idem",
                    "argument_fingerprint": "fp",
                },
                "pending_tool_call_status": "approved",
                "consumed_at": None,
                "replayed_tool_call_id": None,
                "blocking": False,
                "created_by_tool_call_id": None,
            }
        )
        assert repo.mark_pending_tool_call_replayed(
            "decision_1", consumed_at=NOW, replayed_tool_call_id="tc_replay"
        )
        assert not repo.mark_pending_tool_call_replayed(
            "decision_1", consumed_at=NOW, replayed_tool_call_id="tc_replay_2"
        )


def test_jobs_claim_only_one_worker_and_unique_idempotency(tmp_path: Path) -> None:
    workspace = _prepare_workspace(tmp_path)
    engine = create_workspace_engine(workspace)
    job = {
        "job_id": "job_1",
        "kind": "annotation",
        "status": "pending",
        "project_id": None,
        "case_id": None,
        "requested_by_case_id": None,
        "asset_id": None,
        "idempotency_key": "asset_1:cheap",
        "payload_json": {},
        "result_json": None,
        "error_json": None,
        "attempts": 0,
        "max_retries": 2,
        "next_run_at": NOW,
        "progress": None,
        "worker_id": None,
        "heartbeat_at": None,
        "created_at": NOW,
        "started_at": None,
        "finished_at": None,
    }

    with begin_immediate(engine) as connection:
        JobsRepository(connection).insert(job)

    with begin_immediate(engine) as connection:
        assert JobsRepository(connection).claim_next(worker_id="worker_1", now=NOW) == "job_1"

    with begin_immediate(engine) as connection:
        assert JobsRepository(connection).claim_next(worker_id="worker_2", now=NOW) is None
        with pytest.raises(IntegrityError):
            JobsRepository(connection).insert({**job, "job_id": "job_2"})


def test_jobs_heartbeat_and_stale_running_reset(tmp_path: Path) -> None:
    workspace = _prepare_workspace(tmp_path)
    engine = create_workspace_engine(workspace)
    with begin_immediate(engine) as connection:
        repo = JobsRepository(connection)
        repo.insert(
            {
                "job_id": "job_1",
                "kind": "render_preview",
                "status": "running",
                "project_id": None,
                "case_id": None,
                "requested_by_case_id": None,
                "asset_id": None,
                "idempotency_key": "preview",
                "payload_json": {},
                "result_json": None,
                "error_json": None,
                "attempts": 0,
                "max_retries": 2,
                "next_run_at": NOW,
                "progress": 0.1,
                "worker_id": "worker_1",
                "heartbeat_at": "2026-07-04T00:00:01+00:00",
                "created_at": NOW,
                "started_at": NOW,
                "finished_at": None,
            }
        )
        assert repo.heartbeat(
            "job_1",
            worker_id="worker_1",
            now="2026-07-04T00:00:02+00:00",
            progress=0.5,
        )
        assert (
            repo.reset_stale_running(
                heartbeat_before="2026-07-04T00:00:03+00:00",
                next_run_at="2026-07-04T00:00:04+00:00",
            )
            == 1
        )
        reset = repo.get("job_1")

    assert reset is not None
    assert reset["status"] == "pending"
    assert reset["worker_id"] is None


def test_transcripts_repository_persists_document_json(tmp_path: Path) -> None:
    workspace = _prepare_workspace(tmp_path)
    _insert_project_and_case(workspace)
    engine = create_workspace_engine(workspace)
    with begin_immediate(engine) as connection:
        connection.execute(
            schema.assets.insert().values(
                asset_id="asset_1",
                storage_mode="reference",
                object_hash=None,
                reference_path="/tmp/source.mp4",
                kind="video",
                source="local_path",
                filename="source.mp4",
                hash="hash",
                mtime=1,
                size=1,
                probe=None,
                proxy_object_hash=None,
                ingest_status="imported",
                annotation_status="pending",
                annotation_pass="none",
                index_status="none",
                usable=True,
                failure=None,
            )
        )
        repo = TranscriptsRepository(connection)
        repo.insert_document(
            TranscriptDocument(
                transcript_id="tr_1",
                asset_id="asset_1",
                language="zh",
                provider_id="aliyun_paraformer_v2",
                raw_preserved=True,
                utterances=[
                    TranscriptUtterance(
                        utterance_id="u_001",
                        text="呃",
                        start_ms=0,
                        end_ms=100,
                        words=[TranscriptWord(w="呃", start_ms=0, end_ms=100, type="filler")],
                    )
                ],
                vad_segments=[VadSegment(start_ms=100, end_ms=800, kind="silence")],
            )
        )
        row = repo.get("tr_1")

    assert row is not None
    assert row["utterances"][0]["words"][0]["type"] == "filler"
    assert row["vad_segments"] == [{"start_ms": 100, "end_ms": 800, "kind": "silence"}]
