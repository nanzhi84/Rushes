"""DashScope Paraformer-v2 adapter for the asr.transcribe capability."""

from __future__ import annotations

import asyncio
import hashlib
import os
import time
import unicodedata
from collections.abc import Mapping, Sequence
from typing import Any, Literal, cast
from urllib.parse import urlparse

import httpx
from pydantic import BaseModel, ConfigDict

from contracts.provider import ProviderDescriptor, ProviderError, ProviderResult
from contracts.transcript import TranscriptDocument, TranscriptUtterance, TranscriptWord
from providers.capabilities import ASR_TRANSCRIBE, ProviderRequest

ALIYUN_PARAFORMER_ASR_PROVIDER_ID = "aliyun_paraformer_v2"
DEFAULT_DASHSCOPE_BASE_URL = "https://dashscope.aliyuncs.com"
DEFAULT_PARAFORMER_MODEL = "paraformer-v2"
_SUBMIT_PATH = "/api/v1/services/audio/asr/transcription"
_TASK_PATH_PREFIX = "/api/v1/tasks/"
_FILLER_WORDS = frozenset({"呃", "嗯", "啊", "哦", "额", "呐", "唔", "就是", "就是说"})
_FAILED_STATUSES = frozenset({"FAILED", "CANCELED", "CANCELLED", "UNKNOWN"})
WordKind = Literal["filler", "word", "punct"]
JsonObject = dict[str, Any]


class AliyunParaformerASRConfig(BaseModel):
    model_config = ConfigDict(extra="forbid")

    base_url: str = DEFAULT_DASHSCOPE_BASE_URL
    api_key_env: str = "RUSHES_DASHSCOPE_API_KEY"
    model: str = DEFAULT_PARAFORMER_MODEL
    priority: int = 10
    timeout_seconds: float = 60.0
    poll_interval_seconds: float = 3.0
    poll_timeout_seconds: float = 600.0


