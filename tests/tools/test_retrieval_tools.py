from __future__ import annotations

from array import array
from pathlib import Path
from typing import Any

from sqlalchemy.engine import Connection

from contracts.case import CaseState
from contracts.provider import ProviderError, ProviderResult
from providers import EMBEDDING_TEXT
from providers.gateway import ProviderGatewayResult
from storage import schema
from storage.db import create_workspace_engine
from storage.repositories._json import dump_json, load_json
from tools import ToolExecutionContext, build_default_tool_registry, tool_specs
from tools.retrieval import search_candidates
from tools.specs import RetrievalSearchCandidatesInput

NOW = "2026-07-05T00:00:00+00:00"


def test_retrieval_search_candidates_creates_pack_with_mock_embedding(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_clip(connection, "asset_1", "clip_1", "product closeup", [1.0, 0.0])
        result = search_candidates(
            RetrievalSearchCandidatesInput(),
            _context(
                connection,
                metadata={"embedding_gateway": _EmbeddingGateway([[1.0, 0.0]])},
            ),
        )
        row = connection.execute(schema.candidate_packs.select()).one()._mapping

    assert result.status == "succeeded"
    assert result.data["degraded"] is False
    assert result.events[-1]["event"] == "CandidatePackCreated"
    assert result.events[0]["event"] == "ProviderCallRecorded"
    assert row["candidate_pack_id"] == result.data["candidate_pack_id"]
    assert load_json(row["slots"])[0]["candidates"][0]["clip_id"] == "clip_1"


def test_retrieval_search_candidates_degrades_to_keyword_when_embedding_fails(
    tmp_path: Path,
) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_clip(connection, "asset_1", "clip_1", "product closeup", [1.0, 0.0])
        result = search_candidates(
            RetrievalSearchCandidatesInput(),
            _context(
                connection,
                metadata={"embedding_gateway": _EmbeddingGateway(errors=True)},
            ),
        )

    assert result.status == "succeeded"
    assert result.data["degraded"] is True
    assert [event["event"] for event in result.events] == [
        "ProviderCallRecorded",
        "CapabilityDegraded",
        "CandidatePackCreated",
    ]
    pack = result.data["candidate_pack"]
    assert pack["slots"][0]["candidates"][0]["score"]["vector_rank"] == 0


def test_retrieval_tool_is_registered_with_prd_preconditions() -> None:
    registry = build_default_tool_registry()
    names = {spec.name for spec in tool_specs()}

    assert "retrieval.search_candidates" in names
    spec = registry.require("retrieval.search_candidates").spec
    assert spec.requires_artifacts == [
        "audio_plan_confirmed",
        "cut_plan_exists",
        "usable_asset_exists",
    ]
    assert spec.emits_events == [
        "CandidatePackCreated",
        "CapabilityDegraded",
        "ProviderCallRecorded",
    ]


def _engine(tmp_path: Path):
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)
        connection.execute(
            schema.projects.insert().values(
                project_id="project_1",
                name="Project",
                status="active",
                defaults="{}",
                created_at=NOW,
                updated_at=NOW,
            )
        )
        connection.execute(
            schema.cases.insert().values(
                case_id="case_1",
                project_id="project_1",
                name="Case",
                state_version=0,
                status="active",
                timeline_validated=False,
                rough_cut_approved=False,
                running_jobs="[]",
                brief='{"goal": "test", "confirmed_facts": []}',
                selected_asset_ids="[]",
                disabled_asset_ids="[]",
                scratch_memory="{}",
            )
        )
    return engine


def _context(
    connection: Connection,
    *,
    metadata: dict[str, Any] | None = None,
) -> ToolExecutionContext:
    return ToolExecutionContext(
        tool_call_id="tc_1",
        turn_id="turn_1",
        case_state=CaseState.model_validate(
            {
                "case_id": "case_1",
                "project_id": "project_1",
                "name": "Case",
                "brief": {"goal": "test", "confirmed_facts": []},
                "audio_plan": {"mode": "tts"},
                "cut_plan": {
                    "schema": "CutPlan.v1",
                    "slots": [
                        {
                            "slot_id": "slot_1",
                            "brief": "product closeup",
                            "target_duration_sec": [1.0, 4.0],
                        }
                    ],
                    "total_target_duration_sec": 3.0,
                },
                "selected_asset_ids": [],
                "disabled_asset_ids": [],
                "scratch_memory": {},
            }
        ),
        readonly_connection=connection,
        created_at=NOW,
        metadata=metadata or {},
    )


