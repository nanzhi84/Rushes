"""Video annotation cheap/deep pipeline."""

from __future__ import annotations

import base64
import json
from collections.abc import Iterable, Sequence
from dataclasses import dataclass, field
from datetime import UTC, datetime
from pathlib import Path
from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field, ValidationError, field_validator

from annotation.quality import QualityConfig, clean_spans, detect_quality_events
from annotation.shot_split import Shot, ShotSplitConfig, split_shots
from contracts.annotation import (
    AnnotationClip,
    AnnotationDocument,
    AnnotationExtensions,
    AnnotationGenerator,
    EditingAffordanceExtension,
    QualityEvent,
    VisionBasicExtension,
    VisionCompositionExtension,
)
from providers import VLM_ANNOTATION, ProviderGateway, ProviderRequest


class AnnotationPipelineError(RuntimeError):
    """Raised when an annotation pipeline cannot produce a valid document."""


class VLMCompositionPayload(BaseModel):
    model_config = ConfigDict(extra="ignore")

    safe_area: Literal["ok", "overflow", "unknown"] = "unknown"
    subtitle_occlusion_risk: Literal["none", "low", "medium", "high", "unknown"] = "unknown"
    subject_position: str | None = None
    framing_notes: list[str] = Field(default_factory=list)

    # 真实 VLM 会给出枚举外的值（如 safe_area="center"）；两次重试仍可能
    # 违规，schema 必须自己收敛而不是让整个标注 job 失败（M9 路径 1 实测）。
    @field_validator("safe_area", mode="before")
    @classmethod
    def _coerce_safe_area(cls, value: object) -> object:
        return value if value in {"ok", "overflow", "unknown"} else "unknown"

    @field_validator("subtitle_occlusion_risk", mode="before")
    @classmethod
    def _coerce_occlusion(cls, value: object) -> object:
        return value if value in {"none", "low", "medium", "high", "unknown"} else "unknown"

    @field_validator("framing_notes", mode="before")
    @classmethod
    def _coerce_notes(cls, value: object) -> list[str]:
        if value is None:
            return []
        if isinstance(value, str):
            return [value]
        if isinstance(value, Sequence) and not isinstance(value, bytes | bytearray):
            return [str(item) for item in value if str(item)]
        return []


class VLMShotAnnotation(BaseModel):
    model_config = ConfigDict(extra="ignore")

    summary: str
    keywords: list[str] = Field(default_factory=list)
    subject_type: str | None = None
    scene_type: str | None = None
    action: str | None = None
    contains_face: bool | None = None
    shot_type: str | None = None
    good_for: list[str] = Field(default_factory=list)
    avoid: bool = False
    quality_score: float | None = None
    role: Literal["a_roll_candidate", "b_roll_candidate", "avoid"] | None = None
    composition: VLMCompositionPayload | None = None

    @field_validator("keywords", "good_for", mode="before")
    @classmethod
    def _coerce_string_list(cls, value: object) -> list[str]:
        if value is None:
            return []
        if isinstance(value, str):
            return [value]
        if isinstance(value, Sequence) and not isinstance(value, bytes | bytearray):
            return [str(item) for item in value if str(item)]
        return []

    @field_validator("avoid", mode="before")
    @classmethod
    def _coerce_avoid(cls, value: object) -> object:
        # VLM 偶尔把 avoid 填成理由列表/字符串；非 bool 一律保守取 False，
        # 不因输出噪声把素材排除掉
        return value if isinstance(value, bool) else False

    @field_validator("role", mode="before")
    @classmethod
    def _coerce_role(cls, value: object) -> object:
        return value if value in {"a_roll_candidate", "b_roll_candidate", "avoid", None} else None


@dataclass(frozen=True, slots=True)
class VideoAnnotationConfig:
    shot_split: ShotSplitConfig = field(default_factory=ShotSplitConfig)
    quality: QualityConfig = field(default_factory=QualityConfig)
    cheap_keyframes_per_shot: int = 3
    deep_keyframes_per_shot: int = 6
    created_at: str | None = None
    model: str | None = None


