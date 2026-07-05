from __future__ import annotations

from contracts.transcript import TranscriptDocument, TranscriptUtterance, TranscriptWord
from media.align import align_script_to_transcript


def test_align_script_to_transcript_exact_match() -> None:
    alignment = align_script_to_transcript(
        "你好世界。第二句。",
        _document(["你好世界", "第二句"]),
    )

    assert len(alignment.sentences) == 2
    assert alignment.warnings == ()
    assert alignment.sentences[0].alignment_confidence == "high"
    assert alignment.sentences[0].start_ms == 0
    assert alignment.sentences[0].end_ms == 400
    assert alignment.sentences[1].utterance_ids == ("u_002",)


def test_align_script_to_transcript_tolerates_inserted_asr_chars() -> None:
    alignment = align_script_to_transcript("你好世界。", _document(["你好额世界"]))

    assert len(alignment.sentences) == 1
    assert alignment.sentences[0].alignment_confidence == "high"
    assert alignment.sentences[0].matched_chars == 4
    assert alignment.sentences[0].start_ms == 0
    assert alignment.sentences[0].end_ms == 500


def test_align_script_to_transcript_warns_on_low_confidence() -> None:
    alignment = align_script_to_transcript(
        "完全不同。",
        _document(["abcdef"]),
        low_confidence_threshold=0.8,
    )

    assert len(alignment.sentences) == 1
    assert alignment.sentences[0].alignment_confidence == "low"
    assert alignment.sentences[0].warning == "alignment_low_confidence:1"
    assert alignment.warnings == ("alignment_low_confidence:1",)


def _document(texts: list[str]) -> TranscriptDocument:
    utterances: list[TranscriptUtterance] = []
    cursor = 0
    for index, text in enumerate(texts, start=1):
        duration = len(text) * 100
        utterances.append(
            TranscriptUtterance(
                utterance_id=f"u_{index:03d}",
                text=text,
                start_ms=cursor,
                end_ms=cursor + duration,
                words=[
                    TranscriptWord(
                        w=text,
                        start_ms=cursor,
                        end_ms=cursor + duration,
                        type="word",
                    )
                ],
            )
        )
        cursor += duration
    return TranscriptDocument(
        transcript_id="tr_align",
        asset_id="asset_voiceover",
        language="zh",
        provider_id="asr",
        raw_preserved=True,
        utterances=utterances,
    )


def test_alignment_empty_inputs_return_warning() -> None:
    from contracts.transcript import TranscriptDocument
    from media.align import align_script_to_transcript

    empty_doc = TranscriptDocument(
        transcript_id="tr_e",
        asset_id="a",
        language="zh",
        provider_id="p",
        raw_preserved=True,
        utterances=[],
        vad_segments=[],
    )
    result = align_script_to_transcript("你好。", empty_doc)
    assert result.sentences == ()
    assert "alignment_empty_input" in result.warnings

    result2 = align_script_to_transcript("", empty_doc)
    assert result2.sentences == ()


def test_alignment_skips_punct_words_and_flags_low_confidence() -> None:
    from contracts.transcript import TranscriptDocument, TranscriptUtterance, TranscriptWord
    from media.align import align_script_to_transcript

    doc = TranscriptDocument(
        transcript_id="tr_p",
        asset_id="a",
        language="zh",
        provider_id="p",
        raw_preserved=True,
        utterances=[
            TranscriptUtterance(
                utterance_id="u_1",
                text="你好，世界",
                start_ms=0,
                end_ms=1000,
                words=[
                    TranscriptWord(w="你好", start_ms=0, end_ms=400, type="word"),
                    TranscriptWord(w="，", start_ms=400, end_ms=410, type="punct"),
                    TranscriptWord(w="世界", start_ms=410, end_ms=1000, type="word"),
                ],
            )
        ],
        vad_segments=[],
    )
    # 脚本与转写差异大 → 低置信 warning 路径
    result = align_script_to_transcript("完全不同的一句话。", doc)
    assert result.sentences
    assert any("low" in w or "confidence" in w for w in result.warnings) or any(
        s.alignment_confidence == "low" for s in result.sentences
    )
