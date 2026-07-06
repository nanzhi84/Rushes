from pathlib import Path

from tests.golden.framework import (
    ExpectedToolTrace,
    GoldenAssertions,
    GoldenCase,
    GoldenExecutor,
    GoldenRunResult,
    base_workspace,
    registry_with_timeline_tools,
)


async def test_golden_pending_decision_blocks_and_recovers(tmp_path: Path) -> None:
    case = GoldenCase(
        name="pending_decision_blocks",
        build_workspace=base_workspace,
        user_messages=("ask", "answer"),
        provider_script=(
            {
                "tool_call": {
                    "tool_name": "interaction.ask_user",
                    "arguments": {
                        "question": "确认事实？",
                        "type": "generic",
                        "reduce_target": "scratch_memory",
                    },
                }
            },
            {
                "tool_call": {
                    "tool_name": "interaction.confirm_action",
                    "arguments": {"question": "bad"},
                }
            },
            {
                "tool_call": {
                    "tool_name": "decision.answer",
                    "arguments": {
                        "decision_id": "__pending_decision__",
                        "answer": {
                            "option_id": "yes",
                            "answered_via": "button",
                            "payload": {
                                "reduce_target": "scratch_memory",
                                "value": "confirmed",
                            },
                        },
                    },
                }
            },
            {"normalized_output": {"content": "已恢复"}},
        ),
        expected_tool_trace=(
            ExpectedToolTrace("interaction.ask_user", "requires_user"),
            ExpectedToolTrace("interaction.confirm_action", "failed"),
            ExpectedToolTrace("decision.answer", "succeeded"),
        ),
        assertions=GoldenAssertions(assert_final=_assert_pending_decision_final),
    )

    await GoldenExecutor().run(case, tmp_path)


async def test_golden_illegal_tool_forces_response(tmp_path: Path) -> None:
    case = GoldenCase(
        name="illegal_tool_retry_limit",
        build_workspace=base_workspace,
        user_messages=("run shell",),
        provider_script=(
            {"tool_call": {"tool_name": "shell.exec", "arguments": {}}},
            {"tool_call": {"tool_name": "shell.exec", "arguments": {}}},
            {"tool_call": {"tool_name": "shell.exec", "arguments": {}}},
        ),
        expected_tool_trace=(
            ExpectedToolTrace("shell.exec", "failed"),
            ExpectedToolTrace("shell.exec", "failed"),
            ExpectedToolTrace("shell.exec", "failed"),
        ),
        assertions=GoldenAssertions(assert_final=_assert_illegal_tool_final),
    )

    await GoldenExecutor().run(case, tmp_path)


async def test_golden_version_conflict_retries_on_new_state(tmp_path: Path) -> None:
    case = GoldenCase(
        name="version_conflict_retry",
        build_workspace=lambda engine: base_workspace(engine, state_version=1),
        user_messages=("create stale", "retry"),
        provider_script=(
            {"tool_call": {"tool_name": "test.create_timeline_stale", "arguments": {}}},
            {"tool_call": {"tool_name": "test.create_timeline", "arguments": {}}},
            {"normalized_output": {"content": "timeline ok"}},
        ),
        expected_tool_trace=(
            ExpectedToolTrace("test.create_timeline", "succeeded"),
            ExpectedToolTrace("test.create_timeline", "succeeded"),
        ),
        assertions=GoldenAssertions(assert_final=_assert_version_conflict_final),
        registry_factory=registry_with_timeline_tools,
    )

    await GoldenExecutor().run(case, tmp_path)


def _assert_pending_decision_final(result: GoldenRunResult) -> None:
    assert result.case_state.pending_decision_id is None
    assert result.case_state.scratch_memory
    assert "PolicyRefusal" in result.event_types
    assert "DecisionAnswered" in result.event_types


def _assert_illegal_tool_final(result: GoldenRunResult) -> None:
    assert result.event_types.count("PolicyRefusal") == 3
    assert result.case_state.state_version == 0
    assert any("连续 3 次" in str(message["content"]) for message in result.messages)


def _assert_version_conflict_final(result: GoldenRunResult) -> None:
    assert result.case_state.timeline_current_version == 1
    assert result.case_state.state_version == 2
    assert result.event_types.count("TimelineVersionCreated") == 1
