import pytest
from pydantic import ValidationError

from contracts import (
    AssetRecord,
    Decision,
    SubtitleClip,
    TimelineMediaClip,
    TimelinePatchRequest,
    TimelineState,
    TimelineTrack,
    TranscriptDocument,
)


def test_transcript_word_start_must_be_less_than_end() -> None:
    with pytest.raises(ValidationError):
        TranscriptDocument.model_validate(
            {
                "schema": "TranscriptDocument.v1",
                "transcript_id": "tr_001",
                "asset_id": "asset_001",
                "language": "zh",
                "provider_id": "p",
                "raw_preserved": True,
                "utterances": [
                    {
                        "utterance_id": "u_001",
                        "text": "呃",
                        "start_ms": 0,
                        "end_ms": 100,
                        "words": [{"w": "呃", "start_ms": 50, "end_ms": 50, "type": "filler"}],
                    }
                ],
            }
        )


def test_copy_mode_rejects_reference_path() -> None:
    with pytest.raises(ValidationError):
        AssetRecord.model_validate(
            {
                "asset_id": "asset_001",
                "storage_mode": "copy",
                "workspace_object_uri": "object://source",
                "reference_path": "/Users/me/a.mp4",
                "kind": "video",
                "source": "upload",
                "filename": "a.mp4",
                "hash": "sha256:x",
                "mtime": 1,
                "size": 1,
                "probe": {"duration_sec": 1, "has_audio": False},
                "ingest_status": "imported",
                "usable": False,
            }
        )


def test_case_scope_decision_requires_case_id() -> None:
    with pytest.raises(ValidationError):
        Decision.model_validate(
            {
                "decision_id": "dec_001",
                "scope_type": "case",
                "project_id": "project_001",
                "type": "generic",
                "question": "?",
            }
        )


def test_patch_request_rejects_frame_fields() -> None:
    with pytest.raises(ValidationError):
        TimelinePatchRequest.model_validate(
            {
                "schema": "TimelinePatchRequest.v1",
                "case_id": "case_001",
                "reference": {"timeline_version": 1, "preview_id": "prev_001"},
                "op": {
                    "kind": "delete_range",
                    "time_range_sec": [1.0, 2.0],
                    "scope": "all_tracks",
                    "ripple": True,
                    "start_frame": 30,
                },
                "reason": "illegal frame field",
            }
        )


def test_timeline_media_clip_rejects_invalid_ranges() -> None:
    with pytest.raises(ValidationError):
        TimelineMediaClip.model_validate(
            {
                **_media_clip_payload(),
                "timeline_start_frame": 10,
                "timeline_end_frame": 10,
            }
        )

    with pytest.raises(ValidationError):
        TimelineMediaClip.model_validate(
            {
                **_media_clip_payload(),
                "source_start_frame": 10,
                "source_end_frame": 10,
            }
        )


def test_timeline_state_requires_exact_canonical_tracks() -> None:
    payload = _timeline_payload()
    payload["tracks"] = payload["tracks"][:-1]

    with pytest.raises(ValidationError):
        TimelineState.model_validate(payload)


def test_timeline_state_rejects_wrong_track_type_and_clip_track_mismatch() -> None:
    wrong_type = _timeline_payload()
    wrong_type["tracks"][0]["track_type"] = "audio"
    with pytest.raises(ValidationError):
        TimelineState.model_validate(wrong_type)

    mismatch = _timeline_payload()
    mismatch["tracks"][0]["clips"] = [
        {
            **_media_clip_payload(),
            "timeline_clip_id": "vo_1",
            "track_id": "voiceover",
            "role": "voiceover",
        }
    ]
    with pytest.raises(ValidationError):
        TimelineState.model_validate(mismatch)


def test_timeline_state_defensive_clip_type_guards() -> None:
    media_on_subtitles = TimelineMediaClip.model_construct(
        **{**_media_clip_payload(), "track_id": "subtitles"}
    )
    with pytest.raises(ValueError, match="subtitles track only accepts"):
        _constructed_timeline(
            TimelineTrack.model_construct(
                track_id="subtitles",
                track_type="text",
                clips=[media_on_subtitles],
            )
        ).validate_tracks()

    subtitle_on_audio = SubtitleClip.model_construct(
        timeline_clip_id="sub_1",
        track_id="voiceover",
        text="hello",
        timeline_start_frame=0,
        timeline_end_frame=30,
        style_template_id="subtitle_default",
        binding={"kind": "manual"},
        safe_area_check="ok",
    )
    with pytest.raises(ValueError, match="SubtitleClip is only valid"):
        _constructed_timeline(
            TimelineTrack.model_construct(
                track_id="voiceover",
                track_type="audio",
                clips=[subtitle_on_audio],
            )
        ).validate_tracks()


def test_memory_scope_validators() -> None:
    import pytest as _pytest

    from contracts.memory import Memory

    Memory(
        memory_id="m1",
        scope="project",
        project_id="p1",
        content="c",
        created_at="2026-07-05T00:00:00+00:00",
    )
    with _pytest.raises(ValueError):
        Memory(
            memory_id="m2",
            scope="project",
            project_id=None,
            content="c",
            created_at="2026-07-05T00:00:00+00:00",
        )
    with _pytest.raises(ValueError):
        Memory(
            memory_id="m3",
            scope="user",
            project_id="p1",
            content="c",
            created_at="2026-07-05T00:00:00+00:00",
        )


def _timeline_payload() -> dict[str, object]:
    return {
        "timeline_id": "case_001:v1",
        "case_id": "case_001",
        "version": 1,
        "fps": 30,
        "duration_frames": 30,
        "tracks": [
            {"track_id": "visual_base", "track_type": "primary_visual", "clips": []},
            {"track_id": "visual_overlay", "track_type": "visual_overlay", "clips": []},
            {"track_id": "original_audio", "track_type": "audio", "clips": []},
            {"track_id": "voiceover", "track_type": "audio", "clips": []},
            {"track_id": "bgm", "track_type": "audio", "clips": []},
            {"track_id": "subtitles", "track_type": "text", "clips": []},
        ],
    }


def _media_clip_payload() -> dict[str, object]:
    return {
        "timeline_clip_id": "tc_1",
        "track_id": "visual_base",
        "asset_id": "asset_001",
        "clip_id": "clip_001",
        "role": "b_roll",
        "timeline_start_frame": 0,
        "timeline_end_frame": 30,
        "source_start_frame": 0,
        "source_end_frame": 30,
    }


def _constructed_timeline(replacement: TimelineTrack) -> TimelineState:
    tracks = [
        TimelineTrack.model_construct(
            track_id="visual_base",
            track_type="primary_visual",
            clips=[],
        ),
        TimelineTrack.model_construct(
            track_id="visual_overlay",
            track_type="visual_overlay",
            clips=[],
        ),
        TimelineTrack.model_construct(track_id="original_audio", track_type="audio", clips=[]),
        TimelineTrack.model_construct(track_id="voiceover", track_type="audio", clips=[]),
        TimelineTrack.model_construct(track_id="bgm", track_type="audio", clips=[]),
        TimelineTrack.model_construct(track_id="subtitles", track_type="text", clips=[]),
    ]
    for index, track in enumerate(tracks):
        if track.track_id == replacement.track_id:
            tracks[index] = replacement
            break
    return TimelineState.model_construct(
        timeline_id="case_001:v1",
        case_id="case_001",
        version=1,
        fps=30,
        duration_frames=30,
        tracks=tracks,
    )
