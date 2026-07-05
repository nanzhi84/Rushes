"""Local script-to-voiceover alignment helpers."""

from __future__ import annotations

import re
from dataclasses import dataclass
from typing import Literal

from contracts.transcript import TranscriptDocument, TranscriptUtterance, TranscriptWord

AlignmentConfidence = Literal["high", "low"]


@dataclass(frozen=True, slots=True)
class AlignedSentence:
    sentence_id: str
    text: str
    start_ms: int
    end_ms: int
    alignment_confidence: AlignmentConfidence
    matched_chars: int
    total_chars: int
    utterance_ids: tuple[str, ...]
    warning: str | None = None


@dataclass(frozen=True, slots=True)
class VoiceoverAlignment:
    sentences: tuple[AlignedSentence, ...]
    warnings: tuple[str, ...]


@dataclass(frozen=True, slots=True)
class _TimedChar:
    char: str
    start_ms: int
    end_ms: int
    utterance_id: str


def align_script_to_transcript(
    script_text: str,
    transcript: TranscriptDocument,
    *,
    low_confidence_threshold: float = 0.6,
) -> VoiceoverAlignment:
    """Align script sentences to ASR word timestamps with character-level DP."""

    sentences = _split_sentences(script_text)
    timed_chars = _timed_chars(transcript.utterances)
    if not sentences or not timed_chars:
        return VoiceoverAlignment(sentences=(), warnings=("alignment_empty_input",))

    script_chars = _normalized_chars("".join(sentences))
    asr_chars = [item.char for item in timed_chars]
    char_mapping = _levenshtein_equal_mapping(script_chars, asr_chars)

    aligned: list[AlignedSentence] = []
    warnings: list[str] = []
    cursor = 0
    for index, sentence in enumerate(sentences, start=1):
        sentence_chars = _normalized_chars(sentence)
        span_start = cursor
        span_end = cursor + len(sentence_chars)
        cursor = span_end
        mapped_indices = [
            asr_index
            for script_index, asr_index in char_mapping.items()
            if span_start <= script_index < span_end
        ]
        confidence_ratio = len(mapped_indices) / len(sentence_chars) if sentence_chars else 0.0
        confidence: AlignmentConfidence = (
            "high" if confidence_ratio >= low_confidence_threshold else "low"
        )
        if mapped_indices:
            start_ms = timed_chars[min(mapped_indices)].start_ms
            end_ms = timed_chars[max(mapped_indices)].end_ms
        else:
            start_ms, end_ms = _fallback_sentence_range(
                sentence_index=index - 1,
                sentence_count=len(sentences),
                timed_chars=timed_chars,
            )
        if start_ms >= end_ms:
            end_ms = start_ms + 1
        utterance_ids = _utterance_ids_for_range(transcript.utterances, start_ms, end_ms)
        warning = None
        if confidence == "low":
            warning = f"alignment_low_confidence:{index}"
            warnings.append(warning)
        aligned.append(
            AlignedSentence(
                sentence_id=f"sent_{index:03d}",
                text=sentence,
                start_ms=start_ms,
                end_ms=end_ms,
                alignment_confidence=confidence,
                matched_chars=len(mapped_indices),
                total_chars=len(sentence_chars),
                utterance_ids=utterance_ids,
                warning=warning,
            )
        )
    return VoiceoverAlignment(sentences=tuple(aligned), warnings=tuple(warnings))


def _split_sentences(script_text: str) -> list[str]:
    parts = re.split(r"(?<=[。！？!?；;])\s*|\n+", script_text.strip())
    return [part.strip() for part in parts if part.strip()]


def _normalized_chars(value: str) -> list[str]:
    return [
        char.lower() for char in value if re.match(r"[\w\u4e00-\u9fff]", char, flags=re.UNICODE)
    ]


def _timed_chars(utterances: list[TranscriptUtterance]) -> list[_TimedChar]:
    chars: list[_TimedChar] = []
    for utterance in utterances:
        source_words = utterance.words or [
            TranscriptWord(
                w=utterance.text,
                start_ms=utterance.start_ms,
                end_ms=utterance.end_ms,
                type="word",
            )
        ]
        for word in source_words:
            if word.type == "punct":
                continue
            normalized = _normalized_chars(word.w)
            if not normalized:
                continue
            duration = max(1, word.end_ms - word.start_ms)
            unit = duration / len(normalized)
            for index, char in enumerate(normalized):
                start_ms = int(word.start_ms + unit * index)
                end_ms = int(word.start_ms + unit * (index + 1))
                chars.append(
                    _TimedChar(
                        char=char,
                        start_ms=start_ms,
                        end_ms=max(start_ms + 1, end_ms),
                        utterance_id=utterance.utterance_id,
                    )
                )
    return chars


def _levenshtein_equal_mapping(
    script_chars: list[str],
    asr_chars: list[str],
) -> dict[int, int]:
    rows = len(script_chars) + 1
    cols = len(asr_chars) + 1
    dp = [[0] * cols for _ in range(rows)]
    for row in range(1, rows):
        dp[row][0] = row
    for col in range(1, cols):
        dp[0][col] = col
    for row in range(1, rows):
        for col in range(1, cols):
            replace_cost = 0 if script_chars[row - 1] == asr_chars[col - 1] else 1
            dp[row][col] = min(
                dp[row - 1][col] + 1,
                dp[row][col - 1] + 1,
                dp[row - 1][col - 1] + replace_cost,
            )

    mapping: dict[int, int] = {}
    row = len(script_chars)
    col = len(asr_chars)
    while row > 0 or col > 0:
        if row > 0 and col > 0:
            replace_cost = 0 if script_chars[row - 1] == asr_chars[col - 1] else 1
            if dp[row][col] == dp[row - 1][col - 1] + replace_cost:
                if replace_cost == 0:
                    mapping[row - 1] = col - 1
                row -= 1
                col -= 1
                continue
        if row > 0 and dp[row][col] == dp[row - 1][col] + 1:
            row -= 1
            continue
        col -= 1
    return mapping


def _fallback_sentence_range(
    *,
    sentence_index: int,
    sentence_count: int,
    timed_chars: list[_TimedChar],
) -> tuple[int, int]:
    first = timed_chars[0].start_ms
    last = timed_chars[-1].end_ms
    duration = max(1, last - first)
    start_ms = first + int(duration * sentence_index / sentence_count)
    end_ms = first + int(duration * (sentence_index + 1) / sentence_count)
    return start_ms, max(start_ms + 1, end_ms)


def _utterance_ids_for_range(
    utterances: list[TranscriptUtterance],
    start_ms: int,
    end_ms: int,
) -> tuple[str, ...]:
    ids: list[str] = []
    for utterance in utterances:
        if utterance.end_ms <= start_ms or utterance.start_ms >= end_ms:
            continue
        ids.append(utterance.utterance_id)
    return tuple(dict.fromkeys(ids))
