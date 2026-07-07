from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "scripts" / "e2e_paths"))

from client import choose_decision_answer, parse_sse_events


def test_parse_sse_events_parses_named_event() -> None:
    events = parse_sse_events(
        [
            "id: 42\n",
            "event: case\n",
            'data: {"event": {"event": "DecisionCreated", "decision_id": "decision_1"}}\n',
            "\n",
        ]
    )

    assert len(events) == 1
    assert events[0].event_id == 42
    assert events[0].event_type == "case"
    assert events[0].event["event"] == "DecisionCreated"
    assert events[0].event["decision_id"] == "decision_1"


def test_parse_sse_events_accepts_multiline_data_without_trailing_blank() -> None:
    events = parse_sse_events(
        [
            "id: 7\n",
            "event: case\n",
            'data: {"event": {"event": "JobCompleted",\n',
            'data: "job_id": "job_1"}}\n',
        ]
    )

    assert len(events) == 1
    assert events[0].event_id == 7
    assert events[0].event["event"] == "JobCompleted"
    assert events[0].event["job_id"] == "job_1"


def test_choose_audio_mode_by_path() -> None:
    decision = {
        "type": "audio_mode",
        "options": [
            {"option_id": "tts", "label": "TTS", "payload": {"mode": "tts"}},
            {"option_id": "rough_cut", "label": "原声", "payload": {"mode": "rough_cut"}},
        ],
    }

    path1 = choose_decision_answer(decision, scenario="path1")
    path2 = choose_decision_answer(decision, scenario="path2")

    assert path1.option_id == "rough_cut"
    assert path1.payload == {"mode": "rough_cut"}
    assert path2.option_id == "tts"
    assert path2.payload == {"mode": "tts"}


def test_choose_path1_speech_cut_and_postprocess_decisions() -> None:
    speech_cut = choose_decision_answer(
        {
            "type": "approve_speech_cut",
            "options": [
                {
                    "option_id": "apply_all",
                    "label": "应用",
                    "payload": {"removed_ranges": [{"start_sec": 6.8, "end_sec": 8.1}]},
                }
            ],
        },
        scenario="path1",
    )
    subtitle = choose_decision_answer(
        {
            "type": "subtitle",
            "options": [
                {"option_id": "clean_bottom", "label": "干净底部", "payload": {"enabled": True}},
                {"option_id": "skip", "label": "跳过", "payload": {"enabled": False}},
            ],
        },
        scenario="path1",
    )
    bgm = choose_decision_answer(
        {
            "type": "bgm",
            "options": [
                {
                    "option_id": "upload_bgm",
                    "label": "上传 BGM 素材",
                    "payload": {"enabled": True, "action": "upload"},
                },
                {"option_id": "skip", "label": "跳过 BGM", "payload": {"enabled": False}},
            ],
        },
        scenario="path1",
    )

    assert speech_cut.option_id == "apply_all"
    assert speech_cut.payload["removed_ranges"] == [{"start_sec": 6.8, "end_sec": 8.1}]
    assert subtitle.option_id == "skip"
    assert subtitle.payload == {"enabled": False}
    assert bgm.option_id == "skip"
    assert bgm.payload == {"enabled": False}


def test_choose_path2_subtitle_and_bgm_uses_uploaded_asset() -> None:
    subtitle = choose_decision_answer(
        {
            "type": "subtitle",
            "options": [
                {"option_id": "big_center", "label": "大字", "payload": {"enabled": True}},
                {
                    "option_id": "clean_bottom",
                    "label": "干净底部",
                    "payload": {"enabled": True, "style_template_id": "clean_bottom"},
                },
                {"option_id": "skip", "label": "跳过", "payload": {"enabled": False}},
            ],
        },
        scenario="path2",
    )
    bgm = choose_decision_answer(
        {
            "type": "bgm",
            "options": [
                {
                    "option_id": "asset_bgm_1",
                    "label": "使用素材：配乐.m4a",
                    "payload": {
                        "enabled": True,
                        "asset_id": "asset_bgm_1",
                        "gain_db": -12.0,
                        "duck": True,
                    },
                },
                {
                    "option_id": "upload_bgm",
                    "label": "上传 BGM 素材",
                    "payload": {"enabled": True, "action": "upload"},
                },
                {"option_id": "skip", "label": "跳过 BGM", "payload": {"enabled": False}},
            ],
        },
        scenario="path2",
    )

    assert subtitle.option_id == "clean_bottom"
    assert subtitle.payload["style_template_id"] == "clean_bottom"
    assert bgm.option_id == "asset_bgm_1"
    assert bgm.payload == {
        "enabled": True,
        "asset_id": "asset_bgm_1",
        "gain_db": -12.0,
        "duck": True,
    }


def test_choose_rough_cut_approval_injects_draft_timeline_version() -> None:
    choice = choose_decision_answer(
        {
            "type": "approve_rough_cut",
            "options": [{"option_id": "approve", "label": "确认", "payload": {}}],
        },
        scenario="path1",
        draft_state={"timeline_current_version": 3},
    )

    assert choice.option_id == "approve"
    assert choice.payload["approved"] is True
    assert choice.payload["timeline_version"] == 3


def test_choose_unknown_decision_uses_first_non_skip_option() -> None:
    choice = choose_decision_answer(
        {
            "type": "generic",
            "options": [
                {"option_id": "skip", "label": "跳过", "payload": {"enabled": False}},
                {"option_id": "custom", "label": "继续", "payload": {"enabled": True}},
            ],
        },
        scenario="path2",
    )

    assert choice.option_id == "custom"
    assert choice.payload == {"enabled": True}
