"""Upload local ASR audio to OSS and return a presigned public URL."""

from __future__ import annotations

import os
from dataclasses import dataclass
from datetime import UTC, datetime
from pathlib import Path
from typing import Any, cast


class OssConfigError(RuntimeError):
    """Raised when OSS upload configuration is missing or invalid."""


class OssUploadError(RuntimeError):
    """Raised when OSS upload or cleanup fails."""


@dataclass(frozen=True, slots=True)
class OssUpload:
    bucket: Any
    key: str
    signed_url: str

    def delete(self) -> None:
        self.bucket.delete_object(self.key)


def upload_audio_to_oss(
    audio_path: str | Path,
    *,
    key_prefix: str = "rushes/asr",
    expires_seconds: int = 3600,
) -> OssUpload:
    """Upload an audio file to OSS and return a one-hour GET URL by default."""

    source = Path(audio_path).expanduser().resolve(strict=True)
    config = _oss_config()
    try:
        import oss2
    except ImportError as exc:
        raise OssConfigError("oss2 is required for ASR uploads") from exc
    auth = oss2.Auth(config["access_key"], config["secret_key"])
    bucket = oss2.Bucket(auth, config["endpoint"], config["bucket"])
    key = f"{key_prefix.rstrip('/')}/{_timestamp()}_{source.name}"
    try:
        bucket.put_object_from_file(key, str(source))
        signed_url = cast(str, bucket.sign_url("GET", key, expires_seconds))
    except Exception as exc:
        raise OssUploadError(f"OSS upload failed for {source}") from exc
    return OssUpload(bucket=bucket, key=key, signed_url=signed_url)


def _oss_config() -> dict[str, str]:
    names = {
        "endpoint": "RUSHES_OSS_ENDPOINT",
        "region": "RUSHES_OSS_REGION",
        "bucket": "RUSHES_OSS_BUCKET",
        "access_key": "RUSHES_OSS_ACCESS_KEY",
        "secret_key": "RUSHES_OSS_SECRET_KEY",
    }
    values: dict[str, str] = {}
    missing: list[str] = []
    for key, env_name in names.items():
        value = os.environ.get(env_name)
        if not value:
            missing.append(env_name)
            continue
        values[key] = value
    if missing:
        raise OssConfigError(f"missing OSS env vars: {', '.join(missing)}")
    return values


def _timestamp() -> str:
    return datetime.now(tz=UTC).strftime("%Y%m%dT%H%M%SZ")
