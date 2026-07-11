"""turn 内观察回灌、defer 幂等短路与 job 观察桥（M9 实测缺陷的回归测试）。"""

from __future__ import annotations

from pathlib import Path

import pytest
from sqlalchemy import select

from agent_harness.context_builder import (
    ContextBuilder,
    ContextBuildInput,
)
from agent_harness.loop import _finished_job_observation, _turn_observation_entry
from agent_harness.policy_gate import ToolCall
from contracts.tool_result import ToolResult
from domain.preconditions import DraftArtifactStats, PreconditionContext
from storage import schema
from storage.db import create_workspace_engine
from storage.repositories._json import dump_json

NOW = "2026-07-06T00:00:00+00:00"


def _engine(tmp_path: Path):
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)
        connection.execute(
            schema.drafts.insert().values(
                draft_id="draft_1",
                name="Draft",
                state_version=0,
                status="active",
                defaults=dump_json({"aspect_ratio": "9:16", "fps": 30}),
                timeline_validated=False,
                rough_cut_approved=False,
                running_jobs="[]",
                brief=dump_json({"goal": "test", "confirmed_facts": []}),
                scratch_memory="{}",
                created_at=NOW,
                updated_at=NOW,
            )
        )
    return engine


def _seed_job(engine, job_id: str, *, status: str, result: str | None, error: str | None) -> None:
    with engine.begin() as connection:
        connection.execute(
            schema.jobs.insert().values(
                job_id=job_id,
                kind="asr",
                status=status,
                idempotency_key=f"key_{job_id}",
                payload_json="{}",
                result_json=result,
                error_json=error,
                attempts=1,
                max_retries=2,
                next_run_at=NOW,
                created_at=NOW,
            )
        )


def test_turn_observation_entry_truncates_arguments_and_observation() -> None:
    call = ToolCall(
        tool_call_id="tc_1",
        tool_name="audio.inspect_sources",
        arguments={"asset_ids": ["a" * 300]},
    )
    result = ToolResult(
        tool_call_id="tc_1",
        tool_name="audio.inspect_sources",
        status="succeeded",
        observation="观" * 500,
    )
    entry = _turn_observation_entry(call, result)
    assert entry.startswith("audio.inspect_sources(")
    assert "…" in entry
    assert len(entry) < 700


def test_scan_observation_keeps_all_31_evidence_anchors() -> None:
    call = ToolCall(
        tool_call_id="tc_scan",
        tool_name="understand.materials",
        arguments={"asset_ids": [f"asset_{index:02d}" for index in range(31)], "depth": "scan"},
    )
    observation = "\n".join(
        f"【asset_{index:02d}】摘要；证据：{index:.2f}s/poster；置信度 0.90" for index in range(31)
    )

    entry = _turn_observation_entry(
        call,
        ToolResult(
            tool_call_id="tc_scan",
            tool_name="understand.materials",
            status="succeeded",
            observation=observation,
        ),
    )
    bundle = ContextBuilder().build(
        ContextBuildInput(
            preconditions=PreconditionContext(draft_artifacts=DraftArtifactStats()),
            turn_observations=(entry,),
        )
    )

    assert "0.00s/poster" in entry
    assert "30.00s/poster" in entry
    assert "30.00s/poster" in bundle.blocks["turn_observations"]


def test_turn_observations_block_renders_and_trims() -> None:
    builder = ContextBuilder(budgets={"turn_observations": 20})
    bundle = builder.build(
        ContextBuildInput(
            preconditions=PreconditionContext(draft_artifacts=DraftArtifactStats()),
            turn_observations=("第一条很长的观察" * 10, "最后一条观察"),
        )
    )
    block = bundle.blocks["turn_observations"]
    assert "最后一条观察" in block
    # 超预算时最早的观察被裁掉
    assert "第一条很长的观察" not in block

    empty = builder.build(
        ContextBuildInput(preconditions=PreconditionContext(draft_artifacts=DraftArtifactStats()))
    )
    assert "尚未执行" in empty.blocks["turn_observations"]


