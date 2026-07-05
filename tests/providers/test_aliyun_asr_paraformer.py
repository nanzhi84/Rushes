from __future__ import annotations

import json
from typing import Any

import httpx

from providers import ASR_TRANSCRIBE, ProviderRequest
from providers.aliyun import AliyunParaformerASRProvider, aliyun_paraformer_asr_descriptor


async def test_paraformer_asr_submits_polls_downloads_and_normalizes() -> None:
    requests: list[httpx.Request] = []

    def handler(request: httpx.Request) -> httpx.Response:
        requests.append(request)
        if request.url.path.endswith("/api/v1/services/audio/asr/transcription"):
            body = json.loads(request.content.decode())
            assert request.headers["authorization"] == "Bearer test-key"
            assert request.headers["x-dashscope-async"] == "enable"
            assert body["parameters"]["disfluency_removal_enabled"] is False
            assert body["parameters"]["timestamp_alignment_enabled"] is True
            assert body["parameters"]["language_hints"] == ["zh"]
            assert body["input"]["file_urls"] == ["https://oss.example/audio.wav"]
            return httpx.Response(200, json={"output": {"task_id": "task_1"}})
        if request.url.path.endswith("/api/v1/tasks/task_1"):
            return httpx.Response(
                200,
                json={
                    "output": {
                        "task_status": "SUCCEEDED",
                        "transcription_url": "https://download.example/result.json",
                    }
                },
            )
        if request.url.host == "download.example":
            assert "authorization" not in request.headers
            return httpx.Response(200, json=_transcript_response())
        return httpx.Response(404, json={"error": "unexpected"})

    adapter = AliyunParaformerASRProvider(
        api_key="test-key",
        transport=httpx.MockTransport(handler),
        poll_interval_seconds=0,
    )

    result = await adapter.invoke(
        ProviderRequest(
            capability=ASR_TRANSCRIBE,
            request_id="req_1",
            payload={"audio_url": "https://oss.example/audio.wav", "asset_id": "asset_1"},
        )
    )

    assert result.error is None
    assert [request.url.path for request in requests[:2]] == [
        "/api/v1/services/audio/asr/transcription",
        "/api/v1/tasks/task_1",
    ]
    transcript = result.normalized_output
    assert transcript["schema"] == "TranscriptDocument.v1"
    assert transcript["transcript_id"] == "tr_req_1"
    assert transcript["asset_id"] == "asset_1"
    assert transcript["provider_id"] == "aliyun_paraformer_v2"
    assert transcript["raw_preserved"] is True
    words = transcript["utterances"][0]["words"]
    assert words[0] == {"w": "呃", "start_ms": 0, "end_ms": 180, "type": "filler"}
    assert words[-1]["type"] == "punct"
    assert _words_are_monotonic(words)


async def test_paraformer_asr_classifies_429_as_retryable() -> None:
    adapter = AliyunParaformerASRProvider(
        api_key="test-key",
        transport=httpx.MockTransport(lambda _request: httpx.Response(429, json={})),
        poll_interval_seconds=0,
    )

    result = await adapter.invoke(
        ProviderRequest(
            capability=ASR_TRANSCRIBE,
            payload={"audio_url": "https://oss.example/audio.wav"},
        )
    )

    assert result.error is not None
    assert result.error.error_code == "http_status_429"
    assert result.error.retryable is True


async def test_paraformer_asr_classifies_failed_task() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path.endswith("/api/v1/services/audio/asr/transcription"):
            return httpx.Response(200, json={"output": {"task_id": "task_1"}})
        return httpx.Response(200, json={"output": {"task_status": "FAILED"}})

    adapter = AliyunParaformerASRProvider(
        api_key="test-key",
        transport=httpx.MockTransport(handler),
        poll_interval_seconds=0,
    )

    result = await adapter.invoke(
        ProviderRequest(
            capability=ASR_TRANSCRIBE,
            payload={"audio_url": "https://oss.example/audio.wav"},
        )
    )

    assert result.error is not None
    assert result.error.error_code == "asr_task_failed"
    assert result.error.retryable is False


async def test_paraformer_asr_classifies_submit_non_json() -> None:
    adapter = AliyunParaformerASRProvider(
        api_key="test-key",
        transport=httpx.MockTransport(lambda _request: httpx.Response(200, text="not-json")),
        poll_interval_seconds=0,
    )

    result = await adapter.invoke(
        ProviderRequest(
            capability=ASR_TRANSCRIBE,
            payload={"audio_url": "https://oss.example/audio.wav"},
        )
    )

    assert result.error is not None
    assert result.error.error_code == "invalid_json"
    assert "submit DashScope ASR task" in result.error.message


