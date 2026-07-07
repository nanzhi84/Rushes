from __future__ import annotations

import shutil
import subprocess
from pathlib import Path
from typing import Any

import pytest
from pydantic import ValidationError

from contracts.asset import AssetKind, AssetSource, StorageMode
from contracts.case import CaseState
from contracts.provider import ProviderError, ProviderResult
from contracts.timeline import TimelineState
from providers import VLM_UNDERSTANDING
from providers.gateway import ProviderGatewayResult
from storage import schema
from storage.db import create_workspace_engine
from storage.repositories._json import dump_json
from timeline import store_timeline_version
from tools import ToolExecutionContext
from tools.media_tools import handlers as media_handlers
from tools.media_tools import view_frames
from tools.specs import MediaViewFramesInput

NOW = "2026-07-05T00:00:00+00:00"
DATA_URI = "data:image/jpeg;base64,ZmFrZQ=="


def test_view_frames_sends_question_and_data_uri_to_vlm(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    source = _make_placeholder_video(tmp_path)
    engine = _engine(tmp_path, source)
    gateway = _RecordingVlmGateway(
        {
            "frames": [{"frame_index": 1, "description": "主体清晰，画面稳定。"}],
            "overall_answer": "可用。",
        }
    )

    def fake_extract(path: Path, seconds: float, *, ffmpeg_bin: str = "ffmpeg") -> str:
        assert path == source
        assert seconds == pytest.approx(1.25)
        assert ffmpeg_bin == "ffmpeg"
        return DATA_URI

    monkeypatch.setattr(media_handlers, "_extract_frame_data_uri", fake_extract)
    with engine.connect() as connection:
        result = view_frames(
            MediaViewFramesInput(
                target={"asset_id": "asset_1", "at_sec": [1.25]},
                question="主体是什么？",
            ),
            _context(tmp_path, connection, provider_gateway=gateway),
        )

    assert result.status == "succeeded"
    assert "[t=1.25s] 主体清晰，画面稳定。" in result.observation
    assert "整体回答：可用。" in result.observation
    assert result.data["frames"][0]["status"] == "sampled"
    assert result.events == []

    request = gateway.requests[0]
    assert request.capability == VLM_UNDERSTANDING
    content = request.payload["messages"][0]["content"]
    assert "主体是什么？" in content[0]["text"]
    image_items = [item for item in content if item["type"] == "image_url"]
    assert image_items[0]["image_url"]["url"] == DATA_URI


def test_view_frames_input_dedupes_sorts_and_rejects_too_many_frames() -> None:
    model = MediaViewFramesInput(
        target={"asset_id": "asset_1", "at_sec": [2.0, 1.0, 2.0]},
    )

    assert model.target.at_sec == [1.0, 2.0]
    with pytest.raises(ValidationError, match="at_sec 数量超过 max_frames"):
        MediaViewFramesInput(
            target={"asset_id": "asset_1", "at_sec": [float(i) for i in range(9)]},
        )
    with pytest.raises(ValidationError):
        MediaViewFramesInput(
            target={"asset_id": "asset_1", "at_sec": [1.0]},
            max_frames=9,
        )


def test_view_frames_degrades_when_gateway_missing(tmp_path: Path) -> None:
    source = _make_placeholder_video(tmp_path)
    engine = _engine(tmp_path, source)
    with engine.connect() as connection:
        result = view_frames(
            MediaViewFramesInput(target={"asset_id": "asset_1", "at_sec": [1.0]}),
            _context(tmp_path, connection),
        )

    assert result.status == "succeeded"
    assert result.data["degraded"]["capability"] == VLM_UNDERSTANDING
    assert "VLM 通道不可用" in result.observation
    assert result.events == []


def test_view_frames_maps_timeline_seconds_and_marks_no_clip(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    source = _make_placeholder_video(tmp_path)
    engine = _engine(tmp_path, source, timeline_current_version=1)
    gateway = _RecordingVlmGateway(
        {
            "frames": [{"frame_index": 1, "description": "时间线命中的画面。"}],
            "overall_answer": "只有第二个时刻有画面。",
        }
    )
    extracted_seconds: list[float] = []

    def fake_extract(_path: Path, seconds: float, *, ffmpeg_bin: str = "ffmpeg") -> str:
        del ffmpeg_bin
        extracted_seconds.append(seconds)
        return DATA_URI

    monkeypatch.setattr(media_handlers, "_extract_frame_data_uri", fake_extract)
    with engine.begin() as connection:
        store_timeline_version(connection, _timeline(), created_at=NOW)
    with engine.connect() as connection:
        result = view_frames(
            MediaViewFramesInput(target={"timeline_version": 1, "at_sec": [0.5, 1.2]}),
            _context(
                tmp_path,
                connection,
                timeline_current_version=1,
                provider_gateway=gateway,
            ),
        )

    assert result.status == "succeeded"
    assert extracted_seconds == [pytest.approx(10.2)]
    assert "[t=0.5s] 该时刻无画面 clip。" in result.observation
    assert "[t=1.2s] 时间线命中的画面。" in result.observation
    assert result.data["frames"][0]["status"] == "no_clip"
    assert result.data["frames"][1]["source_sec"] == pytest.approx(10.2)
    assert result.data["frames"][1]["timeline_clip_id"] == "tc_1"


def test_view_frames_fails_for_missing_asset(tmp_path: Path) -> None:
    engine = _engine(tmp_path, source=None)
    with engine.connect() as connection:
        result = view_frames(
            MediaViewFramesInput(target={"asset_id": "missing", "at_sec": [0.0]}),
            _context(tmp_path, connection, provider_gateway=_RecordingVlmGateway({})),
        )

    assert result.status == "failed"
    assert result.error is not None
    assert result.error.error_code == "asset_not_found"


def test_view_frames_degrades_when_vlm_errors(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    source = _make_placeholder_video(tmp_path)
    engine = _engine(tmp_path, source)
    monkeypatch.setattr(
        media_handlers,
        "_extract_frame_data_uri",
        lambda _path, _seconds, ffmpeg_bin="ffmpeg": DATA_URI,
    )
    with engine.connect() as connection:
        result = view_frames(
            MediaViewFramesInput(target={"asset_id": "asset_1", "at_sec": [0.25]}),
            _context(tmp_path, connection, provider_gateway=_ErrorVlmGateway()),
        )

    assert result.status == "succeeded"
    assert result.data["degraded"]["capability"] == VLM_UNDERSTANDING
    assert "vlm_down" in result.observation


@pytest.mark.skipif(shutil.which("ffmpeg") is None, reason="ffmpeg not installed")
@pytest.mark.ffmpeg
def test_view_frames_extracts_real_lavfi_frames_with_mock_vlm(tmp_path: Path) -> None:
    source = _make_lavfi_video(tmp_path)
    engine = _engine(tmp_path, source)
    gateway = _RecordingVlmGateway(
        {
            "frames": [
                {"frame_index": 1, "description": "第一帧。"},
                {"frame_index": 2, "description": "第二帧。"},
            ],
            "overall_answer": "两帧均可读。",
        }
    )

    with engine.connect() as connection:
        result = view_frames(
            MediaViewFramesInput(target={"asset_id": "asset_1", "at_sec": [0.2, 1.2]}),
            _context(tmp_path, connection, provider_gateway=gateway),
        )

    assert result.status == "succeeded"
    content = gateway.requests[0].payload["messages"][0]["content"]
    image_urls = [item["image_url"]["url"] for item in content if item["type"] == "image_url"]
    assert len(image_urls) == 2
    assert all(url.startswith("data:image/jpeg;base64,") for url in image_urls)
    assert all(len(url) > len("data:image/jpeg;base64,") for url in image_urls)


class _RecordingVlmGateway:
    def __init__(self, output: dict[str, Any]) -> None:
        self.output = output
        self.requests: list[Any] = []

    async def call(self, request: Any) -> ProviderGatewayResult:
        self.requests.append(request)
        return ProviderGatewayResult(
            result=ProviderResult(
                provider_id="mock_vlm",
                capability=VLM_UNDERSTANDING,
                request_id=request.request_id,
                model="mock",
                latency_ms=1,
                normalized_output=self.output,
            )
        )


class _ErrorVlmGateway:
    async def call(self, request: Any) -> ProviderGatewayResult:
        return ProviderGatewayResult(
            result=ProviderResult(
                provider_id="mock_vlm",
                capability=VLM_UNDERSTANDING,
                request_id=request.request_id,
                model="mock",
                latency_ms=1,
                error=ProviderError(
                    error_code="vlm_down",
                    message="provider unavailable",
                    retryable=True,
                ),
            )
        )


def _engine(
    tmp_path: Path,
    source: Path | None,
    *,
    timeline_current_version: int | None = None,
) -> Any:
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
                pending_decision_id=None,
                running_jobs="[]",
                last_error=None,
                brief=dump_json({"goal": "test", "confirmed_facts": []}),
                content_plan=None,
                audio_plan=None,
                cut_plan=None,
                timeline_current_version=timeline_current_version,
                timeline_validated=False,
                preview_current_id=None,
                last_viewed_preview_id=None,
                rough_cut_approved=False,
                rough_cut_approved_version=None,
                postprocess_plan=None,
                export_current_id=None,
                selected_asset_ids=dump_json(["asset_1"] if source is not None else []),
                disabled_asset_ids="[]",
                scratch_memory="{}",
            )
        )
        if source is not None:
            connection.execute(
                schema.assets.insert().values(
                    asset_id="asset_1",
                    storage_mode=StorageMode.REFERENCE.value,
                    object_hash=None,
                    reference_path=str(source),
                    kind=AssetKind.VIDEO.value,
                    source=AssetSource.LOCAL_PATH.value,
                    filename=source.name,
                    hash="hash",
                    mtime=1,
                    size=max(1, source.stat().st_size),
                    probe=None,
                    proxy_object_hash=None,
                    ingest_status="imported",
                    usable=True,
                    failure=None,
                )
            )
            connection.execute(
                schema.project_asset_links.insert().values(
                    project_id="project_1",
                    asset_id="asset_1",
                    enabled=True,
                    linked_at=NOW,
                    note="",
                )
            )
    return engine


def _context(
    tmp_path: Path,
    connection: Any,
    *,
    timeline_current_version: int | None = None,
    provider_gateway: Any | None = None,
) -> ToolExecutionContext:
    metadata: dict[str, Any] = {"workspace_path": tmp_path}
    if provider_gateway is not None:
        metadata["provider_gateway"] = provider_gateway
    return ToolExecutionContext(
        tool_call_id="tc_1",
        turn_id="turn_1",
        case_state=_case_state(timeline_current_version=timeline_current_version),
        readonly_connection=connection,
        metadata=metadata,
    )


def _case_state(*, timeline_current_version: int | None = None) -> CaseState:
    return CaseState.model_validate(
        {
            "case_id": "case_1",
            "project_id": "project_1",
            "name": "Case",
            "brief": {"goal": "test", "confirmed_facts": []},
            "timeline_current_version": timeline_current_version,
            "selected_asset_ids": ["asset_1"],
            "disabled_asset_ids": [],
            "scratch_memory": {},
        }
    )


def _timeline() -> TimelineState:
    return TimelineState.model_validate(
        {
            "timeline_id": "case_1:v1",
            "case_id": "case_1",
            "version": 1,
            "fps": 30,
            "duration_frames": 60,
            "tracks": [
                {
                    "track_id": "visual_base",
                    "track_type": "primary_visual",
                    "clips": [
                        {
                            "timeline_clip_id": "tc_1",
                            "track_id": "visual_base",
                            "asset_id": "asset_1",
                            "clip_id": "clip_1",
                            "role": "b_roll",
                            "timeline_start_frame": 30,
                            "timeline_end_frame": 60,
                            "source_start_frame": 300,
                            "source_end_frame": 330,
                        }
                    ],
                },
                {"track_id": "visual_overlay", "track_type": "visual_overlay", "clips": []},
                {"track_id": "original_audio", "track_type": "audio", "clips": []},
                {"track_id": "voiceover", "track_type": "audio", "clips": []},
                {"track_id": "bgm", "track_type": "audio", "clips": []},
                {"track_id": "subtitles", "track_type": "text", "clips": []},
            ],
        }
    )


def _make_placeholder_video(tmp_path: Path) -> Path:
    source = tmp_path / "source.mp4"
    source.write_bytes(b"placeholder")
    return source


def _make_lavfi_video(tmp_path: Path) -> Path:
    video = tmp_path / "fixture.mp4"
    subprocess.run(
        [
            "ffmpeg",
            "-y",
            "-f",
            "lavfi",
            "-i",
            "testsrc=duration=2:size=128x128:rate=30",
            "-pix_fmt",
            "yuv420p",
            str(video),
        ],
        check=True,
        capture_output=True,
        text=True,
    )
    return video
