"""Tool and patch-op registries."""

from __future__ import annotations

from collections.abc import Awaitable, Callable, Iterable, Mapping
from dataclasses import dataclass
from typing import Any

from pydantic import BaseModel

from contracts.events import event_registry
from contracts.tool import PatchOpSpec, ToolSpec
from contracts.tool_result import ToolResult
from domain.decision_effects import validate_decision_type_registered
from domain.preconditions import assert_known_preconditions

from .context import ToolExecutionContext

# 工具 handler 既可同步返回 ToolResult，也可为 async def 返回 Awaitable[ToolResult]；
# 执行侧（agent_harness._execute_tool）统一在事件循环内 await 兼容两者。
ToolHandler = Callable[[Any, ToolExecutionContext], ToolResult | Awaitable[ToolResult]]


@dataclass(frozen=True, slots=True)
class RegisteredTool:
    spec: ToolSpec
    handler: ToolHandler


class ToolRegistry:
    """Versioned registry for ToolSpec declarations and handlers."""

    def __init__(self) -> None:
        self._tools: dict[str, dict[str, RegisteredTool]] = {}

    def register(self, spec: ToolSpec, handler: ToolHandler) -> None:
        _validate_tool_spec(spec)
        versions = self._tools.setdefault(spec.name, {})
        if spec.version in versions:
            raise ValueError(f"tool already registered: {spec.name}@{spec.version}")
        versions[spec.version] = RegisteredTool(spec=spec, handler=handler)

    def get(
        self,
        name: str,
        *,
        version: str | None = None,
        include_experimental: bool = False,
    ) -> RegisteredTool | None:
        versions = self._tools.get(name)
        if versions is None:
            return None
        if version is not None:
            registered = versions.get(version)
            if registered is None:
                return None
            if registered.spec.status == "experimental" and not include_experimental:
                return None
            if registered.spec.status == "deprecated":
                return None
            return registered
        return _latest_visible_tool(versions.values(), include_experimental=include_experimental)

    def require(
        self,
        name: str,
        *,
        version: str | None = None,
        include_experimental: bool = False,
    ) -> RegisteredTool:
        registered = self.get(
            name,
            version=version,
            include_experimental=include_experimental,
        )
        if registered is None:
            raise KeyError(f"tool is not registered or not visible: {name}")
        return registered

    def list_stable(self, *, exposure: str | None = None) -> list[ToolSpec]:
        specs = [
            registered.spec for registered in self._latest_per_name(include_experimental=False)
        ]
        if exposure is not None:
            specs = [spec for spec in specs if spec.exposure == exposure]
        return sorted(specs, key=lambda spec: spec.name)

    def list_visible(
        self,
        *,
        include_experimental: bool = False,
        exposure: str | None = None,
    ) -> list[ToolSpec]:
        specs = [
            registered.spec
            for registered in self._latest_per_name(include_experimental=include_experimental)
        ]
        if exposure is not None:
            specs = [spec for spec in specs if spec.exposure == exposure]
        return sorted(specs, key=lambda spec: spec.name)

    def specs_by_name(self, *, include_experimental: bool = False) -> dict[str, ToolSpec]:
        return {
            registered.spec.name: registered.spec
            for registered in self._latest_per_name(include_experimental=include_experimental)
        }

    def handlers_by_name(self, *, include_experimental: bool = False) -> dict[str, ToolHandler]:
        return {
            registered.spec.name: registered.handler
            for registered in self._latest_per_name(include_experimental=include_experimental)
        }

    def _latest_per_name(self, *, include_experimental: bool) -> list[RegisteredTool]:
        visible: list[RegisteredTool] = []
        for versions in self._tools.values():
            registered = _latest_visible_tool(
                versions.values(),
                include_experimental=include_experimental,
            )
            if registered is not None:
                visible.append(registered)
        return visible


class PatchOpRegistry:
    """Registry for TimelinePatch op-level policy metadata."""

    def __init__(self, specs: Iterable[PatchOpSpec] = ()) -> None:
        self._specs: dict[str, PatchOpSpec] = {}
        for spec in specs:
            self.register(spec)

    def register(self, spec: PatchOpSpec) -> None:
        _validate_patch_op_spec(spec)
        if spec.kind in self._specs:
            raise ValueError(f"patch op already registered: {spec.kind}")
        self._specs[spec.kind] = spec

    def get(self, kind: str) -> PatchOpSpec | None:
        return self._specs.get(kind)

    def require(self, kind: str) -> PatchOpSpec:
        spec = self.get(kind)
        if spec is None:
            raise KeyError(f"patch op is not registered: {kind}")
        return spec

    def as_mapping(self) -> Mapping[str, PatchOpSpec]:
        return dict(self._specs)

    def list(self) -> list[PatchOpSpec]:
        return [self._specs[kind] for kind in sorted(self._specs)]


def _validate_tool_spec(spec: ToolSpec) -> None:
    known_events = set(event_registry())
    unknown_events = sorted(set(spec.emits_events) - known_events)
    if unknown_events:
        raise ValueError(
            f"{spec.name}@{spec.version} emits unknown events: {', '.join(unknown_events)}"
        )
    assert_known_preconditions(spec.requires_artifacts)
    if spec.requires_confirmation and spec.confirmation_decision_type is None:
        raise ValueError(f"{spec.name}@{spec.version} requires confirmation_decision_type")
    if spec.confirmation_decision_type is not None:
        validate_decision_type_registered(spec.confirmation_decision_type)
    if not issubclass(spec.input_model, BaseModel):
        raise TypeError(f"{spec.name}@{spec.version} input_model must be a Pydantic model")


def _validate_patch_op_spec(spec: PatchOpSpec) -> None:
    assert_known_preconditions(spec.requires_artifacts)
    if spec.requires_confirmation and spec.confirmation_decision_type is None:
        raise ValueError(f"{spec.kind} requires confirmation_decision_type")
    if spec.confirmation_decision_type is not None:
        validate_decision_type_registered(spec.confirmation_decision_type)
    if not issubclass(spec.params_model, BaseModel):
        raise TypeError(f"{spec.kind} params_model must be a Pydantic model")


def _latest_visible_tool(
    tools: Iterable[RegisteredTool],
    *,
    include_experimental: bool,
) -> RegisteredTool | None:
    stable = [tool for tool in tools if tool.spec.status == "stable"]
    if stable and not include_experimental:
        return max(stable, key=lambda tool: _version_key(tool.spec.version))
    candidates = [
        tool
        for tool in tools
        if tool.spec.status == "stable"
        or (include_experimental and tool.spec.status == "experimental")
    ]
    if not candidates:
        return None
    return max(candidates, key=lambda tool: _version_key(tool.spec.version))


def _version_key(version: str) -> tuple[tuple[int, int | str], ...]:
    parts: list[tuple[int, int | str]] = []
    for raw_part in version.replace("-", ".").split("."):
        if raw_part.isdigit():
            parts.append((0, int(raw_part)))
        else:
            parts.append((1, raw_part))
    return tuple(parts)
