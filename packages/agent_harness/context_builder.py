"""Context bundle rendering from structured DraftState."""

from __future__ import annotations

import json
import re
from collections.abc import Mapping, Sequence
from typing import Any, Protocol

from pydantic import BaseModel, ConfigDict, Field

from contracts.decision import Decision
from contracts.draft import DraftState
from contracts.subtitle import SubtitleClip
from contracts.timeline import TimelineMediaClip, TimelineState
from contracts.tool import ToolSpec
from domain.draft_stage import derive_stage
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
    "draft_header": 500,
    "artifacts": 6000,
    "pending_decision": 1000,
    "memory": 1500,
    "assets": 2000,
    "messages": 8000,
    "turn_observations": 2500,
    "allowed_tools": 1200,
}
FIXED_BLOCKS = frozenset(
    {"system", "workspace", "draft_header", "pending_decision", "allowed_tools"}
)


class ContextMessage(BaseModel):
    model_config = ConfigDict(extra="forbid")

    role: str
    content: str
    created_at: str | None = None
    draft_id: str | None = None


class AssetDigestRow(BaseModel):
    """单条素材摘要索引行：join assets 基础事实 + latest_ready 摘要（Spec C §C3）。"""

    model_config = ConfigDict(extra="forbid")

    asset_id: str
    filename: str
    kind: str
    duration_sec: float | None = None
    understanding_status: str = "none"
    semantic_role: str | None = None
    overall: str | None = None


class ContextBuildInput(BaseModel):
    model_config = ConfigDict(extra="forbid", arbitrary_types_allowed=True)

    preconditions: PreconditionContext
    decisions: tuple[Decision, ...] = Field(default_factory=tuple)
    pending_decision: Decision | None = None
    timeline: TimelineState | None = None
    memory_summaries: tuple[str, ...] = Field(default_factory=tuple)
    messages: tuple[ContextMessage, ...] = Field(default_factory=tuple)
    turn_observations: tuple[str, ...] = Field(default_factory=tuple)
    rolling_summary: str | None = None
    current_action: str | None = None
    allowed_tools: tuple[ToolSpec, ...] = Field(default_factory=tuple)
    asset_digest: tuple[AssetDigestRow, ...] = Field(default_factory=tuple)


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
            "workspace": _render_workspace_block(context.preconditions.draft_state),
            "draft_header": _render_draft_header_block(context),
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
            "assets": _render_assets_block(
                context.preconditions,
                context.asset_digest,
                self._budgets["assets"],
                self._counter,
            ),
            "messages": _render_messages_block(
                context.messages,
                context.rolling_summary,
                self._budgets["messages"],
                self._counter,
            ),
            "turn_observations": _render_turn_observations_block(
                context.turn_observations,
                self._budgets["turn_observations"],
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
            "DraftState is the source of truth; do not rely on lossy chat history for artifacts.",
            "PolicyGate exposes only allowed tool schemas and rechecks every tool call.",
            "Human-gated actions use Decision + PendingToolCall; do not execute them directly.",
            f"Context total budget target: {total_budget} tokens.",
            "",
            "阶段推进指引（draft_header 的 stage 字段标记当前阶段；工具列表按前置条件动态暴露，"
            "看不到的工具说明其前置未满足，先完成当前阶段的关键动作）：",
            "- briefing：从最低成本的证据开始、逐级升级，证据够了就停；不必为动手而理解全部素材，"
            "超出需要的深度理解是浪费。先读 assets 块与 asset.list_assets（免费；文件名/时长/尺寸/"
            "方向/音轨/状态往往已够判断素材构成）；需要看画面内容时再用 media.view_frames 对少量"
            "候选抽帧提问（昂贵，一次少量帧）；只对真正要进剪辑决策的素材调 understand.materials "
            "生成带时间戳摘要（昂贵；逐素材增量完成，再次调用同素材命中缓存直接回摘要），"
            "配合 audio.inspect_sources 检查音频。向用户提的问题必须落在素材的具体内容上"
            "（比如某段画面适不适合开头、这首 BGM 用不用），不要问“主题/平台/风格”这类不看素材"
            "也能问的问卷式问题。audio_plan 未确定且素材含人声时，必须用 interaction.ask_user "
            "创建 audio_mode 决策（原声粗剪 / TTS 配音 / 静音），这是解锁后续工具的唯一路径。",
            "- drafting：按 audio_plan 推进——原声：audio.asr_original → audio.rough_cut_speech；"
            "TTS：audio.generate_tts。cut_plan 与 timeline 就绪后 render.preview → "
            "interaction.show_preview；用户表达满意时用 interaction.confirm_action "
            "创建 approve_rough_cut 确认。",
            "- refining：粗剪已确认。逐项询问字幕与 BGM（timeline.apply_patch 的 "
            "generate_subtitles / add_bgm op；postprocess_plan 缺失时 gate 会自动转决策，"
            "属预期行为）；用户的修改指令走 timeline.apply_patch。",
            "- exporting：render.final_mp4（导出确认由 gate 自动创建决策）；导出完成后可"
            "建议 memory.extract_from_draft → memory.ask_scope 沉淀经验。",
            "同一只读工具同参数不要重复调用：结果不会变化，信息足够就立刻推进下一步动作。",
            "",
            "向用户提问的唯一方式是 interaction.ask_user：question 具体、options 给 2-4 个"
            "可直接点选的候选（description 可写推荐理由），绝不在正文里罗列问题清单等用户打字。"
            "能从素材内容、brief 或记忆推断的就自己拿主意，一次只问一个最关键的问题。",
            "输出格式：正文用 Markdown（列表/加粗/小标题）。带 tool_call 的 content 是一句话"
            "叙述（正在做什么、发现了什么），不要复读上一条叙述；最终回复直接给结论，"
            "不要把前面叙述过的内容再复述一遍。",
        )
    )