class AliyunParaformerASRProvider:
    """Submit one public audio URL to DashScope ASR and normalize the transcript."""

    def __init__(
        self,
        *,
        base_url: str = DEFAULT_DASHSCOPE_BASE_URL,
        api_key: str | None = None,
        model: str = DEFAULT_PARAFORMER_MODEL,
        timeout: float | httpx.Timeout = 60.0,
        poll_interval_seconds: float = 3.0,
        poll_timeout_seconds: float = 600.0,
        transport: httpx.AsyncBaseTransport | None = None,
        force_ipv4: bool = True,
    ) -> None:
        self.provider_id = ALIYUN_PARAFORMER_ASR_PROVIDER_ID
        self._base_url = base_url.rstrip("/") + "/"
        self._api_key = (
            api_key if api_key is not None else os.environ.get("RUSHES_DASHSCOPE_API_KEY")
        )
        self._model = model
        self._timeout = timeout
        self._poll_interval_seconds = max(0.0, poll_interval_seconds)
        self._poll_timeout_seconds = max(0.0, poll_timeout_seconds)
        if transport is None and force_ipv4:
            transport = httpx.AsyncHTTPTransport(local_address="0.0.0.0")
        self._transport = transport

    async def invoke(self, request: ProviderRequest) -> ProviderResult:
        started = time.monotonic()
        request_id = request.request_id or (
            f"asr_{hashlib.sha256(str(time.time_ns()).encode()).hexdigest()[:16]}"
        )
        audio_url = _payload_url(request.payload.get("audio_url"))
        if audio_url is None:
            return self._error_result(
                request,
                request_id,
                started,
                ProviderError(
                    error_code="invalid_asr_request",
                    message="asr.transcribe payload requires a public http(s) audio_url",
                    retryable=False,
                ),
            )
        if not self._api_key:
            return self._error_result(
                request,
                request_id,
                started,
                ProviderError(
                    error_code="missing_api_key",
                    message="RUSHES_DASHSCOPE_API_KEY is not configured",
                    retryable=False,
                ),
            )

        async with httpx.AsyncClient(
            base_url=self._base_url,
            timeout=self._timeout,
            trust_env=False,
            transport=self._transport,
        ) as client:
            submit = await self._submit(client, audio_url)
            if isinstance(submit, ProviderError):
                return self._error_result(request, request_id, started, submit)
            task_id = _first_required_string(submit, ("task_id", "taskId"), "task_id")
            if task_id is None:
                return self._schema_error(
                    request,
                    request_id,
                    started,
                    "submit response missing task_id",
                    submit,
                )
            task = await self._poll_task(client, task_id)
            if isinstance(task, ProviderError):
                return self._error_result(request, request_id, started, task)
            transcription_url = _extract_transcription_url(task)
            if transcription_url is None:
                return self._schema_error(
                    request,
                    request_id,
                    started,
                    "succeeded task response missing transcription_url",
                    task,
                )
            raw_transcript = await self._download_json(client, transcription_url)
            if isinstance(raw_transcript, ProviderError):
                return self._error_result(request, request_id, started, raw_transcript)

        document = _normalize_asr_response(
            raw_transcript,
            asset_id=_asset_id(request),
            request_id=request_id,
        )
        return ProviderResult(
            provider_id=self.provider_id,
            capability=request.capability,
            request_id=request_id,
            model=self._model,
            latency_ms=_elapsed_ms(started),
            raw_ref=task_id,
            normalized_output=document.model_dump(mode="json", by_alias=True),
            warnings=document.warnings,
        )

    async def _submit(
        self,
        client: httpx.AsyncClient,
        audio_url: str,
    ) -> JsonObject | ProviderError:
        payload: JsonObject = {
            "model": self._model,
            "input": {"file_urls": [audio_url]},
            "parameters": {
                "disfluency_removal_enabled": False,
                "timestamp_alignment_enabled": True,
                "language_hints": ["zh"],
            },
        }
        try:
            response = await client.post(
                _SUBMIT_PATH,
                json=payload,
                headers={
                    "Authorization": f"Bearer {self._api_key}",
                    "Content-Type": "application/json",
                    "X-DashScope-Async": "enable",
                },
            )
        except httpx.TimeoutException as exc:
            return _transport_error("timeout", exc, retryable=True)
        except httpx.TransportError as exc:
            return _transport_error("network_error", exc, retryable=True)
        return _checked_json(response, context="submit DashScope ASR task")

    async def _poll_task(
        self,
        client: httpx.AsyncClient,
        task_id: str,
    ) -> JsonObject | ProviderError:
        deadline = time.monotonic() + self._poll_timeout_seconds
        while time.monotonic() <= deadline:
            try:
                response = await client.get(
                    f"{_TASK_PATH_PREFIX}{task_id}",
                    headers={"Authorization": f"Bearer {self._api_key}"},
                )
            except httpx.TimeoutException as exc:
                return _transport_error("timeout", exc, retryable=True)
            except httpx.TransportError as exc:
                return _transport_error("network_error", exc, retryable=True)
            data = _checked_json(response, context="poll DashScope ASR task")
            if isinstance(data, ProviderError):
                return data
            status = _first_required_string(
                data,
                ("task_status", "taskStatus", "status"),
                "task_status",
            )
            if status is None:
                return ProviderError(
                    error_code="asr_response_schema_error",
                    message="task response missing task_status",
                    retryable=False,
                    details={"response": data},
                )
            normalized_status = status.upper()
            if normalized_status == "SUCCEEDED":
                return data
            if normalized_status in _FAILED_STATUSES:
                return ProviderError(
                    error_code="asr_task_failed",
                    message=f"DashScope ASR task ended with {status}",
                    retryable=False,
                    details={"task_id": task_id, "response": data},
                )
            if self._poll_interval_seconds > 0:
                await asyncio.sleep(self._poll_interval_seconds)
        return ProviderError(
            error_code="asr_timeout",
            message=f"DashScope ASR task did not finish within {self._poll_timeout_seconds:.0f}s",
            retryable=True,
            details={"task_id": task_id},
        )

    async def _download_json(
        self,
        client: httpx.AsyncClient,
        url: str,
    ) -> JsonObject | ProviderError:
        try:
            response = await client.get(url)
        except httpx.TimeoutException as exc:
            return _transport_error("timeout", exc, retryable=True)
        except httpx.TransportError as exc:
            return _transport_error("network_error", exc, retryable=True)
        return _checked_json(response, context="download DashScope ASR transcript")

    def _schema_error(
        self,
        request: ProviderRequest,
        request_id: str,
        started: float,
        message: str,
        response: Mapping[str, Any],
    ) -> ProviderResult:
        return self._error_result(
            request,
            request_id,
            started,
            ProviderError(
                error_code="asr_response_schema_error",
                message=message,
                retryable=False,
                details={"response": dict(response)},
            ),
        )

    def _error_result(
        self,
        request: ProviderRequest,
        request_id: str,
        started: float,
        error: ProviderError,
    ) -> ProviderResult:
        return ProviderResult(
            provider_id=self.provider_id,
            capability=request.capability,
            request_id=request_id,
            model=self._model,
            latency_ms=_elapsed_ms(started),
            error=error,
        )


