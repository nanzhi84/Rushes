"""可复用的 ASR 编排：wav 抽取 → OSS 上传 → provider 转写 → TranscriptDocument。

从 ``apps/worker/audio_jobs`` 的 ASR 流程里抽出的纯计算部分，供理解子代理的
``transcribe`` 动作同步调用（也可被 worker 复用）。**不写 DB、不派发事件**——落库由
调用方（loop 的 ``_persist_tool_result_data``）负责。放在 ``tools`` 下是因为它同时依赖
``media``（抽取/上传）与 ``providers``（gateway/请求），而 ``media`` 不允许 import
``providers``（PRD §15 依赖方向）。
"""

from __future__ import annotations

import contextlib
from collections.abc import Awaitable, Callable
from pathlib import Path
from typing import Any, Protocol

from contracts.transcript import TranscriptDocument
from media.asr_upload import OssUpload, upload_audio_to_oss
from media.audio_extract import ExtractedAudio, extract_audio_to_wav
from providers import ASR_TRANSCRIBE, ProviderGatewayResult, ProviderRequest
from storage.workspace_paths import WorkspacePaths


class AsrPipelineError(RuntimeError):
    """Raised when the ASR pipeline cannot produce a transcript."""


class _Gateway(Protocol):
    def call(
        self,
        request: ProviderRequest,
        *,
        provider_id: str | None = None,
        require_raw_transcript: bool = False,
    ) -> Awaitable[ProviderGatewayResult]:
        """ProviderGateway-compatible call shape."""


async def transcribe_to_document(
    source_path: str | Path,
    *,
    paths: WorkspacePaths,
    gateway: _Gateway,
    asset_id: str,
    transcript_id: str,
    request_id: str,
    provider_id: str | None = None,
    uploader: Callable[..., OssUpload] = upload_audio_to_oss,
    extractor: Callable[..., ExtractedAudio] = extract_audio_to_wav,
) -> TranscriptDocument:
    """Produce a :class:`TranscriptDocument` for ``source_path`` without DB writes."""

    try:
        extracted = extractor(source_path, paths=paths)
    except Exception as exc:  # surface any extract failure as a pipeline error
        raise AsrPipelineError(f"audio extract failed: {exc}") from exc

    upload: OssUpload | None = None
    try:
        upload = uploader(extracted.path, key_prefix=f"rushes/understand/{request_id}")
        gateway_result = await gateway.call(
            ProviderRequest(
                capability=ASR_TRANSCRIBE,
                request_id=request_id,
                payload={"audio_url": upload.signed_url, "asset_id": asset_id},
                metadata={"asset_id": asset_id, "timestamp_source": "understand"},
            ),
            provider_id=provider_id,
            require_raw_transcript=True,
        )
    finally:
        if upload is not None:
            with contextlib.suppress(Exception):
                upload.delete()

    result = gateway_result.result
    if result.error is not None:
        raise AsrPipelineError(f"{result.error.error_code}: {result.error.message}")
    try:
        document = TranscriptDocument.model_validate(result.normalized_output)
    except Exception as exc:  # invalid provider payload
        raise AsrPipelineError(f"invalid transcript document: {exc}") from exc
    payload: dict[str, Any] = document.model_dump(mode="json", by_alias=True)
    payload["transcript_id"] = transcript_id
    payload["asset_id"] = asset_id
    return TranscriptDocument.model_validate(payload)
