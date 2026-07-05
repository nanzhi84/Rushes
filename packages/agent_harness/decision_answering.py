"""Resolve natural-language user messages into structured DecisionAnswer values."""

from __future__ import annotations

import json
from collections.abc import Mapping, Sequence
from dataclasses import dataclass
from typing import Any, Protocol

from contracts.case import CaseState
from contracts.decision import Decision, DecisionAnswer
from providers.capabilities import LLM_CHAT, ProviderRequest
from providers.gateway import ProviderCallRecorder, ProviderGateway
from providers.openai_compatible import (
    DEFAULT_OPENAI_COMPATIBLE_BASE_URL,
    DEFAULT_OPENAI_COMPATIBLE_MODEL,
    OpenAICompatibleLLMProvider,
    openai_compatible_llm_descriptor,
)
from providers.registry import ProviderRegistry

RESOLVE_DECISION_ANSWER_TOOL = "resolve_decision_answer"


class DecisionAnsweringError(RuntimeError):
    """Raised when the answer resolver cannot produce a trustworthy result."""


@dataclass(frozen=True, slots=True)
class DecisionAnswerResolution:
    answer: DecisionAnswer | None

    @classmethod
    def unanswered(cls) -> DecisionAnswerResolution:
        return cls(answer=None)


class DecisionAnswerResolver(Protocol):
    async def resolve(
        self,
        *,
        case_state: CaseState,
        decision: Decision,
        user_message: str,
    ) -> DecisionAnswerResolution:
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


class GatewayDecisionAnswerResolver:
    def __init__(
        self,
        gateway: ProviderGateway,
        *,
        model: str | None = None,
        provider_id: str | None = None,
        params: Mapping[str, Any] | None = None,
    ) -> None:
        self._gateway = gateway
        self._model = model
        self._provider_id = provider_id
        self._params = {"temperature": 0, **dict(params or {})}

    async def resolve(
        self,
        *,
        case_state: CaseState,
        decision: Decision,
        user_message: str,
    ) -> DecisionAnswerResolution:
        response = await self._gateway.call(
            ProviderRequest(
                capability=LLM_CHAT,
                model=self._model,
                case_id=case_state.case_id,
                payload={
                    "messages": _messages(decision, user_message),
                    "tools": [_resolve_tool_schema(decision)],
                    "tool_choice": {
                        "type": "function",
                        "function": {"name": RESOLVE_DECISION_ANSWER_TOOL},
                    },
                    "params": self._params,
                },
            ),
            provider_id=self._provider_id,
        )
        if response.result.error is not None:
            error = response.result.error
            raise DecisionAnsweringError(f"{error.error_code}: {error.message}")
        arguments = _first_resolve_tool_arguments(response.result.normalized_output)
        if arguments is None:
            raise DecisionAnsweringError("resolver did not call resolve_decision_answer")
        return _resolution_from_arguments(decision, arguments)


def build_openai_compatible_decision_answer_resolver(
    *,
    base_url: str = DEFAULT_OPENAI_COMPATIBLE_BASE_URL,
    api_key: str,
    model: str = DEFAULT_OPENAI_COMPATIBLE_MODEL,
    timeout: float = 60.0,
    priority: int = 100,
    recorder: ProviderCallRecorder | None = None,
    params: Mapping[str, Any] | None = None,
) -> GatewayDecisionAnswerResolver:
    registry = ProviderRegistry()
    registry.register(
        openai_compatible_llm_descriptor(priority=priority),
        OpenAICompatibleLLMProvider(
            base_url=base_url,
            api_key=api_key,
            model=model,
            timeout=timeout,
            default_params=params,
        ),
    )
    return GatewayDecisionAnswerResolver(
        ProviderGateway(registry=registry, recorder=recorder),
        model=model,
        provider_id="openai_compatible.llm",
    )


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