async def test_paraformer_asr_classifies_download_failure() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path.endswith("/api/v1/services/audio/asr/transcription"):
            return httpx.Response(200, json={"output": {"taskId": "task_1"}})
        if request.url.path.endswith("/api/v1/tasks/task_1"):
            return httpx.Response(
                200,
                json={
                    "output": {
                        "status": "SUCCEEDED",
                        "resultUrl": "https://download.example/result.json",
                    }
                },
            )
        return httpx.Response(503, json={"message": "temporarily unavailable"})

    adapter = AliyunParaformerASRProvider(
        api_key="test-key",
        transport=httpx.MockTransport(handler),
        poll_interval_seconds=0,
    )

    result = await adapter.invoke(
        ProviderRequest(
            capability=ASR_TRANSCRIBE,
            payload={"audio_url": "https://oss.example/audio.wav"},
        )
    )

    assert result.error is not None
    assert result.error.error_code == "http_status_503"
    assert result.error.retryable is True
    assert "download DashScope ASR transcript" in result.error.message


async def test_paraformer_asr_parses_nested_transcripts_and_sentence_variants() -> None:
    adapter = AliyunParaformerASRProvider(
        api_key="test-key",
        transport=httpx.MockTransport(
            lambda request: _successful_asr_response(request, _nested_transcript_response())
        ),
        poll_interval_seconds=0,
    )

    result = await adapter.invoke(
        ProviderRequest(
            capability=ASR_TRANSCRIBE,
            request_id="req_nested",
            payload={"audio_url": "https://oss.example/audio.wav"},
        )
    )

    assert result.error is None
    utterance = result.normalized_output["utterances"][0]
    assert utterance["text"] == "嗯你好！"
    assert utterance["start_ms"] == 200
    assert utterance["end_ms"] == 1200
    assert [word["type"] for word in utterance["words"]] == ["filler", "word", "punct"]
    assert result.normalized_output["raw_preserved"] is True


async def test_paraformer_asr_records_word_and_utterance_warnings() -> None:
    adapter = AliyunParaformerASRProvider(
        api_key="test-key",
        transport=httpx.MockTransport(
            lambda request: _successful_asr_response(request, _warning_transcript_response())
        ),
        poll_interval_seconds=0,
    )

    result = await adapter.invoke(
        ProviderRequest(
            capability=ASR_TRANSCRIBE,
            request_id="req_warnings",
            payload={"audio_url": "https://oss.example/audio.wav"},
        )
    )

    assert result.error is None
    assert result.normalized_output["utterances"][0]["words"] == [
        {"w": "后", "start_ms": 50, "end_ms": 80, "type": "word"}
    ]
    assert {
        "word 1 missing text; skipped",
        "word '缺' missing start/end timestamps; skipped",
        "word '倒' has invalid range 30-20; skipped",
        "word '乱' is non-monotonic; skipped",
        "utterance 2 missing start/end timestamps; skipped",
        "utterance 3 has invalid range 500-400; skipped",
    } <= set(result.warnings)


async def test_paraformer_asr_falls_back_to_full_text_without_sentences() -> None:
    adapter = AliyunParaformerASRProvider(
        api_key="test-key",
        transport=httpx.MockTransport(
            lambda request: _successful_asr_response(request, {"output": {"text": "只有全文"}})
        ),
        poll_interval_seconds=0,
    )

    result = await adapter.invoke(
        ProviderRequest(
            capability=ASR_TRANSCRIBE,
            request_id="req_text_only",
            payload={"audio_url": "https://oss.example/audio.wav"},
        )
    )

    assert result.error is None
    assert result.normalized_output["utterances"][0]["text"] == "只有全文"
    assert result.warnings == [
        "no sentence-level timestamps found; created untimed fallback utterance"
    ]


def test_paraformer_descriptor_declares_raw_word_timestamps() -> None:
    descriptor = aliyun_paraformer_asr_descriptor()

    assert descriptor.supports_word_timestamps is True
    assert descriptor.supports_raw_transcript is True
    assert descriptor.capabilities == [ASR_TRANSCRIBE]


def _transcript_response() -> dict[str, Any]:
    return {
        "sentences": [
            {
                "text": "呃这个产品。",
                "begin_time": 0,
                "end_time": 900,
                "words": [
                    {"text": "呃", "begin_time": 0, "end_time": 180},
                    {"text": "这", "begin_time": 180, "end_time": 300},
                    {"text": "个", "begin_time": 300, "end_time": 420},
                    {"text": "产", "begin_time": 420, "end_time": 560},
                    {"text": "品", "begin_time": 560, "end_time": 760},
                    {"text": "。", "begin_time": 760, "end_time": 800},
                ],
            }
        ]
    }


