import json
from pathlib import Path

from tests.golden.framework import (
    ExpectedToolTrace,
    GoldenAssertions,
    GoldenCase,
    GoldenExecutor,
    GoldenRunResult,
    base_workspace,
    registry_with_timeline_tools,
    understand_compose_workspace,
)

_UNDERSTAND_SUMMARY = {
    "semantic_role": "footage",
    "overall": "一段稳定的城市天际线空镜，光线均匀。",
    "language": None,
    "segments": [
        {
            "start_s": 0.0,
            "end_s": 6.0,
            "description": "航拍城市天际线，缓慢右移",
            "tags": ["空镜", "城市"],
            "quality": "good",
        }
    ],
}


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


async def test_golden_understand_summary_then_compose_initial(tmp_path: Path) -> None:
    # Spec C 主链路：主代理派理解子代理（脚本化 VLM 经 MockProvider 立即 emit_summary）
    # → 再次 understand.materials 命中缓存直接读回摘要（承接原 asset.read_summary）
    # → 基于摘要时间戳直接 compose_initial 组装初剪 → 纯 content 步收尾。
    case = GoldenCase(
        name="understand_summary_compose_initial",
        build_workspace=understand_compose_workspace,
        user_messages=("把这段素材理解一下，挑好画面剪个初版",),
        provider_script=(
            {
                "content": "我先派子代理理解这段素材。",
                "tool_call": {
                    "tool_name": "understand.materials",
                    "arguments": {"asset_ids": ["asset_1"]},
                },
            },
            {
                "tool_call": {
                    "tool_name": "understand.materials",
                    "arguments": {"asset_ids": ["asset_1"]},
                }
            },
            {
                "tool_call": {
                    "tool_name": "timeline.compose_initial",
                    "arguments": {
                        "clips": [
                            {
                                "asset_id": "asset_1",
                                "source_start_s": 0.0,
                                "source_end_s": 6.0,
                                "role": "a_roll",
                            }
                        ]
                    },
                }
            },
            {"content": "初剪已就绪：用了 0-6s 那段空镜，共 1 段。"},
        ),
        vlm_script=(
            {"content": json.dumps({"action": "emit_summary", "summary": _UNDERSTAND_SUMMARY})},
        ),
        expected_tool_trace=(
            ExpectedToolTrace("understand.materials", "succeeded"),
            ExpectedToolTrace("understand.materials", "succeeded"),
            ExpectedToolTrace("timeline.compose_initial", "succeeded"),
        ),
        assertions=GoldenAssertions(assert_final=_assert_understand_compose_final),
    )

    await GoldenExecutor().run(case, tmp_path)


def _reply_messages(result: GoldenRunResult) -> list[dict[str, object]]:
    return [message for message in result.messages if message.get("kind") == "reply"]


def _narration_messages(result: GoldenRunResult) -> list[dict[str, object]]:
    return [message for message in result.messages if message.get("kind") == "narration"]


def _assert_pending_decision_final(result: GoldenRunResult) -> None:
    assert result.draft_state.pending_decision_id is None
    assert result.draft_state.scratch_memory
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
    assert result.draft_state.timeline_current_version == 1
    assert result.event_types.count("TimelineVersionCreated") == 1
    assert "TurnEnded" in result.event_types


def _assert_illegal_tool_final(result: GoldenRunResult) -> None:
    assert result.event_types.count("PolicyRefusal") == 3
    assert result.draft_state.state_version == 0
    # 连续非法输出触顶后由 harness 兜底：写一条 reply 行 + TurnEnded 收尾。
    assert "TurnEnded" in result.event_types
    replies = _reply_messages(result)
    assert len(replies) == 1
    assert replies[0]["role"] == "assistant"
    assert "连续 3 次" in str(replies[0]["content"])


def _assert_version_conflict_final(result: GoldenRunResult) -> None:
    assert result.draft_state.timeline_current_version == 1
    assert result.draft_state.state_version == 2
    assert result.event_types.count("TimelineVersionCreated") == 1
    # 重试成功后靠纯 content 步收尾，最后一条 reply 即成功回复。
    assert "TurnEnded" in result.event_types
    replies = _reply_messages(result)
    assert replies[-1]["content"] == "timeline ok"


def _assert_understand_compose_final(result: GoldenRunResult) -> None:
    # 理解落库：子代理产出摘要 → MaterialUnderstanding 事件成对出现。
    assert "MaterialUnderstandingStarted" in result.event_types
    assert "MaterialUnderstandingCompleted" in result.event_types
    tool_names = [trace.tool_name for trace in result.traces]
    assert tool_names == [
        "understand.materials",
        "understand.materials",
        "timeline.compose_initial",
    ]
    # 第二次 understand.materials 同 turn 命中刚落库的摘要（观察串带「缓存命中」+overall 正文），
    # 证明 mid-turn 持久化生效、缓存路径承接了原 asset.read_summary 的取用职责。
    cached_summary_trace = result.traces[1]
    assert "城市天际线" in cached_summary_trace.observation
    # 摘要时间戳直接组装出 v1 初剪并通过校验。
    assert result.draft_state.timeline_current_version == 1
    assert result.event_types.count("TimelineVersionCreated") == 1
    assert "TimelineValidated" in result.event_types
    # 纯 content 步收尾。
    assert "TurnEnded" in result.event_types
    replies = _reply_messages(result)
    assert replies[-1]["content"] == "初剪已就绪：用了 0-6s 那段空镜，共 1 段。"
    assert replies[-1]["role"] == "assistant"
