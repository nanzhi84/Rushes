"""Seed helpers for the draft-model Playwright run.

This file is deliberately outside the production API. It creates the synthetic
provider-dependent state the draft-materials spec needs (seeded timeline +
export + user memory) while keeping draft/material management on the real
API/UI paths.
"""

from __future__ import annotations

import argparse
import hashlib
import subprocess
import sys
from pathlib import Path
from typing import Any

from agent_harness.loop import _load_state
from agent_harness.reducer import ReducerApplyResult, apply
from contracts.events import (
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

MEMORY_ID = "e2e_mem_draft_a"
MEMORY_CANDIDATE_ID = "e2e_mem_candidate_draft_a"
EXPORT_ID = "e2e_export_draft_a"
CLIP_ID = "e2e_clip_draft_a"
FIXTURE_NAME = "path3-fixture.mp4"
NOW = "2026-07-05T00:00:00+00:00"


def main() -> None:
    parser = argparse.ArgumentParser(description="Seed draft-model E2E data.")
    subparsers = parser.add_subparsers(dest="command", required=True)

    init = subparsers.add_parser("init")
    init.add_argument("--fixture-dir", required=True)

    seed_draft = subparsers.add_parser("seed-draft-a")
    seed_draft.add_argument("--workspace", required=True)
    seed_draft.add_argument("--draft-id", required=True)
    seed_draft.add_argument("--asset-id", required=True)
    seed_draft.add_argument("--fixture-path", required=True)

    verify_memory = subparsers.add_parser("verify-memory")
    verify_memory.add_argument("--workspace", required=True)
    verify_memory.add_argument("--draft-id", required=True)
    verify_memory.add_argument("--memory-id", default=MEMORY_ID)
    verify_memory.add_argument("--contains", default="前三秒直接给结论")

    args = parser.parse_args()
    if args.command == "init":
        command_init(args)
        return
    if args.command == "seed-draft-a":
        command_seed_draft_a(args)
        return
    if args.command == "verify-memory":
        command_verify_memory(args)
        return
    raise ValueError(f"unsupported command: {args.command}")


def command_init(args: argparse.Namespace) -> None:
    fixture_dir = Path(args.fixture_dir).expanduser().resolve(strict=False)
    fixture_dir.mkdir(parents=True, exist_ok=True)
    make_fixture_video(fixture_dir / FIXTURE_NAME)


def command_seed_draft_a(args: argparse.Namespace) -> None:
    """Attach a seeded timeline/export/user-memory onto a UI-created draft.

    时间线事件是 strict（版本链），逐条从草稿当前 state_version 起锚；导出与记忆
    是 merge 事件，base_version=None。草稿本体已由「开始创作」经真实 REST 建好。
    """
    engine = create_workspace_engine(args.workspace)
    fixture_path = Path(args.fixture_path).expanduser().resolve(strict=True)
    workspace_paths = WorkspacePaths.from_root(args.workspace).initialize()
    export_hash = copy_fixture_to_object_store(workspace_paths, fixture_path)
    upsert_object(engine, export_hash, fixture_path.stat().st_size)

    apply_checked(
        apply(
            (
                TimelineVersionCreated(
                    draft_id=args.draft_id,
                    timeline_version=1,
                    payload={
                        "timeline_id": f"{args.draft_id}:v1",
                        "timeline_version": 1,
                        "timeline": timeline_document(args.draft_id, args.asset_id),
                    },
                ),
            ),
            engine=engine,
            base_version=draft_state_version(engine, args.draft_id),
            actor="system",
            created_at=NOW,
        )
    )
    apply_checked(
        apply(
            (
                TimelineValidated(
                    draft_id=args.draft_id,
                    timeline_version=1,
                    payload={"validation_report": {"valid": True, "checks": []}},
                ),
            ),
            engine=engine,
            base_version=draft_state_version(engine, args.draft_id),
            actor="system",
            created_at=NOW,
        )
    )
    apply_checked(
        apply(
            (
                ExportCompleted(
                    draft_id=args.draft_id,
                    timeline_version=1,
                    artifact_id=EXPORT_ID,
                    payload={
                        "object_hash": export_hash,
                        "quality": {"profile": "e2e"},
                        "created_at": NOW,
                    },
                ),
                MemoryCandidateExtracted(
                    draft_id=args.draft_id,
                    candidate_id=MEMORY_CANDIDATE_ID,
                    payload={
                        "draft_id": args.draft_id,
                        "content": memory_content(),
                        "suggested_scope": "user",
                        "status": "pending",
                        "created_at": NOW,
                    },
                ),
                MemorySaved(
                    draft_id=args.draft_id,
                    memory_id=MEMORY_ID,
                    candidate_id=MEMORY_CANDIDATE_ID,
                    payload={
                        "candidate_id": MEMORY_CANDIDATE_ID,
                        "scope": "user",
                        "content": memory_content(),
                        "tags": ["e2e", "path3"],
                        "created_from_draft_id": args.draft_id,
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
    loaded = _load_state(engine, args.draft_id)
    matches = [
        item for item in loaded.memory_summaries if args.memory_id in item and args.contains in item
    ]
    if not matches:
        payload = "\n".join(loaded.memory_summaries) or "(空)"
        raise SystemExit(f"memory summary not injected for {args.draft_id}:\n{payload}")
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


def timeline_document(draft_id: str, asset_id: str) -> dict[str, Any]:
    return {
        "timeline_id": f"{draft_id}:v1",
        "draft_id": draft_id,
        "version": 1,
        "fps": 30,
        "duration_frames": 60,
        "tracks": [
            {
                "track_id": "visual_base",
                "track_type": "primary_visual",
                "clips": [
                    {
                        "timeline_clip_id": CLIP_ID,
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
                        "effects": [{"summary": "E2E draft fixture"}],
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
                        "text": "草稿 A 已导出",
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


def draft_state_version(engine: Any, draft_id: str) -> int:
    with engine.connect() as connection:
        row = connection.execute(
            schema.drafts.select().where(schema.drafts.c.draft_id == draft_id)
        ).one()
    return int(row._mapping["state_version"])


if __name__ == "__main__":
    try:
        main()
    except Exception as exc:
        print(str(exc), file=sys.stderr)
        raise
