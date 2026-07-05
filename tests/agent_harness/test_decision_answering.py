"""GatewayDecisionAnswerResolver coverage with a scripted mock provider."""

from __future__ import annotations

from typing import Any

import pytest
from pydantic import BaseModel, ConfigDict

from agent_harness.decision_answering import ScriptedDecisionAnswerResolver
from contracts.case import CaseState
from contracts.decision import Decision
from contracts.provider import ProviderDescriptor
from providers import LLM_CHAT, ProviderGateway, ProviderRegistry
from providers.decision_answering import (
    DecisionAnsweringError,
    GatewayDecisionAnswerResolver,
)
from providers.mock import MockProvider


class EmptyConfig(BaseModel):
    model_config = ConfigDict(extra="forbid")


def _case_state() -> CaseState:
    return CaseState.model_validate(
        {
            "case_id": "case_1",
            "project_id": "project_1",
            "name": "Case",
            "brief": {"goal": "demo", "confirmed_facts": []},
            "selected_asset_ids": [],
            "disabled_asset_ids": [],
            "scratch_memory": {},
        }
    )


def _decision() -> Decision:
    return Decision.model_validate(
        {
            "decision_id": "dec_1",
            "scope_type": "case",
            "project_id": "project_1",
            "case_id": "case_1",
            "type": "subtitle",
            "question": "要字幕吗？",
            "allow_free_text": True,
            "options": [
                {"option_id": "style_a", "label": "白色简约"},
                {"option_id": "skip", "label": "跳过"},
            ],
            "blocking": True,
        }
    )


def _gateway(script: list[dict[str, Any]]) -> ProviderGateway:
    registry = ProviderRegistry()
    registry.register(
        ProviderDescriptor(
            provider_id="mock_llm",
            display_name="mock_llm",
            version="1",
            capabilities=[LLM_CHAT],
            config_model=EmptyConfig,
            client_ref="tests.mock_llm",
        ),
        MockProvider(provider_id="mock_llm", scripts={LLM_CHAT: script}),
    )
    return ProviderGateway(registry=registry)


def _tool_call(arguments: dict[str, Any]) -> dict[str, Any]:
    # 形状 = openai_compatible adapter 归一化后的扁平 tool_calls
    return {
        "tool_calls": [
            {
                "id": "call_1",
                "type": "function",
                "name": "resolve_decision_answer",
                "arguments": arguments,
            }
        ]
    }


async def test_gateway_resolver_parses_option_and_side_intents() -> None:
    resolver = GatewayDecisionAnswerResolver(
        _gateway(
            [
                {
                    "normalized_output": _tool_call(
                        {
                            "option_id": "skip",
                            "unanswered": False,
                            "side_intents": ["加轻快 BGM"],
                        }
                    )
                }
            ]
        )
    )

    resolution = await resolver.resolve(
        case_state=_case_state(), decision=_decision(), user_message="不要字幕，但是加个轻快 BGM"
    )

    assert resolution.answer is not None
    assert resolution.answer.option_id == "skip"
    assert resolution.answer.answered_via == "natural_language"
    assert resolution.answer.payload is not None
    assert resolution.answer.payload["side_intents"] == ["加轻快 BGM"]


async def test_gateway_resolver_unanswered_and_free_text_paths() -> None:
    resolver = GatewayDecisionAnswerResolver(
        _gateway(
            [
                {"normalized_output": _tool_call({"unanswered": True})},
                {
                    "normalized_output": _tool_call(
                        {"free_text": "字幕要淡黄色", "unanswered": False}
                    )
                },
            ]
        )
    )

    first = await resolver.resolve(
        case_state=_case_state(), decision=_decision(), user_message="进度怎么样了？"
    )
    assert first.answer is None

    second = await resolver.resolve(
        case_state=_case_state(), decision=_decision(), user_message="字幕要淡黄色"
    )
    assert second.answer is not None
    assert second.answer.free_text == "字幕要淡黄色"


async def test_gateway_resolver_provider_error_raises() -> None:
    resolver = GatewayDecisionAnswerResolver(
        _gateway(
            [
                {
                    "error": {
                        "error_code": "timeout",
                        "message": "llm timed out",
                        "retryable": True,
                    }
                }
            ]
        )
    )

    with pytest.raises(DecisionAnsweringError):
        await resolver.resolve(case_state=_case_state(), decision=_decision(), user_message="跳过")


async def test_gateway_resolver_malformed_output_raises() -> None:
    resolver = GatewayDecisionAnswerResolver(
        _gateway([{"normalized_output": {"content": "not a tool call"}}])
    )

    with pytest.raises(DecisionAnsweringError):
        await resolver.resolve(case_state=_case_state(), decision=_decision(), user_message="跳过")


async def test_scripted_resolver_exhaustion_returns_unanswered() -> None:
    resolver = ScriptedDecisionAnswerResolver([])
    resolution = await resolver.resolve(
        case_state=_case_state(), decision=_decision(), user_message="任何话"
    )
    assert resolution.answer is None
