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


async def test_golden_narration_then_tool_then_reply(tmp_path: Path) -> None:
    case = GoldenCase(
        name="narration_then_tool",
        build_workspace=base_workspace,
        user_messages=("建一版时间线",),
        provider_script=(
            {
                "content": "我先基于当前状态建一版时间线。",
                "tool_call": {"tool_name": "test.create_timeline", "arguments": {}},
            },
            {"content": "时间线已就绪。"},
        ),
        expected_tool_trace=(ExpectedToolTrace("test.create_timeline", "succeeded"),),
        assertions=GoldenAssertions(assert_final=_assert_narration_then_tool_final),
        registry_factory=registry_with_timeline_tools,
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


def _reply_messages(result: GoldenRunResult) -> list[dict[str, object]]:
    return [message for message in result.messages if message.get("kind") == "reply"]


def _narration_messages(result: GoldenRunResult) -> list[dict[str, object]]:
    return [message for message in result.messages if message.get("kind") == "narration"]


def _assert_pending_decision_final(result: GoldenRunResult) -> None:
    assert result.case_state.pending_decision_id is None
    assert result.case_state.scratch_memory
    assert "PolicyRefusal" in result.event_types
    assert "DecisionAnswered" in result.event_types
    # content 终止协议：恢复后靠纯 content 步收尾——reply 行落库 + TurnEnded。
    assert "TurnEnded" in result.event_types
    replies = _reply_messages(result)
    assert [message["content"] for message in replies] == ["已恢复"]
    assert all(message["role"] == "assistant" for message in replies)


def _assert_narration_then_tool_final(result: GoldenRunResult) -> None:
    # 混合步：同一步既有叙述又发起工具调用——叙述先落 narration 行，工具照常执行。
    narrations = _narration_messages(result)
    assert [message["content"] for message in narrations] == ["我先基于当前状态建一版时间线。"]
    assert all(message["role"] == "assistant" for message in narrations)
    # 随后纯 content 步收尾。
    replies = _reply_messages(result)
    assert [message["content"] for message in replies] == ["时间线已就绪。"]
    assert result.case_state.timeline_current_version == 1
    assert result.event_types.count("TimelineVersionCreated") == 1
    assert "TurnEnded" in result.event_types


def _assert_illegal_tool_final(result: GoldenRunResult) -> None:
    assert result.event_types.count("PolicyRefusal") == 3
    assert result.case_state.state_version == 0
    # 连续非法输出触顶后由 harness 兜底：写一条 reply 行 + TurnEnded 收尾。
    assert "TurnEnded" in result.event_types
    replies = _reply_messages(result)
    assert len(replies) == 1
    assert replies[0]["role"] == "assistant"
    assert "连续 3 次" in str(replies[0]["content"])


def _assert_version_conflict_final(result: GoldenRunResult) -> None:
    assert result.case_state.timeline_current_version == 1
    assert result.case_state.state_version == 2
    assert result.event_types.count("TimelineVersionCreated") == 1
    # 重试成功后靠纯 content 步收尾，最后一条 reply 即成功回复。
    assert "TurnEnded" in result.event_types
    replies = _reply_messages(result)
    assert replies[-1]["content"] == "timeline ok"