def _messages(decision: Decision, user_message: str) -> list[dict[str, str]]:
    options = [
        {
            "option_id": option.option_id,
            "label": option.label,
            "description": option.description,
            "payload": option.payload,
        }
        for option in decision.options
    ]
    return [
        {
            "role": "system",
            "content": (
                "You resolve whether the latest user message answers one pending Rushes "
                "Decision. Always call resolve_decision_answer. If the message does not "
                "answer the pending question, set unanswered=true. If it answers, set "
                "exactly one valid option_id or free_text. Put additional independent user "
                "intents in side_intents."
            ),
        },
        {
            "role": "user",
            "content": json.dumps(
                {
                    "decision": {
                        "decision_id": decision.decision_id,
                        "type": decision.type,
                        "question": decision.question,
                        "options": options,
                        "allow_free_text": decision.allow_free_text,
                    },
                    "latest_user_message": user_message,
                },
                ensure_ascii=False,
                sort_keys=True,
            ),
        },
    ]


def _resolve_tool_schema(decision: Decision) -> dict[str, Any]:
    properties: dict[str, Any] = {
        "unanswered": {
            "type": "boolean",
            "description": "True when the user message does not answer the pending decision.",
        },
        "side_intents": {
            "type": "array",
            "items": {"type": "string"},
            "description": "Independent intents in the same user message, excluding the answer.",
        },
    }
    option_ids = [option.option_id for option in decision.options]
    if option_ids:
        properties["option_id"] = {
            "type": "string",
            "enum": option_ids,
            "description": "The selected pending-decision option_id.",
        }
    if decision.allow_free_text:
        properties["free_text"] = {
            "type": "string",
            "description": "A free-text answer when no option_id applies.",
        }
    return {
        "type": "function",
        "function": {
            "name": RESOLVE_DECISION_ANSWER_TOOL,
            "description": "Resolve one user message against one pending Decision.",
            "parameters": {
                "type": "object",
                "properties": properties,
                "required": ["unanswered"],
                "additionalProperties": False,
            },
            "strict": True,
        },
    }


def _first_resolve_tool_arguments(output: Mapping[str, Any]) -> dict[str, Any] | None:
    tool_call = output.get("tool_call")
    if isinstance(tool_call, Mapping):
        return _arguments_for_resolve_tool(tool_call)
    tool_calls = output.get("tool_calls")
    if isinstance(tool_calls, Sequence) and not isinstance(tool_calls, str | bytes | bytearray):
        for item in tool_calls:
            if isinstance(item, Mapping):
                arguments = _arguments_for_resolve_tool(item)
                if arguments is not None:
                    return arguments
    return None


def _arguments_for_resolve_tool(tool_call: Mapping[str, Any]) -> dict[str, Any] | None:
    name = tool_call.get("name")
    if name != RESOLVE_DECISION_ANSWER_TOOL:
        return None
    arguments = tool_call.get("arguments")
    if isinstance(arguments, Mapping):
        return dict(arguments)
    return None


def _resolution_from_arguments(
    decision: Decision,
    arguments: Mapping[str, Any],
) -> DecisionAnswerResolution:
    if arguments.get("unanswered") is True:
        return DecisionAnswerResolution.unanswered()
    option_id = _optional_str(arguments.get("option_id"))
    if option_id is not None and option_id not in {option.option_id for option in decision.options}:
        raise DecisionAnsweringError(f"resolver returned invalid option_id: {option_id}")
    free_text = _optional_str(arguments.get("free_text")) if decision.allow_free_text else None
    if option_id is None and free_text is None:
        return DecisionAnswerResolution.unanswered()
    side_intents = _string_list(arguments.get("side_intents"))
    payload: dict[str, Any] = {}
    if side_intents:
        payload["side_intents"] = side_intents
    return DecisionAnswerResolution(
        answer=DecisionAnswer(
            option_id=option_id,
            free_text=free_text,
            answered_via="natural_language",
            payload=payload,
        )
    )


def _optional_str(value: object) -> str | None:
    return value if isinstance(value, str) and value != "" else None


def _string_list(value: object) -> list[str]:
    if not isinstance(value, Sequence) or isinstance(value, str | bytes | bytearray):
        return []
    result: list[str] = []
    for item in value:
        if isinstance(item, str) and item:
            result.append(item)
    return result
