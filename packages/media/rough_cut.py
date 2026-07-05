"""Rule-based speech rough-cut proposal helpers."""

from __future__ import annotations

import re
from difflib import SequenceMatcher
from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field, model_validator

from contracts.transcript import TranscriptDocument, TranscriptUtterance

RoughCutKind = Literal["filler", "pause", "repeat", "off_topic"]

DEFAULT_FILLER_WORDS: frozenset[str] = frozenset(
    {
        "呃",
        "嗯",
        "啊",
        "哦",
        "额",
        "呐",
        "就是",
        "就是说",
        "然后然后",
    }
)


class RoughCutRange(BaseModel):
    model_config = ConfigDict(extra="forbid")

    start_ms: int
    end_ms: int

    @model_validator(mode="after")
    def validate_range(self) -> RoughCutRange:
        if self.start_ms >= self.end_ms:
            raise ValueError("rough-cut range must satisfy start_ms < end_ms")
        return self


class RoughCutProposal(BaseModel):
    model_config = ConfigDict(extra="forbid")

    range_ms: RoughCutRange
    kind: RoughCutKind
    confidence: float = Field(ge=0.0, le=1.0)
    transcript_excerpt: str


def rule_based_proposals(
    document: TranscriptDocument,
    *,
    filler_words: set[str] | frozenset[str] | None = None,
    pause_threshold_ms: int = 600,
    repeat_similarity_threshold: float = 0.88,
    include_fillers: bool = True,
) -> list[RoughCutProposal]:
    """Build deterministic rough-cut candidates from transcript words, VAD, and repeats."""

    proposals: list[RoughCutProposal] = []
    if include_fillers:
        proposals.extend(
            _filler_proposals(document, filler_words=filler_words or DEFAULT_FILLER_WORDS)
        )
    proposals.extend(_repeat_proposals(document, threshold=repeat_similarity_threshold))
    proposals.extend(_pause_proposals(document, threshold_ms=pause_threshold_ms))
    return _dedupe_proposals(proposals)


def semantic_proposals(
    document: TranscriptDocument,
    suggestions: list[dict[str, Any]],
) -> list[RoughCutProposal]:
    """Convert LLM utterance-id suggestions into timestamped proposals from the transcript."""

    utterances = {utterance.utterance_id: utterance for utterance in document.utterances}
    proposals: list[RoughCutProposal] = []
    for suggestion in suggestions:
        utterance_id = suggestion.get("utterance_id")
        if not isinstance(utterance_id, str):
            continue
        utterance = utterances.get(utterance_id)
        if utterance is None:
            continue
        proposals.append(
            RoughCutProposal(
                range_ms=RoughCutRange(start_ms=utterance.start_ms, end_ms=utterance.end_ms),
                kind="off_topic",
                confidence=_confidence(suggestion.get("confidence"), default=0.65),
                transcript_excerpt=_excerpt(utterance.text),
            )
        )
    return _dedupe_proposals(proposals)


def removed_ranges_from_proposals(
    proposals: list[RoughCutProposal],
    *,
    source: str = "approve_speech_cut",
) -> list[dict[str, Any]]:
    return [
        {
            "start_ms": proposal.range_ms.start_ms,
            "end_ms": proposal.range_ms.end_ms,
            "kind": proposal.kind,
            "source": source,
        }
        for proposal in proposals
    ]


def utterance_prompt_rows(document: TranscriptDocument) -> list[dict[str, Any]]:
    return [
        {
            "utterance_id": utterance.utterance_id,
            "text": utterance.text,
            "start_ms": utterance.start_ms,
            "end_ms": utterance.end_ms,
        }
        for utterance in document.utterances
    ]


def _filler_proposals(
    document: TranscriptDocument,
    *,
    filler_words: set[str] | frozenset[str],
) -> list[RoughCutProposal]:
    normalized_fillers = {_normalize_text(word) for word in filler_words if word}
    proposals: list[RoughCutProposal] = []
    for utterance in document.utterances:
        for word in utterance.words:
            if word.type != "filler":
                continue
            normalized = _normalize_text(word.w)
            if normalized not in normalized_fillers:
                continue
            proposals.append(
                RoughCutProposal(
                    range_ms=RoughCutRange(start_ms=word.start_ms, end_ms=word.end_ms),
                    kind="filler",
                    confidence=0.95,
                    transcript_excerpt=_excerpt(word.w or utterance.text),
                )
            )
        proposals.extend(
            _multi_word_filler_proposals(
                utterance,
                normalized_fillers=normalized_fillers,
            )
        )
    return proposals


