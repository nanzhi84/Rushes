from __future__ import annotations

from collections.abc import Callable
from pathlib import Path
from typing import Any

import pytest
from sqlalchemy import func, select
from sqlalchemy.engine import Engine

from agent_harness.policy_gate import PolicyContext, PolicyGate
from agent_harness.reducer import apply
from contracts.asset import StorageMode
from contracts.case import CaseState
from contracts.decision import Decision, DecisionAnswer
from contracts.events import AssetImported, AssetLinked, CaseCreated, ProjectCreated
from contracts.project import ProjectState
from contracts.tool_result import ToolResult
from domain.preconditions import PreconditionContext, ProjectArtifactStats
from storage import schema
from storage.db import create_workspace_engine
from storage.object_store import ObjectStore
from storage.repositories._json import load_json
from storage.workspace_paths import WorkspacePaths
from tools import ToolExecutionContext, tool_specs
from tools.asset import (
    disable_for_case,
    import_local_file,
    import_url,
    link_to_project,
    list_case_scope,
    list_project_assets,
    select_for_case,
    unlink_from_project,
    upload_complete,
)
from tools.specs import (
    AssetDisableForCaseInput,
    AssetImportLocalFileInput,
    AssetImportUrlInput,
    AssetLinkInput,
    AssetListCaseScopeInput,
    AssetListProjectInput,
    AssetSelectForCaseInput,
    AssetUnlinkInput,
    AssetUploadCompleteInput,
)

Handler = Callable[[Any, ToolExecutionContext], ToolResult]
_PROJECT_STATE_SENTINEL = object()
_CASE_STATE_SENTINEL = object()


def test_upload_complete_copies_file_and_queues_proxy(tmp_path: Path) -> None:
    paths = WorkspacePaths.from_root(tmp_path / "workspace").initialize()
    upload = tmp_path / "upload.mp4"
    upload.write_bytes(b"upload")

    result = upload_complete(
        AssetUploadCompleteInput(
            project_id="project_1",
            path=str(upload),
            filename="clip.mp4",
        ),
        _context(paths=paths),
    )

    assert result.status == "succeeded"
    assert [event["event"] for event in result.events] == [
        "AssetImported",
        "AssetLinked",
        "JobEnqueued",
    ]
    object_hash = result.data["object_hash"]
    assert paths.object_path(object_hash).read_bytes() == b"upload"
    assert result.events[0]["payload"]["storage_mode"] == "copy"


def test_import_local_file_defaults_to_reference_and_queues_proxy(tmp_path: Path) -> None:
    source = tmp_path / "local.mp4"
    source.write_bytes(b"local")

    result = import_local_file(
        AssetImportLocalFileInput(project_id="project_1", path=str(source)),
        _context(paths=WorkspacePaths.from_root(tmp_path / "workspace").initialize()),
    )

    assert result.status == "succeeded"
    assert [event["event"] for event in result.events] == [
        "AssetImported",
        "AssetLinked",
        "JobEnqueued",
    ]
    assert result.events[0]["payload"]["storage_mode"] == "reference"
    assert result.events[0]["payload"]["reference_path"] == str(source)
    assert result.events[0]["payload"]["object_hash"] is None


def test_import_url_handler_queues_import_url_job(tmp_path: Path) -> None:
    result = import_url(
        AssetImportUrlInput(
            project_id="project_1",
            asset_id="asset_url",
            url="https://example.test/clip.mp4",
            filename="clip.mp4",
        ),
        _context(paths=WorkspacePaths.from_root(tmp_path / "workspace").initialize()),
    )

    assert result.status == "running"
    assert result.data["asset_id"] == "asset_url"
    assert result.events[0]["event"] == "JobEnqueued"
    assert result.events[0]["payload"]["kind"] == "import_url"
    assert result.events[0]["payload"]["job_payload"]["url"] == "https://example.test/clip.mp4"


def test_link_to_project_emits_asset_linked() -> None:
    result = link_to_project(
        AssetLinkInput(
            project_id="project_1",
            asset_id="asset_1",
            enabled=False,
            note="hold",
        ),
        _context(),
    )

    assert result.status == "succeeded"
    assert result.events[0]["event"] == "AssetLinked"
    assert result.events[0]["payload"] == {"enabled": False, "note": "hold"}


def test_unlink_from_project_emits_asset_unlinked() -> None:
    result = unlink_from_project(
        AssetUnlinkInput(project_id="project_1", asset_id="asset_1"),
        _context(),
    )

    assert result.status == "succeeded"
    assert result.events[0]["event"] == "AssetUnlinked"
    assert result.events[0]["asset_id"] == "asset_1"