@dataclass(frozen=True, slots=True)
class VideoAnnotationResult:
    document: AnnotationDocument
    events: tuple[dict[str, Any], ...] = ()


async def run_video_annotation(
    video_path: str | Path,
    *,
    asset_id: str,
    gateway: ProviderGateway,
    pass_: Literal["cheap", "deep"] = "cheap",
    existing_document: AnnotationDocument | None = None,
    config: VideoAnnotationConfig | None = None,
    job_id: str | None = None,
    case_id: str | None = None,
) -> VideoAnnotationResult:
    cfg = config or VideoAnnotationConfig()
    path = Path(video_path).expanduser().resolve(strict=True)
    shot_config = cfg.shot_split
    if case_id is not None and shot_config.case_id is None:
        shot_config = ShotSplitConfig(
            content_threshold=shot_config.content_threshold,
            min_scene_len=shot_config.min_scene_len,
            transnetv2_onnx_path=shot_config.transnetv2_onnx_path,
            case_id=case_id,
        )
    split = split_shots(path, config=shot_config)
    quality_events = detect_quality_events(path, split.shots, config=cfg.quality)
    keyframe_count = (
        max(1, cfg.deep_keyframes_per_shot)
        if pass_ == "deep"
        else max(1, cfg.cheap_keyframes_per_shot)
    )
    clips: list[AnnotationClip] = []
    provider_refs: list[str] = []
    provider_events: list[dict[str, Any]] = []
    for index, shot in enumerate(split.shots, start=1):
        frames = _keyframe_data_uris(path, shot, count=keyframe_count)
        annotation, refs, events = await _annotate_shot(
            gateway,
            frames=frames,
            shot=shot,
            pass_=pass_,
            asset_id=asset_id,
            job_id=job_id,
            case_id=case_id,
            model=cfg.model,
        )
        provider_refs.extend(refs)
        provider_events.extend(events)
        clips.append(
            _clip_from_annotation(
                annotation,
                shot=shot,
                clip_id=f"clip_{index:04d}",
                quality_events=quality_events,
                include_composition=pass_ == "deep",
            )
        )
    document = AnnotationDocument(
        annotation_id=(
            existing_document.annotation_id if existing_document is not None else f"ann_{asset_id}"
        ),
        asset_id=asset_id,
        asset_kind="video",
        status="completed",
        generator=AnnotationGenerator.model_validate(
            {
                "pipeline_version": "annotation.video.v1",
                "pass": pass_,
                "provider_refs": provider_refs,
            }
        ),
        clips=clips,
        asset_level_extensions=AnnotationExtensions(),
        quality_events=list(quality_events),
        evidence_frames=[],
        created_at=(
            existing_document.created_at
            if existing_document is not None
            else cfg.created_at or _now_iso()
        ),
    )
    if pass_ == "deep" and existing_document is not None:
        document = _merge_deep_document(existing_document, document)
    return VideoAnnotationResult(document=document, events=split.events + tuple(provider_events))


def _clip_from_annotation(
    annotation: VLMShotAnnotation,
    *,
    shot: Shot,
    clip_id: str,
    quality_events: Sequence[QualityEvent],
    include_composition: bool,
) -> AnnotationClip:
    hard_ids, soft_ids = _quality_event_ids(shot, quality_events)
    role = _classify_role(annotation, hard_ids=hard_ids)
    extensions = AnnotationExtensions(
        vision_basic_v1=VisionBasicExtension(
            subject_type=annotation.subject_type,
            scene_type=annotation.scene_type,
            action=annotation.action,
            contains_face=annotation.contains_face,
            shot_type=annotation.shot_type,
        ),
        editing_affordance_v1=EditingAffordanceExtension(good_for=annotation.good_for),
        vision_composition_v1=(
            VisionCompositionExtension.model_validate(annotation.composition.model_dump())
            if include_composition and annotation.composition is not None
            else None
        ),
    )
    quality_score = annotation.quality_score
    if quality_score is None:
        quality_score = 0.25 if hard_ids else 0.75
    return AnnotationClip(
        clip_id=clip_id,
        source_start_frame=shot.start_frame,
        source_end_frame=shot.end_frame,
        role=role,
        summary=annotation.summary,
        keywords=annotation.keywords,
        quality_score=quality_score,
        hard_quality_event_ids=list(hard_ids),
        soft_quality_event_ids=list(soft_ids),
        extensions=extensions,
    )