def _multi_word_filler_proposals(
    utterance: TranscriptUtterance,
    *,
    normalized_fillers: set[str],
) -> list[RoughCutProposal]:
    proposals: list[RoughCutProposal] = []
    words = [word for word in utterance.words if word.type != "punct"]
    for start_index in range(len(words)):
        text = ""
        for end_index in range(start_index, min(len(words), start_index + 4)):
            text += words[end_index].w
            if _normalize_text(text) not in normalized_fillers:
                continue
            first = words[start_index]
            last = words[end_index]
            proposals.append(
                RoughCutProposal(
                    range_ms=RoughCutRange(start_ms=first.start_ms, end_ms=last.end_ms),
                    kind="filler",
                    confidence=0.9,
                    transcript_excerpt=_excerpt(text),
                )
            )
    return proposals


def _repeat_proposals(
    document: TranscriptDocument,
    *,
    threshold: float,
) -> list[RoughCutProposal]:
    proposals: list[RoughCutProposal] = []
    previous: TranscriptUtterance | None = None
    for utterance in document.utterances:
        if previous is None:
            previous = utterance
            continue
        previous_text = _normalize_for_repeat(previous.text)
        current_text = _normalize_for_repeat(utterance.text)
        if not previous_text or not current_text:
            previous = utterance
            continue
        ratio = SequenceMatcher(a=previous_text, b=current_text).ratio()
        contained = previous_text in current_text or current_text in previous_text
        if ratio >= threshold or contained:
            proposals.append(
                RoughCutProposal(
                    range_ms=RoughCutRange(
                        start_ms=utterance.start_ms,
                        end_ms=utterance.end_ms,
                    ),
                    kind="repeat",
                    confidence=max(0.7, min(0.98, ratio)),
                    transcript_excerpt=_excerpt(utterance.text),
                )
            )
        previous = utterance
    return proposals


def _pause_proposals(
    document: TranscriptDocument,
    *,
    threshold_ms: int,
) -> list[RoughCutProposal]:
    proposals: list[RoughCutProposal] = []
    for segment in document.vad_segments:
        if segment.kind != "silence":
            continue
        duration = segment.end_ms - segment.start_ms
        if duration <= threshold_ms:
            continue
        proposals.append(
            RoughCutProposal(
                range_ms=RoughCutRange(start_ms=segment.start_ms, end_ms=segment.end_ms),
                kind="pause",
                confidence=min(0.95, 0.65 + duration / 4000),
                transcript_excerpt="",
            )
        )
    return proposals


def _dedupe_proposals(proposals: list[RoughCutProposal]) -> list[RoughCutProposal]:
    best_by_key: dict[tuple[int, int, str], RoughCutProposal] = {}
    for proposal in proposals:
        key = (proposal.range_ms.start_ms, proposal.range_ms.end_ms, proposal.kind)
        existing = best_by_key.get(key)
        if existing is None or proposal.confidence > existing.confidence:
            best_by_key[key] = proposal
    return sorted(
        best_by_key.values(),
        key=lambda item: (item.range_ms.start_ms, item.range_ms.end_ms, item.kind),
    )


def _normalize_text(value: str) -> str:
    return re.sub(r"\s+", "", value).lower()


def _normalize_for_repeat(value: str) -> str:
    return re.sub(r"[^\w\u4e00-\u9fff]+", "", value).lower()


def _excerpt(value: str, *, limit: int = 48) -> str:
    if len(value) <= limit:
        return value
    return value[: limit - 3] + "..."


def _confidence(value: object, *, default: float) -> float:
    if not isinstance(value, int | float | str):
        return default
    try:
        number = float(value)
    except (TypeError, ValueError):
        return default
    return max(0.0, min(1.0, number))
