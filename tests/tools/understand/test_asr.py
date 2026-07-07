"""可复用 ASR 编排 transcribe_to_document 的单元测试（打桩 gateway/上传/抽取）。"""

from __future__ import annotations

from pathlib import Path
from typing import Any

import pytest

from contracts.provider import ProviderError, ProviderResult
from providers import ASR_TRANSCRIBE
from providers.gateway import ProviderGatewayResult
from storage.workspace_paths import WorkspacePaths
from tools.understand.asr import AsrPipelineError, transcribe_to_document

NOW_DOC = {
    "schema": "TranscriptDocument.v1",
    "transcript_id": "provider_tr",
    "asset_id": "provider_asset",
    "language": "zh",
    "provider_id": "mock_asr",
    "raw_preserved": True,
    "utterances": [
        {"utterance_id": "u1", "text": "你好", "start_ms": 0, "end_ms": 800, "words": []}
    ],
    "vad_segments": [],
    "warnings": [],
}


class _Extracted:
    def __init__(self, path: Path) -> None:
        self.path = path
        self.stderr_summary = ""


class _Upload:
    def __init__(self) -> None:
        self.signed_url = "https://oss.example/audio.wav"
        self.deleted = False

    def delete(self) -> None:
        self.deleted = True


class _OkGateway:
    def __init__(self) -> None:
        self.requests: list[Any] = []

    async def call(
        self, request: Any, *, provider_id: str | None = None, require_raw_transcript: bool = False
    ) -> ProviderGatewayResult:
        self.requests.append(request)
        assert require_raw_transcript is True
        return ProviderGatewayResult(
            result=ProviderResult(
                provider_id="mock_asr",
                capability=ASR_TRANSCRIBE,
                request_id=request.request_id,
                model="paraformer",
                latency_ms=1,
                normalized_output=dict(NOW_DOC),
            )
        )


class _ErrGateway:
    async def call(
        self, request: Any, *, provider_id: str | None = None, require_raw_transcript: bool = False
    ) -> ProviderGatewayResult:
        return ProviderGatewayResult(
            result=ProviderResult(
                provider_id="mock_asr",
                capability=ASR_TRANSCRIBE,
                request_id=request.request_id,
                model="paraformer",
                latency_ms=1,
                error=ProviderError(error_code="asr_down", message="boom", retryable=True),
            )
        )


async def test_transcribe_to_document_retargets_ids_and_cleans_upload(tmp_path: Path) -> None:
    upload = _Upload()
    gateway = _OkGateway()
    document = await transcribe_to_document(
        tmp_path / "src.mp4",
        paths=WorkspacePaths.from_root(tmp_path),
        gateway=gateway,
        asset_id="asset_x",
        transcript_id="tr_x",
        request_id="req_1",
        uploader=lambda _path, *, key_prefix: upload,
        extractor=lambda _path, *, paths: _Extracted(tmp_path / "audio.wav"),
    )

    assert document.transcript_id == "tr_x"
    assert document.asset_id == "asset_x"
    assert document.utterances[0].text == "你好"
    assert gateway.requests[0].payload["audio_url"] == upload.signed_url
    assert upload.deleted is True


async def test_transcribe_to_document_raises_on_provider_error(tmp_path: Path) -> None:
    upload = _Upload()
    with pytest.raises(AsrPipelineError, match="asr_down"):
        await transcribe_to_document(
            tmp_path / "src.mp4",
            paths=WorkspacePaths.from_root(tmp_path),
            gateway=_ErrGateway(),
            asset_id="asset_x",
            transcript_id="tr_x",
            request_id="req_1",
            uploader=lambda _path, *, key_prefix: upload,
            extractor=lambda _path, *, paths: _Extracted(tmp_path / "audio.wav"),
        )
    assert upload.deleted is True