async def _annotate_shot(
    gateway: ProviderGateway,
    *,
    frames: Sequence[str],
    shot: Shot,
    pass_: Literal["cheap", "deep"],
    asset_id: str,
    job_id: str | None,
    case_id: str | None,
    model: str | None,
) -> tuple[VLMShotAnnotation, list[str], list[dict[str, Any]]]:
    validation_error: Exception | None = None
    refs: list[str] = []
    events: list[dict[str, Any]] = []
    for attempt in range(2):
        request = ProviderRequest(
            capability=VLM_ANNOTATION,
            model=model,
            case_id=case_id,
            job_id=job_id,
            payload={
                "messages": _vlm_messages(
                    frames,
                    shot=shot,
                    pass_=pass_,
                    validation_error=validation_error,
                ),
                "params": {
                    "temperature": 0,
                    "response_format": {"type": "json_object"},
                },
                "asset_id": asset_id,
                "shot": {
                    "shot_id": shot.shot_id,
                    "start_frame": shot.start_frame,
                    "end_frame": shot.end_frame,
                },
                "pass": pass_,
            },
        )
        result = await gateway.call(request)
        refs.append(result.result.request_id)
        events.extend(result.events)
        if result.result.error is not None:
            raise AnnotationPipelineError(
                f"VLM annotation failed: {result.result.error.error_code}"
            )
        try:
            return _parse_vlm_annotation(result.result.normalized_output), refs, events
        except (ValidationError, ValueError) as exc:
            validation_error = exc
            if attempt == 1:
                raise AnnotationPipelineError("VLM output failed annotation schema") from exc
    raise AnnotationPipelineError("VLM annotation retry loop exhausted")


def _parse_vlm_annotation(output: dict[str, Any]) -> VLMShotAnnotation:
    if isinstance(output.get("annotation"), dict):
        return VLMShotAnnotation.model_validate(output["annotation"])
    content = output.get("content")
    if isinstance(content, str) and content:
        parsed = json.loads(content)
        if isinstance(parsed, dict):
            if isinstance(parsed.get("annotation"), dict):
                return VLMShotAnnotation.model_validate(parsed["annotation"])
            return VLMShotAnnotation.model_validate(parsed)
    return VLMShotAnnotation.model_validate(output)


def _vlm_messages(
    frames: Sequence[str],
    *,
    shot: Shot,
    pass_: Literal["cheap", "deep"],
    validation_error: Exception | None,
) -> list[dict[str, Any]]:
    instruction = (
        "Return strict JSON for one video shot with keys summary, keywords, subject_type, "
        "scene_type, action, contains_face, shot_type, good_for, avoid, quality_score, role. "
        "For deep pass also include composition.safe_area, composition.subtitle_occlusion_risk, "
        "composition.subject_position, composition.framing_notes."
    )
    if validation_error is not None:
        instruction += " Previous response failed schema validation; return corrected JSON only."
    content: list[dict[str, Any]] = [
        {
            "type": "text",
            "text": (
                f"{instruction}\npass={pass_}; shot_id={shot.shot_id}; "
                f"frame_range=[{shot.start_frame},{shot.end_frame})."
            ),
        }
    ]
    for frame in frames:
        content.append({"type": "image_url", "image_url": {"url": frame}})
    return [{"role": "user", "content": content}]


def _keyframe_data_uris(path: Path, shot: Shot, *, count: int) -> tuple[str, ...]:
    try:
        import cv2
    except ImportError as exc:
        raise AnnotationPipelineError("opencv-python-headless is required") from exc
    frames = _keyframe_numbers(shot, count=count)
    capture = cv2.VideoCapture(str(path))
    if not capture.isOpened():
        raise AnnotationPipelineError(f"cannot open video for keyframes: {path}")
    uris: list[str] = []
    try:
        for frame_number in frames:
            capture.set(cv2.CAP_PROP_POS_FRAMES, frame_number)
            ok, frame = capture.read()
            if not ok:
                continue
            ok, encoded = cv2.imencode(".jpg", frame)
            if not ok:
                continue
            payload = base64.b64encode(encoded.tobytes()).decode("ascii")
            uris.append(f"data:image/jpeg;base64,{payload}")
    finally:
        capture.release()
    if not uris:
        raise AnnotationPipelineError(f"no keyframes extracted for {shot.shot_id}")
    return tuple(uris)


