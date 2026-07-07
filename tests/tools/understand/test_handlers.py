"""understand.materials / asset.read_summary handler 集成测试（Spec C §C3）。

VLM 一律用脚本化 gateway（喂 provider 形态的 normalized_output，内含动作 JSON 串），
抽帧与 ASR 打桩，不碰真实 ffmpeg/网络。落库路径复用 loop 的 ``_persist_tool_result_data``。
"""

from __future__ import annotations

import asyncio
import json
import re
from pathlib import Path
from typing import Any

import pytest

from agent_harness.loop import _persist_tool_result_data
from contracts.case import CaseState
from contracts.project import ProjectState
from contracts.provider import ProviderResult
from providers import VLM_ANNOTATION
from providers.gateway import ProviderGatewayResult
from storage import schema
from storage.db import create_workspace_engine
from storage.repositories import MaterialSummariesRepository, TranscriptsRepository
from storage.repositories._json import dump_json
from tools import ToolExecutionContext
from tools.specs import AssetReadSummaryInput, UnderstandMaterialsInput
from tools.understand import handlers as understand_handlers
from tools.understand.handlers import materials, read_summary
from tools.understand.subagent import TranscribeResult

NOW = "2026-07-06T00:00:00+00:00"
DATA_URI = "data:image/jpeg;base64,ZmFrZQ=="

_GOOD_SUMMARY = {
    "semantic_role": "footage",
    "overall": "一段产品特写。",
    "language": "zh",
    "segments": [
        {"start_s": 0.0, "end_s": 12.0, "description": "特写", "tags": [], "quality": "good"}
    ],
}


def _emit(summary: dict[str, Any] | None = None) -> dict[str, Any]:
    return {"action": "emit_summary", "summary": summary or _GOOD_SUMMARY}


def _asset_from_messages(messages: list[dict[str, Any]]) -> str:
    text = messages[1]["content"][0]["text"]
    match = re.search(r"asset_id=([^；\s]+)", text)
    return match.group(1) if match else ""


class ScriptedVlmGateway:
    """按 asset_id 分发脚本化动作，报文形态贴近真实 OpenAI 兼容 VLM。"""

    def __init__(self, scripts: dict[str, list[dict[str, Any]]]) -> None:
        self.scripts = {key: list(value) for key, value in scripts.items()}
        self.calls: list[str] = []
        self.prompts: list[str] = []

    async def call(self, request: Any) -> ProviderGatewayResult:
        messages = request.payload["messages"]
        asset_id = _asset_from_messages(messages)
        self.calls.append(asset_id)
        self.prompts.append(messages[1]["content"][0]["text"])
        queue = self.scripts.get(asset_id) or []
        action = queue.pop(0) if queue else {"action": "noop"}
        return ProviderGatewayResult(
            result=ProviderResult(
                provider_id="mock_vlm",
                capability=VLM_ANNOTATION,
                request_id=request.request_id,
                model="mock",
                latency_ms=1,
                normalized_output={"content": json.dumps(action, ensure_ascii=False)},
            )
        )


class ConcurrencyProbeGateway:
    """记录同时在飞的 VLM 调用峰值，用于验证信号量上限。"""

    def __init__(self, script: list[dict[str, Any]]) -> None:
        self.script = script
        self._per_asset: dict[str, int] = {}
        self.inflight = 0
        self.max_inflight = 0

    async def call(self, request: Any) -> ProviderGatewayResult:
        asset_id = _asset_from_messages(request.payload["messages"])
        self.inflight += 1
        self.max_inflight = max(self.max_inflight, self.inflight)
        try:
            await asyncio.sleep(0.02)
            index = self._per_asset.get(asset_id, 0)
            self._per_asset[asset_id] = index + 1
            action = self.script[index] if index < len(self.script) else {"action": "noop"}
        finally:
            self.inflight -= 1
        return ProviderGatewayResult(
            result=ProviderResult(
                provider_id="mock_vlm",
                capability=VLM_ANNOTATION,
                request_id=request.request_id,
                model="mock",
                latency_ms=1,
                normalized_output={"content": json.dumps(action, ensure_ascii=False)},
            )
        )