def _seed_clip(
    connection: Connection,
    asset_id: str,
    clip_id: str,
    summary: str,
    vector: list[float],
) -> None:
    annotation_id = f"ann_{asset_id}"
    connection.execute(
        schema.assets.insert().values(
            asset_id=asset_id,
            storage_mode="reference",
            object_hash=None,
            reference_path=f"/tmp/{asset_id}.mp4",
            kind="video",
            source="local_path",
            filename=f"{asset_id}.mp4",
            hash=f"hash_{asset_id}",
            mtime=1,
            size=1,
            probe=dump_json({"duration_sec": 10.0, "fps": 30.0}),
            proxy_object_hash=None,
            ingest_status="indexed",
            annotation_status="completed",
            annotation_pass="cheap",
            index_status="ready",
            usable=True,
            failure=None,
        )
    )
    connection.execute(
        schema.project_asset_links.insert().values(
            project_id="project_1",
            asset_id=asset_id,
            enabled=True,
            linked_at=NOW,
            note="",
        )
    )
    document = {
        "schema": "AnnotationDocument.v1",
        "annotation_id": annotation_id,
        "asset_id": asset_id,
        "asset_kind": "video",
        "status": "completed",
        "generator": {"pipeline_version": "annotation.video.v1", "pass": "cheap"},
        "clips": [],
        "quality_events": [],
        "created_at": NOW,
    }
    connection.execute(
        schema.annotations_table.insert().values(
            annotation_id=annotation_id,
            asset_id=asset_id,
            schema="AnnotationDocument.v1",
            status="completed",
            document_json=dump_json(document),
            created_at=NOW,
            updated_at=NOW,
        )
    )
    connection.execute(
        schema.annotation_clip_projection.insert().values(
            clip_id=clip_id,
            annotation_id=annotation_id,
            asset_id=asset_id,
            start_frame=0,
            end_frame=90,
            role="b_roll_candidate",
            summary=summary,
            keywords_json=dump_json(summary.split()),
            quality_score=0.9,
            usable=True,
            embedding=array("f", vector).tobytes(),
        )
    )
    connection.exec_driver_sql(
        (
            "INSERT INTO clip_fts "
            "(clip_id, summary, keywords, retrieval_sentence, ocr_text) "
            "VALUES (?, ?, ?, ?, ?)"
        ),
        (clip_id, summary, " ".join(summary.split()), summary, ""),
    )


class _EmbeddingGateway:
    def __init__(
        self,
        vectors: list[list[float]] | None = None,
        *,
        errors: bool = False,
    ) -> None:
        self._vectors = list(vectors or [])
        self._errors = errors

    async def call(self, request: Any, **_kwargs: Any) -> ProviderGatewayResult:
        assert request.capability == EMBEDDING_TEXT
        if self._errors:
            result = ProviderResult(
                provider_id="mock_embedding",
                capability=EMBEDDING_TEXT,
                request_id=request.request_id,
                model="mock",
                latency_ms=1,
                error=ProviderError(
                    error_code="embedding_down",
                    message="provider unavailable",
                    retryable=True,
                ),
            )
        else:
            result = ProviderResult(
                provider_id="mock_embedding",
                capability=EMBEDDING_TEXT,
                request_id=request.request_id,
                model="mock",
                latency_ms=1,
                normalized_output={"embedding": self._vectors.pop(0)},
            )
        return ProviderGatewayResult(
            result=result,
            events=(
                {
                    "event": "ProviderCallRecorded",
                    "provider_call_id": request.request_id,
                    "payload": {"status": "failed" if self._errors else "succeeded"},
                },
            ),
        )
