from pathlib import Path

from scripts import check_contracts


def test_dependency_checker_reports_reverse_import(tmp_path: Path) -> None:
    packages = tmp_path / "packages"
    (packages / "tools").mkdir(parents=True)
    (packages / "agent_harness").mkdir(parents=True)
    (packages / "tools" / "bad.py").write_text(
        "from agent_harness import loop\n",
        encoding="utf-8",
    )
    (packages / "agent_harness" / "__init__.py").write_text("", encoding="utf-8")

    _edges, violations = check_contracts.check_dependency_directions(tmp_path)

    assert violations
    assert violations[0].edge.source_group == "tools"
    assert violations[0].edge.target_group == "agent_harness"


def test_event_and_tool_contract_reports_are_clean() -> None:
    event_report = check_contracts.check_event_consistency()
    tool_report = check_contracts.check_tool_registry()

    assert event_report["event_count"] == 54
    assert event_report["missing_metadata"] == []
    assert event_report["missing_in_reducer"] == []
    assert event_report["extra_in_reducer"] == []
    assert tool_report["errors"] == []
    assert len(tool_report["patch_ops"]) == 12
