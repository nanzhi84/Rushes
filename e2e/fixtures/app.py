"""E2E-only ASGI factory with deterministic planner/VLM behavior."""

from __future__ import annotations

import asyncio
import json
import os
import re
from collections.abc import Callable, Sequence
from pathlib import Path
from typing import Any

from apps.api.main import create_app

from agent_harness.context_builder import ContextBundle
from agent_harness.loop import NoProviderPlanner, PlannerStep, run_turn
from agent_harness.policy_gate import ToolCall
from contracts.provider import ProviderResult
from contracts.tool import ToolSpec
from providers import VLM_UNDERSTANDING
from providers.gateway import ProviderGatewayResult
from storage.db import create_workspace_engine

CANCEL_TRIGGER = "E2E_CANCEL_UNDERSTANDING"
READY_ASSET_ID = "e2e_cancel_ready"
SLOW_ASSET_ID = "e2e_cancel_slow"


class E2EPlanner:
    async def plan(
        self,
        context: ContextBundle,
        tools: Sequence[ToolSpec],
        *,
        on_delta: Callable[[str], None] | None = None,
    ) -> PlannerStep:
        del tools
        messages = context.blocks.get("messages", "")
        observations = context.blocks.get("turn_observations", "")
        if CANCEL_TRIGGER in messages and "understand.materials(" not in observations:
            return PlannerStep(
                tool_call=ToolCall(
                    tool_name="understand.materials",
                    arguments={"asset_ids": [READY_ASSET_ID, SLOW_ASSET_ID], "depth": "deep"},
                    tool_call_id="e2e_cancel_understanding",
                )
            )
        content = NoProviderPlanner.MESSAGE
        if on_delta is not None:
            on_delta(content)
        return PlannerStep(content=content)


class E2EUnderstandingGateway:
    async def call(self, request: Any) -> ProviderGatewayResult:
        asset_id = _asset_id_from_request(request)
        if asset_id == SLOW_ASSET_ID:
            await asyncio.Event().wait()
        summary = {
            "action": "emit_summary",
            "summary": {
                "semantic_role": "footage",
                "overall": "E2E 已完成素材摘要",
                "language": "zh",
                "segments": [
                    {
                        "start_s": 0.0,
                        "end_s": 2.0,
                        "description": "E2E fixture",
                        "tags": ["e2e"],
                        "quality": "good",
                    }
                ],
            },
        }
        return ProviderGatewayResult(
            result=ProviderResult(
                provider_id="e2e_vlm",
                capability=VLM_UNDERSTANDING,
                request_id=request.request_id,
                model="e2e-vlm",
                latency_ms=1,
                normalized_output={"content": json.dumps(summary, ensure_ascii=False)},
            )
        )


def create_app_from_env() -> Any:
    workspace = Path(os.environ["RUSHES_WORKSPACE_PATH"])
    engine = create_workspace_engine(workspace)
    holder: dict[str, Any] = {}
    planner = E2EPlanner()
    gateway = E2EUnderstandingGateway()

    async def runner(item: Any, stop_token: Any) -> None:
        app = holder["app"]
        state = app.state.api_state
        await run_turn(
            item,
            engine=engine,
            planner=planner,
            turn_queue=state.turn_queue,
            stop_token=stop_token,
            tool_gateway=gateway,
            turn_listener=state.turn_stream_hub.listener_for(item.draft_id),
        )

    app = create_app(
        workspace,
        token=os.environ.get("RUSHES_API_TOKEN"),
        fs_roots=_fs_roots(),
        turn_runner=runner,
        tool_gateway=gateway,
        startup_port=int(os.environ.get("RUSHES_API_PORT", "18000")),
    )
    holder["app"] = app
    return app


def _asset_id_from_request(request: Any) -> str:
    for message in request.payload.get("messages", []):
        content = message.get("content") if isinstance(message, dict) else None
        parts = content if isinstance(content, list) else [{"text": content}]
        for part in parts:
            text = part.get("text") if isinstance(part, dict) else None
            if not isinstance(text, str):
                continue
            match = re.search(r"asset_id=([^；\s]+)", text)
            if match:
                return match.group(1)
    return ""


def _fs_roots() -> list[str] | None:
    raw = os.environ.get("RUSHES_FS_ROOTS")
    return raw.split(os.pathsep) if raw else None
