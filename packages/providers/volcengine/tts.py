"""Volcengine Speech SaaS adapter for the tts.speech capability."""

from __future__ import annotations

import base64
import binascii
import json
import os
import time
import uuid
from collections.abc import Mapping
from dataclasses import dataclass
from typing import Any, cast

import httpx
from pydantic import BaseModel, ConfigDict

from contracts.provider import ProviderDescriptor, ProviderError, ProviderResult
from providers.capabilities import TTS_SPEECH, ProviderRequest
from providers.volcengine._sigv4 import signed_headers

VOLCENGINE_TTS_PROVIDER_ID = "volcengine_tts"
OPENAPI_HOST = "open.volcengineapi.com"
OPENAPI_KEY_VERSION = "2025-05-20"
DATA_BASE_URL = "https://openspeech.bytedance.com"
DATA_TTS_PATH = "/api/v1/tts"
DEFAULT_KEY_NAME = "rushes"
DEFAULT_UID = "rushes"
DEFAULT_ENCODING = "mp3"
DEFAULT_VOICE_TYPE = "S_UDXV2pG62"
SUCCESS_CODE = 3000
GRANT_NOT_FOUND_CODE = 3001
JsonObject = dict[str, Any]


class VolcengineTTSConfig(BaseModel):
    model_config = ConfigDict(extra="forbid")

    aksk_env: str = "RUSHES_VOLC_TTS_AKSK"
    appid_env: str = "RUSHES_VOLC_TTS_APPID"
    cluster_env: str = "RUSHES_VOLC_TTS_CLUSTER"
    voice_type_env: str = "RUSHES_VOLC_TTS_VOICE_TYPE"
    priority: int = 10
    timeout_seconds: float = 120.0


@dataclass(frozen=True, slots=True)
class _Credentials:
    access_key_id: str
    secret_access_key: str
    appid: str
    cluster: str


