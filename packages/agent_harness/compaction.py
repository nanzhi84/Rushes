"""Write-before-compaction helpers for free-form messages."""

from __future__ import annotations

import hashlib
import re
from collections.abc import Mapping, Sequence
from dataclasses import dataclass
from typing import Any, Protocol

from pydantic import BaseModel, ConfigDict

from contracts.events import BriefUpdated, ContextCompacted, DomainEventBase


class TokenCounter(Protocol):
    def __call__(self, text: str) -> int:
        """Return an approximate token count for text."""


class CompactionMessage(BaseModel):
    model_config = ConfigDict(extra="forbid")

    role: str
    content: str
    created_at: str | None = None
    draft_id: str | None = None

    @classmethod
    def from_input(
        cls,
        value: CompactionMessage | Mapping[str, Any] | str,
    ) -> CompactionMessage:
        if isinstance(value, CompactionMessage):
            return value
        if isinstance(value, str):
            return cls(role="unknown", content=value)
        return cls.model_validate(value)


@dataclass(frozen=True, slots=True)
class CompactionResult:
    kept_messages: tuple[CompactionMessage, ...]
    summary_text: str
    extracted_facts: tuple[str, ...]
    events: tuple[DomainEventBase, ...]


_FACT_PATTERNS = (
    "决定",
    "确认",
    "偏好",
    "喜欢",
    "不喜欢",
    "以后",
    "固定",
    "选择",
    "要",
    "不要",
    "使用",
    "prefer",
    "preference",
    "decided",
    "confirmed",
    "always",
    "never",
    "use ",
)


def compact_messages(
    messages: Sequence[CompactionMessage | Mapping[str, Any] | str],
    budget: int,
    counter: TokenCounter,
) -> CompactionResult:
    normalized = tuple(CompactionMessage.from_input(message) for message in messages)
    extracted_facts = _extract_facts(normalized)
    kept = _keep_recent_messages(normalized, budget, counter)
    compacted = normalized[: max(0, len(normalized) - len(kept))]
    summary_text = _summarize_messages(compacted)
    events = _compaction_events(normalized, kept, summary_text, extracted_facts)
    return CompactionResult(
        kept_messages=kept,
        summary_text=summary_text,
        extracted_facts=extracted_facts,
        events=events,
    )


def _extract_facts(messages: Sequence[CompactionMessage]) -> tuple[str, ...]:
    facts: list[str] = []
    seen: set[str] = set()
    for message in messages:
        for sentence in _split_short_sentences(message.content):
            lowered = sentence.lower()
            if any(pattern in lowered for pattern in _FACT_PATTERNS) and sentence not in seen:
                facts.append(sentence)
                seen.add(sentence)
    return tuple(facts)


def _split_short_sentences(text: str) -> tuple[str, ...]:
    candidates = re.split(r"[。！？!?;\n]+", text)
    return tuple(candidate.strip() for candidate in candidates if 0 < len(candidate.strip()) <= 120)


def _keep_recent_messages(
    messages: Sequence[CompactionMessage],
    budget: int,
    counter: TokenCounter,
) -> tuple[CompactionMessage, ...]:
    kept: list[CompactionMessage] = []
    for message in reversed(messages):
        candidate = tuple(reversed((*kept, message)))
        if counter(_messages_text(candidate)) <= budget:
            kept.insert(0, message)
    if not kept and messages:
        kept.append(messages[-1])
    return tuple(kept)


def _summarize_messages(messages: Sequence[CompactionMessage]) -> str:
    if not messages:
        return ""
    lines = []
    for message in messages:
        content = " ".join(message.content.split())
        if len(content) > 160:
            content = content[:157] + "..."
        lines.append(f"- {message.role}: {content}")
    return "Earlier conversation summary:\n" + "\n".join(lines)


def _compaction_events(
    messages: Sequence[CompactionMessage],
    kept: Sequence[CompactionMessage],
    summary_text: str,
    extracted_facts: Sequence[str],
) -> tuple[DomainEventBase, ...]:
    draft_id = _first_draft_id(messages)
    payload = {
        "kept_message_count": len(kept),
        "compacted_message_count": max(0, len(messages) - len(kept)),
        "summary_text": summary_text,
        "extracted_facts": list(extracted_facts),
    }
    compaction_id = (
        "ctxc_"
        + hashlib.sha256((summary_text + "|".join(extracted_facts)).encode("utf-8")).hexdigest()[
            :16
        ]
    )
    events: list[DomainEventBase] = []
    if draft_id is not None and extracted_facts:
        events.append(
            BriefUpdated(
                draft_id=draft_id,
                payload={
                    "confirmed_facts_append": list(extracted_facts),
                    "source": "write_before_compaction",
                },
            )
        )
    events.append(ContextCompacted(compaction_id=compaction_id, draft_id=draft_id, payload=payload))
    return tuple(events)


def _first_draft_id(messages: Sequence[CompactionMessage]) -> str | None:
    for message in messages:
        if message.draft_id is not None:
            return message.draft_id
    return None


def _messages_text(messages: Sequence[CompactionMessage]) -> str:
    return "\n".join(f"{message.role}: {message.content}" for message in messages)
