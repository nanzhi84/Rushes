"""turn 内观察回灌、defer 幂等短路与 job 观察桥（M9 实测缺陷的回归测试）。"""

from __future__ import annotations

import asyncio
from pathlib import Path
from typing import Any

import pytest

from agent_harness.context_builder import (
    ContextBuilder,
    ContextBuildInput,
)
from agent_harness.loop import _finished_job_observation, _turn_observation_entry
from agent_harness.policy_gate import ToolCall
from agent_harness.turn_queue import TurnQueue, TurnQueueItem
from contracts.tool_result import ToolResult
from domain.preconditions import PreconditionContext, ProjectArtifactStats
from storage import schema
from storage.db import create_workspace_engine
from storage.repositories._json import dump_json

NOW = "2026-07-06T00:00:00+00:00"


def _engine(tmp_path: Path):
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)
        connection.execute(
            schema.projects.insert().values(
                project_id="project_1",
                name="Project",
                status="active",
                defaults=dump_json({"aspect_ratio": "9:16", "fps": 30}),
                created_at=NOW,
                updated_at=NOW,
            )
        )
        connection.execute(
            schema.cases.insert().values(
                case_id="case_1",
                project_id="project_1",
                name="Case",
                state_version=0,
                status="active",
                timeline_validated=False,
                rough_cut_approved=False,
                running_jobs="[]",
                brief=dump_json({"goal": "test", "confirmed_facts": []}),
                selected_asset_ids="[]",
                disabled_asset_ids="[]",
                scratch_memory="{}",
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


def test_turn_observations_block_renders_and_trims() -> None:
    builder = ContextBuilder(budgets={"turn_observations": 20})
    bundle = builder.build(
        ContextBuildInput(
            preconditions=PreconditionContext(project_artifacts=ProjectArtifactStats()),
            turn_observations=("第一条很长的观察" * 10, "最后一条观察"),
        )
    )
    block = bundle.blocks["turn_observations"]
    assert "最后一条观察" in block
    # 超预算时最早的观察被裁掉
    assert "第一条很长的观察" not in block

    empty = builder.build(
        ContextBuildInput(
            preconditions=PreconditionContext(project_artifacts=ProjectArtifactStats())
        )
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


@pytest.mark.anyio
async def test_job_observation_bridge_enqueues_terminal_job_events(tmp_path: Path) -> None:
    from apps.api.main import _job_observation_bridge

    engine = _engine(tmp_path)
    received: list[TurnQueueItem] = []

    async def runner(item: TurnQueueItem, stop_token: Any) -> None:
        received.append(item)

    queue = TurnQueue(runner)
    task = asyncio.create_task(_job_observation_bridge(engine, queue, poll_interval=0.05))
    try:
        await asyncio.sleep(0.1)
        with engine.begin() as connection:
            connection.execute(
                schema.event_log.insert().values(
                    event_type="JobSucceeded",
                    actor="job",
                    case_id="case_1",
                    payload_json=dump_json(
                        {
                            "event": "JobSucceeded",
                            "job_id": "job_x",
                            "requested_by_case_id": "case_1",
                            "payload": {"kind": "asr"},
                        }
                    ),
                    created_at=NOW,
                )
            )
        for _ in range(40):
            await asyncio.sleep(0.05)
            if received:
                break
    finally:
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task
        await queue.shutdown()

    assert received
    assert received[0].kind == "job_observation"
    assert received[0].payload["job_id"] == "job_x"


@pytest.fixture
def anyio_backend() -> str:
    return "asyncio"


def test_build_default_tool_gateway_registers_capabilities(monkeypatch) -> None:
    """gateway 工厂：DASHSCOPE key 单钥覆盖三能力；无 key 返回 None（M9 回归）。"""
    from providers.tool_gateway import build_default_tool_gateway

    for name in (
        "RUSHES_DASHSCOPE_API_KEY",
        "RUSHES_LLM_API_KEY",
        "RUSHES_VLM_API_KEY",
        "RUSHES_EMBEDDING_API_KEY",
        "RUSHES_LLM_MODEL",
    ):
        monkeypatch.delenv(name, raising=False)
    assert build_default_tool_gateway() is None

    monkeypatch.setenv("RUSHES_DASHSCOPE_API_KEY", "sk-test")
    monkeypatch.setenv("RUSHES_LLM_MODEL", "qwen-max")
    gateway = build_default_tool_gateway()
    assert gateway is not None


def test_tool_context_metadata_includes_gateway(tmp_path: Path) -> None:
    from agent_harness.loop import _tool_context_metadata

    engine = _engine(tmp_path)
    marker = object()
    metadata = _tool_context_metadata(engine, marker)
    assert metadata["provider_gateway"] is marker
    assert "workspace_path" in metadata
    assert "provider_gateway" not in _tool_context_metadata(engine, None)
