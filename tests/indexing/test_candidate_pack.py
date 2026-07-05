from __future__ import annotations

from array import array
from pathlib import Path
from typing import Any

from sqlalchemy import update
from sqlalchemy.engine import Connection

from contracts.case import CaseState
from indexing import build_candidate_pack, revalidate_pack
from indexing.rrf import fuse_rankings
from indexing.types import RankedClip
from storage import schema
from storage.db import create_workspace_engine
from storage.repositories._json import dump_json

NOW = "2026-07-05T00:00:00+00:00"


def test_candidate_pack_enforces_scope_selected_and_disabled(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_clip(connection, "asset_good", "clip_good", "product closeup", [1.0, 0.0])
        _seed_clip(
            connection,
            "asset_failed",
            "clip_failed",
            "product closeup failed",
            [1.0, 0.0],
            annotation_status="failed",
        )
        _seed_clip(
            connection,
            "asset_disabled",
            "clip_disabled",
            "product closeup",
            [1.0, 0.0],
        )
        _seed_clip(
            connection,
            "asset_unselected",
            "clip_unselected",
            "product closeup",
            [1.0, 0.0],
        )
    case_state = _case_state(
        selected_asset_ids=["asset_good", "asset_failed", "asset_disabled"],
        disabled_asset_ids=["asset_disabled"],
    )

    pack = build_candidate_pack(
        engine,
        case_state,
        case_state.cut_plan,
        {"slot_1": [1.0, 0.0]},
    )

    assert _candidate_asset_ids(pack) == ["asset_good"]
    assert pack.snapshot.annotation_versions == {
        "asset_good": "ann_asset_good@2026-07-05T00:00:00+00:00"
    }


def test_candidate_pack_caps_each_slot_to_eight(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        for index in range(10):
            _seed_clip(
                connection,
                f"asset_{index}",
                f"clip_{index}",
                "product closeup",
                [1.0, 0.0],
            )

    case_state = _case_state()
    pack = build_candidate_pack(engine, case_state, case_state.cut_plan, {})

    assert len(pack.slots[0].candidates) == 8


def test_rrf_fuses_keyword_and_vector_rankings() -> None:
    fused = fuse_rankings(
        [
            RankedClip("clip_a", rank=1, score=-1.0),
            RankedClip("clip_b", rank=2, score=-0.5),
        ],
        [
            RankedClip("clip_b", rank=1, score=0.99),
            RankedClip("clip_c", rank=2, score=0.8),
        ],
    )

    assert [item.clip_id for item in fused] == ["clip_b", "clip_a", "clip_c"]
    assert fused[0].bm25_rank == 2
    assert fused[0].vector_rank == 1


def test_candidate_pack_filters_duration_and_quality(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_clip(
            connection,
            "asset_good",
            "clip_good",
            "product closeup",
            [1.0, 0.0],
            end_frame=90,
            quality_score=0.9,
        )
        _seed_clip(
            connection,
            "asset_short",
            "clip_short",
            "product closeup",
            [1.0, 0.0],
            end_frame=15,
            quality_score=0.9,
        )
        _seed_clip(
            connection,
            "asset_low_quality",
            "clip_low_quality",
            "product closeup",
            [1.0, 0.0],
            end_frame=90,
            quality_score=0.2,
        )

    case_state = _case_state()
    pack = build_candidate_pack(engine, case_state, case_state.cut_plan, {})

    assert [candidate.clip_id for candidate in pack.slots[0].candidates] == ["clip_good"]


def test_hard_quality_event_overlap_drops_whole_clip(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_clip(
            connection,
            "asset_hard",
            "clip_hard",
            "product closeup",
            [1.0, 0.0],
            hard_events=[
                {
                    "event_id": "q_1",
                    "kind": "blur",
                    "severity": "hard",
                    "start_frame": 10,
                    "end_frame": 20,
                }
            ],
        )
        _seed_clip(connection, "asset_clean", "clip_clean", "product closeup", [1.0, 0.0])

    case_state = _case_state()
    pack = build_candidate_pack(engine, case_state, case_state.cut_plan, {})

    assert [candidate.clip_id for candidate in pack.slots[0].candidates] == ["clip_clean"]


def test_revalidate_pack_removes_disabled_and_marks_scope_changed(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_clip(connection, "asset_good", "clip_good", "product closeup", [1.0, 0.0])
    case_state = _case_state()
    pack = build_candidate_pack(
        engine,
        case_state,
        case_state.cut_plan,
        {"slot_1": [1.0, 0.0]},
    )

    result = revalidate_pack(
        engine,
        _case_state(disabled_asset_ids=["asset_good"]),
        pack,
    )

    assert result.scope_changed is True
    assert [removed.reason for removed in result.removed] == ["asset_or_clip_not_in_scope"]
    assert result.valid_candidates["slot_1"] == ()


def test_revalidate_pack_marks_stale_annotation(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_clip(connection, "asset_good", "clip_good", "product closeup", [1.0, 0.0])
    case_state = _case_state()
    pack = build_candidate_pack(
        engine,
        case_state,
        case_state.cut_plan,
        {"slot_1": [1.0, 0.0]},
    )
    with engine.begin() as connection:
        connection.execute(
            update(schema.annotations_table)
            .where(schema.annotations_table.c.asset_id == "asset_good")
            .values(updated_at="2026-07-05T00:01:00+00:00")
        )

    result = revalidate_pack(engine, case_state, pack)

    assert result.scope_changed is False
    assert result.stale_annotations == ("asset_good",)
    assert [removed.reason for removed in result.removed] == ["stale_annotation"]


def test_revalidate_pack_marks_scope_changed_when_asset_pool_changes(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_clip(connection, "asset_good", "clip_good", "product closeup", [1.0, 0.0])
    case_state = _case_state()
    pack = build_candidate_pack(
        engine,
        case_state,
        case_state.cut_plan,
        {"slot_1": [1.0, 0.0]},
    )
    with engine.begin() as connection:
        _seed_clip(connection, "asset_new", "clip_new", "product closeup", [0.8, 0.2])

    result = revalidate_pack(engine, case_state, pack)

    assert result.scope_changed is True
    assert result.removed == ()


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
    return engine


def _case_state(
    *,
    selected_asset_ids: list[str] | None = None,
    disabled_asset_ids: list[str] | None = None,
) -> CaseState:
    return CaseState.model_validate(
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
            "selected_asset_ids": selected_asset_ids or [],
            "disabled_asset_ids": disabled_asset_ids or [],
            "scratch_memory": {},
        }
    )


def _seed_clip(
    connection: Connection,
    asset_id: str,
    clip_id: str,
    summary: str,
    vector: list[float],
    *,
    annotation_status: str = "completed",
    index_status: str = "ready",
    start_frame: int = 0,
    end_frame: int = 90,
    role: str = "b_roll_candidate",
    quality_score: float = 0.9,
    hard_events: list[dict[str, Any]] | None = None,
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
            annotation_status=annotation_status,
            annotation_pass="cheap",
            index_status=index_status,
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
        "status": "completed" if annotation_status == "completed" else "failed",
        "generator": {"pipeline_version": "annotation.video.v1", "pass": "cheap"},
        "clips": [
            {
                "clip_id": clip_id,
                "source_start_frame": start_frame,
                "source_end_frame": end_frame,
                "role": role,
                "summary": summary,
                "keywords": summary.split(),
                "quality_score": quality_score,
            }
        ],
        "quality_events": hard_events or [],
        "created_at": NOW,
    }
    connection.execute(
        schema.annotations_table.insert().values(
            annotation_id=annotation_id,
            asset_id=asset_id,
            schema="AnnotationDocument.v1",
            status=document["status"],
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
            start_frame=start_frame,
            end_frame=end_frame,
            role=role,
            summary=summary,
            keywords_json=dump_json(summary.split()),
            quality_score=quality_score,
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


def _candidate_asset_ids(pack: Any) -> list[str]:
    return [candidate.asset_id for slot in pack.slots for candidate in slot.candidates]


def test_keyword_and_vector_edge_branches(tmp_path: Path) -> None:
    import numpy as np

    from indexing.keyword import build_match_query, search_bm25
    from indexing.vector import search_cosine

    engine = _engine(tmp_path)
    with engine.connect() as connection:
        # 空 brief → 空查询直接返回
        assert search_bm25(connection, "", limit=5) == []
        # clip_ids 显式空集合 → 提前返回
        assert search_bm25(connection, "产品", limit=5, clip_ids=[]) == []
        # 空向量 / 零向量 → 空
        assert search_cosine(connection, np.array([], dtype=np.float32), limit=5) == []
        assert search_cosine(connection, np.zeros(4, dtype=np.float32), limit=5) == []

    # 查询分词：ASCII+中文二元组去重
    query = build_match_query("产品 closeup 产品")
    assert "closeup" in query


def test_rrf_and_quote_token_helpers() -> None:
    from indexing.keyword import _quote_fts_token, build_match_query
    from indexing.rrf import fuse_rankings

    assert _quote_fts_token('包含"引号"') == '"包含""引号"""'
    assert build_match_query("") == ""

    from indexing.types import RankedClip

    fused = fuse_rankings(
        [RankedClip(clip_id="c1", rank=1, score=0.9), RankedClip(clip_id="c2", rank=2, score=0.5)],
        [RankedClip(clip_id="c2", rank=1, score=0.8)],
        k=1,
    )
    assert fused[0].clip_id == "c2"


def test_vector_search_scopes_and_shape_mismatch(tmp_path: Path) -> None:
    import numpy as np

    from indexing.vector import search_cosine

    engine = _engine(tmp_path)
    with engine.connect() as connection:
        # clip_ids 空集合提前返回；形状不匹配的向量被跳过
        assert search_cosine(connection, np.ones(4, dtype=np.float32), limit=5, clip_ids=[]) == []
        assert (
            search_cosine(connection, np.ones(3, dtype=np.float32), limit=5, clip_ids=["c_x"]) == []
        )