class VolcengineTTSProvider:
    """Synthesize speech through Volcengine; timestamp fallback is owned by callers."""

    def __init__(
        self,
        *,
        aksk: str | None = None,
        appid: str | None = None,
        cluster: str | None = None,
        voice_type: str | None = None,
        data_base_url: str = DATA_BASE_URL,
        timeout: float | httpx.Timeout = 120.0,
        transport: httpx.AsyncBaseTransport | None = None,
        force_ipv4: bool = True,
        key_name: str = DEFAULT_KEY_NAME,
    ) -> None:
        self.provider_id = VOLCENGINE_TTS_PROVIDER_ID
        self._credentials_error: ProviderError | None = None
        self._credentials = self._load_credentials(aksk=aksk, appid=appid, cluster=cluster)
        self._voice_type = (
            voice_type or os.environ.get("RUSHES_VOLC_TTS_VOICE_TYPE") or DEFAULT_VOICE_TYPE
        )
        self._data_base_url = data_base_url.rstrip("/")
        self._timeout = timeout
        self._key_name = key_name
        if transport is None and force_ipv4:
            transport = httpx.AsyncHTTPTransport(local_address="0.0.0.0")
        self._transport = transport

    async def invoke(self, request: ProviderRequest) -> ProviderResult:
        started = time.monotonic()
        request_id = request.request_id or f"tts_{uuid.uuid4().hex}"
        if self._credentials_error is not None:
            return self._error_result(request, request_id, started, self._credentials_error)
        credentials = self._credentials
        if credentials is None:
            return self._error_result(
                request,
                request_id,
                started,
                ProviderError(
                    error_code="missing_tts_credentials",
                    message="Volcengine TTS credentials are not configured",
                    retryable=False,
                ),
            )
        text = _payload_text(request.payload.get("text"))
        if text is None:
            return self._error_result(
                request,
                request_id,
                started,
                ProviderError(
                    error_code="invalid_tts_request",
                    message="tts.speech payload requires non-empty text",
                    retryable=False,
                ),
            )

        async with httpx.AsyncClient(
            timeout=self._timeout,
            trust_env=False,
            transport=self._transport,
        ) as client:
            api_key = await self._ensure_api_key(client, credentials)
            if isinstance(api_key, ProviderError):
                return self._error_result(request, request_id, started, api_key)
            voice_type = _payload_text(request.payload.get("voice_type")) or self._voice_type
            response = await self._synthesize(
                client,
                credentials,
                api_key=api_key,
                text=text,
                voice_type=voice_type,
                request_id=request_id,
            )
            if isinstance(response, ProviderError):
                return self._error_result(request, request_id, started, response)

        return ProviderResult(
            provider_id=self.provider_id,
            capability=request.capability,
            request_id=request_id,
            model=voice_type,
            latency_ms=_elapsed_ms(started),
            raw_ref=request_id,
            normalized_output={
                "audio_bytes": response["audio_bytes"],
                "encoding": DEFAULT_ENCODING,
                "supports_native_timestamps": False,
                "request_payload": response["request_payload"],
            },
        )

    async def _ensure_api_key(
        self,
        client: httpx.AsyncClient,
        credentials: _Credentials,
    ) -> str | ProviderError:
        listed = await self._list_active_keys(client, credentials)
        if isinstance(listed, ProviderError):
            return listed
        if listed:
            return listed[0]
        created = await self._call_openapi(
            client,
            credentials,
            "CreateAPIKey",
            {"AppID": credentials.appid, "Name": self._key_name},
        )
        if isinstance(created, ProviderError):
            return created
        listed_after_create = await self._list_active_keys(client, credentials, name=self._key_name)
        if isinstance(listed_after_create, ProviderError):
            return listed_after_create
        if not listed_after_create:
            return ProviderError(
                error_code="tts_api_key_missing",
                message="CreateAPIKey succeeded but no active API key was returned",
                retryable=True,
            )
        return listed_after_create[0]

    async def _list_active_keys(
        self,
        client: httpx.AsyncClient,
        credentials: _Credentials,
        *,
        name: str | None = None,
    ) -> list[str] | ProviderError:
        result = await self._call_openapi(
            client,
            credentials,
            "ListAPIKeys",
            {"AppID": credentials.appid},
        )
        if isinstance(result, ProviderError):
            return result
        keys: list[str] = []
        for item in _list_of_mappings(result.get("APIKeys")):
            if item.get("Disable") is True:
                continue
            if name is not None and item.get("Name") != name:
                continue
            api_key = str(item.get("APIKey") or "").strip()
            if api_key:
                keys.append(api_key)
        return keys

    async def _call_openapi(
        self,
        client: httpx.AsyncClient,
        credentials: _Credentials,
        action: str,
        body: Mapping[str, object],
    ) -> JsonObject | ProviderError:
        query = f"Action={action}&Version={OPENAPI_KEY_VERSION}"
        url = f"https://{OPENAPI_HOST}/?{query}"
        raw = json.dumps(dict(body), ensure_ascii=False).encode("utf-8")
        headers = signed_headers(
            access_key_id=credentials.access_key_id,
            secret_access_key=credentials.secret_access_key,
            method="POST",
            url=url,
            body=raw,
        )
        headers["Content-Type"] = "application/json"
        try:
            response = await client.post(url, headers=headers, content=raw, timeout=30.0)
        except httpx.TimeoutException as exc:
            return _transport_error("timeout", exc, retryable=True)
        except httpx.TransportError as exc:
            return _transport_error("network_error", exc, retryable=True)
        data = _checked_json(response, context=f"Volcengine OpenAPI {action}")
        if isinstance(data, ProviderError):
            return data
        metadata = data.get("ResponseMetadata")
        if isinstance(metadata, Mapping):
            nested_error = metadata.get("Error")
            if isinstance(nested_error, Mapping):
                code = str(nested_error.get("Code") or "unknown")
                return ProviderError(
                    error_code=f"volcengine_openapi_{code}",
                    message=f"Volcengine OpenAPI {action} failed: {code}",
                    retryable=False,
                    details={"response": data},
                )
        result = data.get("Result")
        return cast(JsonObject, result) if isinstance(result, dict) else {}

    async def _synthesize(
        self,
        client: httpx.AsyncClient,
        credentials: _Credentials,
        *,
        api_key: str,
        text: str,
        voice_type: str,
        request_id: str,
    ) -> JsonObject | ProviderError:
        payload = _build_tts_payload(
            cluster=credentials.cluster,
            text=text,
            voice_type=voice_type,
            request_id=request_id,
        )
        headers = {"x-api-key": api_key, "Content-Type": "application/json"}
        try:
            response = await client.post(
                f"{self._data_base_url}{DATA_TTS_PATH}",
                headers=headers,
                json=payload,
            )
        except httpx.TimeoutException as exc:
            return _transport_error("timeout", exc, retryable=True)
        except httpx.TransportError as exc:
            return _transport_error("network_error", exc, retryable=True)
        data = _checked_json(response, context="Volcengine TTS synthesize")
        if isinstance(data, ProviderError):
            return data
        code = data.get("code")
        if code != SUCCESS_CODE:
            return _tts_code_error(code, data)
        audio = _decode_audio_bytes(data)
        if not audio:
            return ProviderError(
                error_code="tts_audio_missing",
                message="Volcengine TTS response does not include decodable audio",
                retryable=False,
                details={"response": data},
            )
        return {
            "audio_bytes": audio,
            "request_payload": payload,
            "response_json": data,
        }

    def _load_credentials(
        self,
        *,
        aksk: str | None,
        appid: str | None,
        cluster: str | None,
    ) -> _Credentials | None:
        raw_aksk = aksk or os.environ.get("RUSHES_VOLC_TTS_AKSK")
        raw_appid = appid or os.environ.get("RUSHES_VOLC_TTS_APPID")
        raw_cluster = cluster or os.environ.get("RUSHES_VOLC_TTS_CLUSTER")
        if not raw_aksk or not raw_appid or not raw_cluster:
            return None
        access_key_id, separator, secret_access_key = raw_aksk.partition(":")
        if separator != ":" or not access_key_id.strip() or not secret_access_key.strip():
            self._credentials_error = ProviderError(
                error_code="invalid_tts_credentials",
                message="RUSHES_VOLC_TTS_AKSK must be AccessKeyId:SecretAccessKey",
                retryable=False,
            )
            return None
        return _Credentials(
            access_key_id=access_key_id.strip(),
            secret_access_key=secret_access_key.strip(),
            appid=raw_appid.strip(),
            cluster=raw_cluster.strip(),
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
            model=self._voice_type,
            latency_ms=_elapsed_ms(started),
            error=error,
        )


