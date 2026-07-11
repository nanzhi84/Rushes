"""Asset record contracts."""

from enum import StrEnum
from typing import Literal

from pydantic import BaseModel, ConfigDict, Field, model_validator


class StorageMode(StrEnum):
    COPY = "copy"
    REFERENCE = "reference"


class AssetKind(StrEnum):
    VIDEO = "video"
    IMAGE = "image"
    AUDIO = "audio"
    FONT = "font"


class AssetSource(StrEnum):
    UPLOAD = "upload"
    LOCAL_PATH = "local_path"
    URL = "url"


class AssetProbe(BaseModel):
    model_config = ConfigDict(extra="forbid")

    duration_sec: float
    fps: float | None = None
    width: int | None = None
    height: int | None = None
    has_audio: bool = False


class AssetFailure(BaseModel):
    model_config = ConfigDict(extra="forbid")

    error_code: str
    message: str
    retryable: bool = False


class AssetRecord(BaseModel):
    model_config = ConfigDict(extra="forbid")

    asset_id: str
    storage_mode: StorageMode
    workspace_object_uri: str | None = None
    reference_path: str | None = None
    kind: AssetKind
    source: AssetSource
    filename: str
    hash: str
    mtime: int
    size: int
    probe: AssetProbe
    proxy_object_uri: str | None = None
    ingest_status: Literal[
        "imported", "probing", "probed", "proxying", "indexed", "ready", "failed"
    ]
    usable: bool
    failure: AssetFailure | None = None

    @model_validator(mode="after")
    def validate_storage_mode(self) -> "AssetRecord":
        if self.storage_mode is StorageMode.COPY:
            if self.reference_path is not None:
                raise ValueError("copy storage mode requires reference_path to be None")
            if self.workspace_object_uri is None:
                raise ValueError("copy storage mode requires workspace_object_uri")
        if self.storage_mode is StorageMode.REFERENCE:
            if self.workspace_object_uri is not None:
                raise ValueError("reference storage mode requires workspace_object_uri to be None")
            if self.reference_path is None:
                raise ValueError("reference storage mode requires reference_path")
        return self


class AssetManifestEntry(BaseModel):
    """asset.list_assets 面向 planner 的紧凑素材清单条目。"""

    model_config = ConfigDict(extra="forbid")

    asset_id: str
    filename: str
    kind: AssetKind
    rel_dir: str | None = None
    duration_sec: float | None = None
    fps: float | None = None
    width: int | None = None
    height: int | None = None
    orientation: Literal["landscape", "portrait", "square"] | None = None
    has_audio: bool | None = None
    usable: bool
    ingest_status: str
    understanding_status: str
    has_summary: bool
    thumbnail_ready: bool


class AssetListAssetsResult(BaseModel):
    model_config = ConfigDict(extra="forbid")

    draft_id: str
    assets: list[AssetManifestEntry] = Field(default_factory=list)
    total: int = Field(ge=0)
    next_after: str | None = None
