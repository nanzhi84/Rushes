#!/usr/bin/env python3
"""Manual real-planner evaluation for Issue #55's understanding cost gradient.

This script intentionally stays out of CI. It calls the configured real LLM planner, while
all VLM/tool observations are deterministic mocks, so it measures planner tool selection
without uploading media or paying for visual understanding.
"""

from __future__ import annotations

import argparse
import asyncio
import json
import os
import subprocess
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from scripts.e2e_paths.client import load_dotenv

from agent_harness.context_builder import (
    AssetDigestRow,
    ContextBuilder,
    ContextBuildInput,
    ContextMessage,
)
from agent_harness.loop import MappingPlannerAdapter
from agent_harness.policy_gate import PolicyContext, PolicyGate
from contracts.draft import DraftState
from domain.preconditions import DraftArtifactStats, PreconditionContext
from providers import build_openai_compatible_planner
from tools import PATCH_OP_REGISTRY, build_default_tool_registry

DEFAULT_BASE_URL = "https://dashscope.aliyuncs.com/compatible-mode/v1"
DEFAULT_MODEL = "qwen3.7-max"
FIXED_ASSETS = tuple(
    AssetDigestRow(
        asset_id=f"asset_{index:02d}",
        filename=f"课程混剪_{index:02d}.mp4",
        kind="video",
        duration_sec=180.0 + index * 3,
        understanding_status="none",
    )
    for index in range(1, 13)
)
SCAN_CANDIDATES = ("asset_03", "asset_07", "asset_11")


@dataclass(frozen=True, slots=True)
class ScenarioResult:
    name: str
    passed: bool
    calls: tuple[dict[str, Any], ...]
    reasons: tuple[str, ...]


async def main() -> int:
    parser = argparse.ArgumentParser(
        description="用真 planner + mock VLM 手工回归理解成本梯度（不进 CI）。"
    )
    parser.add_argument("--max-steps", type=int, default=6)
    args = parser.parse_args()
    _load_project_dotenv()
    key = os.environ.get("RUSHES_DASHSCOPE_API_KEY") or os.environ.get("RUSHES_LLM_API_KEY")
    if not key:
        raise SystemExit(
            "缺少 RUSHES_DASHSCOPE_API_KEY / RUSHES_LLM_API_KEY（会自动读仓库根 .env）"
        )
    planner = MappingPlannerAdapter(
        build_openai_compatible_planner(
            base_url=os.environ.get("RUSHES_LLM_BASE_URL", DEFAULT_BASE_URL),
            api_key=key,
            model=os.environ.get("RUSHES_LLM_MODEL", DEFAULT_MODEL),
        )
    )
    results = (
        await _run_gradient(planner, max_steps=args.max_steps),
        await _run_point_query(planner, max_steps=args.max_steps),
    )
    print(
        json.dumps(
            {
                "model": os.environ.get("RUSHES_LLM_MODEL", DEFAULT_MODEL),
                "vlm": "mock",
                "results": [
                    {
                        "name": result.name,
                        "passed": result.passed,
                        "calls": list(result.calls),
                        "reasons": list(result.reasons),
                    }
                    for result in results
                ],
            },
            ensure_ascii=False,
            indent=2,
        )
    )
    return 0 if all(result.passed for result in results) else 1


def _load_project_dotenv() -> None:
    """Load this checkout's .env, or the primary checkout's .env from a git worktree."""

    local = Path(__file__).resolve().parents[1] / ".env"
    if local.is_file():
        load_dotenv(local)
        return
    try:
        result = subprocess.run(
            ["git", "rev-parse", "--path-format=absolute", "--git-common-dir"],
            capture_output=True,
            check=True,
            text=True,
            timeout=10,
        )
    except (OSError, subprocess.SubprocessError):
        return
    common_dir = Path(result.stdout.strip())
    primary = common_dir.parent / ".env" if common_dir.name == ".git" else None
    if primary is not None and primary.is_file():
        load_dotenv(primary)


async def _run_gradient(planner: Any, *, max_steps: int) -> ScenarioResult:
    calls = await _drive(
        planner,
        user_message=(
            "先查看这 12 个课程混剪素材的证据，为一条节奏明快的 30 秒预告挑出少量候选；"
            "现在只做素材取证和候选选择，不要开始写时间线。"
        ),
        max_steps=max_steps,
    )
    understand = [call for call in calls if call["tool_name"] == "understand.materials"]
    reasons: list[str] = []
    if not understand:
        reasons.append("没有调用 understand.materials")
    elif understand[0]["arguments"].get("depth") != "scan":
        reasons.append("首次理解调用不是 depth=scan")
    scan_calls = [call for call in understand if call["arguments"].get("depth") == "scan"]
    if len(scan_calls) != 1:
        reasons.append(f"同一批素材 scan 调用了 {len(scan_calls)} 次，预期恰好 1 次")
    scan_index = next(
        (
            index
            for index, call in enumerate(calls)
            if call["tool_name"] == "understand.materials"
            and call["arguments"].get("depth") == "scan"
        ),
        None,
    )
    deep_calls = [call for call in understand if call["arguments"].get("depth") == "deep"]
    if not deep_calls:
        reasons.append("scan 后没有对少量候选调用 deep")
    for index, call in enumerate(calls):
        arguments = call["arguments"]
        if call["tool_name"] != "understand.materials" or arguments.get("depth") != "deep":
            continue
        ids = arguments.get("asset_ids")
        if scan_index is None or index < scan_index:
            reasons.append("deep 出现在 scan 之前")
        if not isinstance(ids, list) or not 1 <= len(ids) <= 3:
            reasons.append("归档 deep 未限制为 1-3 个候选")
        elif not set(ids).issubset(SCAN_CANDIDATES):
            reasons.append("deep 选择了 mock scan 候选之外的素材")
    return ScenarioResult("l0_scan_then_few_deep", not reasons, tuple(calls), tuple(reasons))


