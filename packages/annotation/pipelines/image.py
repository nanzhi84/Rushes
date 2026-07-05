"""Image annotation pipeline."""

from __future__ import annotations

import base64
import json
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from pydantic import BaseModel, ConfigDict, Field

from contracts.annotation import (
    AnnotationClip,
    AnnotationDocument,
    AnnotationExtensions,
    AnnotationGenerator,
    EditingAffordanceExtension,
    VisionBasicExtension,
)
from providers import VLM_ANNOTATION, ProviderGateway, ProviderRequest


class ImageCaption(BaseModel):
    model_config = ConfigDict(extra="ignore")

    summary: str
    keywords: list[str] = Field(default_factory=list)
    subject_type: str | None = None
    scene_type: str | None = None
    action: str | None = None
    contains_face: bool | None = None
    shot_type: str | None = None
    good_for: list[str] = Field(default_factory=list)


async def run_image_annotation(
    image_path: str | Path,
    *,
    asset_id: str,
    gateway: ProviderGateway,
    job_id: str | None = None,
    case_id: str | None = None,
    model: str | None = None,
) -> AnnotationDocument:
    path = Path(image_path).expanduser().resolve(strict=True)
    payload = base64.b64encode(path.read_bytes()).decode("ascii")
    result = await gateway.call(
        ProviderRequest(
            capability=VLM_ANNOTATION,
            model=model,
            case_id=case_id,
            job_id=job_id,
            payload={
                "messages": [
                    {
                        "role": "user",
                        "content": [
                            {
                                "type": "text",
                                "text": (
                                    "Return strict JSON image caption with summary and keywords."
                                ),
                            },
                            {
                                "type": "image_url",
                                "image_url": {"url": f"data:image/jpeg;base64,{payload}"},
                            },
                        ],
                    }
                ],
                "params": {"temperature": 0, "response_format": {"type": "json_object"}},
            },
        )
    )
    if result.result.error is not None:
        raise RuntimeError(result.result.error.message)
    caption = _parse_caption(result.result.normalized_output)
    return AnnotationDocument(
        annotation_id=f"ann_{asset_id}",
        asset_id=asset_id,
        asset_kind="image",
        status="completed",
        generator=AnnotationGenerator.model_validate(
            {
                "pipeline_version": "annotation.image.v1",
                "pass": "cheap",
                "provider_refs": [result.result.request_id],
            }
        ),
        clips=[
            AnnotationClip(
                clip_id="clip_0001",
                source_start_frame=0,
                source_end_frame=1,
                role="image_candidate",
                summary=caption.summary,
                keywords=caption.keywords,
                quality_score=1.0,
                extensions=AnnotationExtensions(
                    vision_basic_v1=VisionBasicExtension(
                        subject_type=caption.subject_type,
                        scene_type=caption.scene_type,
                        action=caption.action,
                        contains_face=caption.contains_face,
                        shot_type=caption.shot_type,
                    ),
                    editing_affordance_v1=EditingAffordanceExtension(good_for=caption.good_for),
                ),
            )
        ],
        created_at=datetime.now(UTC).isoformat(),
    )


def _parse_caption(output: dict[str, Any]) -> ImageCaption:
    content = output.get("content")
    if isinstance(content, str) and content:
        parsed = json.loads(content)
        if isinstance(parsed, dict):
            return ImageCaption.model_validate(parsed.get("annotation", parsed))
    return ImageCaption.model_validate(output.get("annotation", output))
