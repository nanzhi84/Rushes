"""Static contract checks required by the Rushes PRD."""

from __future__ import annotations

import ast
import sys
from dataclasses import dataclass
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[1]
PACKAGES = ROOT / "packages"
if str(PACKAGES) not in sys.path:
    sys.path.insert(0, str(PACKAGES))
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

LOCAL_GROUPS: frozenset[str] = frozenset(
    {
        "contracts",
        "domain",
        "storage",
        "events",
        "media",
        "providers",
        "agent_harness",
        "tools",
        "apps",
    }
)

ALLOWED_IMPORTS: dict[str, frozenset[str]] = {
    "contracts": frozenset({"contracts"}),
    "domain": frozenset({"domain", "contracts"}),
    "storage": frozenset({"storage", "contracts"}),
    "events": frozenset({"events", "contracts", "storage"}),
    "media": frozenset({"media", "contracts", "storage"}),
    "providers": frozenset({"providers", "contracts"}),
    "agent_harness": frozenset(
        {"agent_harness", "tools", "domain", "contracts", "storage", "events"}
    ),
    # PRD §15：TOOLS→IMPL 边（media/providers/timeline）
    "tools": frozenset(
        {
            "tools",
            "domain",
            "contracts",
            "storage",
            "events",
            "media",
            "providers",
            "timeline",
        }
    ),
    "apps": LOCAL_GROUPS,
}

# 单级草稿模型定版计数（PRD §4.5 事件表 / §6 工具契约）：偏离即视为契约漂移，必须显式改动此处。
EXPECTED_EVENT_COUNT = 45
EXPECTED_TOOL_COUNT = 31

EXPECTED_PATCH_OPS: frozenset[str] = frozenset(
    {
        "delete_range",
        "replace_clip",
        "reorder_blocks",
        "trim_clip",
        "insert_clip",
        "generate_subtitles",
        "set_subtitle_style",
        "edit_subtitle_text",
        "remove_track_clips",
        "add_bgm",
        "adjust_gain",
        "set_playback_rate",
    }
)


@dataclass(frozen=True, slots=True)
class ImportEdge:
    source_group: str
    target_group: str
    path: Path
    line: int
    module: str


@dataclass(frozen=True, slots=True)
class ImportViolation:
    edge: ImportEdge
    reason: str


def check_dependency_directions(
    root: Path = ROOT,
) -> tuple[list[ImportEdge], list[ImportViolation]]:
    edges = _collect_import_edges(root)
    violations: list[ImportViolation] = []
    for edge in edges:
        allowed = ALLOWED_IMPORTS[edge.source_group]
        if edge.target_group not in allowed:
            violations.append(ImportViolation(edge=edge, reason="import direction is not allowed"))
    graph: dict[str, set[str]] = {group: set() for group in LOCAL_GROUPS}
    for edge in edges:
        if edge.source_group != edge.target_group:
            graph[edge.source_group].add(edge.target_group)
    cycle = _find_cycle(graph)
    if cycle is not None:
        path = next((edge.path for edge in edges if edge.source_group == cycle[0]), root)
        violations.append(
            ImportViolation(
                edge=ImportEdge(cycle[0], cycle[1], path, 0, " -> ".join(cycle)),
                reason="local import cycle detected",
            )
        )
    return edges, violations


def check_event_consistency() -> dict[str, Any]:
    from agent_harness.reducer import REDUCER_DISPATCH_EVENTS
    from contracts.events import EVENT_CLASSES, event_registry

    registry = event_registry()
    missing_metadata = [
        event_class.__name__
        for event_class in EVENT_CLASSES
        if not hasattr(event_class, "version_mode") or not hasattr(event_class, "merge_key")
    ]
    event_names = set(registry)
    reducer_names = set(REDUCER_DISPATCH_EVENTS)
    return {
        "event_count": len(event_names),
        "missing_metadata": missing_metadata,
        "missing_in_reducer": sorted(event_names - reducer_names),
        "extra_in_reducer": sorted(reducer_names - event_names),
    }


def check_tool_registry() -> dict[str, Any]:
    from contracts.events import event_registry
    from domain.preconditions import assert_known_preconditions
    from tools import PATCH_OP_REGISTRY, build_default_tool_registry

    known_events = set(event_registry())
    registry = build_default_tool_registry()
    tools = registry.list_visible()
    rows = []
    errors: list[str] = []
    for spec in tools:
        try:
            assert_known_preconditions(spec.requires_artifacts)
        except ValueError as exc:
            errors.append(f"{spec.name}: {exc}")
        unknown_events = sorted(set(spec.emits_events) - known_events)
        if unknown_events:
            errors.append(f"{spec.name}: unknown events {unknown_events}")
        rows.append(
            {
                "name": spec.name,
                "requires_artifacts": ",".join(spec.requires_artifacts) or "-",
                "emits_events": ",".join(spec.emits_events) or "-",
                "confirmation": spec.confirmation_decision_type or "-",
            }
        )
    if len(rows) != EXPECTED_TOOL_COUNT:
        errors.append(f"registered tool count: expected {EXPECTED_TOOL_COUNT}, got {len(rows)}")
    patch_ops = {spec.kind for spec in PATCH_OP_REGISTRY.list()}
    missing_patch_ops = sorted(EXPECTED_PATCH_OPS - patch_ops)
    extra_patch_ops = sorted(patch_ops - EXPECTED_PATCH_OPS)
    if missing_patch_ops:
        errors.append(f"missing patch ops: {missing_patch_ops}")
    if extra_patch_ops:
        errors.append(f"extra patch ops: {extra_patch_ops}")
    return {
        "tool_rows": rows,
        "patch_ops": sorted(patch_ops),
        "errors": errors,
    }


