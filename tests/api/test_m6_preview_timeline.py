from __future__ import annotations

from pathlib import Path
from typing import Any

from apps.api.main import create_app
from fastapi import FastAPI
from fastapi.testclient import TestClient
from sqlalchemy import select
from sqlalchemy.engine import Engine

from contracts.timeline import TimelineState
from storage import schema
from storage.db import begin_immediate
from storage.repositories import DraftsRepository
from storage.repositories._json import dump_json
from timeline import store_timeline_version

TOKEN = "test-token"
BASE_URL = "http://127.0.0.1:8000"
AUTH = {"Authorization": f"Bearer {TOKEN}"}
NOW = "2026-07-05T00:00:00+00:00"


def test_draft_timeline_404s_for_missing_draft_or_version(tmp_path: Path) -> None:
    app = _app(tmp_path)
    engine = _engine(app)
    _seed_draft(engine, timeline_current_version=None)
    client = _client(app)

    missing_draft = client.get(
        "/api/drafts/missing/timeline",
        headers=AUTH,
    )
    missing_current_version = client.get(
        "/api/drafts/draft_1/timeline",
        headers=AUTH,
    )
    missing_record = client.get(
        "/api/drafts/draft_1/timeline?version=99",
        headers=AUTH,
    )

    assert missing_draft.status_code == 404
    assert missing_current_version.status_code == 404
    assert missing_current_version.json()["detail"] == {"reason": "not_found"}
    assert missing_record.status_code == 404
    assert missing_record.json()["detail"] == {"reason": "not_found"}


def test_draft_timeline_returns_current_timeline_and_latest_preview(tmp_path: Path) -> None:
    app = _app(tmp_path)
    engine = _engine(app)
    _seed_draft(engine)
    with begin_immediate(engine) as connection:
        store_timeline_version(connection, _timeline(), created_at=NOW)
        _seed_preview(connection, preview_id="prev_old", object_hash="hash_old", created_at=NOW)
        _seed_preview(
            connection,
            preview_id="prev_new",
            object_hash="hash_new",
            created_at="2026-07-05T00:00:01+00:00",
        )

    response = _client(app).get(
        "/api/drafts/draft_1/timeline",
        headers=AUTH,
    )

    assert response.status_code == 200
    payload = response.json()
    assert payload["draft_id"] == "draft_1"
    assert payload["timeline_version"] == 1
    assert payload["timeline"]["timeline_id"] == "draft_1:v1"
    assert payload["summary"].startswith("Timeline v1 · 2.0s @30fps · 9:16")
    assert payload["preview_id"] == "prev_new"


def test_preview_viewed_404s_for_missing_or_foreign_preview(tmp_path: Path) -> None:
    app = _app(tmp_path)
    engine = _engine(app)
    _seed_draft(engine)
    with begin_immediate(engine) as connection:
        _seed_draft_rows(connection, draft_id="draft_2")
        _seed_preview(
            connection,
            preview_id="prev_foreign",
            object_hash="hash_foreign",
            draft_id="draft_2",
            created_at=NOW,
        )
    client = _client(app)

    missing = client.post(
        "/api/drafts/draft_1/previews/missing/viewed",
        headers=AUTH,
        json={},
    )
    foreign = client.post(
        "/api/drafts/draft_1/previews/prev_foreign/viewed",
        headers=AUTH,
        json={},
    )

    assert missing.status_code == 404
    assert foreign.status_code == 404


def test_preview_viewed_is_idempotent_and_updates_draft(tmp_path: Path) -> None:
    app = _app(tmp_path)
    engine = _engine(app)
    _seed_draft(engine)
    with begin_immediate(engine) as connection:
        store_timeline_version(connection, _timeline(), created_at=NOW)
        _seed_preview(connection, preview_id="prev_1", object_hash="hash_preview", created_at=NOW)
    client = _client(app)
    path = "/api/drafts/draft_1/previews/prev_1/viewed"

    first = client.post(path, headers=AUTH, json={})
    second = client.post(path, headers=AUTH, json={})

    assert first.status_code == 200
    assert first.json()["draft"]["last_viewed_preview_id"] == "prev_1"
    assert len(first.json()["event_ids"]) == 1
    assert second.status_code == 200
    assert second.json()["draft"]["last_viewed_preview_id"] == "prev_1"
    assert second.json()["event_ids"] == []
    with engine.connect() as connection:
        draft = DraftsRepository(connection).get("draft_1")
        preview_events = connection.execute(
            select(schema.event_log.c.event_id).where(
                schema.event_log.c.event_type == "PreviewViewed"
            )
        ).all()
    assert draft is not None
    assert draft["last_viewed_preview_id"] == "prev_1"
    assert len(preview_events) == 1


