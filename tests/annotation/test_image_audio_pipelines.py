"""Image pipeline main path + M4 skeleton boundaries."""

from __future__ import annotations

import json
from pathlib import Path

import pytest
from pydantic import BaseModel, ConfigDict

from annotation.pipelines.audio import run_audio_annotation
from annotation.pipelines.bgm import run_bgm_annotation
from annotation.pipelines.image import run_image_annotation
from contracts.provider import ProviderDescriptor
from providers import VLM_ANNOTATION, ProviderGateway, ProviderRegistry
from providers.mock import MockProvider


class EmptyConfig(BaseModel):
    model_config = ConfigDict(extra="forbid")


def _image_gateway(script: list[dict[str, object]]) -> ProviderGateway:
    registry = ProviderRegistry()
    registry.register(
        ProviderDescriptor(
            provider_id="mock_vlm",
            display_name="mock_vlm",
            version="1",
            capabilities=[VLM_ANNOTATION],
            config_model=EmptyConfig,
            client_ref="tests.mock_vlm",
        ),
        MockProvider(provider_id="mock_vlm", scripts={VLM_ANNOTATION: script}),
    )
    return ProviderGateway(registry=registry)


def _fake_jpeg(tmp_path: Path) -> Path:
    path = tmp_path / "frame.jpg"
    path.write_bytes(b"\xff\xd8\xff\xe0fakejpegbytes")
    return path


async def test_image_pipeline_builds_single_clip_document(tmp_path: Path) -> None:
    gateway = _image_gateway(
        [
            {
                "normalized_output": {
                    "content": json.dumps(
                        {
                            "summary": "产品特写",
                            "keywords": ["product"],
                            "subject_type": "product",
                            "scene_type": "desk",
                            "contains_face": False,
                            "shot_type": "closeup",
                            "good_for": ["opening"],
                        }
                    )
                }
            }
        ]
    )

    document = await run_image_annotation(
        _fake_jpeg(tmp_path), asset_id="asset_img", gateway=gateway
    )

    assert document.asset_id == "asset_img"
    assert document.asset_kind == "image"
    assert len(document.clips) == 1
    clip = document.clips[0]
    assert clip.role == "image_candidate"
    assert clip.summary == "产品特写"
    extensions = clip.extensions.model_dump(mode="json", by_alias=True)
    assert extensions["vision.basic.v1"]["subject_type"] == "product"
    assert extensions["editing.affordance.v1"]["good_for"] == ["opening"]


async def test_image_pipeline_surfaces_provider_error(tmp_path: Path) -> None:
    gateway = _image_gateway(
        [
            {
                "error": {
                    "error_code": "vlm_failed",
                    "message": "provider exploded",
                    "retryable": False,
                }
            }
        ]
    )

    with pytest.raises(RuntimeError, match="provider exploded"):
        await run_image_annotation(_fake_jpeg(tmp_path), asset_id="asset_img", gateway=gateway)


def test_audio_and_bgm_pipelines_are_explicit_m4_boundaries() -> None:
    with pytest.raises(NotImplementedError, match="M4"):
        run_audio_annotation()
    with pytest.raises(NotImplementedError, match="M4"):
        run_bgm_annotation()


def test_parse_caption_accepts_dict_and_nested_annotation() -> None:
    from annotation.pipelines.image import _parse_caption

    direct = _parse_caption({"summary": "s", "keywords": ["k"]})
    assert direct.summary == "s"

    nested = _parse_caption({"annotation": {"summary": "n"}})
    assert nested.summary == "n"

    from_content = _parse_caption({"content": json.dumps({"annotation": {"summary": "c"}})})
    assert from_content.summary == "c"
