import pytest
from pydantic import ValidationError

from contracts.understanding import (
    MaterialSummary,
    SummarySegment,
    SummarySpent,
)

_VALID_SUMMARY: dict[str, object] = {
    "asset_id": "asset_001",
    "version": 2,
    "focus": None,
    "semantic_role": "speech_footage",
    "overall": "口播产品介绍，画面稳定。",
    "language": "zh",
    "segments": [
        {
            "start_s": 0.0,
            "end_s": 12.4,
            "description": "主播正对镜头介绍产品",
            "transcript": "大家好，今天带来的是……",
            "tags": ["产品特写", "口播"],
            "quality": "good",
            "notes": None,
        }
    ],
    "generated_at": "2026-07-06T00:00:00+00:00",
    "model": "qwen-vl-max",
    "spent": {"frames_viewed": 9, "asr_seconds": 84.0},
}


def test_material_summary_roundtrips_spec_c3_shape() -> None:
    summary = MaterialSummary.model_validate(_VALID_SUMMARY)

    assert summary.semantic_role == "speech_footage"
    assert summary.segments[0].quality == "good"
    assert summary.spent.frames_viewed == 9
    assert summary.model_dump(mode="json")["segments"][0]["end_s"] == 12.4


def test_material_summary_defaults_for_images_and_fonts() -> None:
    summary = MaterialSummary.model_validate(
        {
            "asset_id": "asset_img",
            "version": 1,
            "semantic_role": "photo",
            "overall": "一张风景照",
            "generated_at": "2026-07-06T00:00:00+00:00",
            "model": "qwen-vl-max",
        }
    )

    assert summary.focus is None
    assert summary.language is None
    assert summary.segments == []
    assert summary.spent == SummarySpent()


def test_material_summary_rejects_unknown_semantic_role() -> None:
    payload = dict(_VALID_SUMMARY)
    payload["semantic_role"] = "b_roll"
    with pytest.raises(ValidationError):
        MaterialSummary.model_validate(payload)


def test_summary_segment_rejects_unknown_quality_and_extra_fields() -> None:
    with pytest.raises(ValidationError):
        SummarySegment.model_validate(
            {"start_s": 0.0, "end_s": 1.0, "description": "x", "quality": "perfect"}
        )
    with pytest.raises(ValidationError):
        SummarySegment.model_validate(
            {
                "start_s": 0.0,
                "end_s": 1.0,
                "description": "x",
                "quality": "good",
                "unexpected": 1,
            }
        )


def test_material_summary_forbids_extra_fields() -> None:
    payload = dict(_VALID_SUMMARY)
    payload["extra"] = "nope"
    with pytest.raises(ValidationError):
        MaterialSummary.model_validate(payload)
