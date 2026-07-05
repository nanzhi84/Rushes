from __future__ import annotations

import json
import shutil
import subprocess
from pathlib import Path

import pytest
from pydantic import BaseModel, ConfigDict
from sqlalchemy import func, select

from annotation import shot_split as shot_split_module
from annotation.migrations import rebuild_all_annotation_projections
from annotation.projection import build_annotation_projection, persist_annotation_projection
from annotation.quality import (
    QualityConfig,
    _optical_flow_motion_score,
    clean_spans,
    detect_quality_events,
)
from annotation.shot_split import Shot, ShotSplitConfig, split_shots
from contracts.annotation import (
    AnnotationClip,
    AnnotationDocument,
    AnnotationExtensions,
    AnnotationGenerator,
    QualityEvent,
    VisionBasicExtension,
)
from contracts.provider import ProviderDescriptor
from providers import EMBEDDING_TEXT, ProviderGateway, ProviderRegistry
from providers.mock import MockProvider
from storage import schema
from storage.db import create_workspace_engine

FFMPEG_AVAILABLE = shutil.which("ffmpeg") is not None and shutil.which("ffprobe") is not None


class EmptyConfig(BaseModel):
    model_config = ConfigDict(extra="forbid")


@pytest.mark.skipif(not FFMPEG_AVAILABLE, reason="ffmpeg/ffprobe not installed")
@pytest.mark.ffmpeg
def test_shot_split_detects_two_lavfi_scenes(tmp_path: Path) -> None:
    video = _make_two_scene_video(tmp_path)

    result = split_shots(
        video,
        config=ShotSplitConfig(content_threshold=12.0, min_scene_len=3),
    )

    assert len(result.shots) >= 2
    assert result.shots[0].start_frame == 0


def test_shot_split_degrades_when_transnet_weights_are_missing(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    video = tmp_path / "source.mp4"
    video.write_bytes(b"placeholder")
    monkeypatch.setattr(
        shot_split_module,
        "_pyscenedetect_shots",
        lambda *_args, **_kwargs: (Shot("shot_0001", 0, 10),),
    )

    result = split_shots(
        video,
        config=ShotSplitConfig(
            transnetv2_onnx_path=tmp_path / "missing.onnx",
            case_id="case_1",
        ),
    )

    assert result.shots == (Shot("shot_0001", 0, 10),)
    assert result.events[0]["event"] == "CapabilityDegraded"
    assert result.events[0]["capability"] == "shot_split.transnetv2"


@pytest.mark.skipif(not FFMPEG_AVAILABLE, reason="ffmpeg/ffprobe not installed")
@pytest.mark.ffmpeg
def test_quality_detects_hard_blur_on_gaussian_blurred_video(tmp_path: Path) -> None:
    video = _make_blurred_video(tmp_path)

    events = detect_quality_events(
        video,
        (Shot("shot_1", 0, 20),),
        config=QualityConfig(sample_stride=2, blur_hard_threshold=80.0),
    )

    assert any(event.kind == "blur" and event.severity == "hard" for event in events)


def test_quality_uses_optical_flow_for_motion_score() -> None:
    cv2 = pytest.importorskip("cv2")
    np = pytest.importorskip("numpy")
    previous = np.zeros((64, 64), dtype=np.uint8)
    current = np.zeros((64, 64), dtype=np.uint8)
    cv2.rectangle(previous, (16, 16), (36, 36), 255, -1)
    cv2.rectangle(current, (24, 16), (44, 36), 255, -1)

    score = _optical_flow_motion_score(previous, current, cv2=cv2, np=np)

    assert score > 0.1


def test_clean_spans_subtracts_hard_quality_events() -> None:
    clip = AnnotationClip(
        clip_id="clip_1",
        source_start_frame=0,
        source_end_frame=100,
        role="b_roll_candidate",
        summary="usable clip",
    )
    events = (
        QualityEvent(
            event_id="q_1",
            kind="blur",
            severity="hard",
            start_frame=30,
            end_frame=60,
        ),
        QualityEvent(
            event_id="q_2",
            kind="shake",
            severity="soft",
            start_frame=70,
            end_frame=80,
        ),
    )

    spans = clean_spans((clip,), events)

    assert [(span.start_frame, span.end_frame) for span in spans] == [(0, 30), (60, 100)]


async def test_projection_writes_rows_fts_and_embedding_blob(tmp_path: Path) -> None:
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)
        _insert_asset(connection)
    gateway = _embedding_gateway()
    document = _document()

    projection = await build_annotation_projection(document, gateway=gateway)
    with engine.begin() as connection:
        persist_annotation_projection(connection, document, projection)
        clip = connection.execute(schema.annotation_clip_projection.select()).one()._mapping
        signal_count = connection.execute(
            select(func.count()).select_from(schema.annotation_signal_projection)
        ).scalar_one()
        fts_rows = connection.exec_driver_sql(
            "SELECT clip_id FROM clip_fts WHERE clip_fts MATCH 'product'"
        ).all()

    assert clip["annotation_id"] == "ann_asset_1"
    assert len(clip["embedding"]) == 12
    assert signal_count >= 1
    assert [row[0] for row in fts_rows] == ["clip_1"]