def _keyframe_numbers(shot: Shot, *, count: int) -> tuple[int, ...]:
    frame_count = max(1, shot.end_frame - shot.start_frame)
    samples = min(count, frame_count)
    if samples == 1:
        return (shot.start_frame + frame_count // 2,)
    return tuple(
        min(shot.end_frame - 1, shot.start_frame + round((frame_count - 1) * i / (samples - 1)))
        for i in range(samples)
    )


def _quality_event_ids(
    shot: Shot,
    quality_events: Sequence[QualityEvent],
) -> tuple[tuple[str, ...], tuple[str, ...]]:
    hard: list[str] = []
    soft: list[str] = []
    for event in quality_events:
        if event.end_frame <= shot.start_frame or event.start_frame >= shot.end_frame:
            continue
        if event.severity == "hard":
            hard.append(event.event_id)
        else:
            soft.append(event.event_id)
    return tuple(hard), tuple(soft)


def _classify_role(annotation: VLMShotAnnotation, *, hard_ids: Sequence[str]) -> str:
    if hard_ids or annotation.avoid or annotation.role == "avoid":
        return "avoid"
    if annotation.role in {"a_roll_candidate", "b_roll_candidate"}:
        return annotation.role
    if annotation.contains_face or annotation.subject_type in {"person", "speaker", "host"}:
        return "a_roll_candidate"
    return "b_roll_candidate"


def _merge_deep_document(
    existing: AnnotationDocument,
    deep_document: AnnotationDocument,
) -> AnnotationDocument:
    existing_by_range = {
        (clip.source_start_frame, clip.source_end_frame): clip for clip in existing.clips
    }
    merged_clips: list[AnnotationClip] = []
    for deep_clip in deep_document.clips:
        existing_clip = existing_by_range.get(
            (deep_clip.source_start_frame, deep_clip.source_end_frame)
        )
        if existing_clip is None:
            merged_clips.append(deep_clip)
            continue
        extensions_data = existing_clip.extensions.model_dump(
            mode="json",
            by_alias=True,
            exclude_none=True,
        )
        extensions_data.update(
            deep_clip.extensions.model_dump(mode="json", by_alias=True, exclude_none=True)
        )
        merged_clips.append(
            existing_clip.model_copy(
                update={
                    "summary": deep_clip.summary or existing_clip.summary,
                    "keywords": sorted(set(existing_clip.keywords) | set(deep_clip.keywords)),
                    "quality_score": deep_clip.quality_score,
                    "hard_quality_event_ids": deep_clip.hard_quality_event_ids,
                    "soft_quality_event_ids": deep_clip.soft_quality_event_ids,
                    "extensions": AnnotationExtensions.model_validate(extensions_data),
                }
            )
        )
    if not merged_clips:
        merged_clips = list(deep_document.clips)
    return deep_document.model_copy(
        update={
            "clips": merged_clips,
            "quality_events": list(deep_document.quality_events),
            "evidence_frames": sorted(
                set(existing.evidence_frames) | set(deep_document.evidence_frames)
            ),
        }
    )


def usable_spans(document: AnnotationDocument) -> tuple[tuple[str, int, int], ...]:
    """Return usable clean spans after hard quality events are subtracted."""

    spans = clean_spans(document.clips, document.quality_events)
    return tuple(
        (span.clip_id, span.start_frame, span.end_frame)
        for span in spans
        if _clip_by_id(document.clips, span.clip_id).role != "avoid"
    )


def _clip_by_id(clips: Iterable[AnnotationClip], clip_id: str) -> AnnotationClip:
    for clip in clips:
        if clip.clip_id == clip_id:
            return clip
    raise KeyError(clip_id)


def _now_iso() -> str:
    return datetime.now(UTC).isoformat()
