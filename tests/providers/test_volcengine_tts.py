from __future__ import annotations

import base64
import hashlib
import json
import time

import httpx

from providers import TTS_SPEECH, ProviderRequest
from providers.volcengine import VolcengineTTSProvider, _sigv4, volcengine_tts_descriptor


async def test_volcengine_tts_ensures_api_key_and_sends_x_api_key() -> None:
    calls: list[httpx.Request] = []

    def handler(request: httpx.Request) -> httpx.Response:
        calls.append(request)
        if request.url.params.get("Action") == "ListAPIKeys" and len(calls) == 1:
            return httpx.Response(200, json={"Result": {"APIKeys": []}})
        if request.url.params.get("Action") == "CreateAPIKey":
            return httpx.Response(200, json={"Result": {}})
        if request.url.params.get("Action") == "ListAPIKeys":
            return httpx.Response(
                200,
                json={"Result": {"APIKeys": [{"APIKey": "api-key-1", "Name": "rushes"}]}},
            )
        assert request.url.path == "/api/v1/tts"
        assert request.headers["x-api-key"] == "api-key-1"
        body = json.loads(request.content.decode())
        assert body["app"] == {"cluster": "volcano_icl"}
        assert "appid" not in body["app"]
        assert body["request"]["text"] == "你好"
        return httpx.Response(200, json={"code": 3000, "data": base64.b64encode(b"mp3").decode()})

    adapter = VolcengineTTSProvider(
        aksk="ak:sk",
        appid="appid",
        cluster="volcano_icl",
        voice_type="voice_1",
        transport=httpx.MockTransport(handler),
    )

    result = await adapter.invoke(
        ProviderRequest(capability=TTS_SPEECH, request_id="req_1", payload={"text": "你好"})
    )

    assert result.error is None
    assert result.normalized_output["audio_bytes"] == b"mp3"
    assert result.normalized_output["supports_native_timestamps"] is False


async def test_volcengine_tts_classifies_3001_grant_error() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.params.get("Action") == "ListAPIKeys":
            return httpx.Response(
                200,
                json={"Result": {"APIKeys": [{"APIKey": "api-key-1", "Name": "rushes"}]}},
            )
        return httpx.Response(200, json={"code": 3001, "message": "grant not found"})

    adapter = VolcengineTTSProvider(
        aksk="ak:sk",
        appid="appid",
        cluster="volcano_icl",
        transport=httpx.MockTransport(handler),
    )

    result = await adapter.invoke(
        ProviderRequest(capability=TTS_SPEECH, request_id="req_1", payload={"text": "你好"})
    )

    assert result.error is not None
    assert result.error.error_code == "tts_grant_not_found"
    assert result.error.retryable is False


async def test_volcengine_tts_reports_missing_key_after_create() -> None:
    actions: list[str] = []

    def handler(request: httpx.Request) -> httpx.Response:
        action = str(request.url.params.get("Action"))
        actions.append(action)
        if action == "ListAPIKeys":
            return httpx.Response(200, json={"Result": {"APIKeys": []}})
        if action == "CreateAPIKey":
            return httpx.Response(200, json={"Result": {}})
        return httpx.Response(500, json={"unexpected": True})

    adapter = VolcengineTTSProvider(
        aksk="ak:sk",
        appid="appid",
        cluster="volcano_icl",
        transport=httpx.MockTransport(handler),
    )

    result = await adapter.invoke(
        ProviderRequest(capability=TTS_SPEECH, request_id="req_1", payload={"text": "你好"})
    )

    assert result.error is not None
    assert result.error.error_code == "tts_api_key_missing"
    assert result.error.retryable is True
    assert actions == ["ListAPIKeys", "CreateAPIKey", "ListAPIKeys"]


async def test_volcengine_tts_classifies_openapi_401() -> None:
    adapter = VolcengineTTSProvider(
        aksk="ak:sk",
        appid="appid",
        cluster="volcano_icl",
        transport=httpx.MockTransport(lambda _request: httpx.Response(401, json={"message": "no"})),
    )

    result = await adapter.invoke(
        ProviderRequest(capability=TTS_SPEECH, request_id="req_1", payload={"text": "你好"})
    )

    assert result.error is not None
    assert result.error.error_code == "http_status_401"
    assert result.error.retryable is False


async def test_volcengine_tts_classifies_openapi_auth_code() -> None:
    adapter = VolcengineTTSProvider(
        aksk="ak:sk",
        appid="appid",
        cluster="volcano_icl",
        transport=httpx.MockTransport(
            lambda _request: httpx.Response(
                200,
                json={"ResponseMetadata": {"Error": {"Code": "AUTHFailure"}}},
            )
        ),
    )

    result = await adapter.invoke(
        ProviderRequest(capability=TTS_SPEECH, request_id="req_1", payload={"text": "你好"})
    )

    assert result.error is not None
    assert result.error.error_code == "volcengine_openapi_AUTHFailure"
    assert result.error.retryable is False


