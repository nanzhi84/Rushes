from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import pytest
from sqlalchemy.engine import Connection

from contracts.case import CaseState, CutPlan
from contracts.provider import ProviderResult
from providers import LLM_CHAT
from providers.gateway import ProviderGatewayResult
from storage import schema
from storage.db import create_workspace_engine
from storage.repositories._json import dump_json
from tools import ToolExecutionContext
from tools.content import create_plan, revise_plan
from tools.specs import ContentCreatePlanInput, ContentRevisePlanInput

NOW = "2026-07-05T00:00:00+00:00"


def _llm_output(
    *,
    storyline: str = "一条清晰的故事线",
    brief: str = "风景镜头",
) -> dict[str, Any]:
    return {
        "content_plan": {
            "schema": "ContentPlan.v1",
            "storyline": storyline,
            "sections": [{"section_id": "section_001", "intent": "开场", "notes": "铺垫"}],
            "status": "draft",
        },
        "cut_plan": {
            "schema": "CutPlan.v1",
            "slots": [
                {
                    "slot_id": "slot_001",
                    "brief": brief,
                    "target_duration_sec": [4.0, 6.0],
                }
            ],
            "removed_ranges": [],
            "total_target_duration_sec": 10.0,
        },
    }


def test_create_plan_silent_emits_content_and_cut_plan_with_mock_gateway(
    tmp_path: Path,
) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_clip(connection, "asset_1", "clip_1", "溪流慢镜头", quality_score=0.9)
        result = create_plan(
            ContentCreatePlanInput(target_duration_sec=18.0),
            _context(
                connection,
                metadata={
                    "provider_gateway": _LlmGateway(
                        _llm_output(
                            storyline="从溪流开场，过渡到远景收束。",
                            brief="溪流特写",
                        )
                    )
                },
            ),
        )

    assert result.status == "succeeded"
    assert [event["event"] for event in result.events] == [
        "ContentPlanUpdated",
        "CutPlanUpdated",
    ]
    assert (
        result.events[0]["payload"]["content_plan"]["storyline"] == "从溪流开场，过渡到远景收束。"
    )
    cut_plan = result.events[1]["payload"]["cut_plan"]
    CutPlan.model_validate(cut_plan)
    assert cut_plan["total_target_duration_sec"] == 18.0
    assert all(slot["brief"] for slot in cut_plan["slots"])