async def test_rebuild_all_annotation_projections_is_idempotent(tmp_path: Path) -> None:
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)
        _insert_asset(connection)
    document = _document()
    projection = await build_annotation_projection(document)
    with engine.begin() as connection:
        persist_annotation_projection(connection, document, projection)
        connection.execute(schema.annotation_signal_projection.delete())
        connection.execute(
            schema.annotation_clip_projection.update().values(
                summary="stale",
                keywords_json=json.dumps([]),
            )
        )
        connection.exec_driver_sql("DELETE FROM clip_fts")
        first_count = await rebuild_all_annotation_projections(connection)
        second_count = await rebuild_all_annotation_projections(connection)
        clip = connection.execute(schema.annotation_clip_projection.select()).one()._mapping
        signal_count = connection.execute(
            select(func.count()).select_from(schema.annotation_signal_projection)
        ).scalar_one()
        fts_rows = connection.exec_driver_sql(
            "SELECT clip_id FROM clip_fts WHERE clip_fts MATCH 'product'"
        ).all()

    assert first_count == 1
    assert second_count == 1
    assert clip["summary"] == "product closeup"
    assert signal_count >= 1
    assert [row[0] for row in fts_rows] == ["clip_1"]


def _document() -> AnnotationDocument:
    return AnnotationDocument(
        annotation_id="ann_asset_1",
        asset_id="asset_1",
        asset_kind="video",
        status="completed",
        generator=AnnotationGenerator(
            pipeline_version="annotation.video.v1",
            pass_="cheap",
            provider_refs=["pc_1"],
        ),
        clips=[
            AnnotationClip(
                clip_id="clip_1",
                source_start_frame=0,
                source_end_frame=30,
                role="b_roll_candidate",
                summary="product closeup",
                keywords=["product", "closeup"],
                quality_score=0.9,
                extensions=AnnotationExtensions(
                    vision_basic_v1=VisionBasicExtension(
                        subject_type="product",
                        scene_type="desk",
                    )
                ),
            )
        ],
        created_at="2026-07-05T00:00:00+00:00",
    )


def _embedding_gateway() -> ProviderGateway:
    registry = ProviderRegistry()
    registry.register(
        ProviderDescriptor(
            provider_id="mock_embedding",
            display_name="mock_embedding",
            version="1",
            capabilities=[EMBEDDING_TEXT],
            config_model=EmptyConfig,
            client_ref="tests.mock_embedding",
        ),
        MockProvider(
            provider_id="mock_embedding",
            scripts={EMBEDDING_TEXT: [{"normalized_output": {"embedding": [0.1, 0.2, 0.3]}}]},
        ),
    )
    return ProviderGateway(registry=registry)


def _insert_asset(connection) -> None:
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
            probe=json.dumps({}),
            proxy_object_hash=None,
            ingest_status="annotating",
            annotation_status="pending",
            annotation_pass="none",
            index_status="none",
            usable=False,
            failure=None,
        )
    )


def _make_two_scene_video(tmp_path: Path) -> Path:
    video = tmp_path / "two_scenes.mp4"
    subprocess.run(
        [
            "ffmpeg",
            "-y",
            "-f",
            "lavfi",
            "-i",
            "color=c=red:duration=0.5:size=160x120:rate=30",
            "-f",
            "lavfi",
            "-i",
            "color=c=blue:duration=0.5:size=160x120:rate=30",
            "-filter_complex",
            "[0:v][1:v]concat=n=2:v=1:a=0,format=yuv420p",
            str(video),
        ],
        check=True,
        capture_output=True,
        text=True,
    )
    return video


def _make_blurred_video(tmp_path: Path) -> Path:
    video = tmp_path / "blurred.mp4"
    subprocess.run(
        [
            "ffmpeg",
            "-y",
            "-f",
            "lavfi",
            "-i",
            "testsrc=duration=1:size=160x120:rate=20",
            "-vf",
            "gblur=sigma=20,format=yuv420p",
            str(video),
        ],
        check=True,
        capture_output=True,
        text=True,
    )
    return video