def aliyun_paraformer_asr_descriptor(*, priority: int = 10) -> ProviderDescriptor:
    return ProviderDescriptor(
        provider_id=ALIYUN_PARAFORMER_ASR_PROVIDER_ID,
        display_name="Aliyun DashScope Paraformer-v2 ASR",
        version="1",
        capabilities=[ASR_TRANSCRIBE],
        config_model=AliyunParaformerASRConfig,
        client_ref="providers.aliyun.asr_paraformer.AliyunParaformerASRProvider",
        supports_word_timestamps=True,
        supports_raw_transcript=True,
        priority=priority,
    )


def _normalize_asr_response(
    raw: JsonObject,
    *,
    asset_id: str,
    request_id: str,
) -> TranscriptDocument:
    warnings: list[str] = []
    utterances = [
        utterance
        for index, sentence in enumerate(_collect_sentence_mappings(raw), start=1)
        if (utterance := _normalize_sentence(sentence, index, warnings)) is not None
    ]
    if not utterances:
        text = _extract_full_text(raw)
        if text:
            warnings.append(
                "no sentence-level timestamps found; created untimed fallback utterance"
            )
            utterances.append(
                TranscriptUtterance(
                    utterance_id="u_001",
                    text=text,
                    start_ms=0,
                    end_ms=1,
                    words=[],
                )
            )
        else:
            warnings.append("ASR response has no normalizable text")
    utterances.sort(key=lambda utterance: (utterance.start_ms, utterance.end_ms))
    full_text = "".join(utterance.text for utterance in utterances)
    filler_hits = {filler for filler in _FILLER_WORDS if filler in full_text}
    return TranscriptDocument(
        transcript_id=f"tr_{_slug(request_id)}",
        asset_id=asset_id,
        language="zh",
        provider_id=ALIYUN_PARAFORMER_ASR_PROVIDER_ID,
        raw_preserved=bool(filler_hits),
        utterances=utterances,
        vad_segments=[],
        warnings=warnings,
    )


def _collect_sentence_mappings(raw: JsonObject) -> list[Mapping[str, object]]:
    candidates: list[Mapping[str, object]] = []
    for mapping in _iter_json_objects(raw):
        candidates.extend(
            _first_mapping_list(
                mapping,
                ("sentences", "sentence", "utterances", "segments", "paragraphs"),
            )
        )
    return [candidate for candidate in candidates if _has_text_or_words(candidate)]


