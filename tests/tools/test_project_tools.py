from pathlib import Path

from agent_harness.reducer import apply
from contracts.case import CaseState
from contracts.events import AssetImported, AssetLinked, CaseCreated, ProjectCreated
from contracts.project import ProjectState
from storage import schema
from storage.db import begin_immediate, create_workspace_engine
from storage.repositories._json import dump_json
from tools import ToolExecutionContext
from tools.project import (
    close_case,
    copy,
    create,
    create_case,
    delete,
    list_tree,
    move_case,
    rename,
)
from tools.specs import (
    ProjectCloseCaseInput,
    ProjectCopyInput,
    ProjectCreateCaseInput,
    ProjectCreateInput,
    ProjectDeleteInput,
    ProjectListTreeInput,
    ProjectMoveCaseInput,
    ProjectRenameInput,
)


def test_project_lifecycle_handlers_emit_only_tool_results_and_events() -> None:
    context = _context()

    created = create(ProjectCreateInput(project_id="project_new", name="New"), context)
    renamed = rename(ProjectRenameInput(name="Renamed"), context)
    copied = copy(ProjectCopyInput(project_id="project_copy", name="Copy"), context)
    trashed = delete(ProjectDeleteInput(), context)
    case_created = create_case(ProjectCreateCaseInput(case_id="case_new", goal="剪 30 秒"), context)
    moved = move_case(ProjectMoveCaseInput(target_project_id="project_2"), context)
    closed = close_case(ProjectCloseCaseInput(), context)

    assert created.events[0]["event"] == "ProjectCreated"
    assert renamed.events[0]["event"] == "ProjectRenamed"
    assert copied.events[0]["event"] == "ProjectCopied"
    assert copied.events[0]["source_project_id"] == "project_1"
    assert trashed.events[0]["event"] == "ProjectTrashed"
    assert case_created.events[0]["event"] == "CaseCreated"
    assert case_created.events[0]["payload"]["brief"]["goal"] == "剪 30 秒"
    assert moved.events[0]["event"] == "CaseMoved"
    assert moved.events[0]["target_project_id"] == "project_2"
    assert closed.events[0]["event"] == "CaseClosed"


def test_project_list_tree_returns_only_project_case_levels(tmp_path: Path) -> None:
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)
    result = apply(
        (
            ProjectCreated(project_id="project_1", name="Project"),
            CaseCreated(
                project_id="project_1",
                case_id="case_1",
                payload={"name": "Case", "brief": {"goal": "test"}},
            ),
            AssetImported(asset_id="asset_1", job_id="job_import"),
            AssetLinked(project_id="project_1", asset_id="asset_1"),
        ),
        engine=engine,
        base_version=None,
        actor="user",
    )
    assert result.status == "applied"
    with begin_immediate(engine) as connection:
        connection.execute(
            schema.memories.insert().values(
                memory_id="memory_1",
                scope="project",
                project_id="project_1",
                content="style",
                tags=dump_json([]),
                created_from_case_id=None,
                created_at="2026-07-04T00:00:00+00:00",
            )
        )
        context = ToolExecutionContext(
            tool_call_id="tc_1",
            turn_id="turn_1",
            readonly_connection=connection,
        )
        tree = list_tree(ProjectListTreeInput(), context)

    assert tree.status == "succeeded"
    assert tree.data["projects"] == [
        {
            "project_id": "project_1",
            "name": "Project",
            "status": "active",
            "cases": [
                {
                    "case_id": "case_1",
                    "project_id": "project_1",
                    "name": "Case",
                    "status": "active",
                }
            ],
        }
    ]
    assert "asset_1" not in str(tree.data["projects"])
    assert "memory_1" not in str(tree.data["projects"])


def _context() -> ToolExecutionContext:
    return ToolExecutionContext(
        tool_call_id="tc_1",
        turn_id="turn_1",
        project_state=ProjectState.model_validate(
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
        ),
        case_state=CaseState.model_validate(
            {
                "case_id": "case_1",
                "project_id": "project_1",
                "name": "Case",
                "brief": {"goal": "test"},
                "selected_asset_ids": ["asset_1"],
            }
        ),
    )


def test_handlers_fail_cleanly_without_active_project_or_case() -> None:
    bare = ToolExecutionContext(tool_call_id="tc_x", turn_id="turn_x")

    failures = [
        rename(ProjectRenameInput(name="X"), bare),
        delete(ProjectDeleteInput(), bare),
        copy(ProjectCopyInput(project_id="p_new", name="C"), bare),
        create_case(ProjectCreateCaseInput(case_id="c_new", goal="g"), bare),
        move_case(ProjectMoveCaseInput(target_project_id="p_2"), bare),
        close_case(ProjectCloseCaseInput(), bare),
    ]

    for result in failures:
        assert result.status == "failed"
        assert result.error is not None
        assert result.events == []


def test_active_project_resolution_rejects_mismatched_request() -> None:
    context = _context()

    mismatched = rename(ProjectRenameInput(project_id="project_other", name="X"), context)
    assert mismatched.status == "failed"

    explicit = rename(ProjectRenameInput(project_id="project_1", name="X"), context)
    assert explicit.status == "succeeded"
