from __future__ import annotations

import shutil
import subprocess
from pathlib import Path
from typing import Any

import pytest

from contracts.provider import ProviderResult
from providers import VLM_UNDERSTANDING
from providers.gateway import ProviderGatewayResult
from tools.media_tools import (
    LabeledImage,
    ask_vlm_about_frames,
    extract_frame_data_uri,
    image_path_data_uri,
    multimodal_messages,
)
from tools.specs import build_default_tool_registry

DATA_URI = "data:image/jpeg;base64,ZmFrZQ=="


def test_frame_primitives_are_internal_and_media_view_frames_is_not_registered() -> None:
    registry = build_default_tool_registry()

    assert "media.view_frames" not in registry.specs_by_name()
    messages = multimodal_messages(
        "主体是什么？",
        [LabeledImage(label="[t=1.25s]", data_uri=DATA_URI)],
    )
    content = messages[0]["content"]
    assert content[0]["text"] == "主体是什么？"
    assert content[1]["text"] == "[t=1.25s]"
    assert content[2]["image_url"]["url"] == DATA_URI


@pytest.mark.parametrize(
    ("suffix", "mime"),
    [(".jpg", "image/jpeg"), (".png", "image/png"), (".webp", "image/webp")],
)
def test_image_data_uri_uses_real_file_mime(tmp_path: Path, suffix: str, mime: str) -> None:
    image = tmp_path / f"still{suffix}"
    image.write_bytes(b"image-bytes")

    assert image_path_data_uri(image).startswith(f"data:{mime};base64,")


def test_image_data_uri_accepts_explicit_mime_for_extensionless_poster(tmp_path: Path) -> None:
    poster = tmp_path / ("a" * 64)
    poster.write_bytes(b"jpeg-poster-bytes")

    assert image_path_data_uri(poster, mime="image/jpeg").startswith("data:image/jpeg;base64,")


@pytest.mark.parametrize(
    ("payload", "mime"),
    [
        (b"\xff\xd8\xff\xe0jpeg", "image/jpeg"),
        (b"\x89PNG\r\n\x1a\npng", "image/png"),
        (b"RIFF\x04\x00\x00\x00WEBPwebp", "image/webp"),
    ],
)
def test_image_data_uri_sniffs_extensionless_copy_objects(
    tmp_path: Path, payload: bytes, mime: str
) -> None:
    object_path = tmp_path / ("b" * 64)
    object_path.write_bytes(payload)

    assert image_path_data_uri(object_path).startswith(f"data:{mime};base64,")


@pytest.mark.asyncio
async def test_internal_frame_question_uses_vlm_gateway() -> None:
    gateway = _RecordingVlmGateway(
        {
            "frames": [{"index": 1, "description": "主体清晰，画面稳定。"}],
            "overall_answer": "可用。",
        }
    )

    answer = await ask_vlm_about_frames(
        [LabeledImage(label="[t=1.25s]", data_uri=DATA_URI)],
        gateway=gateway,
        request_id="req_1",
        question="主体是什么？",
        draft_id="draft_1",
    )

    assert answer.descriptions == ("主体清晰，画面稳定。",)
    assert answer.overall_answer == "可用。"
    request = gateway.requests[0]
    assert request.capability == VLM_UNDERSTANDING
    assert request.draft_id == "draft_1"


@pytest.mark.skipif(shutil.which("ffmpeg") is None, reason="ffmpeg not installed")
@pytest.mark.ffmpeg
def test_internal_frame_primitive_extracts_real_lavfi_frame(tmp_path: Path) -> None:
    source = tmp_path / "sample.mp4"
    subprocess.run(
        [
            "ffmpeg",
            "-hide_banner",
            "-loglevel",
            "error",
            "-f",
            "lavfi",
            "-i",
            "testsrc2=size=320x180:rate=10:duration=1",
            "-c:v",
            "libx264",
            "-pix_fmt",
            "yuv420p",
            str(source),
        ],
        check=True,
        timeout=30,
    )

    data_uri = extract_frame_data_uri(source, 0.2)

    assert data_uri.startswith("data:image/jpeg;base64,")
    assert len(data_uri) > len("data:image/jpeg;base64,")


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