def _render_workspace_block(draft_state: DraftState | None) -> str:
    # 单级草稿模型：无上级项目实体，defaults 挂在草稿上（POST /drafts 时从 workspace 拷贝）。
    if draft_state is None:
        return "workspace: no active draft"
    defaults = draft_state.defaults
    return (
        f"defaults: aspect_ratio={defaults.aspect_ratio}, fps={defaults.fps}, "
        f"preview_quality={defaults.preview_quality}, export_quality={defaults.export_quality}"
    )


def _render_draft_header_block(context: ContextBuildInput) -> str:
    draft_state = context.preconditions.draft_state
    if draft_state is None:
        return "draft: none"
    jobs = ", ".join(
        f"{job.kind}:{job.status}:{job.progress if job.progress is not None else '-'}"
        for job in draft_state.running_jobs
    )
    last_error = (
        "none"
        if draft_state.last_error is None
        else f"{draft_state.last_error.error_code}: {draft_state.last_error.message}"
    )
    return "\n".join(
        (
            f"draft: {draft_state.name} ({draft_state.draft_id})",
            f"stage: {derive_stage(draft_state)}",
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
    draft_state = context.preconditions.draft_state
    if draft_state is None:
        return [("draft", "artifacts: no active draft")]
    parts: list[tuple[str, str]] = [
        (
            "brief",
            _section(
                "brief",
                {
                    "goal": draft_state.brief.goal,
                    "platform": draft_state.brief.platform,
                    "target_duration_sec": draft_state.brief.target_duration_sec,
                    "style_notes": draft_state.brief.style_notes,
                    "confirmed_facts": draft_state.brief.confirmed_facts,
                },
            ),
        )
    ]
    if draft_state.scratch_memory:
        parts.append(("scratch_memory", _section("scratch_memory", draft_state.scratch_memory)))
    if draft_state.content_plan is not None:
        parts.append(("content_plan", _section("content_plan", draft_state.content_plan)))
    if draft_state.audio_plan is not None:
        parts.append(
            (
                "audio_plan",
                _section("audio_plan", draft_state.audio_plan.model_dump(mode="json")),
            )
        )
    if draft_state.cut_plan is not None:
        parts.append(
            (
                "cut_plan",
                _section(
                    "cut_plan",
                    {
                        "slot_count": len(draft_state.cut_plan.slots),
                        "removed_range_count": len(draft_state.cut_plan.removed_ranges),
                        "total_target_duration_sec": draft_state.cut_plan.total_target_duration_sec,
                        "slots": [
                            {
                                "slot_id": slot.slot_id,
                                "brief": slot.brief,
                                "target_duration_sec": slot.target_duration_sec,
                            }
                            for slot in draft_state.cut_plan.slots
                        ],
                    },
                ),
            )
        )
    if context.timeline is not None:
        aspect_ratio = draft_state.defaults.aspect_ratio if draft_state is not None else "unknown"
        parts.append(
            (
                "timeline",
                render_timeline_summary(context.timeline, aspect_ratio=aspect_ratio),
            )
        )
    return parts


def _artifact_priority(name: str, current_action: str | None) -> int:
    if current_action is None or "timeline" in current_action or "render" in current_action:
        base = ["timeline", "cut_plan", "audio_plan", "content_plan", "brief"]
    elif "audio" in current_action:
        base = ["audio_plan", "content_plan", "brief", "cut_plan", "timeline"]
    else:
        base = ["brief", "content_plan", "audio_plan", "cut_plan", "timeline"]
    try:
        return base.index(name)
    except ValueError:
        return len(base)


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
    decision = context.pending_decision or _draft_pending_decision(context)
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


def _draft_pending_decision(context: ContextBuildInput) -> Decision | None:
    draft_state = context.preconditions.draft_state
    if draft_state is None or draft_state.pending_decision_id is None:
        return None
    for decision in context.decisions:
        if decision.decision_id == draft_state.pending_decision_id:
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


MAX_ASSET_INDEX_ROWS = 50
_OVERALL_LIMIT = 80
_FILENAME_LIMIT = 60
# 空白（含换行）与 C0/C1 控制字符、DEL：折叠成单空格，防止外部字符串伪造多行条目。
_INLINE_UNSAFE_RUN = re.compile(r"[\s\x00-\x1f\x7f-\x9f]+")


def _render_assets_block(
    context: PreconditionContext,
    digest: Sequence[AssetDigestRow],
    budget: int,
    counter: TokenCounter,
) -> str:
    stats = context.draft_artifacts
    # 单级草稿模型：链接存在即在册，无 selected/disabled 维度。
    header_lines = [
        "assets:",
        f"usable_count: {stats.usable_asset_count or len(stats.usable_asset_ids)}",
        f"with_audio_count: {len(stats.asset_ids_with_audio)}",
        f"transcript_assets: {', '.join(sorted(stats.transcript_asset_ids)) or 'none'}",
        "transcript_with_vad_assets: "
        f"{', '.join(sorted(stats.transcript_with_vad_asset_ids)) or 'none'}",
    ]
    total = len(digest)
    if total == 0:
        return "\n".join(header_lines)

    index_header = f"index (共 {total} 个素材):"
    index_lines = [_render_asset_index_line(row) for row in digest]
    cap = min(total, MAX_ASSET_INDEX_ROWS)
    shown = 0
    # 逐行贪心，行数上限 50 与预算双重约束，超出尾行「另有 N 个素材」。
    while shown < cap:
        omitted = total - (shown + 1)
        tail = [f"另有 {omitted} 个素材"] if omitted > 0 else []
        candidate = "\n".join((*header_lines, index_header, *index_lines[: shown + 1], *tail))
        if counter(candidate) > budget:
            break
        shown += 1

    omitted = total - shown
    lines = [*header_lines, index_header, *index_lines[:shown]]
    if omitted > 0:
        lines.append(f"另有 {omitted} 个素材")
    return "\n".join(lines)


def _render_asset_index_line(row: AssetDigestRow) -> str:
    duration = "时长未知" if row.duration_sec is None else f"{row.duration_sec:.1f}s"
    filename = _clip_inline(row.filename, _FILENAME_LIMIT)
    parts = [f"- {row.asset_id} {filename} [{row.kind}] {duration} 理解:{row.understanding_status}"]
    if row.semantic_role:
        parts.append(f"role={_fold_inline(row.semantic_role)}")
    if row.overall:
        parts.append(_clip_inline(row.overall, _OVERALL_LIMIT))
    return " · ".join(parts)


def _fold_inline(text: str) -> str:
    """外部来源字符串（文件名/模型输出）折叠成安全单行：控制字符与空白连跑变单空格。"""

    return _INLINE_UNSAFE_RUN.sub(" ", text).strip()


def _clip_inline(text: str, limit: int) -> str:
    folded = _fold_inline(text)
    if len(folded) <= limit:
        return folded
    return folded[:limit] + "…"


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


def _render_turn_observations_block(
    observations: tuple[str, ...],
    budget: int,
    counter: TokenCounter,
) -> str:
    """本回合已执行的工具与观察结果。

    工具 observation 不落消息表，turn 内多步规划全靠这里回灌——缺了它
    planner 每步都失忆，会无限重复同一个只读工具（M9 路径 1 实测）。
    超预算时保留最近的条目。
    """

    if not observations:
        return "本回合尚未执行任何工具。"
    lines = [f"- {item}" for item in observations]
    while lines and counter("\n".join(lines)) > budget:
        lines.pop(0)
    header = "本回合已执行（时间顺序，不要重复相同调用）："
    return header + "\n" + "\n".join(lines)


_COST_TIER_LABELS: dict[str, str] = {"free": "免费", "cheap": "便宜", "expensive": "昂贵"}
_COST_TIER_ORDER: dict[str, int] = {"free": 0, "cheap": 1, "expensive": 2}


def _render_allowed_tools_block(allowed_tools: Sequence[ToolSpec]) -> str:
    """能力目录：一行一工具，只给名字/成本/描述。

    完整参数 Schema 已随原生 tools 参数下发，这里再 dump 一份 JSON Schema 纯属重复占用
    上下文，故只保留一句话能力说明，并按成本从低到高分组，引导模型先用便宜工具取证。
    """

    if not allowed_tools:
        return "allowed_tools: none"
    ordered = sorted(
        allowed_tools,
        key=lambda spec: (_COST_TIER_ORDER[spec.cost_tier], spec.name),
    )
    lines = [
        "能力目录（完整参数 Schema 已在原生 tools 参数里；这里只列能力，按成本从低到高选用）：",
        *(
            f"- {spec.name}（{_COST_TIER_LABELS[spec.cost_tier]}）：{spec.description}"
            for spec in ordered
        ),
    ]
    return "\n".join(lines)


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