class SlowGateway:
    async def call(self, request: Any) -> ProviderGatewayResult:
        await asyncio.sleep(1.0)
        return ProviderGatewayResult(
            result=ProviderResult(
                provider_id="mock_vlm",
                capability=VLM_ANNOTATION,
                request_id=request.request_id,
                model="mock",
                latency_ms=1,
                normalized_output={"content": json.dumps(_emit())},
            )
        )


def _engine(tmp_path: Path, asset_specs: list[dict[str, Any]]) -> Any:
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)
        connection.execute(
            schema.projects.insert().values(
                project_id="project_1",
                name="Project",
                status="active",
                defaults="{}",
                created_at=NOW,
                updated_at=NOW,
            )
        )
        for spec in asset_specs:
            asset_id = spec["asset_id"]
            source = tmp_path / f"{asset_id}.mp4"
            source.write_bytes(b"placeholder")
            connection.execute(
                schema.assets.insert().values(
                    asset_id=asset_id,
                    storage_mode="reference",
                    object_hash=None,
                    reference_path=str(source),
                    kind=spec.get("kind", "video"),
                    source="local_path",
                    filename=spec.get("filename", f"{asset_id}.mp4"),
                    hash="hash",
                    mtime=1,
                    size=11,
                    probe=None,
                    proxy_object_hash=None,
                    ingest_status="indexed",
                    usable=True,
                    failure=None,
                    index_json=dump_json(spec.get("index_json", {"duration_sec": 12.0})),
                    understanding_status="none",
                )
            )
            connection.execute(
                schema.project_asset_links.insert().values(
                    project_id="project_1",
                    asset_id=asset_id,
                    enabled=True,
                    linked_at=NOW,
                    note="",
                )
            )
    return engine


def _seed_summary(engine: Any, asset_id: str, *, version: int, overall: str) -> None:
    summary = {
        "asset_id": asset_id,
        "version": version,
        "focus": None,
        "semantic_role": "footage",
        "overall": overall,
        "language": "zh",
        "segments": [],
        "generated_at": NOW,
        "model": "qwen-vl-plus",
        "spent": {"frames_viewed": 1, "asr_seconds": 0.0},
    }
    with engine.begin() as connection:
        MaterialSummariesRepository(connection).insert(
            {
                "summary_id": f"ms_{asset_id}_v{version}",
                "asset_id": asset_id,
                "version": version,
                "focus": None,
                "status": "ready",
                "summary_json": summary,
                "model": "qwen-vl-plus",
                "created_at": NOW,
            }
        )


def _context(
    engine: Any,
    connection: Any,
    *,
    gateway: Any | None,
    tmp_path: Path,
    progress: Any | None = None,
) -> ToolExecutionContext:
    metadata: dict[str, Any] = {"workspace_path": str(tmp_path)}
    if gateway is not None:
        metadata["provider_gateway"] = gateway
    if progress is not None:
        metadata["turn_progress"] = progress
    return ToolExecutionContext(
        tool_call_id="tc_understand",
        turn_id="turn_1",
        case_state=_case_state(),
        project_state=ProjectState.model_validate(
            {
                "project_id": "project_1",
                "name": "Project",
                "status": "active",
                "created_at": NOW,
                "updated_at": NOW,
            }
        ),
        readonly_connection=connection,
        metadata=metadata,
    )


def _case_state() -> CaseState:
    return CaseState.model_validate(
        {
            "case_id": "case_1",
            "project_id": "project_1",
            "name": "Case",
            "brief": {"goal": "test", "confirmed_facts": []},
            "selected_asset_ids": [],
            "disabled_asset_ids": [],
            "scratch_memory": {},
        }
    )


