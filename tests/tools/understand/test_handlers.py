"""understand.materials handler 集成测试（Spec C §C3，单级草稿模型）。

VLM 一律用脚本化 gateway（喂 provider 形态的 normalized_output，内含动作 JSON 串），
抽帧与 ASR 打桩，不碰真实 ffmpeg/网络。落库改用 storage 仓储直接写，纯工具级、不跨层依赖主循环。
"""

from __future__ import annotations

import asyncio
import json
import re
import threading
import time
from pathlib import Path
from typing import Any

import pytest

from contracts.draft import DraftState
from contracts.provider import ProviderResult
from media import Shot
from providers import VLM_UNDERSTANDING
from providers.gateway import ProviderGatewayResult
from providers.openai_compatible.vlm import DEFAULT_OPENAI_COMPATIBLE_VLM_MODEL
from storage import schema
from storage.db import create_workspace_engine
from storage.repositories import MaterialSummariesRepository, TranscriptsRepository
from storage.repositories._json import dump_json
from tools import ToolExecutionContext
from tools.specs import UnderstandMaterialsInput
from tools.understand import handlers as understand_handlers
from tools.understand.handlers import materials
from tools.understand.subagent import UNDERSTAND_PROMPT_VERSION, TranscribeResult

NOW = "2026-07-06T00:00:00+00:00"
DATA_URI = "data:image/jpeg;base64,ZmFrZQ=="
DEFAULT_MODEL = DEFAULT_OPENAI_COMPATIBLE_VLM_MODEL

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


def _persist_rows(result: Any, engine: Any) -> None:
    """把 ToolResult.data 里的落库行经仓储写入（原 loop._persist_tool_result_data 的等价路径）。"""
    summary_rows = result.data.get("material_summary_rows") or []
    transcript_rows = result.data.get("transcript_rows") or []
    with engine.begin() as connection:
        for row in summary_rows:
            MaterialSummariesRepository(connection).insert(dict(row))
        for row in transcript_rows:
            TranscriptsRepository(connection).insert(dict(row))


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
                capability=VLM_UNDERSTANDING,
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
                capability=VLM_UNDERSTANDING,
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
                capability=VLM_UNDERSTANDING,
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
            schema.drafts.insert().values(
                draft_id="draft_1",
                name="Draft",
                state_version=0,
                status="active",
                defaults="{}",
                running_jobs="[]",
                brief=dump_json({"goal": "test", "confirmed_facts": []}),
                timeline_validated=False,
                rough_cut_approved=False,
                scratch_memory="{}",
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
                    index_json=dump_json(
                        spec.get("index_json", {"duration_sec": 12.0, "shots": []})
                    ),
                    understanding_status="none",
                )
            )
            connection.execute(
                schema.draft_asset_links.insert().values(
                    draft_id="draft_1",
                    asset_id=asset_id,
                    linked_at=NOW,
                    note="",
                    rel_dir=None,
                )
            )
    return engine


def _seed_summary(
    engine: Any,
    asset_id: str,
    *,
    version: int,
    overall: str,
    model: str = DEFAULT_MODEL,
    fingerprint: str | None = None,
    prompt_version: str | None = None,
) -> None:
    summary = {
        "asset_id": asset_id,
        "version": version,
        "focus": None,
        "semantic_role": "footage",
        "overall": overall,
        "language": "zh",
        "segments": [],
        "generated_at": NOW,
        "model": model,
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
                "model": model,
                "fingerprint": fingerprint,
                "prompt_version": prompt_version,
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
    sink: Any | None = None,
) -> ToolExecutionContext:
    metadata: dict[str, Any] = {"workspace_path": str(tmp_path)}
    if gateway is not None:
        metadata["provider_gateway"] = gateway
    if progress is not None:
        metadata["turn_progress"] = progress
    if sink is not None:
        metadata["partial_result_sink"] = sink
    return ToolExecutionContext(
        tool_call_id="tc_understand",
        turn_id="turn_1",
        draft_state=_draft_state(),
        readonly_connection=connection,
        metadata=metadata,
    )


