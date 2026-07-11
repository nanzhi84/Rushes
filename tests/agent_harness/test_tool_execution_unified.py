from __future__ import annotations

from pathlib import Path
from typing import Any

import pytest
from apps.api.main import _execute_rest_tool, _load_draft_state, create_app
from fastapi import HTTPException
from fastapi.testclient import TestClient
from sqlalchemy import select

from agent_harness.loop import context_bundle_input_preconditions, load_internal_state
from agent_harness.policy_gate import PolicyContext, PolicyGate, ToolCall
from agent_harness.tool_execution import execute_internal_tool
from agent_harness.tool_router import ToolRouter
from storage import schema
from storage.repositories._json import dump_json, load_json
from tools import PATCH_OP_REGISTRY, build_default_tool_registry

NOW = "2026-07-10T00:00:00+00:00"


async def test_agent_and_rest_share_gate_events_and_materialized_state(tmp_path: Path) -> None:
    source = tmp_path / "clip.mp4"
    source.write_bytes(b"same source")
    agent_app = create_app(tmp_path / "agent", token="test", fs_roots=[tmp_path])
    rest_app = create_app(tmp_path / "rest", token="test", fs_roots=[tmp_path])
    agent_state = agent_app.state.api_state
    rest_state = rest_app.state.api_state
    _seed_draft(agent_state.engine)
    _seed_draft(rest_state.engine)
    arguments = {
        "asset_id": "asset_same",
        "path": str(source),
        "storage_mode": "reference",
        "kind": "video",
        "rel_dir": "",
    }

    registry = build_default_tool_registry()
    gate = PolicyGate(
        tool_specs=registry.specs_by_name(),
        patch_op_specs=PATCH_OP_REGISTRY.as_mapping(),
    )
    loaded = load_internal_state(agent_state.engine, "draft_1")
    agent_execution = await execute_internal_tool(
        ToolCall(
            tool_name="asset.import_local_file",
            arguments=arguments,
            tool_call_id="tc_agent_import",
        ),
        engine=agent_state.engine,
        registry=registry,
        router=ToolRouter(registry),
        policy_gate=gate,
        policy_context=PolicyContext(
            preconditions=context_bundle_input_preconditions(loaded),
            decisions=loaded.decisions,
            pending_decision=loaded.pending_decision,
        ),
        draft_state=loaded.draft_state,
        decisions=loaded.decisions,
        turn_id="turn_agent",
        actor="user",
        base_version=None,
        workspace_paths=agent_state.workspace_paths,
        include_harness_only=True,
    )
    response = TestClient(rest_app, base_url="http://127.0.0.1:8000").post(
        "/api/drafts/draft_1/materials/import-local",
        headers={"Authorization": "Bearer test"},
        json={
            "asset_id": arguments["asset_id"],
            "path": arguments["path"],
            "storage_mode": arguments["storage_mode"],
        },
    )

    assert response.status_code == 200
    assert agent_execution.verdict.status == "allow"
    assert agent_execution.result is not None
    assert [event["event"] for event in agent_execution.result.events] == [
        "AssetImported",
        "AssetLinked",
        "JobEnqueued",
        "JobEnqueued",
        "JobEnqueued",
    ]
    assert _event_types(agent_state.engine) == _event_types(rest_state.engine)
    assert _materialized_snapshot(agent_state.engine) == _materialized_snapshot(rest_state.engine)


@pytest.mark.parametrize(
    ("tool_name", "arguments", "expected_verdict"),
    [
        (
            "asset.import_url",
            {"url": "https://example.com/video.mp4", "kind": "video"},
            "ask",
        ),
        ("render.inspect_preview", {"preview_id": "missing"}, "deny"),
    ],
)
def test_rest_non_allow_returns_structured_4xx_without_decision(
    tmp_path: Path,
    tool_name: str,
    arguments: dict[str, Any],
    expected_verdict: str,
) -> None:
    app = create_app(tmp_path / expected_verdict, token="test", fs_roots=[tmp_path])
    state = app.state.api_state
    _seed_draft(state.engine)

    with pytest.raises(HTTPException) as raised:
        _execute_rest_tool(
            state,
            tool_name=tool_name,
            arguments=arguments,
            draft_state=_load_draft_state(state.engine, "draft_1"),
            actor="user",
        )

    assert 400 <= raised.value.status_code < 500
    assert raised.value.detail["verdict"] == expected_verdict
    assert isinstance(raised.value.detail["reason"], str)
    with state.engine.connect() as connection:
        assert connection.execute(select(schema.decisions)).all() == []
        assert connection.execute(select(schema.event_log)).all() == []


def test_api_has_no_legacy_asset_handler_bypass() -> None:
    source = (Path(__file__).parents[2] / "apps" / "api" / "main.py").read_text(encoding="utf-8")

    assert "_run_asset_tool" not in source
    assert "tools.asset.handlers" not in source


def _seed_draft(engine: Any) -> None:
    with engine.begin() as connection:
        connection.execute(
            schema.drafts.insert().values(
                draft_id="draft_1",
                name="Draft",
                state_version=0,
                status="active",
                defaults=dump_json({"aspect_ratio": "9:16", "fps": 30}),
                running_jobs="[]",
                brief=dump_json({"goal": "test", "confirmed_facts": []}),
                timeline_validated=False,
                rough_cut_approved=False,
                scratch_memory="{}",
                created_at=NOW,
                updated_at=NOW,
            )
        )


def _event_types(engine: Any) -> list[str]:
    with engine.connect() as connection:
        rows = connection.execute(
            select(schema.event_log.c.event_type).order_by(schema.event_log.c.event_id)
        ).all()
    return [str(row._mapping["event_type"]) for row in rows]


def _materialized_snapshot(engine: Any) -> dict[str, Any]:
    with engine.connect() as connection:
        asset = (
            connection.execute(
                select(
                    schema.assets.c.asset_id,
                    schema.assets.c.reference_path,
                    schema.assets.c.kind,
                    schema.assets.c.ingest_status,
                    schema.assets.c.usable,
                )
            )
            .mappings()
            .one()
        )
        link = (
            connection.execute(
                select(
                    schema.draft_asset_links.c.draft_id,
                    schema.draft_asset_links.c.asset_id,
                    schema.draft_asset_links.c.rel_dir,
                )
            )
            .mappings()
            .one()
        )
        jobs = (
            connection.execute(
                select(
                    schema.jobs.c.kind,
                    schema.jobs.c.status,
                    schema.jobs.c.asset_id,
                    schema.jobs.c.idempotency_key,
                    schema.jobs.c.payload_json,
                ).order_by(schema.jobs.c.kind)
            )
            .mappings()
            .all()
        )
    normalized_jobs = []
    for job in jobs:
        job_payload = load_json(job["payload_json"])
        normalized_jobs.append(
            {
                "kind": job["kind"],
                "status": job["status"],
                "asset_id": job["asset_id"],
                "idempotency_key": job["idempotency_key"],
                "arguments": job_payload.get("job_payload", {}).get("arguments", {}),
            }
        )
    return {
        "asset": dict(asset),
        "link": dict(link),
        "jobs": normalized_jobs,
    }
