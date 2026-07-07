from pydantic import BaseModel

from contracts import (
    AssetRecord,
    CaseState,
    CutPlan,
    Decision,
    ProjectState,
    ResolvedTimelinePatch,
    TimelinePatchRequest,
    TimelineState,
    ToolResult,
    TranscriptDocument,
)


def assert_round_trip(model: type[BaseModel], payload: dict[str, object]) -> None:
    parsed = model.model_validate(payload)
    dumped = parsed.model_dump(mode="json", by_alias=True)
    reparsed = model.model_validate(dumped)
    assert reparsed == parsed


def test_case_state_prd_example_round_trips() -> None:
    assert_round_trip(
        CaseState,
        {
            "case_id": "case_007",
            "project_id": "project_001",
            "name": "产品种草第一版",
            "state_version": 31,
            "status": "active",
            "pending_decision_id": None,
            "running_jobs": [],
            "last_error": None,
            "brief": {
                "goal": "30 秒小红书种草",
                "platform": "xiaohongshu",
                "target_duration_sec": 30,
                "style_notes": ["快节奏", "字幕要白色"],
                "confirmed_facts": [],
            },
            "content_plan": None,
            "audio_plan": None,
            "cut_plan": None,
            "timeline_current_version": None,
            "timeline_validated": False,
            "preview_current_id": None,
            "last_viewed_preview_id": None,
            "rough_cut_approved": False,
            "rough_cut_approved_version": None,
            "postprocess_plan": None,
            "export_current_id": None,
            "selected_asset_ids": [],
            "disabled_asset_ids": [],
            "scratch_memory": {},
            "messages_tail_ref": "msg_tail_007",
        },
    )


def test_cut_plan_prd_example_round_trips() -> None:
    assert_round_trip(
        CutPlan,
        {
            "schema": "CutPlan.v1",
            "slots": [
                {
                    "slot_id": "slot_hook",
                    "brief": "开头钩子",
                    "target_duration_sec": [2.0, 4.0],
                    "narration_ref": {"utterance_ids": ["u_001"]},
                }
            ],
            "removed_ranges": [
                {
                    "start_ms": 7000,
                    "end_ms": 8400,
                    "kind": "filler",
                    "source": "approve_speech_cut",
                }
            ],
            "total_target_duration_sec": 30,
        },
    )


def test_project_state_prd_example_round_trips() -> None:
    assert_round_trip(
        ProjectState,
        {
            "project_id": "project_001",
            "name": "七月产品内容",
            "status": "active",
            "asset_links": [
                {"asset_id": "asset_001", "enabled": True, "linked_at": "...", "note": ""}
            ],
            "case_ids": ["case_001", "case_002"],
            "memory_ids": ["mem_project_001"],
            "defaults": {
                "aspect_ratio": "9:16",
                "fps": 30,
                "preview_quality": "low",
                "export_quality": "high",
            },
            "created_at": "...",
            "updated_at": "...",
        },
    )


def test_asset_record_prd_example_round_trips() -> None:
    assert_round_trip(
        AssetRecord,
        {
            "asset_id": "asset_001",
            "storage_mode": "reference",
            "workspace_object_uri": None,
            "reference_path": "/Users/me/Movies/raw/a.mp4",
            "kind": "video",
            "source": "local_path",
            "filename": "source.mp4",
            "hash": "sha256:...",
            "mtime": 1751600000,
            "size": 1048576000,
            "probe": {
                "duration_sec": 48.2,
                "fps": 29.97,
                "width": 1080,
                "height": 1920,
                "has_audio": True,
            },
            "proxy_object_uri": "object://...",
            "ingest_status": "imported",
            "usable": False,
            "failure": None,
        },
    )


def test_transcript_document_prd_example_round_trips() -> None:
    assert_round_trip(
        TranscriptDocument,
        {
            "schema": "TranscriptDocument.v1",
            "transcript_id": "tr_001",
            "asset_id": "asset_001",
            "language": "zh",
            "provider_id": "aliyun_paraformer_v2",
            "raw_preserved": True,
            "utterances": [
                {
                    "utterance_id": "u_001",
                    "text": "呃这个产品我用了三周",
                    "start_ms": 1200,
                    "end_ms": 4800,
                    "words": [{"w": "呃", "start_ms": 1200, "end_ms": 1450, "type": "filler"}],
                }
            ],
            "vad_segments": [{"start_ms": 0, "end_ms": 1200, "kind": "silence"}],
            "warnings": [],
        },
    )