async def test_volcengine_tts_reports_success_without_audio_field() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.params.get("Action") == "ListAPIKeys":
            return httpx.Response(
                200,
                json={"Result": {"APIKeys": [{"APIKey": "api-key-1", "Name": "rushes"}]}},
            )
        return httpx.Response(200, json={"code": 3000, "message": "ok"})

    adapter = VolcengineTTSProvider(
        aksk="ak:sk",
        appid="appid",
        cluster="volcano_icl",
        transport=httpx.MockTransport(handler),
    )

    result = await adapter.invoke(
        ProviderRequest(capability=TTS_SPEECH, request_id="req_1", payload={"text": "你好"})
    )

    assert result.error is not None
    assert result.error.error_code == "tts_audio_missing"
    assert result.error.retryable is False


def test_volcengine_sigv4_canonicalizes_query_and_signed_headers(monkeypatch) -> None:
    monkeypatch.setattr(
        _sigv4.time,
        "gmtime",
        lambda: time.strptime("2026-07-05T01:02:03Z", "%Y-%m-%dT%H:%M:%SZ"),
    )
    body = b'{"text":"hi"}'

    headers = _sigv4.signed_headers(
        access_key_id="ak",
        secret_access_key="sk",
        method="post",
        url="https://open.volcengineapi.com/path?b=2&a=hello world&a=",
        body=body,
    )

    assert _sigv4._canonical_query("b=2&a=hello world&a=") == "a=&a=hello%20world&b=2"
    assert _sigv4._canonical_query("") == ""
    assert headers["Host"] == "open.volcengineapi.com"
    assert headers["X-Date"] == "20260705T010203Z"
    assert headers["X-Content-Sha256"] == hashlib.sha256(body).hexdigest()
    assert "Credential=ak/20260705/cn-north-1/speech_saas_prod/request" in headers["Authorization"]
    assert "SignedHeaders=host;x-content-sha256;x-date" in headers["Authorization"]


def test_volcengine_tts_descriptor_has_no_native_timestamps() -> None:
    descriptor = volcengine_tts_descriptor()

    assert descriptor.supports_native_timestamps is False
    assert descriptor.capabilities == [TTS_SPEECH]


async def test_volcengine_tts_transport_errors_and_key_filters() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.params.get("Action") == "ListAPIKeys":
            return httpx.Response(
                200,
                json={
                    "Result": {
                        "APIKeys": [
                            {"APIKey": "dead", "Name": "rushes", "Disable": True},
                            {"APIKey": "", "Name": "rushes"},
                            {"APIKey": "other", "Name": "someone-else"},
                            {"APIKey": "live-key", "Name": "rushes"},
                        ]
                    }
                },
            )
        raise httpx.ConnectError("tts down", request=request)

    adapter = VolcengineTTSProvider(
        aksk="ak:sk",
        appid="appid",
        cluster="volcano_icl",
        voice_type="voice_1",
        key_name="rushes",
        transport=httpx.MockTransport(handler),
    )

    result = await adapter.invoke(ProviderRequest(capability=TTS_SPEECH, payload={"text": "你好"}))

    assert result.error is not None
    assert result.error.error_code == "network_error"
    assert result.error.retryable is True


async def test_volcengine_tts_openapi_timeout_is_retryable() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        raise httpx.ReadTimeout("mgmt slow", request=request)

    adapter = VolcengineTTSProvider(
        aksk="ak:sk",
        appid="appid",
        cluster="volcano_icl",
        voice_type="voice_1",
        transport=httpx.MockTransport(handler),
    )

    result = await adapter.invoke(ProviderRequest(capability=TTS_SPEECH, payload={"text": "你好"}))

    assert result.error is not None
    assert result.error.error_code == "timeout"


async def test_volcengine_tts_invalid_aksk_is_terminal() -> None:
    adapter = VolcengineTTSProvider(
        aksk="not-a-pair",
        appid="appid",
        cluster="volcano_icl",
        voice_type="voice_1",
        transport=httpx.MockTransport(lambda request: httpx.Response(500)),
    )

    result = await adapter.invoke(ProviderRequest(capability=TTS_SPEECH, payload={"text": "你好"}))

    assert result.error is not None
    assert result.error.error_code == "invalid_tts_credentials"
    assert result.error.retryable is False


async def test_volcengine_tts_missing_text_rejected() -> None:
    adapter = VolcengineTTSProvider(
        aksk="ak:sk",
        appid="appid",
        cluster="volcano_icl",
        voice_type="voice_1",
        transport=httpx.MockTransport(lambda request: httpx.Response(500)),
    )

    result = await adapter.invoke(ProviderRequest(capability=TTS_SPEECH, payload={}))

    assert result.error is not None