def test_finished_job_observation_reports_terminal_states(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    _seed_job(
        engine,
        "job_ok",
        status="succeeded",
        result=dump_json({"transcript_id": "tr_1"}),
        error=None,
    )
    _seed_job(
        engine, "job_bad", status="failed", result=None, error=dump_json({"error_code": "boom"})
    )
    _seed_job(engine, "job_run", status="running", result=None, error=None)

    ok = _finished_job_observation(engine, "job_ok", tool_name="audio.asr_original")
    assert ok is not None
    assert ok[0] == "succeeded"
    assert "无需重复发起" in ok[1]
    assert "tr_1" in ok[1]

    bad = _finished_job_observation(engine, "job_bad", tool_name="audio.asr_original")
    assert bad is not None
    assert bad[0] == "failed"

    assert _finished_job_observation(engine, "job_run", tool_name="audio.asr_original") is None
    assert _finished_job_observation(engine, "job_ghost", tool_name="audio.asr_original") is None


def test_build_default_tool_gateway_registers_capabilities(monkeypatch) -> None:
    """gateway 工厂：DASHSCOPE key 单钥覆盖 LLM/VLM 能力；无 key 返回 None（M9 回归）。"""
    from providers.tool_gateway import build_default_tool_gateway

    for name in (
        "RUSHES_DASHSCOPE_API_KEY",
        "RUSHES_LLM_API_KEY",
        "RUSHES_VLM_API_KEY",
        "RUSHES_LLM_MODEL",
    ):
        monkeypatch.delenv(name, raising=False)
    assert build_default_tool_gateway() is None

    monkeypatch.setenv("RUSHES_DASHSCOPE_API_KEY", "sk-test")
    monkeypatch.setenv("RUSHES_LLM_MODEL", "qwen-max")
    gateway = build_default_tool_gateway()
    assert gateway is not None


def test_tool_context_metadata_includes_gateway(tmp_path: Path) -> None:
    from agent_harness.tool_execution import _tool_metadata

    engine = _engine(tmp_path)
    marker = object()
    metadata = _tool_metadata(
        engine,
        gateway=marker,
        turn_progress=None,
        stop_token=None,
        workspace_paths=None,
    )
    assert metadata["provider_gateway"] is marker
    assert "workspace_path" in metadata
    assert "provider_gateway" not in _tool_metadata(
        engine,
        gateway=None,
        turn_progress=None,
        stop_token=None,
        workspace_paths=None,
    )


def test_partial_result_sink_raises_on_non_applied(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    from agent_harness import tool_execution as execution_mod
    from agent_harness.reducer import ReducerApplyResult

    engine = _engine(tmp_path)
    # 伪造归约拒绝（validator 未通过）：sink 不能静默吞，必须 raise 让工具诚实失败；
    # 真实执行路径由 Reducer 同一事务保证 rows/events 全部回滚。
    monkeypatch.setattr(
        execution_mod,
        "apply_tool_events",
        lambda *a, **k: ReducerApplyResult(status="validation_failed"),
    )
    collected: list[ReducerApplyResult] = []
    sink = execution_mod._make_partial_result_sink(engine, None, "agent", collected)
    with pytest.raises(RuntimeError, match="partial_result_sink"):
        sink({}, [{"event": "MaterialUnderstandingCompleted", "asset_id": "a1"}])
    assert collected and collected[0].status == "validation_failed"


def test_partial_result_sink_collects_applied_results(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    from agent_harness import loop as loop_mod
    from agent_harness import tool_execution as execution_mod
    from agent_harness.reducer import AppliedEvent, ReducerApplyResult

    engine = _engine(tmp_path)
    applied = ReducerApplyResult(
        status="applied",
        applied_events=(
            AppliedEvent(event_id=1, event_type="MaterialUnderstandingStarted", state_version=None),
        ),
    )
    monkeypatch.setattr(execution_mod, "apply_tool_events", lambda *a, **k: applied)
    collected: list[ReducerApplyResult] = []
    sink = execution_mod._make_partial_result_sink(engine, None, "agent", collected)
    sink({}, [{"event": "MaterialUnderstandingStarted", "asset_id": "a1"}])
    assert collected == [applied]
    # _record_sink_results 把结果并入主记录处（accumulator），不再静默吞掉。
    accumulator = loop_mod._RunAccumulator()
    loop_mod._record_sink_results(collected, accumulator=accumulator, tracer=None)
    assert accumulator.reducer_results == [applied]


def test_partial_result_sink_advances_version_between_commits(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    from agent_harness import tool_execution as execution_mod
    from agent_harness.reducer import ReducerApplyResult

    engine = _engine(tmp_path)
    observed_versions: list[int | None] = []

    def _apply(*args, **kwargs):
        observed_versions.append(kwargs["base_version"])
        next_version = len(observed_versions)
        return ReducerApplyResult(status="applied", draft_state_versions={"draft_1": next_version})

    monkeypatch.setattr(execution_mod, "apply_tool_events", _apply)
    current_version: list[int | None] = [0]
    sink = execution_mod._make_partial_result_sink(engine, 0, "agent", [], current_version)

    sink({}, [{"event": "DraftRenamed", "draft_id": "draft_1"}])
    sink({}, [{"event": "DraftRenamed", "draft_id": "draft_1"}])

    assert observed_versions == [0, 1]
    assert current_version == [2]


def test_result_rows_roll_back_on_reducer_version_conflict(tmp_path: Path) -> None:
    from agent_harness.tool_execution import apply_tool_events

    engine = _engine(tmp_path)
    result = apply_tool_events(
        [
            {
                "event": "BriefUpdated",
                "draft_id": "draft_1",
                "payload": {"brief": {"goal": "New", "confirmed_facts": []}},
            }
        ],
        engine=engine,
        base_version=-1,
        actor="agent",
        rows={
            "message_row": {
                "message_id": "msg_orphan",
                "draft_id": "draft_1",
                "role": "assistant",
                "kind": "reply",
                "content": {"text": "must roll back"},
                "created_at": NOW,
            }
        },
    )

    assert result.status == "version_conflict"
    with engine.connect() as connection:
        assert connection.execute(select(schema.messages)).all() == []
        assert connection.execute(select(schema.event_log)).all() == []