def _normalize_sentence(
    sentence: Mapping[str, object],
    index: int,
    warnings: list[str],
) -> TranscriptUtterance | None:
    words = _normalize_words(sentence, warnings)
    text = _first_string(sentence, ("text", "sentence", "content", "transcript"))
    if text is None:
        text = "".join(word.w for word in words)
    start_ms = _first_ms(
        sentence,
        ("start_ms", "begin_time", "start_time", "startTime", "beginTime", "start", "begin"),
    )
    end_ms = _first_ms(
        sentence,
        ("end_ms", "end_time", "endTime", "finish_time", "finishTime", "end", "finish"),
    )
    if words:
        start_ms = start_ms if start_ms is not None else min(word.start_ms for word in words)
        end_ms = end_ms if end_ms is not None else max(word.end_ms for word in words)
    if start_ms is None or end_ms is None:
        warnings.append(f"utterance {index} missing start/end timestamps; skipped")
        return None
    if start_ms >= end_ms:
        warnings.append(f"utterance {index} has invalid range {start_ms}-{end_ms}; skipped")
        return None
    if not words:
        warnings.append(f"utterance {index} has no word timestamps")
    return TranscriptUtterance(
        utterance_id=f"u_{index:03d}",
        text=text,
        start_ms=start_ms,
        end_ms=end_ms,
        words=words,
    )


def _normalize_words(sentence: Mapping[str, object], warnings: list[str]) -> list[TranscriptWord]:
    mappings = _first_mapping_list(sentence, ("words", "tokens", "characters", "chars"))
    candidates: list[TranscriptWord] = []
    for index, word in enumerate(mappings, start=1):
        text = _first_string(word, ("w", "word", "text", "char", "value", "content"))
        start_ms = _first_ms(
            word,
            ("start_ms", "begin_time", "start_time", "startTime", "beginTime", "start", "begin"),
        )
        end_ms = _first_ms(
            word,
            ("end_ms", "end_time", "endTime", "finish_time", "finishTime", "end", "finish"),
        )
        if text is None:
            warnings.append(f"word {index} missing text; skipped")
            continue
        if start_ms is None or end_ms is None:
            warnings.append(f"word {text!r} missing start/end timestamps; skipped")
            continue
        if start_ms >= end_ms:
            warnings.append(f"word {text!r} has invalid range {start_ms}-{end_ms}; skipped")
            continue
        candidates.append(
            TranscriptWord(
                w=text,
                start_ms=start_ms,
                end_ms=end_ms,
                type=_word_type(text),
            )
        )
    candidates.sort(key=lambda word: (word.start_ms, word.end_ms))
    normalized: list[TranscriptWord] = []
    previous_end = -1
    for candidate in candidates:
        if candidate.start_ms < previous_end:
            warnings.append(f"word {candidate.w!r} is non-monotonic; skipped")
            continue
        normalized.append(candidate)
        previous_end = candidate.end_ms
    return normalized


def _word_type(text: str) -> WordKind:
    if text in _FILLER_WORDS:
        return "filler"
    if text and all(unicodedata.category(char).startswith("P") for char in text):
        return "punct"
    return "word"


def _has_text_or_words(mapping: Mapping[str, object]) -> bool:
    return (
        _first_string(mapping, ("text", "sentence", "content", "transcript")) is not None
        or _first_list(mapping, ("words", "tokens", "characters", "chars")) is not None
    )


def _extract_full_text(raw: JsonObject) -> str:
    pieces: list[str] = []
    for mapping in _iter_json_objects(raw):
        text = _first_string(mapping, ("text", "content", "transcript"))
        if text is not None:
            pieces.append(text)
    return max(pieces, key=len, default="")


def _iter_json_objects(value: object) -> list[Mapping[str, object]]:
    objects: list[Mapping[str, object]] = []
    if isinstance(value, Mapping):
        objects.append(cast(Mapping[str, object], value))
        for child in value.values():
            objects.extend(_iter_json_objects(child))
    elif isinstance(value, list):
        for child in value:
            objects.extend(_iter_json_objects(child))
    return objects


