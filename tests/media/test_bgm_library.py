from __future__ import annotations

import subprocess
from pathlib import Path

import pytest
from sqlalchemy import func, select

from media import bgm_library
from storage import schema
from storage.db import create_workspace_engine
from storage.repositories._json import dump_json, load_json
from storage.workspace_paths import WorkspacePaths

NOW = "2026-07-05T00:00:00+00:00"


def test_default_bgm_registry_is_bounded() -> None:
    tracks = bgm_library.list_default_bgm_tracks()

    assert 1 <= len(tracks) <= 10
    assert {track.bgm_id for track in tracks} == {
        "default_bgm_calm",
        "default_bgm_upbeat",
        "default_bgm_ambient",
    }


def test_synthesize_default_bgm_is_idempotent(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    calls: list[list[str]] = []

    def fake_run(
        command: list[str],
        *,
        capture_output: bool,
        check: bool,
        text: bool,
    ) -> subprocess.CompletedProcess[str]:
        assert capture_output is True
        assert check is False
        assert text is True
        calls.append(command)
        Path(command[-1]).write_bytes(b"fake m4a")
        return subprocess.CompletedProcess(command, 0, "", "")

    monkeypatch.setattr(bgm_library.subprocess, "run", fake_run)
    track = bgm_library.get_default_bgm_track("default_bgm_calm")
    out_path = tmp_path / "calm.m4a"

    bgm_library.synthesize_default_bgm(track, out_path)
    bgm_library.synthesize_default_bgm(track, out_path)

    assert out_path.read_bytes() == b"fake m4a"
    assert len(calls) == 1


def test_synthesize_default_bgm_reports_ffmpeg_failure(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    def fake_run(
        command: list[str],
        *,
        capture_output: bool,
        check: bool,
        text: bool,
    ) -> subprocess.CompletedProcess[str]:
        del capture_output, check, text
        return subprocess.CompletedProcess(command, 1, "", "bad filter\n")

    monkeypatch.setattr(bgm_library.subprocess, "run", fake_run)

    with pytest.raises(bgm_library.DefaultBgmSynthesisError, match="bad filter"):
        bgm_library.synthesize_default_bgm(
            bgm_library.get_default_bgm_track("default_bgm_calm"),
            tmp_path / "calm.m4a",
        )


def test_ensure_default_bgm_asset_registers_and_links_idempotently(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    def fake_synthesize(
        track: bgm_library.DefaultBgmTrack,
        out_path: str | Path,
        *,
        ffmpeg_bin: str = "ffmpeg",
    ) -> None:
        del track, ffmpeg_bin
        Path(out_path).parent.mkdir(parents=True, exist_ok=True)
        Path(out_path).write_bytes(b"default bgm")

    monkeypatch.setattr(bgm_library, "synthesize_default_bgm", fake_synthesize)
    engine = create_workspace_engine(tmp_path)
    paths = WorkspacePaths.from_root(tmp_path / "workspace").initialize()

    with engine.begin() as connection:
        schema.create_all(connection)
        connection.execute(
            schema.projects.insert().values(
                project_id="project_1",
                name="Project",
                status="active",
                defaults=dump_json({"aspect_ratio": "9:16", "fps": 30}),
                created_at=NOW,
                updated_at=NOW,
            )
        )
        first = bgm_library.ensure_default_bgm_asset(
            connection,
            paths,
            "project_1",
            "default_bgm_calm",
            NOW,
        )
        second = bgm_library.ensure_default_bgm_asset(
            connection,
            paths,
            "project_1",
            "default_bgm_calm",
            NOW,
        )
        asset_count = connection.execute(
            select(func.count()).select_from(schema.assets)
        ).scalar_one()
        link_count = connection.execute(
            select(func.count()).select_from(schema.project_asset_links)
        ).scalar_one()
        asset = connection.execute(
            select(schema.assets).where(schema.assets.c.asset_id == first)
        ).one()

    assert first == second
    assert first.startswith("asset_default_bgm_calm_")
    assert asset_count == 1
    assert link_count == 1
    assert asset._mapping["kind"] == "bgm"
    assert asset._mapping["source"] == "default_library"
    assert load_json(str(asset._mapping["probe"]))["default_library"] == {
        "bgm_id": "default_bgm_calm"
    }