def test_select_for_case_emits_case_scope_change() -> None:
    result = select_for_case(
        AssetSelectForCaseInput(case_id="case_1", asset_id="asset_2"),
        _context(case_state=_case_state(disabled_asset_ids=["asset_2"])),
    )

    assert result.status == "succeeded"
    assert result.events[0]["event"] == "CaseAssetScopeChanged"
    assert result.events[0]["payload"]["selected_asset_ids"] == ["asset_2"]
    assert result.events[0]["payload"]["disabled_asset_ids"] == []


def test_disable_for_case_emits_case_scope_change() -> None:
    result = disable_for_case(
        AssetDisableForCaseInput(case_id="case_1", asset_id="asset_1"),
        _context(case_state=_case_state(selected_asset_ids=["asset_1"])),
    )

    assert result.status == "succeeded"
    assert result.events[0]["event"] == "CaseAssetScopeChanged"
    assert result.events[0]["payload"]["selected_asset_ids"] == []
    assert result.events[0]["payload"]["disabled_asset_ids"] == ["asset_1"]


def test_list_project_assets_reads_linked_assets(tmp_path: Path) -> None:
    engine = _engine_with_project_asset(tmp_path)

    with engine.connect() as connection:
        result = list_project_assets(
            AssetListProjectInput(project_id="project_1"),
            _context(connection=connection, project_state=None, case_state=None),
        )

    assert result.status == "succeeded"
    assert result.data["assets"][0]["asset_id"] == "asset_1"
    assert result.data["assets"][0]["enabled"] is True


def test_list_case_scope_reads_selected_and_disabled_ids() -> None:
    result = list_case_scope(
        AssetListCaseScopeInput(case_id="case_1"),
        _context(case_state=_case_state(selected_asset_ids=["asset_1"], disabled_asset_ids=[])),
    )

    assert result.status == "succeeded"
    assert result.data["selected_asset_ids"] == ["asset_1"]
    assert result.data["disabled_asset_ids"] == []


@pytest.mark.parametrize(
    ("handler", "input_model", "expected_error"),
    [
        (
            upload_complete,
            AssetUploadCompleteInput(path="/missing.mp4"),
            "missing_project",
        ),
        (
            import_local_file,
            AssetImportLocalFileInput(path="/missing.mp4"),
            "missing_project",
        ),
        (
            import_url,
            AssetImportUrlInput(url="https://example.test/clip.mp4"),
            "missing_project",
        ),
        (link_to_project, AssetLinkInput(asset_id="asset_1"), "missing_project"),
        (unlink_from_project, AssetUnlinkInput(asset_id="asset_1"), "missing_project"),
        (select_for_case, AssetSelectForCaseInput(asset_id="asset_1"), "missing_case"),
        (disable_for_case, AssetDisableForCaseInput(asset_id="asset_1"), "missing_case"),
        (list_project_assets, AssetListProjectInput(), "missing_project"),
        (list_case_scope, AssetListCaseScopeInput(), "missing_case"),
    ],
)
def test_asset_handlers_fail_cleanly_without_required_scope(
    handler: Handler,
    input_model: Any,
    expected_error: str,
) -> None:
    result = handler(input_model, ToolExecutionContext(tool_call_id="tc_1", turn_id="turn_1"))

    assert result.status == "failed"
    assert result.error is not None
    assert result.error.error_code == expected_error
    assert result.events == []


def test_select_and_disable_only_mutate_case_state(tmp_path: Path) -> None:
    engine = _engine_with_project_asset(tmp_path, include_case=True)

    with engine.connect() as connection:
        selected = select_for_case(
            AssetSelectForCaseInput(case_id="case_1", asset_id="asset_1"),
            _context(connection=connection, project_state=None, case_state=None),
        )
    selected_result = apply(selected.events, engine=engine, base_version=0, actor="user")
    assert selected_result.status == "applied"

    case_after_select = _case_row(engine)
    with engine.connect() as connection:
        disabled = disable_for_case(
            AssetDisableForCaseInput(case_id="case_1", asset_id="asset_1"),
            _context(connection=connection, project_state=None, case_state=None),
        )
    disabled_result = apply(
        disabled.events,
        engine=engine,
        base_version=int(case_after_select["state_version"]),
        actor="user",
    )
    assert disabled_result.status == "applied"

    case_after_disable = _case_row(engine)
    assert load_json(case_after_disable["selected_asset_ids"]) == []
    assert load_json(case_after_disable["disabled_asset_ids"]) == ["asset_1"]
    with engine.connect() as connection:
        link_count = connection.execute(
            select(func.count()).select_from(schema.project_asset_links)
        ).scalar_one()
    assert link_count == 1