def main() -> int:
    edges, import_violations = check_dependency_directions(ROOT)
    event_report = check_event_consistency()
    tool_report = check_tool_registry()

    print("Dependency edges")
    _print_table(
        [
            {
                "source": edge.source_group,
                "target": edge.target_group,
                "file": str(edge.path.relative_to(ROOT)),
                "line": edge.line,
                "module": edge.module,
            }
            for edge in edges
        ],
        ("source", "target", "file", "line", "module"),
    )
    print()
    print("Event coverage")
    _print_table(
        [
            {"check": "event_count", "value": event_report["event_count"]},
            {
                "check": "missing_metadata",
                "value": ",".join(event_report["missing_metadata"]) or "-",
            },
            {
                "check": "missing_in_reducer",
                "value": ",".join(event_report["missing_in_reducer"]) or "-",
            },
            {
                "check": "extra_in_reducer",
                "value": ",".join(event_report["extra_in_reducer"]) or "-",
            },
        ],
        ("check", "value"),
    )
    print()
    print("Tool registry")
    _print_table(
        tool_report["tool_rows"],
        ("name", "requires_artifacts", "emits_events", "confirmation"),
    )
    print()
    print("Patch ops")
    _print_table([{"kind": kind} for kind in tool_report["patch_ops"]], ("kind",))

    errors = []
    errors.extend(_format_import_violation(violation) for violation in import_violations)
    if event_report["event_count"] != EXPECTED_EVENT_COUNT:
        errors.append(
            f"event count: expected {EXPECTED_EVENT_COUNT}, got {event_report['event_count']}"
        )
    errors.extend(
        f"event consistency: {key}={event_report[key]}"
        for key in ("missing_metadata", "missing_in_reducer", "extra_in_reducer")
        if event_report[key]
    )
    errors.extend(f"tool registry: {error}" for error in tool_report["errors"])
    if errors:
        print()
        print("Contract check failures")
        for error in errors:
            print(f"- {error}")
        return 1
    return 0


def _collect_import_edges(root: Path) -> list[ImportEdge]:
    edges: list[ImportEdge] = []
    for file_path in _iter_python_files(root):
        source_group = _source_group(root, file_path)
        if source_group is None:
            continue
        tree = ast.parse(file_path.read_text(encoding="utf-8"), filename=str(file_path))
        for node in ast.walk(tree):
            module = _imported_module(node, source_group)
            if module is None:
                continue
            target_group = _target_group(module)
            if target_group is None:
                continue
            edges.append(
                ImportEdge(
                    source_group=source_group,
                    target_group=target_group,
                    path=file_path,
                    line=getattr(node, "lineno", 0),
                    module=module,
                )
            )
    return edges


def _iter_python_files(root: Path) -> list[Path]:
    search_roots = [root / "packages", root / "apps"]
    files: list[Path] = []
    for search_root in search_roots:
        if not search_root.exists():
            continue
        files.extend(path for path in search_root.rglob("*.py") if "__pycache__" not in path.parts)
    return sorted(files)


def _source_group(root: Path, file_path: Path) -> str | None:
    relative = file_path.relative_to(root)
    if len(relative.parts) < 2:
        return None
    if relative.parts[0] == "packages":
        group = relative.parts[1]
        return group if group in LOCAL_GROUPS else None
    if relative.parts[0] == "apps":
        return "apps"
    return None


def _imported_module(node: ast.AST, source_group: str) -> str | None:
    if isinstance(node, ast.Import):
        if not node.names:
            return None
        return node.names[0].name
    if isinstance(node, ast.ImportFrom):
        if node.level:
            return source_group
        return node.module
    return None


def _target_group(module: str | None) -> str | None:
    if module is None:
        return None
    first = module.split(".", 1)[0]
    return first if first in LOCAL_GROUPS else None


def _find_cycle(graph: dict[str, set[str]]) -> tuple[str, ...] | None:
    visiting: set[str] = set()
    visited: set[str] = set()
    stack: list[str] = []

    def visit(node: str) -> tuple[str, ...] | None:
        if node in visiting:
            start = stack.index(node)
            return tuple((*stack[start:], node))
        if node in visited:
            return None
        visiting.add(node)
        stack.append(node)
        for target in sorted(graph[node]):
            cycle = visit(target)
            if cycle is not None:
                return cycle
        stack.pop()
        visiting.remove(node)
        visited.add(node)
        return None

    for node in sorted(graph):
        cycle = visit(node)
        if cycle is not None:
            return cycle
    return None


def _print_table(rows: list[dict[str, Any]], headers: tuple[str, ...]) -> None:
    if not rows:
        print("(none)")
        return
    widths = {
        header: max(len(header), *(len(str(row.get(header, ""))) for row in rows))
        for header in headers
    }
    print(" | ".join(header.ljust(widths[header]) for header in headers))
    print(" | ".join("-" * widths[header] for header in headers))
    for row in rows:
        print(" | ".join(str(row.get(header, "")).ljust(widths[header]) for header in headers))


def _format_import_violation(violation: ImportViolation) -> str:
    edge = violation.edge
    return (
        f"{edge.path}:{edge.line}: {edge.source_group} -> {edge.target_group} "
        f"({edge.module}): {violation.reason}"
    )


if __name__ == "__main__":
    raise SystemExit(main())
