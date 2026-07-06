"""Seed helpers for M9 path 3 Playwright runs.

This file is deliberately outside the production API. It creates the synthetic
provider-dependent state that path 3 needs while keeping Project/Case/material
management on the real API/UI paths.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import subprocess
import sys
from pathlib import Path
from typing import Any
from urllib import error, request

from agent_harness.loop import _load_state
from agent_harness.reducer import ReducerApplyResult, apply
from contracts.events import (
    CaseAssetScopeChanged,
    CaseCreated,
    ExportCompleted,
    MemoryCandidateExtracted,
    MemorySaved,
    TimelineValidated,
    TimelineVersionCreated,
)
from storage import schema
from storage.db import begin_immediate, create_workspace_engine
from storage.repositories.objects import ObjectsRepository
from storage.workspace_paths import WorkspacePaths

PROJECT_A_ID = "e2e_project_a"
PROJECT_A_NAME = "Project A"
CASE_A_ID = "e2e_case_a"
CASE_A_NAME = "Case A 已导出"
MEMORY_ID = "e2e_mem_project_a"
MEMORY_CANDIDATE_ID = "e2e_mem_candidate_case_a"
EXPORT_ID = "e2e_export_case_a"
FIXTURE_NAME = "path3-fixture.mp4"
NOW = "2026-07-05T00:00:00+00:00"


def main() -> None:
    parser = argparse.ArgumentParser(description="Seed M9 path 3 E2E data.")
    subparsers = parser.add_subparsers(dest="command", required=True)

    init = subparsers.add_parser("init")
    init.add_argument("--workspace", required=True)
    init.add_argument("--api-url", required=True)
    init.add_argument("--token", required=True)
    init.add_argument("--fixture-dir", required=True)

    seed_case = subparsers.add_parser("seed-case-a")
    seed_case.add_argument("--workspace", required=True)
    seed_case.add_argument("--asset-id", required=True)
    seed_case.add_argument("--fixture-path", required=True)

    verify_memory = subparsers.add_parser("verify-memory")
    verify_memory.add_argument("--workspace", required=True)
    verify_memory.add_argument("--case-id", required=True)
    verify_memory.add_argument("--memory-id", default=MEMORY_ID)
    verify_memory.add_argument("--contains", default="前三秒直接给结论")

    args = parser.parse_args()
    if args.command == "init":
        command_init(args)
        return
    if args.command == "seed-case-a":
        command_seed_case_a(args)
        return
    if args.command == "verify-memory":
        command_verify_memory(args)
        return
    raise ValueError(f"unsupported command: {args.command}")


def command_init(args: argparse.Namespace) -> None:
    fixture_dir = Path(args.fixture_dir).expanduser().resolve(strict=False)
    fixture_dir.mkdir(parents=True, exist_ok=True)
    make_fixture_video(fixture_dir / FIXTURE_NAME)
    if os.environ.get("RUSHES_E2E_SKIP_API") == "1":
        return
    post_json(
        args.api_url,
        "/api/projects",
        args.token,
        {
            "project_id": PROJECT_A_ID,
            "name": PROJECT_A_NAME,
            "defaults": {"aspect_ratio": "9:16", "fps": 30},
        },
    )


def command_seed_case_a(args: argparse.Namespace) -> None:
    engine = create_workspace_engine(args.workspace)
    fixture_path = Path(args.fixture_path).expanduser().resolve(strict=True)
    workspace_paths = WorkspacePaths.from_root(args.workspace).initialize()
    export_hash = copy_fixture_to_object_store(workspace_paths, fixture_path)
    upsert_object(engine, export_hash, fixture_path.stat().st_size)

    apply_checked(
        apply(
            (
                CaseCreated(
                    project_id=PROJECT_A_ID,
                    case_id=CASE_A_ID,
                    payload={
                        "name": CASE_A_NAME,
                        "brief": {"goal": "护肤口播 Case A 完成导出"},
                        "status": "active",
                    },
                ),
            ),
            engine=engine,
            base_version=None,
            actor="system",
            created_at=NOW,
        )
    )
    state_version = case_state_version(engine, CASE_A_ID)
    apply_checked(
        apply(
            (
                CaseAssetScopeChanged(
                    project_id=PROJECT_A_ID,
                    case_id=CASE_A_ID,
                    payload={"selected_asset_ids": [args.asset_id], "disabled_asset_ids": []},
                ),
            ),
            engine=engine,
            base_version=state_version,
            actor="system",
            created_at=NOW,
        )
    )
    state_version = case_state_version(engine, CASE_A_ID)
    apply_checked(
        apply(
            (
                TimelineVersionCreated(
                    project_id=PROJECT_A_ID,
                    case_id=CASE_A_ID,
                    timeline_version=1,
                    payload={
                        "timeline_id": f"{CASE_A_ID}:v1",
                        "timeline_version": 1,
                        "timeline": timeline_document(CASE_A_ID, args.asset_id),
                    },
                ),
            ),
            engine=engine,
            base_version=state_version,
            actor="system",
            created_at=NOW,
        )
    )
    state_version = case_state_version(engine, CASE_A_ID)
    apply_checked(
        apply(
            (
                TimelineValidated(
                    project_id=PROJECT_A_ID,
                    case_id=CASE_A_ID,
                    timeline_version=1,
                    payload={"validation_report": {"valid": True, "checks": []}},
                ),
            ),
            engine=engine,
            base_version=state_version,
            actor="system",
            created_at=NOW,
        )
    )
    apply_checked(
        apply(
            (
                ExportCompleted(
                    project_id=PROJECT_A_ID,
                    case_id=CASE_A_ID,
                    timeline_version=1,
                    artifact_id=EXPORT_ID,
                    payload={
                        "object_hash": export_hash,
                        "quality": {"profile": "e2e"},
                        "created_at": NOW,
                    },
                ),
                MemoryCandidateExtracted(
                    project_id=PROJECT_A_ID,
                    case_id=CASE_A_ID,
                    candidate_id=MEMORY_CANDIDATE_ID,
                    payload={
                        "case_id": CASE_A_ID,
                        "content": memory_content(),
                        "suggested_scope": "project",
                        "status": "pending",
                        "created_at": NOW,
                    },
                ),
                MemorySaved(
                    project_id=PROJECT_A_ID,
                    case_id=CASE_A_ID,
                    memory_id=MEMORY_ID,
                    candidate_id=MEMORY_CANDIDATE_ID,
                    payload={
                        "candidate_id": MEMORY_CANDIDATE_ID,
                        "scope": "project",
                        "project_id": PROJECT_A_ID,
                        "content": memory_content(),
                        "tags": ["e2e", "path3"],
                        "created_from_case_id": CASE_A_ID,
                        "created_at": NOW,
                    },
                ),
            ),
            engine=engine,
            base_version=None,
            actor="system",
            created_at=NOW,
        )
    )


def command_verify_memory(args: argparse.Namespace) -> None:
    engine = create_workspace_engine(args.workspace)
    loaded = _load_state(engine, args.case_id)
    matches = [
        item
        for item in loaded.memory_summaries
        if args.memory_id in item and args.contains in item
    ]
    if not matches:
        payload = json.dumps(list(loaded.memory_summaries), ensure_ascii=False, indent=2)
        raise SystemExit(f"memory summary not injected for {args.case_id}:\n{payload}")
    print(matches[0])


def make_fixture_video(path: Path) -> None:
    if path.exists() and path.stat().st_size > 0:
        return
    command = [
        "ffmpeg",
        "-hide_banner",
        "-loglevel",
        "error",
        "-y",
        "-f",
        "lavfi",
        "-i",
        "testsrc2=size=320x568:rate=30:duration=2",
        "-pix_fmt",
        "yuv420p",
        "-c:v",
        "libx264",
        str(path),
    ]
    subprocess.run(command, check=True)


def post_json(api_url: str, path: str, token: str, payload: dict[str, Any]) -> dict[str, Any]:
    body = json.dumps(payload).encode("utf-8")
    req = request.Request(
        f"{api_url}{path}",
        data=body,
        method="POST",
        headers={
            "Authorization": f"Bearer {token}",
            "Content-Type": "application/json",
        },
    )
    try:
        with request.urlopen(req, timeout=10) as response:
            raw = response.read().decode("utf-8")
    except error.HTTPError as exc:
        detail = exc.read().decode("utf-8")
        raise RuntimeError(f"POST {path} failed: {exc.code} {detail}") from exc
    return json.loads(raw) if raw else {}


def copy_fixture_to_object_store(paths: WorkspacePaths, fixture_path: Path) -> str:
    digest = hashlib.sha256(fixture_path.read_bytes()).hexdigest()
    target = paths.object_path(digest)
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_bytes(fixture_path.read_bytes())
    return digest


def upsert_object(engine: Any, object_hash: str, size: int) -> None:
    with begin_immediate(engine) as connection:
        ObjectsRepository(connection).upsert(
            object_hash=object_hash,
            rel_path=f"{object_hash[:2]}/{object_hash[2:4]}/{object_hash}",
            size=size,
            created_at=NOW,
        )


def timeline_document(case_id: str, asset_id: str) -> dict[str, Any]:
    return {
        "timeline_id": f"{case_id}:v1",
        "case_id": case_id,
        "version": 1,
        "fps": 30,
        "duration_frames": 60,
        "tracks": [
            {
                "track_id": "visual_base",
                "track_type": "primary_visual",
                "clips": [
                    {
                        "timeline_clip_id": "e2e_clip_project_a",
                        "track_id": "visual_base",
                        "asset_id": asset_id,
                        "clip_id": "e2e_source_clip",
                        "role": "a_roll",
                        "timeline_start_frame": 0,
                        "timeline_end_frame": 60,
                        "source_start_frame": 0,
                        "source_end_frame": 60,
                        "playback_rate": 1.0,
                        "lock_policy": "free",
                        "parent_block_id": "slot_e2e",
                        "effects": [{"summary": "E2E Project A fixture"}],
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
                        "timeline_clip_id": "e2e_subtitle",
                        "track_id": "subtitles",
                        "text": "Case A 已导出",
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


def memory_content() -> str:
    return "护肤口播项目经验：前三秒直接给结论，然后复用已验证素材节奏。"


def apply_checked(result: ReducerApplyResult) -> None:
    if result.status == "applied":
        return
    raise RuntimeError(f"reducer apply failed: {result}")


def case_state_version(engine: Any, case_id: str) -> int:
    with engine.connect() as connection:
        row = connection.execute(
            schema.cases.select().where(schema.cases.c.case_id == case_id)
        ).one()
    return int(row._mapping["state_version"])


if __name__ == "__main__":
    try:
        main()
    except Exception as exc:
        print(str(exc), file=sys.stderr)
        raise