def test_create_plan_tts_only_emits_content_plan(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_clip(connection, "asset_1", "clip_1", "人物开场", quality_score=0.8)
        result = create_plan(
            ContentCreatePlanInput(target_duration_sec=12.0),
            _context(
                connection,
                case_state=_case_state(audio_mode="tts"),
                metadata={"provider_gateway": _LlmGateway(_llm_output())},
            ),
        )

    assert result.status == "succeeded"
    assert [event["event"] for event in result.events] == ["ContentPlanUpdated"]
    assert result.data["cut_plan"] is None


def test_create_plan_without_gateway_falls_back_to_one_slot_per_asset(
    tmp_path: Path,
) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_clip(connection, "asset_1", "clip_1", "瀑布远景", quality_score=0.7)
        _seed_clip(connection, "asset_2", "clip_2", "森林横移", quality_score=0.9)
        result = create_plan(
            ContentCreatePlanInput(target_duration_sec=20.0, slot_count=5),
            _context(connection),
        )

    assert result.status == "succeeded"
    assert result.data["source"] == "fallback"
    assert [event["event"] for event in result.events] == [
        "ContentPlanUpdated",
        "CutPlanUpdated",
    ]
    cut_plan = result.events[1]["payload"]["cut_plan"]
    assert len(cut_plan["slots"]) == 2
    # 离线标注投影已下线：回退 brief 现在取素材文件名（Spec C / Task 9 前）
    assert [slot["brief"] for slot in cut_plan["slots"]] == ["asset_1.mp4", "asset_2.mp4"]
    for slot in cut_plan["slots"]:
        low, high = slot["target_duration_sec"]
        assert (low + high) / 2 == pytest.approx(10.0)


@pytest.mark.parametrize(
    ("output", "storyline", "brief"),
    [
        (
            {"content": json.dumps(_llm_output(storyline="包裹 JSON", brief="湖面晨雾"))},
            "包裹 JSON",
            "湖面晨雾",
        ),
        (
            {
                "tool_call": {
                    "function": {
                        "arguments": json.dumps(
                            _llm_output(storyline="工具调用 JSON", brief="山谷云海")
                        )
                    }
                }
            },
            "工具调用 JSON",
            "山谷云海",
        ),
    ],
)
def test_create_plan_parses_llm_content_string_and_tool_call_shapes(
    tmp_path: Path,
    output: dict[str, Any],
    storyline: str,
    brief: str,
) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_clip(connection, "asset_1", "clip_1", "备用素材", quality_score=0.6)
        result = create_plan(
            ContentCreatePlanInput(target_duration_sec=9.0),
            _context(connection, metadata={"provider_gateway": _LlmGateway(output)}),
        )

    assert result.status == "succeeded"
    assert result.data["source"] == "llm"
    assert result.events[0]["payload"]["content_plan"]["storyline"] == storyline
    assert result.events[1]["payload"]["cut_plan"]["slots"][0]["brief"] == brief


def test_create_plan_garbage_llm_output_falls_back(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_clip(connection, "asset_1", "clip_1", "可用风景", quality_score=0.6)
        result = create_plan(
            ContentCreatePlanInput(target_duration_sec=8.0),
            _context(
                connection,
                metadata={"provider_gateway": _LlmGateway({"content": "坏输出"})},
            ),
        )

    assert result.status == "succeeded"
    assert result.data["source"] == "fallback"
    assert result.events[1]["payload"]["cut_plan"]["slots"][0]["brief"] == "asset_1.mp4"


def test_revise_plan_updates_existing_plan_and_cut_plan(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    case_state = _case_state(content_plan=_existing_content_plan(), cut_plan=_existing_cut_plan())
    with engine.begin() as connection:
        _seed_clip(connection, "asset_1", "clip_1", "旧素材", quality_score=0.5)
        result = revise_plan(
            ContentRevisePlanInput(revision_hint="加强结尾"),
            _context(
                connection,
                case_state=case_state,
                metadata={
                    "provider_gateway": _LlmGateway(
                        _llm_output(storyline="修订后的故事线", brief="新的结尾远景")
                    )
                },
            ),
        )

    assert result.status == "succeeded"
    assert result.data["source"] == "llm"
    assert [event["event"] for event in result.events] == [
        "ContentPlanUpdated",
        "CutPlanUpdated",
    ]
    assert result.events[0]["payload"]["content_plan"]["storyline"] == "修订后的故事线"
    CutPlan.model_validate(result.events[1]["payload"]["cut_plan"])


def test_revise_plan_requires_existing_content_plan(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        result = revise_plan(
            ContentRevisePlanInput(revision_hint="改一下"),
            _context(connection),
        )

    assert result.status == "failed"
    assert result.error is not None
    assert result.error.error_code == "missing_content_plan"


def test_content_tools_guard_missing_case_and_connection(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        bare = ToolExecutionContext(
            tool_call_id="tc_1",
            turn_id="turn_1",
            readonly_connection=connection,
        )
        no_connection = _context(
            None,
            case_state=_case_state(content_plan=_existing_content_plan()),
        )

        create_missing_case = create_plan(ContentCreatePlanInput(), bare)
        revise_missing_case = revise_plan(ContentRevisePlanInput(revision_hint="x"), bare)
        create_missing_connection = create_plan(ContentCreatePlanInput(), no_connection)
        revise_missing_connection = revise_plan(
            ContentRevisePlanInput(revision_hint="x"),
            no_connection,
        )

    assert create_missing_case.error is not None
    assert create_missing_case.error.error_code == "missing_case"
    assert revise_missing_case.error is not None
    assert revise_missing_case.error.error_code == "missing_case"
    assert create_missing_connection.error is not None
    assert create_missing_connection.error.error_code == "missing_connection"
    assert revise_missing_connection.error is not None
    assert revise_missing_connection.error.error_code == "missing_connection"


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
                brief=dump_json(
                    {
                        "goal": "做一条安静风景混剪",
                        "target_duration_sec": 15.0,
                        "style_notes": ["舒缓"],
                        "confirmed_facts": [],
                    }
                ),
                selected_asset_ids="[]",
                disabled_asset_ids="[]",
                scratch_memory="{}",
            )
        )
    return engine


def _context(
    connection: Connection | None,
    *,
    case_state: CaseState | None = None,
    metadata: dict[str, Any] | None = None,
) -> ToolExecutionContext:
    return ToolExecutionContext(
        tool_call_id="tc_1",
        turn_id="turn_1",
        case_state=case_state or _case_state(),
        readonly_connection=connection,
        created_at=NOW,
        metadata=metadata or {},
    )


def _case_state(
    *,
    audio_mode: str | None = "silent",
    content_plan: dict[str, Any] | None = None,
    cut_plan: dict[str, Any] | None = None,
) -> CaseState:
    return CaseState.model_validate(
        {
            "case_id": "case_1",
            "project_id": "project_1",
            "name": "Case",
            "brief": {
                "goal": "做一条安静风景混剪",
                "target_duration_sec": 15.0,
                "style_notes": ["舒缓"],
                "confirmed_facts": [],
            },
            "content_plan": content_plan,
            "audio_plan": None if audio_mode is None else {"mode": audio_mode},
            "cut_plan": cut_plan,
            "selected_asset_ids": [],
            "disabled_asset_ids": [],
            "scratch_memory": {},
        }
    )


def _seed_clip(
    connection: Connection,
    asset_id: str,
    clip_id: str,
    summary: str,
    *,
    quality_score: float,
) -> None:
    annotation_id = f"ann_{asset_id}"
    connection.execute(
        schema.assets.insert().values(
            asset_id=asset_id,
            storage_mode="reference",
            object_hash=None,
            reference_path=f"/tmp/{asset_id}.mp4",
            kind="video",
            source="local_path",
            filename=f"{asset_id}.mp4",
            hash=f"hash_{asset_id}",
            mtime=1,
            size=1,
            probe=dump_json({"duration_sec": 10.0, "fps": 30.0}),
            proxy_object_hash=None,
            ingest_status="indexed",
            usable=True,
            failure=None,
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
    connection.execute(
        schema.annotations_table.insert().values(
            annotation_id=annotation_id,
            asset_id=asset_id,
            schema="AnnotationDocument.v1",
            status="completed",
            document_json=dump_json(
                {
                    "schema": "AnnotationDocument.v1",
                    "annotation_id": annotation_id,
                    "asset_id": asset_id,
                    "asset_kind": "video",
                    "status": "completed",
                    "generator": {"pipeline_version": "annotation.video.v1", "pass": "cheap"},
                    "clips": [],
                    "quality_events": [],
                    "created_at": NOW,
                }
            ),
            created_at=NOW,
            updated_at=NOW,
        )
    )


def _existing_content_plan() -> dict[str, Any]:
    return {
        "schema": "ContentPlan.v1",
        "storyline": "旧故事线",
        "sections": [{"section_id": "section_001", "intent": "旧段落", "notes": "旧备注"}],
        "status": "draft",
    }


def _existing_cut_plan() -> dict[str, Any]:
    return {
        "schema": "CutPlan.v1",
        "slots": [
            {
                "slot_id": "slot_001",
                "brief": "旧画面",
                "target_duration_sec": [3.0, 5.0],
            }
        ],
        "removed_ranges": [],
        "total_target_duration_sec": 8.0,
    }


class _LlmGateway:
    def __init__(self, output: dict[str, Any]) -> None:
        self.output = output

    async def call(self, request: Any, **_kwargs: Any) -> ProviderGatewayResult:
        assert request.capability == LLM_CHAT
        assert request.request_id is not None
        request_id = request.request_id
        return ProviderGatewayResult(
            result=ProviderResult(
                provider_id="mock_llm",
                capability=LLM_CHAT,
                request_id=request_id,
                model="mock",
                latency_ms=1,
                normalized_output=self.output,
            ),
            events=(
                {
                    "event": "ProviderCallRecorded",
                    "provider_call_id": request_id,
                    "payload": {"status": "succeeded"},
                },
            ),
        )


def test_llm_output_parsing_variants() -> None:
    """_plan_from_llm_output 的各回退形态（M9 覆盖补充）。"""
    from tools.content.handlers import (
        _duration_window_from_value,
        _json_mapping,
        _mapping_from_output,
        _mapping_from_tool_call,
        _sections_from_value,
    )

    # content 字符串包 JSON
    parsed = _mapping_from_output({"content": '{"storyline": "s", "slots": []}'})
    assert parsed is not None and parsed["storyline"] == "s"
    # tool_call arguments（直接 mapping 与 JSON 字符串两种）
    assert _mapping_from_tool_call({"arguments": {"storyline": "a", "slots": []}}) is not None
    assert (
        _mapping_from_tool_call({"function": {"arguments": '{"storyline": "b", "slots": []}'}})
        is not None
    )
    assert _mapping_from_tool_call({"arguments": "not json"}) is None
    # tool_calls 列表形态
    listed = _mapping_from_output(
        {"tool_calls": [{"function": {"arguments": '{"storyline": "c", "sections": []}'}}]}
    )
    assert listed is not None and listed["storyline"] == "c"
    # 垃圾输入
    assert _mapping_from_output({"unrelated": 1}) is None
    assert _json_mapping("not json") is None
    assert _json_mapping('["list"]') is None

    # sections 的字符串/字典混合形态
    sections = _sections_from_value(["开场", {"section_id": "s2", "intent": "过渡"}, 42])
    assert len(sections) == 2
    assert _sections_from_value(None) == []
    assert _sections_from_value("单条描述") == []  # 纯字符串不是序列输入

    # 时长窗口的各形态
    assert _duration_window_from_value([2, 5]) == (2.0, 5.0)
    assert _duration_window_from_value(3.0) is not None
    assert _duration_window_from_value("bad") is None
    assert _duration_window_from_value([1]) is None


def test_fallback_helpers_cover_edge_branches() -> None:
    """降级路径的细分支（M9 覆盖补充）。"""
    from tools.content.handlers import (
        _AssetSummary,
        _fallback_plan,
        _fallback_storyline,
        _observation,
        _summary_text,
        _target_duration,
    )

    state = _case_state(audio_mode="silent")
    assets = [
        _AssetSummary(asset_id="a1", filename="海边.mov", summary="日落海岸", quality_score=0.9),
        _AssetSummary(asset_id="a2", filename="", summary="", quality_score=None),
    ]

    assert _fallback_storyline(state, assets, "  自定义故事线  ") == "自定义故事线"
    auto = _fallback_storyline(state, assets, None)
    assert "日落海岸" in auto

    assert _summary_text(assets[0]) == "日落海岸"
    assert _summary_text(assets[1]) == "a2"  # summary 与 filename 均空回退 asset_id

    assert _target_duration(12.0, state) == 12.0
    assert _target_duration(None, state) >= 0.1  # brief/cut_plan 均无 → 默认值

    # 既有 plan 保留（revise 降级路径）
    existing_content = {"storyline": "旧故事线", "sections": []}
    existing_cut = {
        "schema": "CutPlan.v1",
        "slots": [
            {"slot_id": "s1", "brief": "海", "target_duration_sec": [1.0, 3.0]},
        ],
        "total_target_duration_sec": 2.0,
    }
    plan = _fallback_plan(
        state,
        assets,
        target_duration=30.0,
        storyline_hint=None,
        slot_count=None,
        existing_content_plan=existing_content,
        existing_cut_plan=existing_cut,
    )
    assert plan.content_plan["storyline"] == "旧故事线"
    assert plan.content_plan["schema"] == "ContentPlan.v1"
    assert plan.cut_plan["slots"][0]["slot_id"] == "s1"
    text = _observation(plan)
    assert "1 个镜头槽" in text
    assert "旧故事线" in text


def test_call_llm_and_async_bridge_edge_paths(tmp_path: Path) -> None:
    """_call_llm 异常吞掉与 _run_async_sync 线程桥（M9 覆盖补充）。"""
    import asyncio

    from tools.content.handlers import _call_llm, _run_async_sync

    class _BoomGateway:
        async def call(self, request, *, provider_id=None):
            raise RuntimeError("boom")

    engine = _engine(tmp_path)
    with engine.connect() as connection:
        context = _context(connection, case_state=_case_state(audio_mode="silent"))
        context = replace_metadata(context, {"provider_gateway": _BoomGateway()})
        assert _call_llm(context, request_id="r1", payload={}) is None

    async def _inner() -> str:
        return "线程桥结果"

    async def _outer() -> str:
        # 已有事件循环时走线程桥分支
        return _run_async_sync(_inner())

    assert asyncio.run(_outer()) == "线程桥结果"

    async def _raiser() -> None:
        raise ValueError("bridge error")

    async def _outer_error() -> None:
        _run_async_sync(_raiser())

    import pytest as _pytest

    with _pytest.raises(ValueError, match="bridge error"):
        asyncio.run(_outer_error())


def replace_metadata(context, metadata):
    from dataclasses import replace

    return replace(context, metadata=metadata)


def test_revise_plan_uses_llm_output_when_available(tmp_path: Path) -> None:
    """revise 的 LLM 成功路径：修订结果生效并保持事件组合（M9 覆盖补充）。"""
    from tools.content.handlers import revise_plan
    from tools.specs import ContentRevisePlanInput

    llm_payload = {
        "content_plan": {
            "schema": "ContentPlan.v1",
            "storyline": "修订后的故事线",
            "sections": [{"section_id": "s1", "intent": "开场", "notes": "海岸日落"}],
            "status": "draft",
        },
        "cut_plan": {
            "schema": "CutPlan.v1",
            "slots": [{"slot_id": "s1", "brief": "海岸日落", "target_duration_sec": [2, 4]}],
            "removed_ranges": [],
            "total_target_duration_sec": 3.0,
        },
    }

    class _Gateway:
        async def call(self, request, *, provider_id=None):
            import json as _json
            from types import SimpleNamespace

            return SimpleNamespace(
                result=SimpleNamespace(
                    error=None,
                    normalized_output={"content": _json.dumps(llm_payload, ensure_ascii=False)},
                )
            )

    engine = _engine(tmp_path)
    with engine.begin() as connection:
        state = _case_state(
            audio_mode="silent",
            content_plan={"schema": "ContentPlan.v1", "storyline": "旧", "sections": []},
        )
        context = replace_metadata(
            _context(connection, case_state=state), {"provider_gateway": _Gateway()}
        )
        result = revise_plan(ContentRevisePlanInput(revision_hint="更明快"), context)

    assert result.status == "succeeded"
    assert "修订后的故事线" in result.observation
    events = [event["event"] for event in result.events]
    assert events == ["ContentPlanUpdated", "CutPlanUpdated"]


def test_ask_user_defaults_generic_reduce_target_to_scratch_memory(tmp_path: Path) -> None:
    """ask_user 未声明 reduce_target 时默认 scratch_memory（M9 500 修复回归）。"""
    from tools.interaction.handlers import ask_user
    from tools.specs import AskUserInput

    engine = _engine(tmp_path)
    with engine.connect() as connection:
        context = _context(connection, case_state=_case_state(audio_mode="silent"))
        result = ask_user(
            AskUserInput(
                question="用哪种节奏？",
                decision_type="generic",
                options=[{"option_id": "fast", "label": "明快"}],
            ),
            context,
        )
    decision_event = result.events[0]
    assert decision_event["event"] == "DecisionCreated"
    interaction = result.data["interaction"]
    assert interaction["metadata"]["reduce_target"] == "scratch_memory"


def test_create_plan_observation_hints_when_audio_plan_missing(tmp_path: Path) -> None:
    """audio_plan 未定：不产 cut_plan 且 observation 指路（M9 死循环修复回归）。"""
    from tools.content.handlers import create_plan
    from tools.specs import ContentCreatePlanInput

    engine = _engine(tmp_path)
    with engine.connect() as connection:
        state = _case_state(audio_mode=None)
        result = create_plan(ContentCreatePlanInput(), _context(connection, case_state=state))
    assert result.status == "succeeded"
    assert [event["event"] for event in result.events] == ["ContentPlanUpdated"]
    assert "audio_plan 尚未确定" in result.observation
    assert "interaction.ask_user" in result.observation


def test_fallback_storyline_when_everything_empty() -> None:
    from tools.content.handlers import _fallback_sections, _fallback_storyline

    state = _case_state(audio_mode="silent")
    text = _fallback_storyline(state, [], None)
    assert text  # goal 存在时用 goal 组装
    sections = _fallback_sections(text, [], None)
    assert sections[0]["section_id"] == "section_001"
