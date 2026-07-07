"""脚本化 VLM 动作序列驱动素材理解子代理（Spec C §C3）。"""

from __future__ import annotations

import dataclasses
from collections.abc import Sequence
from typing import Any

import pytest

from tools.understand.subagent import (
    SubagentSpec,
    TranscribeResult,
    run_understanding_subagent,
)

NOW = "2026-07-06T00:00:00+00:00"
DATA_URI = "data:image/jpeg;base64,ZmFrZQ=="


class ScriptedVlm:
    """按序返回动作 JSON 字典，并记录每步收到的 messages。"""

    def __init__(self, actions: Sequence[dict[str, Any]]) -> None:
        self._actions = list(actions)
        self.calls: list[list[dict[str, Any]]] = []

    async def __call__(self, messages: list[dict[str, Any]]) -> dict[str, Any]:
        self.calls.append(messages)
        if not self._actions:
            return {"action": "noop"}
        return self._actions.pop(0)


class RecordingTranscribe:
    def __init__(self, result: TranscribeResult) -> None:
        self._result = result
        self.calls: list[tuple[float | None, float | None]] = []

    async def __call__(self, start_s: float | None, end_s: float | None) -> TranscribeResult:
        self.calls.append((start_s, end_s))
        return self._result


def _transcribe_result(text: str = "你好世界") -> TranscribeResult:
    return TranscribeResult(
        text=text,
        language="zh",
        provider_id="mock_asr",
        raw_preserved=True,
        utterances=[
            {"utterance_id": "u1", "text": text, "start_ms": 0, "end_ms": 1000, "words": []}
        ],
        vad_segments=[],
        seconds=90.0,
    )


def _spec(
    actions: Sequence[dict[str, Any]],
    *,
    kind: str = "video",
    has_audio: bool | None = None,
    focus: str | None = None,
    version: int = 1,
    prior_summary: dict[str, Any] | None = None,
    transcribe: Any | None = None,
    extract_frame: Any | None = None,
    progress: Any | None = None,
    step_budget: int = 12,
) -> tuple[SubagentSpec, ScriptedVlm]:
    vlm = ScriptedVlm(actions)
    resolved_has_audio = has_audio if has_audio is not None else kind in {"video", "audio"}
    return (
        SubagentSpec(
            asset_id="asset_1",
            filename="clip.mp4",
            kind=kind,
            duration_sec=120.0,
            index_summary="时长：120s；有音轨：是。",
            version=version,
            model="qwen-vl-plus",
            vlm=vlm,
            extract_frame=extract_frame or (lambda _seconds: DATA_URI),
            transcribe=transcribe or RecordingTranscribe(_transcribe_result()),
            has_audio=resolved_has_audio,
            focus=focus,
            prior_summary=prior_summary,
            progress=progress or (lambda _payload: None),
            now=lambda: NOW,
            step_budget=step_budget,
        ),
        vlm,
    )


def _emit(summary: dict[str, Any]) -> dict[str, Any]:
    return {"action": "emit_summary", "summary": summary}


_GOOD_SUMMARY = {
    "semantic_role": "footage",
    "overall": "一段产品特写。",
    "language": "zh",
    "segments": [
        {
            "start_s": 0.0,
            "end_s": 12.0,
            "description": "产品正面特写",
            "tags": ["产品特写"],
            "quality": "good",
        }
    ],
}


@pytest.mark.asyncio
async def test_view_transcribe_emit_happy_path() -> None:
    notes: list[str] = []
    transcribe = RecordingTranscribe(_transcribe_result())
    spec, vlm = _spec(
        [
            {"action": "view_frames", "timestamps_s": [2.0, 4.0]},
            {"action": "transcribe", "start_s": 0.0, "end_s": 90.0},
            _emit(_GOOD_SUMMARY),
        ],
        transcribe=transcribe,
        progress=lambda payload: notes.append(str(payload["note"])),
    )

    outcome = await run_understanding_subagent(spec)

    assert outcome.status == "ready"
    assert outcome.summary is not None
    assert outcome.summary.asset_id == "asset_1"
    assert outcome.summary.version == 1
    assert outcome.summary.generated_at == NOW
    assert outcome.summary.model == "qwen-vl-plus"
    assert outcome.summary.spent.frames_viewed == 2
    assert outcome.summary.spent.asr_seconds == pytest.approx(90.0)
    assert outcome.frames_viewed == 2
    assert transcribe.calls == [(0.0, 90.0)]
    assert len(outcome.transcribe_results) == 1
    # 进度事件覆盖看帧/转写/产出，且 payload 不含 type 键（由调用方转发时补）。
    assert any("正在查看" in note for note in notes)
    assert any("正在转写" in note for note in notes)
    # 转写后的一步 messages 里带上了两帧图像。
    last_messages = vlm.calls[-1]
    images = [c for c in last_messages[1]["content"] if c.get("type") == "image_url"]
    assert len(images) == 2


@pytest.mark.asyncio
async def test_focus_and_version_flow_into_summary() -> None:
    spec, _ = _spec(
        [_emit(_GOOD_SUMMARY)],
        focus="口播是否清晰",
        version=3,
        prior_summary={"overall": "旧摘要"},
    )
    outcome = await run_understanding_subagent(spec)
    assert outcome.status == "ready"
    assert outcome.summary is not None
    assert outcome.summary.focus == "口播是否清晰"
    assert outcome.summary.version == 3


