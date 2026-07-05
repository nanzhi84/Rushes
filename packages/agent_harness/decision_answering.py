"""Resolve natural-language user messages into structured DecisionAnswer values."""

from __future__ import annotations

from collections.abc import Mapping, Sequence
from dataclasses import dataclass
from typing import Any, Protocol

from contracts.case import CaseState
from contracts.decision import Decision, DecisionAnswer

RESOLVE_DECISION_ANSWER_TOOL = "resolve_decision_answer"


class DecisionAnsweringError(RuntimeError):
    """Raised when the answer resolver cannot produce a trustworthy result."""


@dataclass(frozen=True, slots=True)
class DecisionAnswerResolution:
    answer: DecisionAnswer | None

    @classmethod
    def unanswered(cls) -> DecisionAnswerResolution:
        return cls(answer=None)


class SupportsDecisionResolution(Protocol):
    """结构化返回值协议：providers 侧实现无需 import 本模块的具体 dataclass。"""

    @property
    def answer(self) -> DecisionAnswer | None: ...


class DecisionAnswerResolver(Protocol):
    async def resolve(
        self,
        *,
        case_state: CaseState,
        decision: Decision,
        user_message: str,
    ) -> SupportsDecisionResolution:
        """Return a structured answer when the message answers the pending decision."""


class ScriptedDecisionAnswerResolver:
    """Deterministic resolver used by loop tests."""

    def __init__(
        self,
        resolutions: Sequence[
            DecisionAnswerResolution | DecisionAnswer | Mapping[str, Any] | BaseException
        ],
    ) -> None:
        self._resolutions = list(resolutions)
        self._index = 0

    async def resolve(
        self,
        *,
        case_state: CaseState,
        decision: Decision,
        user_message: str,
    ) -> DecisionAnswerResolution:
        del case_state, decision, user_message
        if self._index >= len(self._resolutions):
            return DecisionAnswerResolution.unanswered()
        resolution = self._resolutions[self._index]
        self._index += 1
        if isinstance(resolution, BaseException):
            raise resolution
        return _scripted_resolution(resolution)


def _scripted_resolution(
    value: DecisionAnswerResolution | DecisionAnswer | Mapping[str, Any],
) -> DecisionAnswerResolution:
    if isinstance(value, DecisionAnswerResolution):
        return value
    if isinstance(value, DecisionAnswer):
        return DecisionAnswerResolution(answer=value)
    if value.get("unanswered") is True:
        return DecisionAnswerResolution.unanswered()
    if "answer" in value and isinstance(value["answer"], Mapping):
        return DecisionAnswerResolution(answer=DecisionAnswer.model_validate(dict(value["answer"])))
    payload = dict(value.get("payload", {})) if isinstance(value.get("payload"), Mapping) else {}
    answer_payload: dict[str, Any] = {
        "option_id": value.get("option_id"),
        "free_text": value.get("free_text"),
        "answered_via": value.get("answered_via", "natural_language"),
        "payload": payload,
    }
    side_intents = _string_list(value.get("side_intents"))
    if side_intents:
        answer_payload["payload"]["side_intents"] = side_intents
    return DecisionAnswerResolution(answer=DecisionAnswer.model_validate(answer_payload))


def _string_list(value: object) -> list[str]:
    if not isinstance(value, Sequence) or isinstance(value, str | bytes | bytearray):
        return []
    return [item for item in value if isinstance(item, str) and item]