def _app(tmp_path: Path) -> FastAPI:
    return create_app(
        tmp_path / "workspace",
        token=TOKEN,
        fs_roots=[tmp_path / "allowed"],
        startup_port=8000,
    )


def _client(app: FastAPI) -> TestClient:
    return TestClient(app, base_url=BASE_URL)


def _engine(app: FastAPI) -> Engine:
    return app.state.api_state.engine


def _seed_draft(engine: Engine, *, timeline_current_version: int | None = 1) -> None:
    with begin_immediate(engine) as connection:
        _seed_draft_rows(
            connection,
            draft_id="draft_1",
            timeline_current_version=timeline_current_version,
        )


def _seed_draft_rows(
    connection: Any,
    *,
    draft_id: str,
    timeline_current_version: int | None = 1,
) -> None:
    connection.execute(
        schema.drafts.insert().values(
            draft_id=draft_id,
            name=f"Draft {draft_id}",
            state_version=0,
            status="active",
            defaults=dump_json({"aspect_ratio": "9:16", "fps": 30}),
            pending_decision_id=None,
            running_jobs=dump_json([]),
            last_error=None,
            brief=dump_json({"goal": "test"}),
            content_plan=None,
            audio_plan=None,
            cut_plan=None,
            timeline_current_version=timeline_current_version,
            timeline_validated=timeline_current_version is not None,
            preview_current_id=None,
            last_viewed_preview_id=None,
            rough_cut_approved=False,
            rough_cut_approved_version=None,
            postprocess_plan=None,
            export_current_id=None,
            scratch_memory=dump_json({}),
            messages_tail_ref=None,
            created_at=NOW,
            updated_at=NOW,
        )
    )


def _seed_preview(
    connection: Any,
    *,
    preview_id: str,
    object_hash: str,
    created_at: str,
    draft_id: str = "draft_1",
    timeline_version: int = 1,
) -> None:
    connection.execute(
        schema.objects.insert().values(
            hash=object_hash,
            rel_path=f"objects/{object_hash}.mp4",
            size=7,
            created_at=created_at,
        )
    )
    connection.execute(
        schema.previews.insert().values(
            preview_id=preview_id,
            draft_id=draft_id,
            timeline_version=timeline_version,
            object_hash=object_hash,
            quality=dump_json({"profile": "preview"}),
            created_at=created_at,
        )
    )


def _timeline() -> TimelineState:
    return TimelineState.model_validate(
        {
            "timeline_id": "draft_1:v1",
            "draft_id": "draft_1",
            "version": 1,
            "fps": 30,
            "duration_frames": 60,
            "tracks": [
                {
                    "track_id": "visual_base",
                    "track_type": "primary_visual",
                    "clips": [_media_clip("tc_visual", "visual_base", 0, 60)],
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
                            "timeline_clip_id": "tc_sub",
                            "track_id": "subtitles",
                            "text": "字幕",
                            "timeline_start_frame": 0,
                            "timeline_end_frame": 30,
                            "style_template_id": "default",
                            "binding": {"kind": "manual", "utterance_id": None},
                            "safe_area_check": "ok",
                        }
                    ],
                },
            ],
        }
    )


def _media_clip(timeline_clip_id: str, track_id: str, start: int, end: int) -> dict[str, Any]:
    return {
        "timeline_clip_id": timeline_clip_id,
        "track_id": track_id,
        "asset_id": "asset_1",
        "clip_id": "clip_1",
        "role": "b_roll",
        "timeline_start_frame": start,
        "timeline_end_frame": end,
        "source_start_frame": 0,
        "source_end_frame": end - start,
        "playback_rate": 1.0,
        "lock_policy": "free",
        "parent_block_id": "slot_1",
        "effects": [{"summary": "产品特写"}],
        "gain_db": 0.0,
    }


def test_answer_decision_reducer_value_error_returns_400(tmp_path: Path) -> None:
    """归约层校验失败应 400 而非 500（M9 路径 1 实测回归）。"""
    from storage.repositories import DecisionsRepository

    app = _app(tmp_path)
    engine = _engine(app)
    _seed_draft(engine, timeline_current_version=None)
    client = _client(app)
    with engine.begin() as connection:
        DecisionsRepository(connection).insert(
            {
                "decision_id": "dec_bad",
                "scope_type": "draft",
                "draft_id": "draft_1",
                "type": "approve_speech_cut",
                "question": "确认粗剪候选？",
                "options": [],
                "status": "pending",
                "blocking": True,
            }
        )
    response = client.post(
        "/api/decisions/dec_bad/answer",
        json={
            "answer": {
                "free_text": "确认",
                "answered_via": "natural_language",
                "payload": {"removed_ranges": "bad"},
            }
        },
        headers=AUTH,
    )
    assert response.status_code == 400
    assert response.json()["detail"]["reason"] == "invalid_answer"
