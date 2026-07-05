"""Tiny Volcengine Speech SaaS client for Rushes POC scripts."""

from __future__ import annotations

import base64
import binascii
import hashlib
import hmac
import json
import time
import uuid
from collections.abc import Mapping
from dataclasses import dataclass
from typing import Any, cast
from urllib.parse import parse_qsl, quote, urlparse

import httpx

JsonObject = dict[str, Any]

OPENAPI_HOST = "open.volcengineapi.com"
OPENAPI_REGION = "cn-north-1"
OPENAPI_SERVICE = "speech_saas_prod"
OPENAPI_KEY_VERSION = "2025-05-20"
DATA_BASE_URL = "https://openspeech.bytedance.com"
DATA_TTS_PATH = "/api/v1/tts"
DEFAULT_KEY_NAME = "rushes-poc"
DEFAULT_UID = "rushes-poc"
DEFAULT_ENCODING = "mp3"
DEFAULT_VOICE_TYPE = "S_UDXV2pG62"
DEFAULT_RESOURCE_ID = "volc.megatts.voiceclone"
SUCCESS_CODE = 3000


class VolcError(RuntimeError):
    """A Volcengine POC call failed."""


@dataclass(frozen=True)
class VolcCredentials:
    access_key_id: str
    secret_access_key: str
    appid: str
    cluster: str

    @classmethod
    def from_values(cls, *, aksk: str, appid: str, cluster: str) -> VolcCredentials:
        access_key_id, separator, secret_access_key = aksk.partition(":")
        if separator != ":" or not access_key_id.strip() or not secret_access_key.strip():
            raise VolcError("RUSHES_VOLC_TTS_AKSK 必须是 AccessKeyId:SecretAccessKey。")
        return cls(
            access_key_id=access_key_id.strip(),
            secret_access_key=secret_access_key.strip(),
            appid=appid.strip(),
            cluster=cluster.strip(),
        )


@dataclass(frozen=True)
class VolcTTSResult:
    request_payload: JsonObject
    response_json: JsonObject
    audio_bytes: bytes


def _canonical_query(raw: str) -> str:
    if not raw:
        return ""
    pairs = sorted(
        (quote(key, safe="-_.~"), quote(value, safe="-_.~"))
        for key, value in parse_qsl(raw, keep_blank_values=True)
    )
    return "&".join(f"{key}={value}" for key, value in pairs)


def signed_headers(
    *,
    access_key_id: str,
    secret_access_key: str,
    method: str,
    url: str,
    body: bytes,
    region: str = OPENAPI_REGION,
    service: str = OPENAPI_SERVICE,
) -> dict[str, str]:
    """Build Volcengine V4 HMAC-SHA256 signed headers."""
    parsed = urlparse(url)
    host = parsed.netloc
    path = parsed.path or "/"
    query = _canonical_query(parsed.query)
    x_date = time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())
    short_date = x_date[:8]
    payload_hash = hashlib.sha256(body).hexdigest()
    signed = "host;x-content-sha256;x-date"
    canonical_headers = f"host:{host}\nx-content-sha256:{payload_hash}\nx-date:{x_date}\n"
    canonical_request = "\n".join(
        [method.upper(), path, query, canonical_headers, signed, payload_hash]
    )
    hashed_request = hashlib.sha256(canonical_request.encode("utf-8")).hexdigest()
    scope = f"{short_date}/{region}/{service}/request"
    string_to_sign = "\n".join(["HMAC-SHA256", x_date, scope, hashed_request])

    def hmac_sha256(key: bytes, content: str) -> bytes:
        return hmac.new(key, content.encode("utf-8"), hashlib.sha256).digest()

    signing_key = hmac_sha256(
        hmac_sha256(
            hmac_sha256(hmac_sha256(secret_access_key.encode("utf-8"), short_date), region),
            service,
        ),
        "request",
    )
    signature = hmac.new(signing_key, string_to_sign.encode("utf-8"), hashlib.sha256).hexdigest()
    return {
        "Host": host,
        "X-Date": x_date,
        "X-Content-Sha256": payload_hash,
        "Authorization": (
            f"HMAC-SHA256 Credential={access_key_id}/{scope}, "
            f"SignedHeaders={signed}, Signature={signature}"
        ),
    }