def volcengine_tts_descriptor(*, priority: int = 10) -> ProviderDescriptor:
    return ProviderDescriptor(
        provider_id=VOLCENGINE_TTS_PROVIDER_ID,
        display_name="Volcengine Speech SaaS TTS",
        version="1",
        capabilities=[TTS_SPEECH],
        config_model=VolcengineTTSConfig,
        client_ref="providers.volcengine.tts.VolcengineTTSProvider",
        supports_native_timestamps=False,
        priority=priority,
    )


def _build_tts_payload(
    *,
    cluster: str,
    text: str,
    voice_type: str,
    request_id: str,
) -> JsonObject:
    timestamp_extra = {
        "with_timestamp": 1,
        "enable_timestamp": 1,
        "frontend": {"with_timestamp": 1, "enable_timestamp": 1},
    }
    return {
        "app": {"cluster": cluster},
        "user": {"uid": DEFAULT_UID},
        "audio": {"voice_type": voice_type, "encoding": DEFAULT_ENCODING},
        "request": {
            "reqid": request_id,
            "text": text,
            "operation": "query",
            "with_timestamp": 1,
            "extra_param": json.dumps(timestamp_extra, ensure_ascii=False),
        },
    }


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


def _tts_code_error(code: object, data: JsonObject) -> ProviderError:
    if code == GRANT_NOT_FOUND_CODE:
        return ProviderError(
            error_code="tts_grant_not_found",
            message="Volcengine TTS grant not found",
            retryable=False,
            details={"code": code, "response": data},
        )
    return ProviderError(
        error_code="tts_provider_error",
        message=f"Volcengine TTS returned code={code}",
        retryable=False,
        details={"code": code, "response": data},
    )


def _decode_audio_bytes(response: Mapping[str, object]) -> bytes:
    for key in ("data", "audio", "audio_data", "audioData"):
        value = response.get(key)
        if isinstance(value, str) and value.strip():
            return _decode_audio_string(value)
    nested = response.get("result")
    if isinstance(nested, Mapping):
        return _decode_audio_bytes(cast(Mapping[str, object], nested))
    return b""


def _decode_audio_string(value: str) -> bytes:
    stripped = value.strip()
    try:
        return base64.b64decode(stripped, validate=True)
    except (ValueError, binascii.Error):
        if len(stripped) % 2 == 0:
            try:
                return bytes.fromhex(stripped)
            except ValueError:
                return b""
        return b""


def _list_of_mappings(value: object) -> list[Mapping[str, object]]:
    if not isinstance(value, list):
        return []
    return [cast(Mapping[str, object], item) for item in value if isinstance(item, Mapping)]


def _transport_error(error_code: str, exc: Exception, *, retryable: bool) -> ProviderError:
    return ProviderError(
        error_code=error_code,
        message=str(exc),
        retryable=retryable,
        details={"exception_type": type(exc).__name__},
    )


def _payload_text(value: object) -> str | None:
    if not isinstance(value, str):
        return None
    stripped = value.strip()
    return stripped or None


def _elapsed_ms(started: float) -> int:
    return max(0, int((time.monotonic() - started) * 1000))
