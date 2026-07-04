from pydantic import BaseModel, ConfigDict

from agent_harness.context_builder import (
    ContextBuilder,
    ContextBuildInput,
    ContextMessage,
    render_timeline_summary,
)
from contracts.case import CaseState
from contracts.project import ProjectState
from contracts.timeline import TimelineState
from contracts.tool import ToolSpec
from domain.preconditions import PreconditionContext, ProjectArtifactStats


class EmptyInput(BaseModel):
    model_config = ConfigDict(extra="forbid")


def _case_state(**overrides: object) -> CaseState:
    data = {
        "case_id": "case_1",
        "project_id": "project_1",
        "name": "Case",
        "brief": {"goal": "make a video", "confirmed_facts": []},
        "selected_asset_ids": ["asset_1"],
        "disabled_asset_ids": ["asset_bad"],
        "scratch_memory": {},
    }
    data.update(overrides)
    return CaseState.model_validate(data)


def _project_state() -> ProjectState:
    return ProjectState.model_validate(
        {
            "project_id": "project_1",
            "name": "Project",
            "status": "active",
            "asset_links": [],
            "case_ids": ["case_1"],
            "memory_ids": [],
            "defaults": {"aspect_ratio": "9:16", "fps": 30},
            "created_at": "2026-07-04T00:00:00Z",
            "updated_at": "2026-07-04T00:00:00Z",
        }
    )


def _tool_spec() -> ToolSpec:
    return ToolSpec(
        name="respond",
        namespace="interaction",
        version="1",
        input_model=EmptyInput,
        result_model=None,
        handler_ref="handlers.respond",
        allowed_scopes=["case_agent_console"],
        requires_artifacts=[],
        requires_active_project=True,
        requires_active_case=False,
        side_effects=[],
        emits_events=[],
        description="respond",
    )


def _timeline() -> TimelineState:
    return TimelineState.model_validate(
        {
            "timeline_id": "tl_1",
            "case_id": "case_1",
            "version": 8,
            "fps": 30,
            "duration_frames": 1350,
            "tracks": [
                {
                    "track_id": "visual_base",
                    "track_type": "primary_visual",
                    "clips": [
                        {
                            "timeline_clip_id": "tlc_1",
                            "track_id": "visual_base",
                            "asset_id": "asset_007",
                            "clip_id": "clip_002",
                            "role": "a_roll",
                            "timeline_start_frame": 0,
                            "timeline_end_frame": 96,
                            "source_start_frame": 0,
                            "source_end_frame": 96,
                            "parent_block_id": "slot_hook",
                            "effects": [{"summary": "开箱特写"}],
                        }
                    ],
                },
                {"track_id": "visual_overlay", "track_type": "visual_overlay", "clips": []},
                {
                    "track_id": "original_audio",
                    "track_type": "audio",
                    "clips": [
                        {
                            "timeline_clip_id": "aud_1",
                            "track_id": "original_audio",
                            "asset_id": "asset_007",
                            "clip_id": "clip_002",
                            "role": "original_audio",
                            "timeline_start_frame": 0,
                            "timeline_end_frame": 96,
                            "source_start_frame": 0,
                            "source_end_frame": 96,
                        }
                    ],
                },
                {"track_id": "voiceover", "track_type": "audio", "clips": []},
                {"track_id": "bgm", "track_type": "audio", "clips": []},
                {
                    "track_id": "subtitles",
                    "track_type": "text",
                    "clips": [
                        {
                            "timeline_clip_id": "sub_1",
                            "track_id": "subtitles",
                            "text": "这瓶精华我回购三次了",
                            "timeline_start_frame": 0,
                            "timeline_end_frame": 96,
                            "style_template_id": "s",
                            "binding": {"kind": "original_audio", "utterance_id": "u_1"},
                            "safe_area_check": "ok",
                        }
                    ],
                },
            ],
        }
    )


def test_timeline_summary_uses_prd_shape() -> None:
    summary = render_timeline_summary(_timeline(), aspect_ratio="9:16")

    assert summary.splitlines()[0] == "Timeline v8 · 45.0s @30fps · 9:16"
    assert "[00.0-03.2] slot_hook  A-roll asset_007/clip_002" in summary
    assert '字幕:"这瓶精华我回购三次了"' in summary
    assert "audio:" in summary


def test_context_builder_truncates_budgeted_blocks_but_not_fixed_blocks() -> None:
    builder = ContextBuilder(
        budgets={"system": 1, "artifacts": 20, "messages": 40},
        counter=len,
    )
    context = ContextBuildInput(
        preconditions=PreconditionContext(
            case_state=_case_state(brief={"goal": "x" * 500, "confirmed_facts": []}),
            project_state=_project_state(),
            project_artifacts=ProjectArtifactStats(
                usable_asset_count=2,
                transcript_asset_ids=frozenset({"asset_1"}),
            ),
        ),
        messages=(
            ContextMessage(role="user", content="old " * 100),
            ContextMessage(role="assistant", content="recent"),
        ),
        rolling_summary="older summary",
        allowed_tools=(_tool_spec(),),
    )

    bundle = builder.build(context)

    assert "[truncated]" not in bundle.blocks["system"]
    assert "truncated" in bundle.blocks["artifacts"]
    assert "recent" in bundle.blocks["messages"]
    assert bundle.token_counts["system"] > 1
    assert [tool.name for tool in bundle.allowed_tools] == ["respond"]
