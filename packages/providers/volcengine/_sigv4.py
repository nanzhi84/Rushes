"""Volcengine OpenAPI V4 signing helpers."""

from __future__ import annotations

import hashlib
import hmac
import time
from urllib.parse import parse_qsl, quote, urlparse

OPENAPI_REGION = "cn-north-1"
OPENAPI_SERVICE = "speech_saas_prod"


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
    """Build Volcengine HMAC-SHA256 V4 signed headers."""

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
    signing_key = _hmac_sha256(
        _hmac_sha256(
            _hmac_sha256(_hmac_sha256(secret_access_key.encode("utf-8"), short_date), region),
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


def _canonical_query(raw: str) -> str:
    if not raw:
        return ""
    pairs = sorted(
        (quote(key, safe="-_.~"), quote(value, safe="-_.~"))
        for key, value in parse_qsl(raw, keep_blank_values=True)
    )
    return "&".join(f"{key}={value}" for key, value in pairs)


def _hmac_sha256(key: bytes, content: str) -> bytes:
    return hmac.new(key, content.encode("utf-8"), hashlib.sha256).digest()
