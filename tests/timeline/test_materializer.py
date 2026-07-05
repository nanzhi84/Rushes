from __future__ import annotations

from array import array
from pathlib import Path
from typing import Any

import pytest
from sqlalchemy.engine import Connection

from contracts.candidate import CandidatePack
from contracts.case import CaseState
from contracts.timeline import TimelineMediaClip
from storage import schema
from storage.db import create_workspace_engine
from storage.repositories._json import dump_json
from timeline import MaterializationError, materialize_from_selection
from timeline.materializer import (
    _asset_total_frames,
    _clean_spans,
    _hard_quality_events,
    _int_value,
    _narration_ms_range,
    _probe_payload,
    _project_fps,
    _source_fps,
)

NOW = "2026-07-05T00:00:00+00:00"


def test_materializer_builds_contiguous_primary_track(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_clip(connection, "asset_1", "clip_1", end_frame=45)
        _seed_clip(connection, "asset_2", "clip_2", end_frame=60)
    timeline = materialize_from_selection(
        engine,
        _case_state(slot_windows=[("slot_1", [1.0, 1.5]), ("slot_2", [1.0, 2.0])]),
        _pack(
            [
                ("slot_1", "cand_1", "asset_1", "clip_1"),
                ("slot_2", "cand_2", "asset_2", "clip_2"),
            ]
        ),
        [
            {"slot_id": "slot_1", "candidate_id": "cand_1"},
            {"slot_id": "slot_2", "candidate_id": "cand_2"},
        ],
    )

    visual = _media_track(timeline, "visual_base")
    assert [(clip.timeline_start_frame, clip.timeline_end_frame) for clip in visual] == [
        (0, 45),
        (45, 105),
    ]
    assert timeline.duration_frames == 105


def test_materializer_avoids_hard_quality_event_span(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_clip(
            connection,
            "asset_1",
            "clip_1",
            end_frame=120,
            hard_events=[
                {
                    "event_id": "q_1",
                    "kind": "blur",
                    "severity": "hard",
                    "start_frame": 30,
                    "end_frame": 90,
                }
            ],
        )
    timeline = materialize_from_selection(
        engine,
        _case_state(slot_windows=[("slot_1", [1.0, 4.0])]),
        _pack([("slot_1", "cand_1", "asset_1", "clip_1")]),
        [{"slot_id": "slot_1", "candidate_id": "cand_1"}],
    )

    clip = _media_track(timeline, "visual_base")[0]
    assert (clip.source_start_frame, clip.source_end_frame) == (0, 30)
    assert clip.timeline_end_frame == 30


def test_materializer_converts_source_fps_to_project_fps_once(tmp_path: Path) -> None:
    engine = _engine(tmp_path, fps=30)
    with engine.begin() as connection:
        _seed_clip(
            connection,
            "asset_1",
            "clip_1",
            end_frame=300,
            probe={"duration_sec": 10.0, "fps": 29.97},
        )
    timeline = materialize_from_selection(
        engine,
        _case_state(slot_windows=[("slot_1", [0.5, 1.0])]),
        _pack([("slot_1", "cand_1", "asset_1", "clip_1")]),
        [{"slot_id": "slot_1", "candidate_id": "cand_1"}],
    )

    clip = _media_track(timeline, "visual_base")[0]
    assert clip.timeline_end_frame == 30
    assert (clip.source_start_frame, clip.source_end_frame) == (0, 30)


def test_materializer_crops_long_clean_span_to_slot_window(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_clip(connection, "asset_1", "clip_1", end_frame=300)
    timeline = materialize_from_selection(
        engine,
        _case_state(slot_windows=[("slot_1", [1.0, 2.0])]),
        _pack([("slot_1", "cand_1", "asset_1", "clip_1")]),
        [{"slot_id": "slot_1", "candidate_id": "cand_1"}],
    )

    clip = _media_track(timeline, "visual_base")[0]
    assert clip.timeline_end_frame == 60
    assert (clip.source_start_frame, clip.source_end_frame) == (0, 60)


def test_materializer_syncs_voiceover_clip_from_narration_ref(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_clip(connection, "asset_1", "clip_1", end_frame=45)
        _seed_asset(connection, "asset_vo", kind="voiceover", probe={"duration_sec": 1.5})
    case_state = _case_state(
        audio_plan={"mode": "tts", "voiceover_asset_id": "asset_vo"},
        slot_windows=[("slot_1", [1.0, 1.5])],
        narration_refs={"slot_1": {"start_ms": 0, "end_ms": 1500, "utterance_ids": ["u_1"]}},
    )

    timeline = materialize_from_selection(
        engine,
        case_state,
        _pack([("slot_1", "cand_1", "asset_1", "clip_1")]),
        [{"slot_id": "slot_1", "candidate_id": "cand_1"}],
    )

    visual = _media_track(timeline, "visual_base")[0]
    voiceover = _media_track(timeline, "voiceover")[0]
    assert (voiceover.timeline_start_frame, voiceover.timeline_end_frame) == (
        visual.timeline_start_frame,
        visual.timeline_end_frame,
    )
    assert (voiceover.source_start_frame, voiceover.source_end_frame) == (0, 45)


def test_materializer_syncs_voiceover_clip_from_transcript_utterance_ids(
    tmp_path: Path,
) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_clip(connection, "asset_1", "clip_1", end_frame=90)
        _seed_asset(
            connection,
            "asset_vo",
            kind="voiceover",
            probe={"fps": 30.0, "frame_count": 120},
        )
        connection.execute(
            schema.transcripts.insert().values(
                transcript_id="tr_vo",
                asset_id="asset_vo",
                provider_id="local_fixture",
                raw_preserved=True,
                utterances=dump_json(
                    [
                        {"utterance_id": "u_1", "start_ms": 0, "end_ms": 400},
                        {"utterance_id": "u_2", "start_ms": 500, "end_ms": 1200},
                        {"utterance_id": "u_3", "start_ms": 1300, "end_ms": 2500},
                        {"utterance_id": "bad", "start_ms": 3000, "end_ms": 3000},
                    ]
                ),
                vad_segments=dump_json([]),
            )
        )
    case_state = _case_state(
        audio_plan={"mode": "tts", "voiceover_asset_id": "asset_vo"},
        slot_windows=[("slot_1", [1.0, 3.0])],
        narration_refs={
            "slot_1": {
                "transcript_id": "tr_vo",
                "utterance_ids": ["u_2", "u_3"],
            }
        },
    )

    timeline = materialize_from_selection(
        engine,
        case_state,
        _pack([("slot_1", "cand_1", "asset_1", "clip_1")]),
        [{"slot_id": "slot_1", "candidate_id": "cand_1"}],
    )

    voiceover = _media_track(timeline, "voiceover")[0]
    assert (voiceover.source_start_frame, voiceover.source_end_frame) == (15, 75)
    assert voiceover.effects == [
        {
            "kind": "narration_ref",
            "narration_ref": {
                "transcript_id": "tr_vo",
                "utterance_ids": ["u_2", "u_3"],
            },
        }
    ]


def test_materializer_omits_voiceover_when_transcript_ref_cannot_resolve(
    tmp_path: Path,
) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_clip(connection, "asset_1", "clip_1", end_frame=45)
        _seed_asset(connection, "asset_vo", kind="voiceover", probe={"duration_sec": 1.5})
        connection.execute(
            schema.transcripts.insert().values(
                transcript_id="tr_vo",
                asset_id="asset_vo",
                provider_id="local_fixture",
                raw_preserved=True,
                utterances=dump_json({"not": "a list"}),
                vad_segments=dump_json([]),
            )
        )
    case_state = _case_state(
        audio_plan={"mode": "tts", "voiceover_asset_id": "asset_vo"},
        slot_windows=[("slot_1", [1.0, 1.5])],
        narration_refs={"slot_1": {"transcript_id": "tr_vo", "utterance_ids": ["u_1"]}},
    )

    timeline = materialize_from_selection(
        engine,
        case_state,
        _pack([("slot_1", "cand_1", "asset_1", "clip_1")]),
        [{"slot_id": "slot_1", "candidate_id": "cand_1"}],
    )

    assert _media_track(timeline, "voiceover") == []


def test_materializer_narration_range_rejects_invalid_transcript_refs(
    tmp_path: Path,
) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_asset(connection, "asset_vo", kind="voiceover", probe={"duration_sec": 2.0})

        assert _narration_ms_range(
            connection,
            "asset_vo",
            {"transcript_id": 123, "utterance_ids": ["u_1"]},
        ) == (None, None)
        assert _narration_ms_range(
            connection,
            "asset_vo",
            {"transcript_id": "tr_missing", "utterance_ids": []},
        ) == (None, None)
        assert _narration_ms_range(
            connection,
            "asset_vo",
            {"transcript_id": "tr_missing", "utterance_ids": ["u_1"]},
        ) == (None, None)


def test_materializer_omits_voiceover_when_narration_exceeds_asset_frames(
    tmp_path: Path,
) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_clip(connection, "asset_1", "clip_1", end_frame=45)
        _seed_asset(
            connection,
            "asset_vo",
            kind="voiceover",
            probe={"fps": 30.0, "frame_count": 10},
        )
    case_state = _case_state(
        audio_plan={"mode": "tts", "voiceover_asset_id": "asset_vo"},
        slot_windows=[("slot_1", [1.0, 1.5])],
        narration_refs={"slot_1": {"start_ms": 1000, "end_ms": 2000}},
    )

    timeline = materialize_from_selection(
        engine,
        case_state,
        _pack([("slot_1", "cand_1", "asset_1", "clip_1")]),
        [{"slot_id": "slot_1", "candidate_id": "cand_1"}],
    )

    assert _media_track(timeline, "voiceover") == []


def test_materializer_uses_image_duration_window_with_single_source_frame(
    tmp_path: Path,
) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_clip(
            connection,
            "asset_image",
            "clip_image",
            start_frame=10,
            end_frame=50,
            kind="image",
            role="image_candidate",
        )

    timeline = materialize_from_selection(
        engine,
        _case_state(slot_windows=[("slot_1", [1.0, 2.0])]),
        _pack([("slot_1", "cand_1", "asset_image", "clip_image")]),
        [{"slot_id": "slot_1", "candidate_id": "cand_1"}],
    )

    clip = _media_track(timeline, "visual_base")[0]
    assert clip.role == "image"
    assert (clip.source_start_frame, clip.source_end_frame) == (10, 11)
    assert (clip.timeline_start_frame, clip.timeline_end_frame) == (0, 60)


def test_materializer_warns_when_no_clean_span_remains(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_clip(
            connection,
            "asset_1",
            "clip_1",
            end_frame=90,
            hard_events=[
                {
                    "event_id": "q_1",
                    "kind": "blur",
                    "severity": "hard",
                    "start_frame": 0,
                    "end_frame": 90,
                }
            ],
        )

    timeline = materialize_from_selection(
        engine,
        _case_state(slot_windows=[("slot_1", [1.0, 3.0])]),
        _pack([("slot_1", "cand_1", "asset_1", "clip_1")]),
        [{"slot_id": "slot_1", "candidate_id": "cand_1"}],
    )

    clip = _media_track(timeline, "visual_base")[0]
    assert (clip.source_start_frame, clip.source_end_frame) == (0, 90)
    assert timeline.validation_report is not None
    assert timeline.validation_report.checks[0]["code"] == "timeline.materialize.no_clean_span"


def test_materializer_warns_when_clean_span_is_shorter_than_slot_minimum(
    tmp_path: Path,
) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_clip(connection, "asset_1", "clip_1", end_frame=15)

    timeline = materialize_from_selection(
        engine,
        _case_state(slot_windows=[("slot_1", [1.0, 2.0])]),
        _pack([("slot_1", "cand_1", "asset_1", "clip_1")]),
        [{"slot_id": "slot_1", "candidate_id": "cand_1"}],
    )

    assert timeline.duration_frames == 15
    assert timeline.validation_report is not None
    assert timeline.validation_report.checks[0]["code"] == "timeline.materialize.short_clean_span"


def test_materializer_defaults_project_and_source_fps_to_30(tmp_path: Path) -> None:
    engine = _engine(tmp_path, fps=0)
    with engine.begin() as connection:
        _seed_clip(
            connection,
            "asset_1",
            "clip_1",
            end_frame=60,
            probe={"duration_sec": 2.0},
        )

    timeline = materialize_from_selection(
        engine,
        _case_state(slot_windows=[("slot_1", [1.0, 3.0])]),
        _pack([("slot_1", "cand_1", "asset_1", "clip_1")]),
        [{"slot_id": "slot_1", "candidate_id": "cand_1"}],
    )

    assert timeline.fps == 30
    assert timeline.duration_frames == 60


def test_materializer_helper_defaults_and_parsing_branches(tmp_path: Path) -> None:
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)
        assert _project_fps(connection, "missing_project") == 30

    assert _clean_spans(0, 10, [(20, 30)]) == [(0, 10)]
    assert _hard_quality_events("[]") == []
    assert _hard_quality_events(dump_json({"quality_events": "bad"})) == []
    assert (
        _hard_quality_events(
            dump_json(
                {
                    "quality_events": [
                        {"severity": "soft", "start_frame": 0, "end_frame": 1},
                        {"severity": "hard", "start_frame": True, "end_frame": 2},
                    ]
                }
            )
        )
        == []
    )
    assert _source_fps(None, default=24.0) == 24.0
    assert _source_fps({"fps": 0}, default=24.0) == 24.0
    assert _asset_total_frames(None, source_fps=24.0) is None
    assert _asset_total_frames({"duration_frames": 42}, source_fps=24.0) == 42
    assert _asset_total_frames({"duration_sec": 0}, source_fps=24.0) is None
    assert _probe_payload(None) is None
    assert _probe_payload({"fps": 30}) == {"fps": 30}
    assert _probe_payload("[]") is None
    assert _int_value(True) is None
    assert _int_value(3) == 3
    assert _int_value(3.0) == 3
    assert _int_value(3.5) is None


def test_materializer_zero_duration_slot_window_still_materializes_one_frame(
    tmp_path: Path,
) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_clip(connection, "asset_1", "clip_1", end_frame=60)
    case_state = _case_state(slot_windows=[("slot_1", [1.0, 2.0])])
    assert case_state.cut_plan is not None
    bad_slot = case_state.cut_plan.slots[0].model_copy(update={"target_duration_sec": (0.0, 0.0)})
    case_state = case_state.model_copy(
        update={"cut_plan": case_state.cut_plan.model_copy(update={"slots": [bad_slot]})}
    )

    timeline = materialize_from_selection(
        engine,
        case_state,
        _pack([("slot_1", "cand_1", "asset_1", "clip_1")]),
        [{"slot_id": "slot_1", "candidate_id": "cand_1"}],
    )

    assert timeline.duration_frames == 1
    assert _media_track(timeline, "visual_base")[0].source_end_frame == 1


def test_materializer_rejects_invalid_selection_inputs(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    pack = _pack([("slot_1", "cand_1", "asset_1", "clip_1")])

    with pytest.raises(MaterializationError, match="candidate pack does not belong"):
        materialize_from_selection(
            engine,
            _case_state(),
            pack.model_copy(update={"case_id": "other_case"}),
            [{"slot_id": "slot_1", "candidate_id": "cand_1"}],
        )
    with pytest.raises(MaterializationError, match="cut_plan is required"):
        materialize_from_selection(
            engine,
            _case_state().model_copy(update={"cut_plan": None}),
            pack,
            [{"slot_id": "slot_1", "candidate_id": "cand_1"}],
        )
    with pytest.raises(MaterializationError, match="each selection requires"):
        materialize_from_selection(engine, _case_state(), pack, [{"slot_id": "slot_1"}])
    with pytest.raises(MaterializationError, match="duplicate selection"):
        materialize_from_selection(
            engine,
            _case_state(),
            pack,
            [
                {"slot_id": "slot_1", "candidate_id": "cand_1"},
                {"slot_id": "slot_1", "candidate_id": "cand_1"},
            ],
        )
    with pytest.raises(MaterializationError, match="missing selection"):
        materialize_from_selection(engine, _case_state(), pack, [])


def test_materializer_rejects_unknown_or_unavailable_candidates(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    pack = _pack([("slot_1", "cand_1", "asset_1", "clip_1")])

    with pytest.raises(MaterializationError, match="candidate missing is not in slot"):
        materialize_from_selection(
            engine,
            _case_state(),
            pack,
            [{"slot_id": "slot_1", "candidate_id": "missing"}],
        )
    with pytest.raises(MaterializationError, match="selection references unknown slot"):
        materialize_from_selection(
            engine,
            _case_state(),
            pack,
            [
                {"slot_id": "slot_1", "candidate_id": "cand_1"},
                {"slot_id": "slot_extra", "candidate_id": "cand_extra"},
            ],
        )


def test_materializer_rejects_missing_clip_projection_or_cut_slot(
    tmp_path: Path,
) -> None:
    engine = _engine(tmp_path)
    pack = _pack([("slot_1", "cand_1", "asset_1", "clip_1")])

    with pytest.raises(MaterializationError, match="selected clip not found"):
        materialize_from_selection(
            engine,
            _case_state(),
            pack,
            [{"slot_id": "slot_1", "candidate_id": "cand_1"}],
        )

    with engine.begin() as connection:
        _seed_clip(connection, "asset_1", "clip_1", end_frame=30)
    with pytest.raises(MaterializationError, match="cut_plan slot not found"):
        materialize_from_selection(
            engine,
            _case_state(slot_windows=[("other_slot", [1.0, 2.0])]),
            pack,
            [{"slot_id": "slot_1", "candidate_id": "cand_1"}],
        )


def test_materializer_rejects_unusable_source_or_unsupported_role(
    tmp_path: Path,
) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_clip(connection, "asset_empty", "clip_empty", start_frame=20, end_frame=20)
        _seed_clip(connection, "asset_role", "clip_role", end_frame=30, role="unexpected")

    with pytest.raises(MaterializationError, match="clip has no usable source frames"):
        materialize_from_selection(
            engine,
            _case_state(),
            _pack([("slot_1", "cand_1", "asset_empty", "clip_empty")]),
            [{"slot_id": "slot_1", "candidate_id": "cand_1"}],
        )
    with pytest.raises(MaterializationError, match="unsupported candidate role"):
        materialize_from_selection(
            engine,
            _case_state(),
            _pack([("slot_1", "cand_1", "asset_role", "clip_role")]),
            [{"slot_id": "slot_1", "candidate_id": "cand_1"}],
        )


def test_materializer_materializes_empty_candidate_pack(tmp_path: Path) -> None:
    engine = _engine(tmp_path)

    timeline = materialize_from_selection(
        engine,
        _case_state(slot_windows=[]),
        _pack([]),
        [],
    )

    assert timeline.duration_frames == 0
    assert _media_track(timeline, "visual_base") == []


def _engine(tmp_path: Path, *, fps: int = 30):
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)
        connection.execute(
            schema.projects.insert().values(
                project_id="project_1",
                name="Project",
                status="active",
                defaults=dump_json({"aspect_ratio": "9:16", "fps": fps}),
                created_at=NOW,
                updated_at=NOW,
            )
        )
    return engine


def _case_state(
    *,
    audio_plan: dict[str, Any] | None = None,
    slot_windows: list[tuple[str, list[float]]] | None = None,
    narration_refs: dict[str, dict[str, Any]] | None = None,
) -> CaseState:
    windows = [("slot_1", [1.0, 3.0])] if slot_windows is None else slot_windows
    refs = narration_refs or {}
    return CaseState.model_validate(
        {
            "case_id": "case_1",
            "project_id": "project_1",
            "name": "Case",
            "brief": {"goal": "test", "confirmed_facts": []},
            "audio_plan": audio_plan or {"mode": "silent"},
            "cut_plan": {
                "schema": "CutPlan.v1",
                "slots": [
                    {
                        "slot_id": slot_id,
                        "brief": "product closeup",
                        "target_duration_sec": window,
                        "narration_ref": refs.get(slot_id),
                    }
                    for slot_id, window in windows
                ],
                "total_target_duration_sec": sum(window[1] for _slot_id, window in windows),
            },
            "selected_asset_ids": [],
            "disabled_asset_ids": [],
            "scratch_memory": {},
        }
    )


def _pack(rows: list[tuple[str, str, str, str]]) -> CandidatePack:
    return CandidatePack.model_validate(
        {
            "candidate_pack_id": "pack_1",
            "case_id": "case_1",
            "query_context": {},
            "snapshot": {
                "generated_at": NOW,
                "asset_scope_hash": "hash",
                "annotation_versions": {
                    asset_id: f"ann_{asset_id}@{NOW}" for _slot, _cand, asset_id, _clip in rows
                },
            },
            "slots": [
                {
                    "slot_id": slot_id,
                    "slot_brief": "product closeup",
                    "target_duration_sec": [1.0, 4.0],
                    "candidates": [
                        {
                            "candidate_id": candidate_id,
                            "asset_id": asset_id,
                            "clip_id": clip_id,
                            "summary_line": "product closeup",
                            "score": {"bm25_rank": 1, "vector_rank": 0, "rrf": 0.1},
                        }
                    ],
                }
                for slot_id, candidate_id, asset_id, clip_id in rows
            ],
        }
    )


def _seed_clip(
    connection: Connection,
    asset_id: str,
    clip_id: str,
    *,
    start_frame: int = 0,
    end_frame: int = 90,
    kind: str = "video",
    role: str = "b_roll_candidate",
    probe: dict[str, Any] | None = None,
    hard_events: list[dict[str, Any]] | None = None,
) -> None:
    _seed_asset(
        connection,
        asset_id,
        kind=kind,
        probe=probe or {"duration_sec": 10.0, "fps": 30.0},
    )
    annotation_id = f"ann_{asset_id}"
    document = {
        "schema": "AnnotationDocument.v1",
        "annotation_id": annotation_id,
        "asset_id": asset_id,
        "asset_kind": kind,
        "status": "completed",
        "generator": {"pipeline_version": "annotation.video.v1", "pass": "cheap"},
        "clips": [],
        "quality_events": hard_events or [],
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
            start_frame=start_frame,
            end_frame=end_frame,
            role=role,
            summary="product closeup",
            keywords_json=dump_json(["product", "closeup"]),
            quality_score=0.9,
            usable=True,
            embedding=array("f", [1.0, 0.0]).tobytes(),
        )
    )


def _seed_asset(
    connection: Connection,
    asset_id: str,
    *,
    kind: str = "video",
    probe: dict[str, Any] | None = None,
) -> None:
    connection.execute(
        schema.assets.insert().values(
            asset_id=asset_id,
            storage_mode="reference",
            object_hash=None,
            reference_path=f"/tmp/{asset_id}.mp4",
            kind=kind,
            source="local_path",
            filename=f"{asset_id}.mp4",
            hash=f"hash_{asset_id}",
            mtime=1,
            size=1,
            probe=dump_json(probe or {"duration_sec": 10.0, "fps": 30.0}),
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


def _media_track(timeline: Any, track_id: str) -> list[TimelineMediaClip]:
    for track in timeline.tracks:
        if track.track_id == track_id:
            return [clip for clip in track.clips if isinstance(clip, TimelineMediaClip)]
    raise AssertionError(f"missing track {track_id}")
