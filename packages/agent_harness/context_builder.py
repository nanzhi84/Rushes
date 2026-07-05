"""Context bundle rendering from structured CaseState."""

from __future__ import annotations

import json
from collections.abc import Mapping, Sequence
from typing import Any, Protocol

from pydantic import BaseModel, ConfigDict, Field

from contracts.candidate import CandidatePack
from contracts.decision import Decision
from contracts.project import ProjectState
from contracts.subtitle import SubtitleClip
from contracts.timeline import TimelineMediaClip, TimelineState
from contracts.tool import ToolSpec
from domain.case_stage import derive_stage
from domain.preconditions import PreconditionContext

from .policy_gate import PolicyContext, PolicyGate


class TokenCounter(Protocol):
    def __call__(self, text: str) -> int:
        """Return an approximate token count for text."""


def heuristic_token_count(text: str) -> int:
    if text == "":
        return 0
    return max(1, len(text) // 4)


DEFAULT_BLOCK_BUDGETS: dict[str, int] = {
    "system": 1500,
    "workspace": 300,
    "case_header": 500,
    "artifacts": 6000,
    "pending_decision": 1000,
    "memory": 1500,
    "assets": 1000,
    "messages": 8000,
    "allowed_tools": 4000,
}
FIXED_BLOCKS = frozenset(
    {"system", "workspace", "case_header", "pending_decision", "allowed_tools"}
)


class ContextMessage(BaseModel):
    model_config = ConfigDict(extra="forbid")

    role: str
    content: str
    created_at: str | None = None
    case_id: str | None = None


class ContextBuildInput(BaseModel):
    model_config = ConfigDict(extra="forbid", arbitrary_types_allowed=True)

    preconditions: PreconditionContext
    decisions: tuple[Decision, ...] = Field(default_factory=tuple)
    pending_decision: Decision | None = None
    timeline: TimelineState | None = None
    candidate_pack: CandidatePack | None = None
    memory_summaries: tuple[str, ...] = Field(default_factory=tuple)
    messages: tuple[ContextMessage, ...] = Field(default_factory=tuple)
    rolling_summary: str | None = None
    current_action: str | None = None
    allowed_tools: tuple[ToolSpec, ...] = Field(default_factory=tuple)


class ContextBundle(BaseModel):
    model_config = ConfigDict(extra="forbid", arbitrary_types_allowed=True)

    blocks: dict[str, str]
    token_counts: dict[str, int]
    allowed_tools: list[ToolSpec]


class ContextBuilder:
    def __init__(
        self,
        *,
        policy_gate: PolicyGate | None = None,
        budgets: Mapping[str, int] | None = None,
        total_budget: int = 24_000,
        counter: TokenCounter | None = None,
    ) -> None:
        self._policy_gate = policy_gate
        self._budgets = {**DEFAULT_BLOCK_BUDGETS, **dict(budgets or {})}
        self._total_budget = total_budget
        self._counter = counter or heuristic_token_count

    def build(self, context: ContextBuildInput) -> ContextBundle:
        allowed_tools = list(context.allowed_tools)
        if not allowed_tools and self._policy_gate is not None:
            allowed_tools = self._policy_gate.compute_allowed_tools(
                PolicyContext(
                    preconditions=context.preconditions,
                    decisions=context.decisions,
                    pending_decision=context.pending_decision,
                )
            )
        blocks = {
            "system": _render_system_block(self._total_budget),
            "workspace": _render_workspace_block(context.preconditions.project_state),
            "case_header": _render_case_header_block(context),
            "artifacts": _render_artifacts_block(
                context,
                self._budgets["artifacts"],
                self._counter,
            ),
            "pending_decision": _render_pending_decision_block(context),
            "memory": _render_memory_block(
                context.memory_summaries,
                self._budgets["memory"],
                self._counter,
            ),
            "assets": _render_assets_block(context.preconditions),
            "messages": _render_messages_block(
                context.messages,
                context.rolling_summary,
                self._budgets["messages"],
                self._counter,
            ),
            "allowed_tools": _render_allowed_tools_block(allowed_tools),
        }
        token_counts = {name: self._counter(text) for name, text in blocks.items()}
        return ContextBundle(blocks=blocks, token_counts=token_counts, allowed_tools=allowed_tools)


def _render_system_block(total_budget: int) -> str:
    return "\n".join(
        (
            "You are Rushes, a chat-first local video editing agent.",
            "CaseState is the source of truth; do not rely on lossy chat history for artifacts.",
            "PolicyGate exposes only allowed tool schemas and rechecks every tool call.",
            "Human-gated actions use Decision + PendingToolCall; do not execute them directly.",
            f"Context total budget target: {total_budget} tokens.",
        )
    )


def _render_workspace_block(project_state: ProjectState | None) -> str:
    if project_state is None:
        return "workspace: no active project"
    defaults = project_state.defaults
    return (
        f"project: {project_state.name} ({project_state.project_id})\n"
        f"defaults: aspect_ratio={defaults.aspect_ratio}, fps={defaults.fps}, "
        f"preview_quality={defaults.preview_quality}, export_quality={defaults.export_quality}"
    )


def _render_case_header_block(context: ContextBuildInput) -> str:
    case_state = context.preconditions.case_state
    if case_state is None:
        return "case: none"
    jobs = ", ".join(
        f"{job.kind}:{job.status}:{job.progress if job.progress is not None else '-'}"
        for job in case_state.running_jobs
    )
    last_error = (
        "none"
        if case_state.last_error is None
        else f"{case_state.last_error.error_code}: {case_state.last_error.message}"
    )
    return "\n".join(
        (
            f"case: {case_state.name} ({case_state.case_id})",
            f"stage: {derive_stage(case_state)}",
            f"last_error: {last_error}",
            f"running_jobs: {jobs or 'none'}",
        )
    )


def _render_artifacts_block(
    context: ContextBuildInput,
    budget: int,
    counter: TokenCounter,
) -> str:
    parts = _artifact_parts(context)
    ordered = sorted(
        parts,
        key=lambda item: _artifact_priority(item[0], context.current_action),
    )
    selected: list[str] = []
    for _name, text in ordered:
        candidate = "\n\n".join((*selected, text)) if selected else text
        if counter(candidate) <= budget:
            selected.append(text)
    if not selected and ordered:
        return _truncate_text_to_budget(ordered[0][1], budget, counter)
    return "\n\n".join(selected)


def _artifact_parts(context: ContextBuildInput) -> list[tuple[str, str]]:
    case_state = context.preconditions.case_state
    if case_state is None:
        return [("case", "artifacts: no active case")]
    parts: list[tuple[str, str]] = [
        (
            "brief",
            _section(
                "brief",
                {
                    "goal": case_state.brief.goal,
                    "platform": case_state.brief.platform,
                    "target_duration_sec": case_state.brief.target_duration_sec,
                    "style_notes": case_state.brief.style_notes,
                    "confirmed_facts": case_state.brief.confirmed_facts,
                },
            ),
        )
    ]
    if case_state.scratch_memory:
        parts.append(("scratch_memory", _section("scratch_memory", case_state.scratch_memory)))
    if case_state.content_plan is not None:
        parts.append(("content_plan", _section("content_plan", case_state.content_plan)))
    if case_state.audio_plan is not None:
        parts.append(
            (
                "audio_plan",
                _section("audio_plan", case_state.audio_plan.model_dump(mode="json")),
            )
        )
    if case_state.cut_plan is not None:
        parts.append(
            (
                "cut_plan",
                _section(
                    "cut_plan",
                    {
                        "slot_count": len(case_state.cut_plan.slots),
                        "removed_range_count": len(case_state.cut_plan.removed_ranges),
                        "total_target_duration_sec": case_state.cut_plan.total_target_duration_sec,
                        "slots": [
                            {
                                "slot_id": slot.slot_id,
                                "brief": slot.brief,
                                "target_duration_sec": slot.target_duration_sec,
                            }
                            for slot in case_state.cut_plan.slots
                        ],
                    },
                ),
            )
        )
    if context.candidate_pack is not None:
        parts.append(("candidate_pack", _render_candidate_pack(context.candidate_pack)))
    if context.timeline is not None:
        aspect_ratio = (
            context.preconditions.project_state.defaults.aspect_ratio
            if context.preconditions.project_state is not None
            else "unknown"
        )
        parts.append(
            (
                "timeline",
                render_timeline_summary(context.timeline, aspect_ratio=aspect_ratio),
            )
        )
    return parts


def _artifact_priority(name: str, current_action: str | None) -> int:
    if current_action is None:
        base = ["timeline", "candidate_pack", "cut_plan", "audio_plan", "content_plan", "brief"]
    elif "timeline" in current_action or "render" in current_action:
        base = ["timeline", "cut_plan", "candidate_pack", "audio_plan", "content_plan", "brief"]
    elif "retrieval" in current_action or "candidate" in current_action:
        base = ["candidate_pack", "cut_plan", "audio_plan", "content_plan", "brief", "timeline"]
    elif "audio" in current_action:
        base = ["audio_plan", "content_plan", "brief", "cut_plan", "candidate_pack", "timeline"]
    else:
        base = ["brief", "content_plan", "audio_plan", "cut_plan", "candidate_pack", "timeline"]
    try:
        return base.index(name)
    except ValueError:
        return len(base)


def _render_candidate_pack(candidate_pack: CandidatePack) -> str:
    lines = [f"candidate_pack: {candidate_pack.candidate_pack_id}"]
    for slot in candidate_pack.slots:
        lines.append(f"- {slot.slot_id}: {slot.slot_brief}")
        for candidate in slot.candidates[:3]:
            lines.append(
                f"  * {candidate.candidate_id} {candidate.asset_id}/{candidate.clip_id}: "
                f"{candidate.summary_line}"
            )
    return "\n".join(lines)


def render_timeline_summary(timeline: TimelineState, *, aspect_ratio: str) -> str:
    duration_sec = timeline.duration_frames / timeline.fps
    lines = [
        f"Timeline v{timeline.version} · {duration_sec:.1f}s @{timeline.fps}fps · {aspect_ratio}"
    ]
    subtitle_clips = _subtitle_clips(timeline)
    visual_clips = [
        clip
        for track in timeline.tracks
        if track.track_id in {"visual_base", "visual_overlay"}
        for clip in track.clips
        if isinstance(clip, TimelineMediaClip)
    ]
    for clip in sorted(visual_clips, key=lambda item: item.timeline_start_frame):
        start = clip.timeline_start_frame / timeline.fps
        end = clip.timeline_end_frame / timeline.fps
        slot = clip.parent_block_id or clip.timeline_clip_id
        role = _role_label(clip.role)
        summary = _clip_summary(clip)
        subtitle = _overlapping_subtitle_text(clip, subtitle_clips)
        subtitle_part = f' 字幕:"{subtitle}"' if subtitle else ""
        lines.append(
            f"[{_format_sec(start)}-{_format_sec(end)}] {slot}  {role} "
            f"{clip.asset_id}/{clip.clip_id or '-'} 「{summary}」{subtitle_part}"
        )
    lines.append(_render_audio_line(timeline))
    return "\n".join(lines)


def _subtitle_clips(timeline: TimelineState) -> list[SubtitleClip]:
    return [
        clip
        for track in timeline.tracks
        if track.track_id == "subtitles"
        for clip in track.clips
        if isinstance(clip, SubtitleClip)
    ]


def _overlapping_subtitle_text(
    clip: TimelineMediaClip,
    subtitle_clips: Sequence[SubtitleClip],
) -> str | None:
    for subtitle in subtitle_clips:
        if (
            subtitle.timeline_start_frame < clip.timeline_end_frame
            and subtitle.timeline_end_frame > clip.timeline_start_frame
        ):
            return subtitle.text
    return None


def _render_audio_line(timeline: TimelineState) -> str:
    voiceover = _audio_track_summary(timeline, "voiceover")
    bgm = _audio_track_summary(timeline, "bgm")
    original_audio = _audio_track_summary(timeline, "original_audio")
    return (
        f"audio: voiceover({voiceover or '无'}) · "
        f"bgm: {bgm or '无'} · 原声: {'开' if original_audio else '关'}"
    )


def _audio_track_summary(timeline: TimelineState, track_id: str) -> str | None:
    for track in timeline.tracks:
        if track.track_id != track_id:
            continue
        clips = [clip for clip in track.clips if isinstance(clip, TimelineMediaClip)]
        if not clips:
            return None
        start = min(clip.timeline_start_frame for clip in clips) / timeline.fps
        end = max(clip.timeline_end_frame for clip in clips) / timeline.fps
        assets = ",".join(sorted({clip.asset_id for clip in clips}))
        return f"{assets} {_format_sec(start)}-{_format_sec(end)}s"
    return None


def _role_label(role: str) -> str:
    return {"a_roll": "A-roll", "b_roll": "B-roll"}.get(role, role)


def _clip_summary(clip: TimelineMediaClip) -> str:
    for effect in clip.effects:
        for key in ("summary", "label", "title"):
            value = effect.get(key)
            if isinstance(value, str) and value:
                return value
    return ""


def _format_sec(value: float) -> str:
    return f"{value:04.1f}"


def _render_pending_decision_block(context: ContextBuildInput) -> str:
    decision = context.pending_decision or _case_pending_decision(context)
    if decision is None:
        return "pending_decision: none"
    option_lines = [
        f"- {option.option_id}: {option.label}"
        + (f" ({option.description})" if option.description else "")
        for option in decision.options
    ]
    return "\n".join(
        (
            f"pending_decision: {decision.decision_id}",
            f"type: {decision.type}",
            f"question: {decision.question}",
            "options:",
            *(option_lines or ["- none"]),
        )
    )


def _case_pending_decision(context: ContextBuildInput) -> Decision | None:
    case_state = context.preconditions.case_state
    if case_state is None or case_state.pending_decision_id is None:
        return None
    for decision in context.decisions:
        if decision.decision_id == case_state.pending_decision_id:
            return decision
    return None


def _render_memory_block(
    memory_summaries: Sequence[str],
    budget: int,
    counter: TokenCounter,
) -> str:
    if not memory_summaries:
        return "memory: none"
    lines = [f"- {item}" for item in memory_summaries[:5]]
    return _fit_lines(lines, budget, counter)


def _render_assets_block(context: PreconditionContext) -> str:
    stats = context.project_artifacts
    case_state = context.case_state
    selected = case_state.selected_asset_ids if case_state is not None else []
    disabled = case_state.disabled_asset_ids if case_state is not None else []
    return "\n".join(
        (
            "assets:",
            f"usable_count: {stats.usable_asset_count or len(stats.usable_asset_ids)}",
            f"with_audio_count: {len(stats.asset_ids_with_audio)}",
            f"transcript_assets: {', '.join(sorted(stats.transcript_asset_ids)) or 'none'}",
            "transcript_with_vad_assets: "
            f"{', '.join(sorted(stats.transcript_with_vad_asset_ids)) or 'none'}",
            f"selected_asset_ids: {', '.join(selected) or 'none'}",
            f"disabled_asset_ids: {', '.join(disabled) or 'none'}",
        )
    )


def _render_messages_block(
    messages: Sequence[ContextMessage],
    rolling_summary: str | None,
    budget: int,
    counter: TokenCounter,
) -> str:
    lines: list[str] = []
    if rolling_summary:
        lines.append(f"earlier_summary: {rolling_summary}")
    recent_lines = [f"{message.role}: {message.content}" for message in messages]
    lines.extend(_fit_lines(recent_lines, budget, counter, from_end=True).splitlines())
    return _fit_lines(lines, budget, counter, from_end=True)


def _render_allowed_tools_block(allowed_tools: Sequence[ToolSpec]) -> str:
    payload = [
        {
            "name": spec.name,
            "namespace": spec.namespace,
            "description": spec.description,
            "input_schema": spec.input_model.model_json_schema(),
        }
        for spec in allowed_tools
    ]
    return json.dumps(payload, ensure_ascii=False, sort_keys=True)


def _section(name: str, payload: Mapping[str, Any]) -> str:
    return f"{name}: " + json.dumps(payload, ensure_ascii=False, sort_keys=True, default=str)


def _fit_lines(
    lines: Sequence[str],
    budget: int,
    counter: TokenCounter,
    *,
    from_end: bool = False,
) -> str:
    ordered = list(reversed(lines)) if from_end else list(lines)
    selected: list[str] = []
    for line in ordered:
        candidate = "\n".join((line, *selected)) if from_end else "\n".join((*selected, line))
        if counter(candidate) <= budget:
            if from_end:
                selected.insert(0, line)
            else:
                selected.append(line)
    if not selected and lines:
        return _truncate_text_to_budget(lines[-1 if from_end else 0], budget, counter)
    return "\n".join(selected)


def _truncate_text_to_budget(text: str, budget: int, counter: TokenCounter) -> str:
    if counter(text) <= budget:
        return text
    if budget <= 0:
        return ""
    suffix = "\n[truncated]"
    limit = max(1, budget * 4)
    truncated = text[:limit]
    while counter(truncated + suffix) > budget and truncated:
        step = max(1, len(truncated) // 10)
        truncated = truncated[:-step]
    if truncated:
        return truncated + suffix
    marker = "[truncated]"
    return marker if counter(marker) <= budget else ""