@pytest.mark.asyncio
async def test_frames_capped_at_six_per_view() -> None:
    spec, _ = _spec(
        [
            {"action": "view_frames", "timestamps_s": [1, 2, 3, 4, 5, 6, 7, 8]},
            _emit(_GOOD_SUMMARY),
        ]
    )
    outcome = await run_understanding_subagent(spec)
    assert outcome.status == "ready"
    assert outcome.frames_viewed == 6


@pytest.mark.asyncio
async def test_transcribe_rejected_for_non_audio_kind() -> None:
    transcribe = RecordingTranscribe(_transcribe_result())
    spec, _ = _spec(
        [
            {"action": "transcribe", "start_s": 0.0, "end_s": 5.0},
            _emit({"semantic_role": "photo", "overall": "一张照片。"}),
        ],
        kind="image",
        transcribe=transcribe,
    )
    outcome = await run_understanding_subagent(spec)
    assert outcome.status == "ready"
    assert transcribe.calls == []  # 无音轨素材不触发转写


@pytest.mark.asyncio
async def test_transcribe_rejected_for_silent_video() -> None:
    # 无声视频：kind 仍是 video，但 has_audio=False（probe/vad/peaks 判定）应拒绝转写。
    transcribe = RecordingTranscribe(_transcribe_result())
    spec, vlm = _spec(
        [
            {"action": "transcribe", "start_s": 0.0, "end_s": 5.0},
            _emit(_GOOD_SUMMARY),
        ],
        kind="video",
        has_audio=False,
        transcribe=transcribe,
    )
    outcome = await run_understanding_subagent(spec)
    assert outcome.status == "ready"
    assert transcribe.calls == []  # 不真跑 ASR
    # 拒绝理由作为 observation 喂回下一步。
    followup_prompt = vlm.calls[1][1]["content"][0]["text"]
    assert "无音轨" in followup_prompt


@pytest.mark.asyncio
async def test_illegal_json_three_times_fails() -> None:
    spec, _ = _spec([{"foo": "bar"}, {"foo": "bar"}, {"foo": "bar"}])
    outcome = await run_understanding_subagent(spec)
    assert outcome.status == "failed"
    assert "合法" in (outcome.failure_reason or "")


@pytest.mark.asyncio
async def test_unknown_action_three_times_fails_fast() -> None:
    # 持续输出未知动作必须在第 3 步失败，而不是烧满步数预算。
    spec, vlm = _spec([{"action": "bogus"}] * 12)
    outcome = await run_understanding_subagent(spec)
    assert outcome.status == "failed"
    assert "未知动作" in (outcome.failure_reason or "")
    assert outcome.steps == 3
    assert len(vlm.calls) == 3


@pytest.mark.asyncio
async def test_executed_action_resets_illegal_counter() -> None:
    # 未知/非法输出穿插已执行动作时计数应清零，不误杀正常会话。
    spec, _ = _spec(
        [
            {"action": "bogus"},
            {"foo": "bar"},
            {"action": "view_frames", "timestamps_s": [1.0]},
            {"action": "bogus"},
            {"foo": "bar"},
            _emit(_GOOD_SUMMARY),
        ]
    )
    outcome = await run_understanding_subagent(spec)
    assert outcome.status == "ready"


@pytest.mark.asyncio
async def test_emit_schema_invalid_then_valid_retries_once() -> None:
    spec, _ = _spec(
        [
            _emit({"semantic_role": "footage"}),  # 缺 overall -> 校验失败
            _emit(_GOOD_SUMMARY),
        ]
    )
    outcome = await run_understanding_subagent(spec)
    assert outcome.status == "ready"


@pytest.mark.asyncio
async def test_emit_schema_invalid_twice_fails() -> None:
    spec, _ = _spec(
        [
            _emit({"semantic_role": "footage"}),
            _emit({"semantic_role": "footage"}),
        ]
    )
    outcome = await run_understanding_subagent(spec)
    assert outcome.status == "failed"
    assert "schema" in (outcome.failure_reason or "")


@pytest.mark.asyncio
async def test_step_budget_exhausted_fails() -> None:
    spec, _ = _spec(
        [{"action": "view_frames", "timestamps_s": [1.0]}] * 3,
        step_budget=3,
    )
    outcome = await run_understanding_subagent(spec)
    assert outcome.status == "failed"
    assert "预算" in (outcome.failure_reason or "")


@pytest.mark.asyncio
async def test_vlm_exception_fails() -> None:
    async def _boom(_messages: list[dict[str, Any]]) -> dict[str, Any]:
        raise RuntimeError("provider down")

    spec, _ = _spec([_emit(_GOOD_SUMMARY)])
    spec = dataclasses.replace(spec, vlm=_boom)
    outcome = await run_understanding_subagent(spec)
    assert outcome.status == "failed"
    assert "VLM 调用失败" in (outcome.failure_reason or "")


@pytest.mark.asyncio
async def test_frame_extraction_failure_is_tolerated() -> None:
    def _bad_extract(_seconds: float) -> str:
        raise RuntimeError("ffmpeg failed")

    spec, _ = _spec(
        [
            {"action": "view_frames", "timestamps_s": [1.0]},
            _emit(_GOOD_SUMMARY),
        ],
        extract_frame=_bad_extract,
    )
    outcome = await run_understanding_subagent(spec)
    assert outcome.status == "ready"
    assert outcome.frames_viewed == 0
