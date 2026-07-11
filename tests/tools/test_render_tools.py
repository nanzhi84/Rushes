from __future__ import annotations

import hashlib
import json
from pathlib import Path
from typing import Any

import pytest
from sqlalchemy.engine import Connection, Engine

from contracts.draft import DraftState
from contracts.preview_inspection import PreviewInspectionIssue
from contracts.provider import ProviderResult
from media.preview_inspection import DeterministicPreviewInspection, PreviewSnapshot
from providers import VLM_UNDERSTANDING
from providers.gateway import ProviderGatewayResult
from storage import schema
from storage.db import create_workspace_engine
from storage.repositories._json import dump_json
from storage.workspace_paths import WorkspacePaths
from tools import ToolExecutionContext
from tools.render_tools import handlers as render_handlers
from tools.render_tools import inspect_preview, preview, status
from tools.specs import RenderInspectPreviewInput, RenderPreviewInput, RenderStatusInput

NOW = "2026-07-05T00:00:00+00:00"


def test_render_preview_handler_enqueues_draft_job(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.connect() as connection:
        result = preview(RenderPreviewInput(), _context(connection, _draft_state()))

    assert result.status == "running"
    assert result.events[0]["event"] == "JobEnqueued"
    assert result.events[0]["payload"]["kind"] == "render_preview"
    assert result.events[0]["draft_id"] == "draft_1"
    assert result.data["draft_id"] == "draft_1"


def test_render_status_reports_artifacts_and_running_jobs(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        connection.execute(
            schema.objects.insert().values(
                hash="a" * 64,
                rel_path=f"{'a' * 2}/{'a' * 2}/{'a' * 64}",
                size=10,
                created_at=NOW,
            )
        )
        connection.execute(
            schema.previews.insert().values(
                preview_id="preview_1",
                draft_id="draft_1",
                timeline_version=1,
                object_hash="a" * 64,
                quality=dump_json({"name": "preview"}),
                created_at=NOW,
            )
        )
        connection.execute(
            schema.jobs.insert().values(
                job_id="job_render",
                kind="render_preview",
                status="running",
                draft_id="draft_1",
                requested_by_draft_id="draft_1",
                asset_id=None,
                idempotency_key="render",
                payload_json=dump_json({"arguments": {}}),
                result_json=None,
                error_json=None,
                attempts=0,
                max_retries=0,
                next_run_at=NOW,
                progress=0.5,
                worker_id=None,
                heartbeat_at=None,
                created_at=NOW,
                started_at=NOW,
                finished_at=None,
            )
        )
    with engine.connect() as connection:
        draft_state = _draft_state(preview_current_id="preview_1")
        result = status(RenderStatusInput(), _context(connection, draft_state))

    assert result.status == "succeeded"
    assert result.data["preview_current_id"] == "preview_1"
    assert result.data["previews"][0]["current"] is True
    assert result.data["running_jobs"][0]["progress"] == 0.5


async def test_inspect_old_preview_is_still_available_and_uses_deterministic_cache(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    engine = _engine(tmp_path)
    paths = _seed_preview(engine, tmp_path, timeline_version=1)
    calls: list[tuple[PreviewSnapshot, tuple[str, ...]]] = []

    def fake_inspect(
        path: Path, *, expected: PreviewSnapshot, checks: tuple[str, ...]
    ) -> DeterministicPreviewInspection:
        calls.append((expected, checks))
        return DeterministicPreviewInspection(
            info=None,
            issues=(
                PreviewInspectionIssue(
                    severity="warning",
                    category="black_frame",
                    description="检测到黑帧",
                    at_sec=1.0,
                ),
            ),
        )

    monkeypatch.setattr(render_handlers, "inspect_preview_file", fake_inspect)
    draft = _draft_state(timeline_current_version=2, preview_current_id=None)
    with engine.connect() as connection:
        first = await inspect_preview(
            RenderInspectPreviewInput(preview_id="preview_1", checks=["black"]),
            _context(connection, draft, paths=paths),
        )
        second = await inspect_preview(
            RenderInspectPreviewInput(preview_id="preview_1", checks=["black"]),
            _context(connection, draft, paths=paths),
        )

    assert len(calls) == 1
    expected = calls[0][0]
    assert expected.width == 320
    assert expected.duration_sec == 3.0
    assert first.data["degraded"] is True
    assert {item["category"] for item in first.data["issues"]} == {
        "black_frame",
        "stale_preview",
    }
    assert first.data == second.data


async def test_inspection_cache_separates_checks_and_vlm_prompt_version(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    engine = _engine(tmp_path)
    paths = _seed_preview(engine, tmp_path, timeline_version=1)
    deterministic_calls = 0
    advisory_calls = 0

    def fake_inspect(*_args: Any, **_kwargs: Any) -> DeterministicPreviewInspection:
        nonlocal deterministic_calls
        deterministic_calls += 1
        return DeterministicPreviewInspection(info=None, issues=())

    async def fake_advisory(*_args: Any, **_kwargs: Any) -> list[PreviewInspectionIssue]:
        nonlocal advisory_calls
        advisory_calls += 1
        return []

    monkeypatch.setattr(render_handlers, "inspect_preview_file", fake_inspect)
    monkeypatch.setattr(render_handlers, "_vlm_advisory", fake_advisory)
    gateway = _CallableGateway()
    draft = _draft_state()
    with engine.connect() as connection:
        context = _context(connection, draft, paths=paths, gateway=gateway)
        for checks in (["black"], ["black"], ["silence"]):
            await inspect_preview(
                RenderInspectPreviewInput(preview_id="preview_1", checks=checks),
                context,
            )
        monkeypatch.setattr(render_handlers, "VLM_INSPECTION_PROMPT_VERSION", "v2")
        await inspect_preview(
            RenderInspectPreviewInput(preview_id="preview_1", checks=["silence"]),
            context,
        )

    assert deterministic_calls == 2
    assert advisory_calls == 3


async def test_vlm_advisory_receives_expected_manifest_and_caps_severity(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    engine = _engine(tmp_path)
    preview_path = tmp_path / "preview.mp4"
    preview_path.write_bytes(b"fixture")
    gateway = _RecordingGateway(
        {
            "issues": [
                {
                    "at_sec": 0.5,
                    "severity": "error",
                    "category": "subtitle_cutoff",
                    "description": "字幕被截断",
                }
            ]
        }
    )
    monkeypatch.setattr(render_handlers, "_inspection_times", lambda *_args: [0.5])
    monkeypatch.setattr(
        render_handlers,
        "_expected_visual_manifest",
        lambda *_args: {"timeline_version": 1, "marker": "declared-overlay"},
    )
    monkeypatch.setattr(
        render_handlers, "extract_frame_data_uri", lambda *_args: "data:image/jpeg;base64,eA=="
    )

    with engine.connect() as connection:
        issues = await render_handlers._vlm_advisory(
            preview_path,
            _context(connection, _draft_state(), gateway=gateway),
            gateway=gateway,
            timeline_version=1,
            duration_sec=1.0,
        )

    prompt = gateway.requests[0].payload["messages"][0]["content"][0]["text"]
    assert "declared-overlay" in prompt
    assert issues[0].category == "subtitle_cutoff"
    assert issues[0].severity == "warning"


async def test_vlm_advisory_rejects_missing_issues_array(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    engine = _engine(tmp_path)
    preview_path = tmp_path / "preview.mp4"
    preview_path.write_bytes(b"fixture")
    gateway = _RecordingGateway({"unexpected": []})
    monkeypatch.setattr(render_handlers, "_inspection_times", lambda *_args: [0.0])
    monkeypatch.setattr(render_handlers, "_expected_visual_manifest", lambda *_args: {})
    monkeypatch.setattr(
        render_handlers, "extract_frame_data_uri", lambda *_args: "data:image/jpeg;base64,eA=="
    )

    with engine.connect() as connection, pytest.raises(RuntimeError, match="缺少 issues"):
        await render_handlers._vlm_advisory(
            preview_path,
            _context(connection, _draft_state(), gateway=gateway),
            gateway=gateway,
            timeline_version=1,
            duration_sec=1.0,
        )


def _engine(tmp_path: Path) -> Engine:
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
                running_jobs="[]",
                brief=dump_json({"goal": "test", "confirmed_facts": []}),
                timeline_current_version=1,
                timeline_validated=True,
                rough_cut_approved=False,
                scratch_memory="{}",
                created_at=NOW,
                updated_at=NOW,
            )
        )
    return engine


def _context(
    connection: Connection,
    draft_state: DraftState,
    *,
    paths: WorkspacePaths | None = None,
    gateway: object | None = None,
) -> ToolExecutionContext:
    metadata: dict[str, object] = {}
    if paths is not None:
        metadata["workspace_paths"] = paths
    if gateway is not None:
        metadata["provider_gateway"] = gateway
    return ToolExecutionContext(
        tool_call_id="tc_1",
        turn_id="turn_1",
        draft_state=draft_state,
        readonly_connection=connection,
        created_at=NOW,
        metadata=metadata,
    )


def _draft_state(
    *,
    preview_current_id: str | None = None,
    timeline_current_version: int = 1,
) -> DraftState:
    return DraftState.model_validate(
        {
            "draft_id": "draft_1",
            "name": "Draft",
            "brief": {"goal": "test", "confirmed_facts": []},
            "timeline_current_version": timeline_current_version,
            "timeline_validated": True,
            "preview_current_id": preview_current_id,
        }
    )


def _seed_preview(
    engine: Engine,
    root: Path,
    *,
    timeline_version: int,
) -> WorkspacePaths:
    paths = WorkspacePaths.from_root(root).initialize()
    payload = b"preview fixture"
    object_hash = hashlib.sha256(payload).hexdigest()
    object_path = paths.object_path(object_hash)
    object_path.parent.mkdir(parents=True, exist_ok=True)
    object_path.write_bytes(payload)
    with engine.begin() as connection:
        connection.execute(
            schema.objects.insert().values(
                hash=object_hash,
                rel_path=str(object_path.relative_to(paths.objects_dir)),
                size=len(payload),
                created_at=NOW,
            )
        )
        connection.execute(
            schema.previews.insert().values(
                preview_id="preview_1",
                draft_id="draft_1",
                timeline_version=timeline_version,
                object_hash=object_hash,
                quality=dump_json({"name": "preview"}),
                render_width=320,
                render_height=180,
                render_fps=30.0,
                expected_duration_sec=3.0,
                created_at=NOW,
            )
        )
    return paths


class _CallableGateway:
    async def call(self, _request: object) -> None:  # pragma: no cover - advisory is mocked
        return None


class _RecordingGateway:
    def __init__(self, output: dict[str, Any]) -> None:
        self.output = output
        self.requests: list[Any] = []

    async def call(self, request: Any) -> ProviderGatewayResult:
        self.requests.append(request)
        return ProviderGatewayResult(
            result=ProviderResult(
                provider_id="test-vlm",
                capability=VLM_UNDERSTANDING,
                request_id=request.request_id,
                model="test-vlm",
                latency_ms=1,
                normalized_output={"content": json.dumps(self.output, ensure_ascii=False)},
            )
        )
