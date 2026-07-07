"""素材理解子代理与 understand 工具（Spec C §C3）。"""

from .handlers import materials
from .subagent import (
    SubagentOutcome,
    SubagentSpec,
    TranscribeResult,
    run_understanding_subagent,
)

__all__ = [
    "SubagentOutcome",
    "SubagentSpec",
    "TranscribeResult",
    "materials",
    "run_understanding_subagent",
]
