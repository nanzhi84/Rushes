import json
from pathlib import Path

from pydantic import BaseModel, ConfigDict
from sqlalchemy.engine import Engine

from contracts.provider import ProviderDescriptor
from providers import LLM_CHAT, ProviderGateway, ProviderRegistry, ProviderRequest
from providers.gateway import ProviderCallRecord
from providers.mock import MockProvider
from storage import schema
from storage.db import begin_immediate, create_workspace_engine
from storage.repositories import ProviderCallsRepository


class EmptyConfig(BaseModel):
    model_config = ConfigDict(extra="forbid")


class StorageRecorder:
    def __init__(self, workspace: Path) -> None:
        self._workspace = workspace

    def record_provider_call(self, record: ProviderCallRecord) -> None:
        engine = create_workspace_engine(self._workspace)
        with begin_immediate(engine) as connection:
            ProviderCallsRepository(connection).insert(
                {
                    "call_id": record.call_id,
                    "provider_id": record.provider_id,
                    "capability": record.capability,
                    "model": record.model,
                    "case_id": record.case_id,
                    "job_id": record.job_id,
                    "latency_ms": record.latency_ms,
                    "usage_json": record.usage_json,
                    "cost_estimate": record.cost_estimate,
                    "status": record.status,
                }
            )


def _seed_project_and_case(engine: Engine) -> None:
    now = "2026-07-04T00:00:00+00:00"
    with begin_immediate(engine) as connection:
        connection.execute(
            schema.projects.insert().values(
                project_id="project_1",
                name="Project",
                status="active",
                defaults=json.dumps({"aspect_ratio": "9:16", "fps": 30}),
                created_at=now,
                updated_at=now,
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
                running_jobs=json.dumps([]),
                brief=json.dumps({"goal": "test", "confirmed_facts": []}),
                selected_asset_ids=json.dumps([]),
                disabled_asset_ids=json.dumps([]),
                scratch_memory=json.dumps({}),
            )
        )


async def test_gateway_selects_by_priority_and_records_provider_call(tmp_path: Path) -> None:
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)
    registry = ProviderRegistry()
    registry.register(
        _descriptor("slow", priority=100),
        MockProvider(
            provider_id="slow",
            scripts={LLM_CHAT: [{"normalized_output": {"text": "slow"}}]},
        ),
    )
    registry.register(
        _descriptor("fast", priority=10),
        MockProvider(
            provider_id="fast",
            scripts={LLM_CHAT: [{"normalized_output": {"text": "fast"}}]},
        ),
    )
    gateway = ProviderGateway(registry=registry, recorder=StorageRecorder(tmp_path))

    result = await gateway.call(
        ProviderRequest(capability=LLM_CHAT, case_id=None, payload={"message": "hi"})
    )

    assert result.result.provider_id == "fast"
    assert result.events[0]["event"] == "ProviderCallRecorded"
    assert _provider_calls(tmp_path)[0]["provider_id"] == "fast"


async def test_gateway_fallback_emits_capability_degraded_and_records_both_calls(
    tmp_path: Path,
) -> None:
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)
    _seed_project_and_case(engine)
    registry = ProviderRegistry()
    registry.register(
        _descriptor("primary", priority=1, fallback_provider_ids=["fallback"]),
        MockProvider(
            provider_id="primary",
            scripts={
                LLM_CHAT: [
                    {
                        "error": {
                            "error_code": "timeout",
                            "message": "timed out",
                            "retryable": True,
                        }
                    }
                ]
            },
        ),
    )
    registry.register(
        _descriptor("fallback", priority=50),
        MockProvider(
            provider_id="fallback",
            scripts={LLM_CHAT: [{"normalized_output": {"text": "ok"}}]},
        ),
    )
    gateway = ProviderGateway(registry=registry, recorder=StorageRecorder(tmp_path))

    result = await gateway.call(ProviderRequest(capability=LLM_CHAT, case_id="case_1"))
    calls = _provider_calls(tmp_path)

    assert result.result.provider_id == "fallback"
    assert [event["event"] for event in result.events] == [
        "ProviderCallRecorded",
        "CapabilityDegraded",
        "ProviderCallRecorded",
    ]
    assert [call["status"] for call in calls] == ["failed", "succeeded"]
    assert calls[0]["provider_id"] == "primary"
    assert calls[1]["provider_id"] == "fallback"


async def test_streaming_failover_streams_primary_partial_then_replays_fallback_once(
    tmp_path: Path,
) -> None:
    # 首个 provider 先流式吐 partial 再失败，故障转移到第二个 provider 成功：
    # 期望的 delta 序列 = primary 的 partial 分片 + fallback 整段回放一次，
    # 绝不出现两路 provider 交错的双重流式。
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)
    _seed_project_and_case(engine)
    registry = ProviderRegistry()
    registry.register(
        _descriptor("primary", priority=1, fallback_provider_ids=["fallback"]),
        MockProvider(
            provider_id="primary",
            scripts={
                LLM_CHAT: [
                    {
                        "content": "PPPPPPPPPPPP",
                        "error": {
                            "error_code": "timeout",
                            "message": "timed out mid-stream",
                            "retryable": True,
                        },
                    }
                ]
            },
        ),
    )
    registry.register(
        _descriptor("fallback", priority=50),
        MockProvider(
            provider_id="fallback",
            scripts={LLM_CHAT: [{"content": "01234567890123456789"}]},
        ),
    )
    gateway = ProviderGateway(registry=registry, recorder=StorageRecorder(tmp_path))
    chunks: list[dict[str, object]] = []

    result = await gateway.call(
        ProviderRequest(capability=LLM_CHAT, case_id="case_1"),
        on_delta=chunks.append,
    )

    assert result.result.provider_id == "fallback"
    texts = [chunk["text"] for chunk in chunks]
    # primary：12 字符按 8 分片 → 两片；fallback：整段回放一次（不再分片）。
    assert texts == ["PPPPPPPP", "PPPP", "01234567890123456789"]
    assert [event["event"] for event in result.events] == [
        "ProviderCallRecorded",
        "CapabilityDegraded",
        "ProviderCallRecorded",
    ]


def _descriptor(
    provider_id: str,
    *,
    priority: int,
    fallback_provider_ids: list[str] | None = None,
) -> ProviderDescriptor:
    return ProviderDescriptor(
        provider_id=provider_id,
        display_name=provider_id,
        version="1",
        capabilities=[LLM_CHAT],
        config_model=EmptyConfig,
        client_ref=f"tests.{provider_id}",
        priority=priority,
        fallback_provider_ids=fallback_provider_ids or [],
    )


def _provider_calls(tmp_path: Path) -> list[dict[str, object]]:
    engine = create_workspace_engine(tmp_path)
    with begin_immediate(engine) as connection:
        rows = connection.execute(schema.provider_calls.select()).all()
    return [dict(row._mapping) for row in rows]