def _successful_asr_response(request: httpx.Request, transcript: dict[str, Any]) -> httpx.Response:
    if request.url.path.endswith("/api/v1/services/audio/asr/transcription"):
        return httpx.Response(200, json={"output": {"task_id": "task_1"}})
    if request.url.path.endswith("/api/v1/tasks/task_1"):
        return httpx.Response(
            200,
            json={
                "output": {
                    "task_status": "SUCCEEDED",
                    "transcription_url": "https://download.example/result.json",
                }
            },
        )
    return httpx.Response(200, json=transcript)


def _nested_transcript_response() -> dict[str, Any]:
    return {
        "output": {
            "transcripts": [
                {
                    "sentences": [
                        {
                            "content": "嗯你好！",
                            "start": 0.2,
                            "end": 1.2,
                            "tokens": [
                                {"value": "嗯", "start": 0.2, "end": 0.4},
                                {"value": "你好", "start": 0.4, "end": 1.0},
                                {"value": "！", "start": 1.0, "end": 1.1},
                            ],
                        }
                    ]
                }
            ]
        }
    }


def _warning_transcript_response() -> dict[str, Any]:
    return {
        "sentences": [
            {
                "text": "bad",
                "begin_time": 0,
                "end_time": 100,
                "words": [
                    {"begin_time": 0, "end_time": 10},
                    {"text": "缺", "begin_time": 10},
                    {"text": "倒", "begin_time": 30, "end_time": 20},
                    {"text": "后", "begin_time": 50, "end_time": 80},
                    {"text": "乱", "begin_time": 70, "end_time": 90},
                ],
            },
            {"text": "missing timestamps"},
            {"text": "invalid range", "begin_time": 500, "end_time": 400},
        ]
    }


def _words_are_monotonic(words: list[dict[str, Any]]) -> bool:
    previous_end = -1
    for word in words:
        if word["start_ms"] < previous_end or word["start_ms"] >= word["end_ms"]:
            return False
        previous_end = word["end_ms"]
    return True


async def test_submit_transport_errors_are_classified() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        raise httpx.ConnectError("refused", request=request)

    adapter = AliyunParaformerASRProvider(
        api_key="test-key",
        transport=httpx.MockTransport(handler),
        poll_interval_seconds=0,
    )
    result = await adapter.invoke(
        ProviderRequest(
            capability=ASR_TRANSCRIBE,
            payload={"audio_url": "https://oss.example/a.wav", "asset_id": "a"},
        )
    )
    assert result.error is not None
    assert result.error.error_code == "network_error"
    assert result.error.retryable is True


async def test_poll_timeout_returns_retryable_asr_timeout() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        if request.method == "POST":
            return httpx.Response(200, json={"output": {"task_id": "task_slow"}})
        return httpx.Response(200, json={"output": {"task_status": "RUNNING"}})

    adapter = AliyunParaformerASRProvider(
        api_key="test-key",
        transport=httpx.MockTransport(handler),
        poll_interval_seconds=0,
        poll_timeout_seconds=0.05,
    )
    result = await adapter.invoke(
        ProviderRequest(
            capability=ASR_TRANSCRIBE,
            payload={"audio_url": "https://oss.example/a.wav", "asset_id": "a"},
        )
    )
    assert result.error is not None
    assert result.error.error_code == "asr_timeout"
    assert result.error.retryable is True


async def test_poll_failed_status_is_terminal() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        if request.method == "POST":
            return httpx.Response(200, json={"output": {"task_id": "task_bad"}})
        return httpx.Response(200, json={"output": {"task_status": "FAILED"}})

    adapter = AliyunParaformerASRProvider(
        api_key="test-key",
        transport=httpx.MockTransport(handler),
        poll_interval_seconds=0,
    )
    result = await adapter.invoke(
        ProviderRequest(
            capability=ASR_TRANSCRIBE,
            payload={"audio_url": "https://oss.example/a.wav", "asset_id": "a"},
        )
    )
    assert result.error is not None
    assert result.error.retryable is False


async def test_download_transport_error_is_classified() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        if request.method == "POST":
            return httpx.Response(200, json={"output": {"task_id": "task_1"}})
        if "tasks" in str(request.url.path):
            return httpx.Response(
                200,
                json={
                    "output": {
                        "task_status": "SUCCEEDED",
                        "transcription_url": "https://download.example/r.json",
                    }
                },
            )
        raise httpx.ReadTimeout("slow", request=request)

    adapter = AliyunParaformerASRProvider(
        api_key="test-key",
        transport=httpx.MockTransport(handler),
        poll_interval_seconds=0,
    )
    result = await adapter.invoke(
        ProviderRequest(
            capability=ASR_TRANSCRIBE,
            payload={"audio_url": "https://oss.example/a.wav", "asset_id": "a"},
        )
    )
    assert result.error is not None
    assert result.error.error_code == "timeout"