async def test_ready_summary_persists_rows_and_emits_events(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr(understand_handlers, "extract_frame_data_uri", lambda _p, _s: DATA_URI)
    engine = _engine(tmp_path, [{"asset_id": "asset_1"}])
    gateway = ScriptedVlmGateway(
        {"asset_1": [{"action": "view_frames", "timestamps_s": [2.0, 4.0]}, _emit()]}
    )
    with engine.connect() as connection:
        result = await materials(
            UnderstandMaterialsInput(asset_ids=["asset_1"]),
            _context(engine, connection, gateway=gateway, tmp_path=tmp_path),
        )

    assert result.status == "succeeded"
    assert result.data["results"]["asset_1"]["status"] == "ready"
    assert len(result.data["material_summary_rows"]) == 1
    event_types = [event["event"] for event in result.events]
    assert event_types == ["MaterialUnderstandingStarted", "MaterialUnderstandingCompleted"]
    assert all(event.get("case_id") is None for event in result.events)

    _persist_tool_result_data(result, engine=engine)
    with engine.connect() as connection:
        latest = MaterialSummariesRepository(connection).latest_ready("asset_1")
    assert latest is not None
    assert latest["version"] == 1
    assert latest["summary_json"]["overall"] == "一段产品特写。"


async def test_cache_hit_skips_subagent(tmp_path: Path) -> None:
    engine = _engine(tmp_path, [{"asset_id": "asset_1"}])
    _seed_summary(engine, "asset_1", version=1, overall="缓存里的摘要")
    gateway = ScriptedVlmGateway({"asset_1": [_emit()]})
    with engine.connect() as connection:
        result = await materials(
            UnderstandMaterialsInput(asset_ids=["asset_1"]),
            _context(engine, connection, gateway=gateway, tmp_path=tmp_path),
        )

    assert result.status == "succeeded"
    assert gateway.calls == []  # 命中缓存不起子代理
    assert result.events == []
    assert "material_summary_rows" not in result.data
    assert "缓存命中" in result.observation
    assert result.data["results"]["asset_1"]["status"] == "cached"


async def test_focus_increments_version_and_feeds_prior(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr(understand_handlers, "extract_frame_data_uri", lambda _p, _s: DATA_URI)
    engine = _engine(tmp_path, [{"asset_id": "asset_1"}])
    _seed_summary(engine, "asset_1", version=1, overall="旧摘要正文")
    gateway = ScriptedVlmGateway({"asset_1": [_emit()]})
    with engine.connect() as connection:
        result = await materials(
            UnderstandMaterialsInput(asset_ids=["asset_1"], focus="口播是否清晰"),
            _context(engine, connection, gateway=gateway, tmp_path=tmp_path),
        )

    assert result.status == "succeeded"
    row = result.data["material_summary_rows"][0]
    assert row["version"] == 2
    assert row["focus"] == "口播是否清晰"
    assert row["summary_json"]["version"] == 2
    # 子代理带着旧摘要增量深挖：prompt 里应出现旧摘要正文与本次 focus。
    assert any("旧摘要正文" in prompt for prompt in gateway.prompts)
    assert any("口播是否清晰" in prompt for prompt in gateway.prompts)


async def test_missing_asset_reports_failure_without_events(tmp_path: Path) -> None:
    engine = _engine(tmp_path, [{"asset_id": "asset_1"}])
    gateway = ScriptedVlmGateway({"asset_1": [_emit()]})
    with engine.connect() as connection:
        result = await materials(
            UnderstandMaterialsInput(asset_ids=["ghost"]),
            _context(engine, connection, gateway=gateway, tmp_path=tmp_path),
        )

    assert result.status == "succeeded"
    assert result.events == []  # 不给幽灵素材派事件
    assert result.data["results"]["ghost"]["status"] == "failed"


async def test_gateway_missing_fails_all_existing_assets(tmp_path: Path) -> None:
    engine = _engine(tmp_path, [{"asset_id": "asset_1"}])
    with engine.connect() as connection:
        result = await materials(
            UnderstandMaterialsInput(asset_ids=["asset_1"]),
            _context(engine, connection, gateway=None, tmp_path=tmp_path),
        )

    assert result.status == "succeeded"
    assert result.data["results"]["asset_1"]["status"] == "failed"
    assert "VLM 通道不可用" in result.data["results"]["asset_1"]["reason"]
    event_types = [event["event"] for event in result.events]
    assert event_types == ["MaterialUnderstandingStarted", "MaterialUnderstandingFailed"]


async def test_concurrency_capped_by_semaphore(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("RUSHES_UNDERSTAND_CONCURRENCY", "2")
    monkeypatch.setattr(understand_handlers, "extract_frame_data_uri", lambda _p, _s: DATA_URI)
    asset_ids = [f"asset_{i}" for i in range(4)]
    engine = _engine(tmp_path, [{"asset_id": a} for a in asset_ids])
    gateway = ConcurrencyProbeGateway([_emit()])
    with engine.connect() as connection:
        result = await materials(
            UnderstandMaterialsInput(asset_ids=asset_ids),
            _context(engine, connection, gateway=gateway, tmp_path=tmp_path),
        )

    assert result.status == "succeeded"
    assert gateway.max_inflight <= 2
    assert all(result.data["results"][a]["status"] == "ready" for a in asset_ids)


async def test_timeout_marks_asset_failed(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("RUSHES_UNDERSTAND_TIMEOUT_S", "1")
    monkeypatch.setattr(understand_handlers, "_timeout_seconds", lambda: 0.05)
    engine = _engine(tmp_path, [{"asset_id": "asset_1"}])
    with engine.connect() as connection:
        result = await materials(
            UnderstandMaterialsInput(asset_ids=["asset_1"]),
            _context(engine, connection, gateway=SlowGateway(), tmp_path=tmp_path),
        )

    assert result.status == "succeeded"
    assert result.data["results"]["asset_1"]["status"] == "failed"
    assert "超时" in result.data["results"]["asset_1"]["reason"]
    assert [event["event"] for event in result.events][-1] == "MaterialUnderstandingFailed"


async def test_transcribe_rows_persisted(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(understand_handlers, "extract_frame_data_uri", lambda _p, _s: DATA_URI)

    async def _fake_transcribe(
        _context: Any, _info: Any, start_s: Any, end_s: Any
    ) -> TranscribeResult:
        return TranscribeResult(
            text="你好",
            language="zh",
            provider_id="mock_asr",
            raw_preserved=True,
            utterances=[
                {"utterance_id": "u1", "text": "你好", "start_ms": 0, "end_ms": 800, "words": []}
            ],
            vad_segments=[],
            seconds=90.0,
        )

    monkeypatch.setattr(understand_handlers, "_transcribe_segment", _fake_transcribe)
    # 索引里带 VAD 语音段 → has_audio=True，transcribe 动作才会放行。
    engine = _engine(
        tmp_path,
        [
            {
                "asset_id": "asset_1",
                "index_json": {
                    "duration_sec": 90.0,
                    "vad": [{"start_ms": 0, "end_ms": 800, "kind": "speech"}],
                },
            }
        ],
    )
    gateway = ScriptedVlmGateway(
        {"asset_1": [{"action": "transcribe", "start_s": 0.0, "end_s": 90.0}, _emit()]}
    )
    with engine.connect() as connection:
        result = await materials(
            UnderstandMaterialsInput(asset_ids=["asset_1"]),
            _context(engine, connection, gateway=gateway, tmp_path=tmp_path),
        )

    assert result.status == "succeeded"
    assert len(result.data["transcript_rows"]) == 1

    _persist_tool_result_data(result, engine=engine)
    with engine.connect() as connection:
        transcripts = TranscriptsRepository(connection).list_for_asset("asset_1")
    assert len(transcripts) == 1
    assert transcripts[0]["utterances"][0]["text"] == "你好"


async def test_read_summary_returns_latest_ready(tmp_path: Path) -> None:
    engine = _engine(tmp_path, [{"asset_id": "asset_1"}, {"asset_id": "asset_2"}])
    _seed_summary(engine, "asset_1", version=1, overall="第一版")
    _seed_summary(engine, "asset_1", version=2, overall="第二版最新")
    with engine.connect() as connection:
        result = read_summary(
            AssetReadSummaryInput(asset_ids=["asset_1", "asset_2"]),
            _context(engine, connection, gateway=None, tmp_path=tmp_path),
        )

    assert result.status == "succeeded"
    assert result.data["summaries"]["asset_1"]["overall"] == "第二版最新"
    assert result.data["summaries"]["asset_1"]["version"] == 2
    assert "asset_2" in result.data["missing"]
    assert "第二版最新" in result.observation