class VolcTTSClient:
    """Minimal management-plane + data-plane client for Volcengine TTS."""

    def __init__(self, credentials: VolcCredentials, *, timeout_s: float = 120.0) -> None:
        self._credentials = credentials
        self._client = httpx.Client(
            timeout=httpx.Timeout(timeout_s, connect=10.0),
            trust_env=False,
        )

    def __enter__(self) -> VolcTTSClient:
        return self

    def __exit__(self, *_: object) -> None:
        self.close()

    def close(self) -> None:
        self._client.close()

    def ensure_api_key(self, *, name: str = DEFAULT_KEY_NAME) -> str:
        existing = self._list_active_keys()
        if existing:
            return existing[0]
        self._call_openapi("CreateAPIKey", {"AppID": self._credentials.appid, "Name": name})
        created = self._list_active_keys(name=name)
        if not created:
            raise VolcError("火山 CreateAPIKey 后未能取回可用 API Key。")
        return created[0]

    def synthesize(
        self,
        *,
        api_key: str,
        text: str,
        voice_type: str,
        resource_id: str = DEFAULT_RESOURCE_ID,
        uid: str = DEFAULT_UID,
        encoding: str = DEFAULT_ENCODING,
    ) -> VolcTTSResult:
        payload = build_tts_payload(
            appid=self._credentials.appid,
            cluster=self._credentials.cluster,
            text=text,
            voice_type=voice_type,
            uid=uid,
            encoding=encoding,
        )
        # 对齐 cutflow 生产实现：/api/v1/tts 用 x-api-key，不带 Resource-Id
        # （Bearer;<key> + Resource-Id 是 mega_tts 上传端点的头，混用会报 3001 grant not found）
        del resource_id
        headers = {
            "x-api-key": api_key,
            "Content-Type": "application/json",
        }
        try:
            response = self._client.post(
                f"{DATA_BASE_URL}{DATA_TTS_PATH}",
                headers=headers,
                json=payload,
            )
        except httpx.HTTPError as exc:
            raise VolcError(f"火山 TTS 合成请求失败：{exc}") from exc
        result = _checked_json(response, context="火山 TTS 合成")
        code = result.get("code")
        if code != SUCCESS_CODE:
            raise VolcError(f"火山 TTS code={code}: {result.get('message') or ''!s}")
        audio = _decode_audio_bytes(result)
        if not audio:
            raise VolcError("火山 TTS 响应缺少可解码音频。")
        return VolcTTSResult(request_payload=payload, response_json=result, audio_bytes=audio)

    def _list_active_keys(self, *, name: str | None = None) -> list[str]:
        result = self._call_openapi("ListAPIKeys", {"AppID": self._credentials.appid})
        keys: list[str] = []
        for item in _list_of_mappings(result.get("APIKeys")):
            if item.get("Disable") is True:
                continue
            api_key = str(item.get("APIKey") or "").strip()
            if not api_key:
                continue
            if name is not None and item.get("Name") != name:
                continue
            keys.append(api_key)
        return keys

    def _call_openapi(self, action: str, body: Mapping[str, object]) -> JsonObject:
        query = f"Action={action}&Version={OPENAPI_KEY_VERSION}"
        url = f"https://{OPENAPI_HOST}/?{query}"
        raw = json.dumps(dict(body), ensure_ascii=False).encode("utf-8")
        headers = signed_headers(
            access_key_id=self._credentials.access_key_id,
            secret_access_key=self._credentials.secret_access_key,
            method="POST",
            url=url,
            body=raw,
        )
        headers["Content-Type"] = "application/json"
        try:
            response = self._client.post(url, headers=headers, content=raw, timeout=30.0)
        except httpx.HTTPError as exc:
            raise VolcError(f"火山 OpenAPI {action} 请求失败：{exc}") from exc
        data = _checked_json(response, context=f"火山 OpenAPI {action}")
        error = data.get("ResponseMetadata")
        if isinstance(error, Mapping):
            nested_error = error.get("Error")
            if isinstance(nested_error, Mapping):
                code = str(nested_error.get("Code") or "unknown")
                raise VolcError(f"火山 OpenAPI {action} 失败 (Code={code})")
        result = data.get("Result")
        return cast(JsonObject, result) if isinstance(result, dict) else {}


def build_tts_payload(
    *,
    appid: str,
    cluster: str,
    text: str,
    voice_type: str,
    uid: str = DEFAULT_UID,
    encoding: str = DEFAULT_ENCODING,
) -> JsonObject:
    timestamp_extra = {
        "with_timestamp": 1,
        "enable_timestamp": 1,
        "frontend": {
            "with_timestamp": 1,
            "enable_timestamp": 1,
        },
    }
    del appid  # cutflow 生产实现的 app 块只带 cluster（appid 由 x-api-key 关联）
    return {
        "app": {"cluster": cluster},
        "user": {"uid": uid},
        "audio": {"voice_type": voice_type, "encoding": encoding},
        "request": {
            "reqid": f"rushes-{uuid.uuid4().hex}",
            "text": text,
            "operation": "query",
            "with_timestamp": 1,
            "extra_param": json.dumps(timestamp_extra, ensure_ascii=False),
        },
    }


def _checked_json(response: httpx.Response, *, context: str) -> JsonObject:
    if response.status_code < 200 or response.status_code >= 300:
        raise VolcError(f"{context} HTTP {response.status_code}: {response.text[:1000]}")
    try:
        data = response.json()
    except ValueError as exc:
        raise VolcError(f"{context} 返回非 JSON: {response.text[:800]}") from exc
    if not isinstance(data, dict):
        raise VolcError(f"{context} 返回 JSON 顶层不是 object。")
    return cast(JsonObject, data)


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