def test_decision_prd_example_round_trips() -> None:
    assert_round_trip(
        Decision,
        {
            "decision_id": "dec_001",
            "scope_type": "case",
            "project_id": "project_001",
            "case_id": "case_007",
            "type": "audio_mode",
            "question": "原视频里有人声，这次怎么处理声音？",
            "options": [
                {"option_id": "keep_original", "label": "保留原声"},
                {"option_id": "rough_cut", "label": "口播粗剪"},
                {"option_id": "uploaded_voiceover", "label": "使用上传配音"},
                {"option_id": "tts", "label": "使用 TTS"},
                {"option_id": "silent", "label": "无旁白视频"},
            ],
            "allow_free_text": True,
            "status": "answered",
            "answer": {
                "option_id": "rough_cut",
                "free_text": None,
                "answered_via": "button",
            },
            "pending_tool_call": None,
            "pending_tool_call_status": None,
            "consumed_at": None,
            "replayed_tool_call_id": None,
            "blocking": True,
            "created_by_tool_call_id": "tc_001",
        },
    )


def test_timeline_state_prd_example_round_trips() -> None:
    assert_round_trip(
        TimelineState,
        {
            "timeline_id": "tl_001",
            "case_id": "case_007",
            "version": 8,
            "fps": 30,
            "duration_frames": 1350,
            "tracks": [
                {
                    "track_id": "visual_base",
                    "track_type": "primary_visual",
                    "clips": [
                        {
                            "timeline_clip_id": "tc_019",
                            "track_id": "visual_base",
                            "asset_id": "asset_007",
                            "clip_id": "clip_002",
                            "role": "a_roll",
                            "timeline_start_frame": 300,
                            "timeline_end_frame": 420,
                            "source_start_frame": 372,
                            "source_end_frame": 492,
                            "playback_rate": 1.0,
                            "lock_policy": "free",
                            "parent_block_id": "block_003",
                            "effects": [],
                            "gain_db": 0.0,
                        }
                    ],
                },
                {"track_id": "visual_overlay", "track_type": "visual_overlay", "clips": []},
                {"track_id": "original_audio", "track_type": "audio", "clips": []},
                {"track_id": "voiceover", "track_type": "audio", "clips": []},
                {"track_id": "bgm", "track_type": "audio", "clips": []},
                {
                    "track_id": "subtitles",
                    "track_type": "text",
                    "clips": [
                        {
                            "timeline_clip_id": "tc_042",
                            "track_id": "subtitles",
                            "text": "这瓶精华我回购三次了",
                            "timeline_start_frame": 0,
                            "timeline_end_frame": 96,
                            "style_template_id": "subtitle_tpl_03",
                            "binding": {"kind": "voiceover", "utterance_id": "u_001"},
                            "safe_area_check": "ok",
                        }
                    ],
                },
            ],
            "parent_version": 7,
            "created_by_patch_id": "patch_012",
            "validation_report": {"valid": True, "checks": []},
        },
    )


def test_timeline_patch_request_prd_example_round_trips() -> None:
    assert_round_trip(
        TimelinePatchRequest,
        {
            "schema": "TimelinePatchRequest.v1",
            "case_id": "case_007",
            "reference": {"timeline_version": 8, "preview_id": "prev_008"},
            "op": {
                "kind": "delete_range",
                "time_range_sec": [7.0, 8.4],
                "scope": "all_tracks",
                "ripple": True,
            },
            "reason": "用户要求删掉 7 秒附近的停顿",
        },
    )


def test_resolved_timeline_patch_prd_example_round_trips() -> None:
    assert_round_trip(
        ResolvedTimelinePatch,
        {
            "schema": "ResolvedTimelinePatch.v1",
            "patch_id": "patch_012",
            "request_ref": {
                "schema": "TimelinePatchRequest.v1",
                "case_id": "case_007",
                "reference": {"timeline_version": 8, "preview_id": "prev_008"},
                "op": {
                    "kind": "delete_range",
                    "time_range_sec": [7.0, 8.4],
                    "scope": "all_tracks",
                    "ripple": True,
                },
                "reason": "用户要求删掉 7 秒附近的停顿",
            },
            "resolved": {
                "start_frame": 210,
                "end_frame": 252,
                "affected_clip_ids": ["tc_019"],
            },
            "produced_timeline_version": 10,
        },
    )


def test_tool_result_prd_example_round_trips() -> None:
    assert_round_trip(
        ToolResult,
        {
            "tool_call_id": "tc_001",
            "tool_name": "timeline.apply_patch",
            "status": "succeeded",
            "observation": "已删除 7.0s-8.4s 的停顿，并同步字幕。",
            "artifacts": [{"artifact_id": "tl_v9", "kind": "timeline_version"}],
            "events": [
                {
                    "event": "TimelineVersionCreated",
                    "case_id": "case_007",
                    "base_version": 31,
                    "payload": {
                        "timeline_version": 9,
                        "patch_id": "patch_012",
                        "parent_version": 8,
                    },
                }
            ],
            "error": None,
        },
    )
