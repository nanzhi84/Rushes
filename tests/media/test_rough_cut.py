from __future__ import annotations

from contracts.transcript import TranscriptDocument, TranscriptUtterance, TranscriptWord, VadSegment
from media.rough_cut import rule_based_proposals, semantic_proposals


def test_rule_based_proposals_cover_filler_repeat_and_pause_thresholds() -> None:
    document = _document()

    proposals = rule_based_proposals(
        document,
        filler_words={"呃"},
        pause_threshold_ms=600,
        repeat_similarity_threshold=0.85,
    )

    by_kind = {proposal.kind: proposal for proposal in proposals}
    assert by_kind["filler"].range_ms.start_ms == 0
    assert by_kind["filler"].range_ms.end_ms == 100
    assert by_kind["repeat"].range_ms.start_ms == 1600
    assert by_kind["repeat"].range_ms.end_ms == 2600
    assert by_kind["pause"].range_ms.start_ms == 2600
    assert by_kind["pause"].range_ms.end_ms == 3400


def test_rule_based_proposals_can_disable_fillers_when_raw_text_is_not_preserved() -> None:
    proposals = rule_based_proposals(_document(), include_fillers=False)

    assert "filler" not in {proposal.kind for proposal in proposals}
    assert {"pause", "repeat"} <= {proposal.kind for proposal in proposals}


def test_semantic_proposals_only_use_known_utterance_ids() -> None:
    proposals = semantic_proposals(
        _document(),
        [
            {"utterance_id": "u_002", "reason": "off topic", "confidence": 0.91},
            {"utterance_id": "missing", "reason": "ignored", "confidence": 1.0},
        ],
    )

    assert len(proposals) == 1
    assert proposals[0].kind == "off_topic"
    assert proposals[0].range_ms.start_ms == 600
    assert proposals[0].confidence == 0.91


def _document() -> TranscriptDocument:
    return TranscriptDocument(
        transcript_id="tr_1",
        asset_id="asset_1",
        language="zh",
        provider_id="asr",
        raw_preserved=True,
        utterances=[
            TranscriptUtterance(
                utterance_id="u_001",
                text="呃 我们开始",
                start_ms=0,
                end_ms=500,
                words=[
                    TranscriptWord(w="呃", start_ms=0, end_ms=100, type="filler"),
                    TranscriptWord(w="我们开始", start_ms=100, end_ms=500, type="word"),
                ],
            ),
            TranscriptUtterance(
                utterance_id="u_002",
                text="这个产品适合新手",
                start_ms=600,
                end_ms=1600,
                words=[
                    TranscriptWord(
                        w="这个产品适合新手",
                        start_ms=600,
                        end_ms=1600,
                        type="word",
                    )
                ],
            ),
            TranscriptUtterance(
                utterance_id="u_003",
                text="这个产品适合新手这个产品适合新手",
                start_ms=1600,
                end_ms=2600,
                words=[
                    TranscriptWord(
                        w="这个产品适合新手这个产品适合新手",
                        start_ms=1600,
                        end_ms=2600,
                        type="word",
                    )
                ],
            ),
        ],
        vad_segments=[
            VadSegment(start_ms=0, end_ms=2600, kind="speech"),
            VadSegment(start_ms=2600, end_ms=3400, kind="silence"),
        ],
    )


def test_rough_cut_range_and_llm_suggestion_edges() -> None:
    import pytest as _pytest

    from media.rough_cut import RoughCutRange, semantic_proposals

    with _pytest.raises(ValueError):
        RoughCutRange(start_ms=100, end_ms=100)

    # 非法/未知 utterance_id 与非字符串 id 均被跳过
    from contracts.transcript import TranscriptDocument, TranscriptUtterance, TranscriptWord

    document = TranscriptDocument(
        transcript_id="tr_1",
        asset_id="a",
        language="zh",
        provider_id="p",
        raw_preserved=True,
        utterances=[
            TranscriptUtterance(
                utterance_id="u_1",
                text="这句话",
                start_ms=0,
                end_ms=500,
                words=[TranscriptWord(w="这句话", start_ms=0, end_ms=500, type="word")],
            )
        ],
        vad_segments=[],
    )
    merged = semantic_proposals(
        document,
        [
            {"utterance_id": 123, "kind": "off_topic"},
            {"utterance_id": "u_ghost", "kind": "off_topic"},
            {"utterance_id": "u_1", "kind": "off_topic", "confidence": 0.9, "reason": "离题"},
        ],
    )
    assert len(merged) == 1
    assert merged[0].kind == "off_topic"


def test_excerpt_and_confidence_helpers() -> None:
    from media.rough_cut import _confidence, _excerpt

    assert _excerpt("短句") == "短句"
    long_text = "字" * 60
    assert _excerpt(long_text).endswith("...")
    assert len(_excerpt(long_text)) == 48

    assert _confidence(None, default=0.4) == 0.4
    assert _confidence("0.9", default=0.4) == 0.9
    assert _confidence("not-a-number", default=0.4) == 0.4
    assert _confidence(5, default=0.4) == 1.0