def test_import_url_policy_gate_asks_for_confirmation_then_defers_job() -> None:
    gate = PolicyGate(
        tool_specs={spec.name: spec for spec in tool_specs()},
        patch_op_specs={},
    )
    arguments = {
        "project_id": "project_1",
        "url": "https://example.test/clip.mp4",
        "filename": "clip.mp4",
    }

    ask = gate.adjudicate(
        {"tool_name": "asset.import_url", "arguments": arguments},
        _policy_context(),
    )
    assert ask.status == "ask"
    assert ask.decision is not None
    assert ask.decision.type == "url_import"
    assert ask.decision.scope_type == "project"
    assert ask.pending_tool_call is not None
    assert ask.pending_tool_call.tool_name == "asset.import_url"

    approved = ask.decision.model_copy(
        update={
            "status": "answered",
            "answer": DecisionAnswer(option_id="approve", answered_via="button"),
            "pending_tool_call_status": "approved",
        }
    )
    defer = gate.adjudicate(
        {"tool_name": "asset.import_url", "arguments": arguments},
        _policy_context(decisions=(approved,)),
    )

    assert defer.status == "defer"
    assert defer.validated_arguments["project_id"] == "project_1"


def _context(
    *,
    paths: WorkspacePaths | None = None,
    connection: Any | None = None,
    project_state: ProjectState | None | object = _PROJECT_STATE_SENTINEL,
    case_state: CaseState | None | object = _CASE_STATE_SENTINEL,
) -> ToolExecutionContext:
    metadata: dict[str, object] = {}
    if paths is not None:
        metadata["workspace_paths"] = paths
    return ToolExecutionContext(
        tool_call_id="tc_1",
        turn_id="turn_1",
        project_state=(
            _project_state() if project_state is _PROJECT_STATE_SENTINEL else project_state
        ),
        case_state=_case_state() if case_state is _CASE_STATE_SENTINEL else case_state,
        readonly_connection=connection,
        metadata=metadata,
    )


def _policy_context(decisions: tuple[Decision, ...] = ()) -> PolicyContext:
    return PolicyContext(
        preconditions=PreconditionContext(
            project_state=_project_state(),
            case_state=None,
            project_artifacts=ProjectArtifactStats(usable_asset_count=0),
        ),
        decisions=decisions,
        allowed_tool_names=frozenset({"asset.import_url"}),
    )


def _project_state() -> ProjectState:
    return ProjectState.model_validate(
        {
            "project_id": "project_1",
            "name": "Project",
            "status": "active",
            "asset_links": [],
            "case_ids": ["case_1"],
            "memory_ids": [],
            "created_at": "2026-07-04T00:00:00+00:00",
            "updated_at": "2026-07-04T00:00:00+00:00",
        }
    )


def _case_state(**overrides: object) -> CaseState:
    values: dict[str, object] = {
        "case_id": "case_1",
        "project_id": "project_1",
        "name": "Case",
        "brief": {"goal": "test"},
        "selected_asset_ids": [],
        "disabled_asset_ids": [],
        "scratch_memory": {},
    }
    values.update(overrides)
    return CaseState.model_validate(values)


def _engine_with_project_asset(tmp_path: Path, *, include_case: bool = False) -> Engine:
    paths = WorkspacePaths.from_root(tmp_path / "workspace").initialize()
    engine = create_workspace_engine(paths)
    with engine.begin() as connection:
        schema.create_all(connection)
    object_ref = ObjectStore(paths).put_bytes(b"asset")
    events: list[Any] = [
        ProjectCreated(project_id="project_1", name="Project"),
        AssetImported(
            project_id="project_1",
            asset_id="asset_1",
            payload={
                "storage_mode": StorageMode.COPY.value,
                "object_hash": object_ref.object_hash,
                "object_size": object_ref.size,
                "filename": "asset.mp4",
                "hash": object_ref.object_hash,
                "size": object_ref.size,
                "mtime": 1,
                "usable": True,
            },
        ),
        AssetLinked(project_id="project_1", asset_id="asset_1"),
    ]
    if include_case:
        events.append(
            CaseCreated(
                project_id="project_1",
                case_id="case_1",
                payload={"name": "Case", "brief": {"goal": "test"}},
            )
        )
    result = apply(events, engine=engine, base_version=None, actor="user")
    assert result.status == "applied"
    return engine


def _case_row(engine: Engine) -> dict[str, object]:
    with engine.connect() as connection:
        row = connection.execute(
            select(schema.cases).where(schema.cases.c.case_id == "case_1")
        ).one()
    return dict(row._mapping)