def _first_required_string(
    data: Mapping[str, object],
    keys: Sequence[str],
    _label: str,
) -> str | None:
    for mapping in _iter_json_objects(data):
        value = _first_string(mapping, keys)
        if value is not None:
            return value
    return None


def _extract_transcription_url(task_response: JsonObject) -> str | None:
    for mapping in _iter_json_objects(task_response):
        value = _first_string(
            mapping,
            ("transcription_url", "transcriptionUrl", "result_url", "resultUrl"),
        )
        if value is not None:
            return value
    return None


def _first_string(mapping: Mapping[str, object], keys: Sequence[str]) -> str | None:
    for key in keys:
        value = mapping.get(key)
        if isinstance(value, str) and value:
            return value
    return None


def _first_list(mapping: Mapping[str, object], keys: Sequence[str]) -> list[object] | None:
    for key in keys:
        value = mapping.get(key)
        if isinstance(value, list):
            return value
    return None


def _first_mapping_list(
    mapping: Mapping[str, object],
    keys: Sequence[str],
) -> list[Mapping[str, object]]:
    values = _first_list(mapping, keys)
    if values is None:
        return []
    return [cast(Mapping[str, object], value) for value in values if isinstance(value, Mapping)]


def _first_ms(mapping: Mapping[str, object], keys: Sequence[str]) -> int | None:
    for key in keys:
        value = mapping.get(key)
        parsed = _parse_time_ms(key, value)
        if parsed is not None:
            return parsed
    return None


def _parse_time_ms(key: str, value: object) -> int | None:
    if value is None or isinstance(value, bool):
        return None
    if isinstance(value, str):
        stripped = value.strip()
        if not stripped:
            return None
        try:
            number = float(stripped)
        except ValueError:
            return None
    elif isinstance(value, int | float):
        number = float(value)
    else:
        return None
    lower_key = key.lower()
    if (
        lower_key.endswith("_s") or lower_key in {"start", "end", "begin", "offset"}
    ) and number < 1000:
        return round(number * 1000)
    return round(number)


def _checked_json(response: httpx.Response, *, context: str) -> JsonObject | ProviderError:
    if response.status_code >= 400:
        return ProviderError(
            error_code=f"http_status_{response.status_code}",
            message=f"{context} HTTP {response.status_code}",
            retryable=response.status_code == 429 or response.status_code >= 500,
            details={"body": response.text[:1000]},
        )
    try:
        data = response.json()
    except ValueError as exc:
        return ProviderError(
            error_code="invalid_json",
            message=f"{context} returned non-JSON: {exc}",
            retryable=False,
            details={"body": response.text[:800]},
        )
    if not isinstance(data, dict):
        return ProviderError(
            error_code="invalid_json",
            message=f"{context} JSON root is not an object",
            retryable=False,
        )
    return cast(JsonObject, data)


def _transport_error(error_code: str, exc: Exception, *, retryable: bool) -> ProviderError:
    return ProviderError(
        error_code=error_code,
        message=str(exc),
        retryable=retryable,
        details={"exception_type": type(exc).__name__},
    )


def _payload_url(value: object) -> str | None:
    if not isinstance(value, str) or not value.strip():
        return None
    parsed = urlparse(value)
    if parsed.scheme not in {"http", "https"} or not parsed.netloc:
        return None
    return value


def _asset_id(request: ProviderRequest) -> str:
    value = request.payload.get("asset_id")
    if isinstance(value, str) and value:
        return value
    metadata_value = request.metadata.get("asset_id")
    if isinstance(metadata_value, str) and metadata_value:
        return metadata_value
    return "unknown_asset"


def _slug(value: str) -> str:
    allowed = [char for char in value if char.isalnum() or char in {"_", "-"}]
    slug = "".join(allowed).strip("_-")
    if slug:
        return slug[:48]
    return hashlib.sha256(value.encode()).hexdigest()[:20]


def _elapsed_ms(started: float) -> int:
    return max(0, int((time.monotonic() - started) * 1000))