async def _run_point_query(planner: Any, *, max_steps: int) -> ScenarioResult:
    calls = await _drive(
        planner,
        user_message="请点查 asset_03 在 02:10 附近的画面是否适合做开头，只需回答这个窄问题。",
        max_steps=max_steps,
    )
    understand = [call for call in calls if call["tool_name"] == "understand.materials"]
    reasons: list[str] = []
    if not understand:
        reasons.append("没有调用 understand.materials")
    else:
        if len(understand) != 1:
            reasons.append(f"同一窄问题重复点查了 {len(understand)} 次，预期恰好 1 次")
        arguments = understand[0]["arguments"]
        if arguments.get("depth") != "deep":
            reasons.append("点查没有使用 depth=deep")
        if arguments.get("asset_ids") != ["asset_03"]:
            reasons.append("点查没有限定单个目标素材")
        steps = arguments.get("max_steps_per_asset")
        if not isinstance(steps, int) or not 1 <= steps <= 4:
            reasons.append("点查没有显式携带 1-4 的低步数参数")
        if not isinstance(arguments.get("focus"), str) or not arguments["focus"].strip():
            reasons.append("点查没有携带具体 focus")
    return ScenarioResult("point_query_low_steps", not reasons, tuple(calls), tuple(reasons))


async def _drive(planner: Any, *, user_message: str, max_steps: int) -> list[dict[str, Any]]:
    registry = build_default_tool_registry()
    gate = PolicyGate(
        tool_specs=registry.specs_by_name(),
        patch_op_specs=PATCH_OP_REGISTRY.as_mapping(),
    )
    draft = DraftState.model_validate(
        {
            "draft_id": "eval_draft",
            "name": "理解梯度评测",
            "brief": {"goal": "制作课程混剪预告", "confirmed_facts": []},
        }
    )
    preconditions = PreconditionContext(
        draft_state=draft,
        draft_artifacts=DraftArtifactStats(
            usable_asset_count=len(FIXED_ASSETS),
            usable_asset_ids=frozenset(asset.asset_id for asset in FIXED_ASSETS),
        ),
    )
    allowed = tuple(gate.compute_allowed_tools(PolicyContext(preconditions=preconditions)))
    observations: list[str] = []
    calls: list[dict[str, Any]] = []
    for _ in range(max_steps):
        bundle = ContextBuilder().build(
            ContextBuildInput(
                preconditions=preconditions,
                messages=(ContextMessage(role="user", content=user_message),),
                turn_observations=tuple(observations),
                allowed_tools=allowed,
                asset_digest=FIXED_ASSETS,
            )
        )
        step = await planner.plan(bundle, allowed)
        if step.tool_call is None:
            break
        raw_arguments = dict(step.tool_call.arguments)
        registered = registry.get(step.tool_call.tool_name)
        if registered is not None:
            raw_arguments = registered.spec.input_model.model_validate(raw_arguments).model_dump(
                mode="json"
            )
        call = {
            "tool_name": step.tool_call.tool_name,
            "arguments": raw_arguments,
        }
        calls.append(call)
        observations.append(_mock_tool_observation(call))
        if call["tool_name"] == "understand.materials" and call["arguments"].get("depth") == "deep":
            break
    return calls


def _mock_tool_observation(call: dict[str, Any]) -> str:
    name = call["tool_name"]
    arguments = call["arguments"]
    if name == "asset.list_assets":
        rows: list[dict[str, Any]] = [
            {
                "asset_id": asset.asset_id,
                "filename": asset.filename,
                "kind": asset.kind,
                "duration_sec": asset.duration_sec,
                "fps": 30,
                "has_audio": True,
                "ingest_status": "indexed",
            }
            for asset in FIXED_ASSETS
        ]
        return f"asset.list_assets succeeded: {json.dumps(rows, ensure_ascii=False)}"
    if name == "understand.materials" and arguments.get("depth") == "scan":
        rows = [
            {
                "asset_id": asset_id,
                "gist": f"{asset_id} 是节奏清晰、主体明确的候选镜头",
                "tags": ["候选", "动感"],
                "relevance_0_100": 96 - index * 3,
                "confidence": 0.9,
                "frames_used": [{"at_sec": 12.0, "source": "poster"}],
            }
            for index, asset_id in enumerate(SCAN_CANDIDATES)
        ]
        payload = json.dumps(rows, ensure_ascii=False)
        return (
            "understand.materials scan 已成功覆盖全部 12 个素材，无 skipped；不要重复 scan。"
            f"相关度前三名如下，其余 9 个均低于 60：{payload}。"
            "请只从这 3 个候选中选择至多 3 个做一次 deep。"
        )
    if name == "understand.materials" and arguments.get("depth") == "deep":
        if arguments.get("asset_ids") == ["asset_03"]:
            return (
                "understand.materials 点查已成功完成；不要重复点查。"
                "asset_03 在 130.0s（02:10）为人物快速推近并看向镜头，主体清晰、"
                "动势强，适合作为开头。"
            )
        return (
            "understand.materials deep 已成功完成全部所选候选；不要重复 deep。"
            "asset_03 适合开头（12.0s），asset_07 适合高潮（48.0s），"
            "asset_11 适合收尾（76.0s）。"
        )
    if name == "audio.inspect_sources":
        return "audio.inspect_sources succeeded (mock): 所有视频均有音轨。"
    return f"{name} 在本手工评测中未执行；请继续完成素材证据选择。"


if __name__ == "__main__":
    raise SystemExit(asyncio.run(main()))