def _draft_state() -> DraftState:
    return DraftState.model_validate(
        {
            "draft_id": "draft_1",
            "name": "Draft",
            "brief": {"goal": "test", "confirmed_facts": []},
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
    assert all(event.get("draft_id") == "draft_1" for event in result.events)

    _persist_rows(result, engine)
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
                    "shots": [],
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

    _persist_rows(result, engine)
    with engine.connect() as connection:
        transcripts = TranscriptsRepository(connection).list_for_asset("asset_1")
    assert len(transcripts) == 1
    assert transcripts[0]["utterances"][0]["text"] == "你好"


class RecordingSink:
    """等价 loop 注入的 partial_result_sink：逐批落库并记录 rows/events。"""

    def __init__(self, engine: Any) -> None:
        self._engine = engine
        self.events: list[dict[str, Any]] = []
        self.rows_batches: list[dict[str, Any]] = []

    def __call__(self, rows: dict[str, Any], events: list[dict[str, Any]]) -> None:
        self.rows_batches.append(dict(rows))
        self.events.extend(events)
        with self._engine.begin() as connection:
            for row in rows.get("material_summary_rows", []):
                MaterialSummariesRepository(connection).insert(dict(row))
            for row in rows.get("transcript_rows", []):
                TranscriptsRepository(connection).insert(dict(row))

    def event_types_for(self, asset_id: str) -> list[str]:
        return [e["event"] for e in self.events if e.get("asset_id") == asset_id]

    @property
    def started_assets(self) -> list[str]:
        return [
            e["asset_id"] for e in self.events if e.get("event") == "MaterialUnderstandingStarted"
        ]


class GatedVlmGateway:
    """slow_asset 的 VLM 调用先等一个外部 gate 事件再 emit，其它素材立即 emit。"""

    def __init__(self, gate: asyncio.Event, slow_asset: str) -> None:
        self.gate = gate
        self.slow_asset = slow_asset
        self.calls: list[str] = []

    async def call(self, request: Any) -> ProviderGatewayResult:
        asset_id = _asset_from_messages(request.payload["messages"])
        self.calls.append(asset_id)
        if asset_id == self.slow_asset:
            await self.gate.wait()
        return ProviderGatewayResult(
            result=ProviderResult(
                provider_id="mock_vlm",
                capability=VLM_UNDERSTANDING,
                request_id=request.request_id,
                model="mock",
                latency_ms=1,
                normalized_output={"content": json.dumps(_emit())},
            )
        )


async def _wait_until(predicate: Any, *, timeout: float = 1.0) -> None:
    loop = asyncio.get_event_loop()
    deadline = loop.time() + timeout
    while not predicate():
        if loop.time() > deadline:
            raise AssertionError("等待条件超时")
        await asyncio.sleep(0.005)


async def test_incremental_sink_persists_first_asset_before_slow_one(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr(understand_handlers, "extract_frame_data_uri", lambda _p, _s: DATA_URI)
    engine = _engine(tmp_path, [{"asset_id": "asset_fast"}, {"asset_id": "asset_slow"}])
    gate = asyncio.Event()
    gateway = GatedVlmGateway(gate, slow_asset="asset_slow")
    sink = RecordingSink(engine)
    with engine.connect() as connection:
        task = asyncio.ensure_future(
            materials(
                UnderstandMaterialsInput(asset_ids=["asset_fast", "asset_slow"]),
                _context(engine, connection, gateway=gateway, tmp_path=tmp_path, sink=sink),
            )
        )
        # 第 1 个素材完成即经 sink 提交，慢素材仍被 gate 卡住、尚未提交。
        await _wait_until(lambda: "asset_fast" in sink.started_assets)
        assert "asset_slow" not in sink.started_assets
        assert sink.event_types_for("asset_fast") == [
            "MaterialUnderstandingStarted",
            "MaterialUnderstandingCompleted",
        ]
        gate.set()
        result = await task

    assert "asset_slow" in sink.started_assets
    # 已增量提交的产物不再进最终 ToolResult（避免 loop 双写）。
    assert "material_summary_rows" not in result.data
    assert result.events == []
    with engine.connect() as connection:
        assert MaterialSummariesRepository(connection).latest_ready("asset_fast") is not None


async def test_failure_isolation_sinks_failure_code(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr(understand_handlers, "extract_frame_data_uri", lambda _p, _s: DATA_URI)
    engine = _engine(tmp_path, [{"asset_id": "ok"}, {"asset_id": "bad"}])
    gateway = ScriptedVlmGateway(
        {"ok": [_emit()], "bad": [{"foo": "x"}, {"foo": "x"}, {"foo": "x"}]}
    )
    sink = RecordingSink(engine)
    with engine.connect() as connection:
        result = await materials(
            UnderstandMaterialsInput(asset_ids=["ok", "bad"]),
            _context(engine, connection, gateway=gateway, tmp_path=tmp_path, sink=sink),
        )

    assert sink.event_types_for("ok") == [
        "MaterialUnderstandingStarted",
        "MaterialUnderstandingCompleted",
    ]
    assert sink.event_types_for("bad") == [
        "MaterialUnderstandingStarted",
        "MaterialUnderstandingFailed",
    ]
    failed = next(e for e in sink.events if e["event"] == "MaterialUnderstandingFailed")
    assert failed["payload"]["failure_code"] == "schema_invalid"
    assert result.data["results"]["bad"]["failure_code"] == "schema_invalid"
    # 失败者不阻塞成功者：成功素材照常落库。
    with engine.connect() as connection:
        assert MaterialSummariesRepository(connection).latest_ready("ok") is not None


async def test_cache_fingerprint_mismatch_reruns(tmp_path: Path) -> None:
    engine = _engine(tmp_path, [{"asset_id": "asset_1"}])
    _seed_summary(
        engine,
        "asset_1",
        version=1,
        overall="旧摘要",
        fingerprint="999:999",
        prompt_version=UNDERSTAND_PROMPT_VERSION,
    )
    gateway = ScriptedVlmGateway({"asset_1": [_emit()]})
    with engine.connect() as connection:
        result = await materials(
            UnderstandMaterialsInput(asset_ids=["asset_1"]),
            _context(engine, connection, gateway=gateway, tmp_path=tmp_path),
        )

    assert gateway.calls == ["asset_1"]  # fingerprint 不匹配 → 重新理解
    row = result.data["material_summary_rows"][0]
    assert row["version"] == 2
    # 新行带当前 fingerprint（{size}:{mtime}=11:1）与 prompt_version。
    assert row["fingerprint"] == "11:1"
    assert row["prompt_version"] == UNDERSTAND_PROMPT_VERSION


async def test_cache_null_history_hits(tmp_path: Path) -> None:
    engine = _engine(tmp_path, [{"asset_id": "asset_1"}])
    # 历史行 fingerprint/prompt_version 为 NULL：视为命中，不惩罚存量摘要。
    _seed_summary(engine, "asset_1", version=1, overall="城市天际线")
    gateway = ScriptedVlmGateway({"asset_1": [_emit()]})
    with engine.connect() as connection:
        result = await materials(
            UnderstandMaterialsInput(asset_ids=["asset_1"]),
            _context(engine, connection, gateway=gateway, tmp_path=tmp_path),
        )

    assert gateway.calls == []
    assert result.data["results"]["asset_1"]["status"] == "cached"


async def test_cache_model_mismatch_reruns(tmp_path: Path) -> None:
    engine = _engine(tmp_path, [{"asset_id": "asset_1"}])
    _seed_summary(
        engine,
        "asset_1",
        version=1,
        overall="旧摘要",
        model="some-old-model",
        fingerprint="11:1",
        prompt_version=UNDERSTAND_PROMPT_VERSION,
    )
    gateway = ScriptedVlmGateway({"asset_1": [_emit()]})
    with engine.connect() as connection:
        result = await materials(
            UnderstandMaterialsInput(asset_ids=["asset_1"]),
            _context(engine, connection, gateway=gateway, tmp_path=tmp_path),
        )

    assert gateway.calls == ["asset_1"]  # 模型不匹配 → 重新理解
    assert result.data["material_summary_rows"][0]["version"] == 2


async def test_cache_all_keys_match_hits(tmp_path: Path) -> None:
    engine = _engine(tmp_path, [{"asset_id": "asset_1"}])
    _seed_summary(
        engine,
        "asset_1",
        version=1,
        overall="城市天际线",
        fingerprint="11:1",
        prompt_version=UNDERSTAND_PROMPT_VERSION,
    )
    gateway = ScriptedVlmGateway({"asset_1": [_emit()]})
    with engine.connect() as connection:
        result = await materials(
            UnderstandMaterialsInput(asset_ids=["asset_1"]),
            _context(engine, connection, gateway=gateway, tmp_path=tmp_path),
        )

    assert gateway.calls == []
    assert result.data["results"]["asset_1"]["status"] == "cached"


async def test_shots_backfilled_on_demand_feeds_subagent_and_sink(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr(understand_handlers, "extract_frame_data_uri", lambda _p, _s: DATA_URI)
    calls: list[Any] = []

    def _fake_split(path: Any, *, paths: Any = None) -> tuple[Shot, ...]:
        calls.append(path)
        return (Shot(shot_id="shot_0001", start_sec=0.0, end_sec=6.0),)

    monkeypatch.setattr(understand_handlers, "split_shots", _fake_split)
    engine = _engine(tmp_path, [{"asset_id": "asset_1", "index_json": {"duration_sec": 12.0}}])
    gateway = ScriptedVlmGateway({"asset_1": [_emit()]})
    sink = RecordingSink(engine)
    with engine.connect() as connection:
        result = await materials(
            UnderstandMaterialsInput(asset_ids=["asset_1"]),
            _context(engine, connection, gateway=gateway, tmp_path=tmp_path, sink=sink),
        )

    assert result.status == "succeeded"
    assert len(calls) == 1  # 无 shots → split_shots 被调
    index_events = [e for e in sink.events if e["event"] == "AssetIndexReady"]
    assert len(index_events) == 1
    assert index_events[0]["payload"]["index_json"]["shots"][0]["start_sec"] == 0.0
    # 分镜结果喂给了当前子代理的 index 摘要（prompt 里出现"分镜"）。
    assert any("分镜" in prompt for prompt in gateway.prompts)


async def test_slow_shots_backfill_does_not_block_fast_asset(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr(understand_handlers, "extract_frame_data_uri", lambda _p, _s: DATA_URI)
    release = threading.Event()

    def _split(path: Any, *, paths: Any = None) -> tuple[Shot, ...]:
        # 慢素材的分镜阻塞在后台线程里，直到测试放行；快素材已有 shots，不会走到这里。
        if "slow" in str(path):
            release.wait(timeout=5)
        return (Shot(shot_id="shot_0001", start_sec=0.0, end_sec=6.0),)

    monkeypatch.setattr(understand_handlers, "split_shots", _split)
    engine = _engine(
        tmp_path,
        [
            {"asset_id": "fast", "index_json": {"duration_sec": 12.0, "shots": []}},
            {"asset_id": "slow", "index_json": {"duration_sec": 12.0}},
        ],
    )
    gateway = ScriptedVlmGateway({"fast": [_emit()], "slow": [_emit()]})
    sink = RecordingSink(engine)
    with engine.connect() as connection:
        task = asyncio.ensure_future(
            materials(
                UnderstandMaterialsInput(asset_ids=["fast", "slow"]),
                _context(engine, connection, gateway=gateway, tmp_path=tmp_path, sink=sink),
            )
        )
        # 快素材（无需分镜）先完成落库，慢素材仍卡在 split_shots、尚未开始理解。
        await _wait_until(lambda: "fast" in sink.started_assets)
        assert "slow" not in sink.started_assets
        release.set()
        await task

    assert "slow" in sink.started_assets


async def test_shots_present_skips_split(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(understand_handlers, "extract_frame_data_uri", lambda _p, _s: DATA_URI)
    called: list[int] = []

    def _fake_split(_path: Any, *, paths: Any = None) -> tuple[Shot, ...]:
        called.append(1)
        return ()

    monkeypatch.setattr(understand_handlers, "split_shots", _fake_split)
    engine = _engine(
        tmp_path, [{"asset_id": "asset_1", "index_json": {"duration_sec": 12.0, "shots": []}}]
    )
    gateway = ScriptedVlmGateway({"asset_1": [_emit()]})
    with engine.connect() as connection:
        await materials(
            UnderstandMaterialsInput(asset_ids=["asset_1"]),
            _context(engine, connection, gateway=gateway, tmp_path=tmp_path),
        )

    assert called == []  # 已有 shots 键 → 不再计算


async def test_shots_split_failure_keeps_asset_ready(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr(understand_handlers, "extract_frame_data_uri", lambda _p, _s: DATA_URI)

    def _boom(_path: Any, *, paths: Any = None) -> tuple[Shot, ...]:
        raise RuntimeError("坏视频")

    monkeypatch.setattr(understand_handlers, "split_shots", _boom)
    engine = _engine(tmp_path, [{"asset_id": "asset_1", "index_json": {"duration_sec": 12.0}}])
    gateway = ScriptedVlmGateway({"asset_1": [_emit()]})
    with engine.connect() as connection:
        result = await materials(
            UnderstandMaterialsInput(asset_ids=["asset_1"]),
            _context(engine, connection, gateway=gateway, tmp_path=tmp_path),
        )

    # 分镜失败仅降级，不让整个素材理解失败。
    assert result.data["results"]["asset_1"]["status"] == "ready"


async def test_shots_backfill_without_sink_batches_event(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr(understand_handlers, "extract_frame_data_uri", lambda _p, _s: DATA_URI)
    monkeypatch.setattr(
        understand_handlers,
        "split_shots",
        lambda _path, *, paths=None: (Shot(shot_id="shot_0001", start_sec=0.0, end_sec=6.0),),
    )
    engine = _engine(tmp_path, [{"asset_id": "asset_1", "index_json": {"duration_sec": 12.0}}])
    gateway = ScriptedVlmGateway({"asset_1": [_emit()]})
    with engine.connect() as connection:
        result = await materials(
            UnderstandMaterialsInput(asset_ids=["asset_1"]),
            _context(engine, connection, gateway=gateway, tmp_path=tmp_path),
        )

    # 无 sink：AssetIndexReady 与理解事件都进最终 ToolResult.events 批量回填。
    event_types = [e["event"] for e in result.events]
    assert "AssetIndexReady" in event_types
    assert event_types[-2:] == [
        "MaterialUnderstandingStarted",
        "MaterialUnderstandingCompleted",
    ]


async def test_shots_backfill_timeout_marks_asset_failed(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr(understand_handlers, "extract_frame_data_uri", lambda _p, _s: DATA_URI)
    monkeypatch.setattr(understand_handlers, "_timeout_seconds", lambda: 0.05)

    def _slow_split(_path: Any, *, paths: Any = None) -> tuple[Shot, ...]:
        # 分镜回填阻塞超过单素材超时：回填现纳入 wait_for，应触发 timeout 失败而非挂死回合。
        time.sleep(0.5)
        return (Shot(shot_id="shot_0001", start_sec=0.0, end_sec=6.0),)

    monkeypatch.setattr(understand_handlers, "split_shots", _slow_split)
    engine = _engine(tmp_path, [{"asset_id": "asset_1", "index_json": {"duration_sec": 12.0}}])
    gateway = ScriptedVlmGateway({"asset_1": [_emit()]})
    with engine.connect() as connection:
        result = await materials(
            UnderstandMaterialsInput(asset_ids=["asset_1"]),
            _context(engine, connection, gateway=gateway, tmp_path=tmp_path),
        )

    assert result.status == "succeeded"
    assert result.data["results"]["asset_1"]["status"] == "failed"
    assert "超时" in result.data["results"]["asset_1"]["reason"]


async def test_consumer_error_cancels_inflight_tasks(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr(understand_handlers, "extract_frame_data_uri", lambda _p, _s: DATA_URI)
    engine = _engine(tmp_path, [{"asset_id": "fast"}, {"asset_id": "slow"}])
    gate = asyncio.Event()  # 从不 set：slow 的 VLM 卡住，直到被取消。
    gateway = GatedVlmGateway(gate, slow_asset="slow")

    class RaisingSink:
        def __init__(self) -> None:
            self.calls: list[list[str | None]] = []

        def __call__(self, rows: dict[str, Any], events: list[dict[str, Any]]) -> None:
            self.calls.append([e.get("asset_id") for e in events])
            raise RuntimeError("sink boom")

    sink = RaisingSink()
    with engine.connect() as connection, pytest.raises(RuntimeError, match="sink boom"):
        await materials(
            UnderstandMaterialsInput(asset_ids=["fast", "slow"]),
            _context(engine, connection, gateway=gateway, tmp_path=tmp_path, sink=sink),
        )

    # 第一个 outcome(fast) 落 sink 即抛错；finally 取消仍在飞的 slow 任务——slow 从未走到 sink，
    # 未继续经 sink 脏写库（否则会有第二次 sink 调用）。
    assert sink.calls == [["fast", "fast"]]


async def test_transcript_id_unique_across_reunderstanding(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
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
    engine = _engine(
        tmp_path,
        [
            {
                "asset_id": "asset_1",
                "index_json": {
                    "duration_sec": 90.0,
                    "shots": [],
                    "vad": [{"start_ms": 0, "end_ms": 800, "kind": "speech"}],
                },
            }
        ],
    )
    # 同一 turn 对同素材二次理解（focus 深挖绕过缓存）各产一条转写行：
    # transcript_id 加随机段才不撞主键。
    for _ in range(2):
        gateway = ScriptedVlmGateway(
            {"asset_1": [{"action": "transcribe", "start_s": 0.0, "end_s": 90.0}, _emit()]}
        )
        with engine.connect() as connection:
            result = await materials(
                UnderstandMaterialsInput(asset_ids=["asset_1"], focus="口播是否清晰"),
                _context(engine, connection, gateway=gateway, tmp_path=tmp_path),
            )
        _persist_rows(result, engine)

    with engine.connect() as connection:
        transcripts = TranscriptsRepository(connection).list_for_asset("asset_1")
    assert len(transcripts) == 2


async def test_null_index_json_backfills_shots_on_first_understanding(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr(understand_handlers, "extract_frame_data_uri", lambda _p, _s: DATA_URI)
    monkeypatch.setattr(
        understand_handlers,
        "split_shots",
        lambda _path, *, paths=None: (Shot(shot_id="shot_0001", start_sec=0.0, end_sec=6.0),),
    )
    # index_json 为 None（index job 还没跑就被理解）也要回填 shots，否则理解成功后缓存永久命中、
    # shots 从此没有计算机会。事件 payload 只带 shots（交 reducer 按键合并、不带缩略图/整份快照）。
    engine = _engine(tmp_path, [{"asset_id": "asset_1", "index_json": None}])
    gateway = ScriptedVlmGateway({"asset_1": [_emit()]})
    with engine.connect() as connection:
        result = await materials(
            UnderstandMaterialsInput(asset_ids=["asset_1"]),
            _context(engine, connection, gateway=gateway, tmp_path=tmp_path),
        )

    assert result.status == "succeeded"
    index_event = next(e for e in result.events if e["event"] == "AssetIndexReady")
    assert index_event["payload"]["index_json"] == {
        "shots": [{"shot_id": "shot_0001", "start_sec": 0.0, "end_sec": 6.0}]
    }
    assert "thumbnail_object_hash" not in index_event["payload"]
