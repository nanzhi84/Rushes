from pathlib import Path
from typing import Any

from sqlalchemy import func, select

from agent_harness.reducer import REDUCER_DISPATCH_EVENTS, apply
from contracts.events import event_registry
from storage import schema
from storage.db import begin_immediate, create_workspace_engine
from storage.repositories import DraftsRepository
from storage.repositories._json import dump_json, load_json
from storage.repositories.event_log import EventLogRepository

NOW = "2026-07-04T00:00:00+00:00"


def test_reducer_dispatch_covers_all_registered_events() -> None:
    # reducer 归约表与 contracts 事件注册表逐一对齐（含 issue #53 新增的 AssetHashComputed）。
    assert len(REDUCER_DISPATCH_EVENTS) == 45
    assert frozenset(event_registry()) == REDUCER_DISPATCH_EVENTS


def _draft_row(draft_id: str, **overrides: object) -> dict[str, object]:
    row: dict[str, object] = {
        "draft_id": draft_id,
        "name": "Draft",
        "state_version": 0,
        "status": "active",
        "defaults": {"aspect_ratio": "9:16", "fps": 30},
        "pending_decision_id": None,
        "running_jobs": [],
        "last_error": None,
        "brief": {"goal": "test", "confirmed_facts": []},
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
        "scratch_memory": {},
        "messages_tail_ref": None,
        "created_at": NOW,
        "updated_at": NOW,
    }
    row.update(overrides)
    return row


def _prepare_workspace(tmp_path: Path) -> None:
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)


def _insert_draft(
    tmp_path: Path,
    *,
    state_version: int = 0,
    timeline_current_version: int | None = None,
    rough_cut_approved: bool = False,
    rough_cut_approved_version: int | None = None,
) -> None:
    engine = create_workspace_engine(tmp_path)
    with begin_immediate(engine) as connection:
        DraftsRepository(connection).insert(
            _draft_row(
                "draft_1",
                state_version=state_version,
                timeline_current_version=timeline_current_version,
                rough_cut_approved=rough_cut_approved,
                rough_cut_approved_version=rough_cut_approved_version,
            )
        )


def _insert_timeline(tmp_path: Path, *, version: int) -> None:
    engine = create_workspace_engine(tmp_path)
    with begin_immediate(engine) as connection:
        connection.execute(
            schema.timeline_versions.insert().values(
                timeline_id=f"draft_1:v{version}",
                draft_id="draft_1",
                version=version,
                parent_version=None,
                created_by_patch_id=None,
                document_json=dump_json(_timeline_doc(version)),
                validation_report=None,
                created_at=NOW,
            )
        )


def _timeline_doc(version: int) -> dict[str, object]:
    return {
        "timeline_id": f"draft_1:v{version}",
        "draft_id": "draft_1",
        "version": version,
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


def _event_log_count(tmp_path: Path) -> int:
    engine = create_workspace_engine(tmp_path)
    with begin_immediate(engine) as connection:
        return len(EventLogRepository(connection).read_after(0))


def _insert_extra_draft(tmp_path: Path, draft_id: str) -> None:
    engine = create_workspace_engine(tmp_path)
    with begin_immediate(engine) as connection:
        DraftsRepository(connection).insert(_draft_row(draft_id))


def test_strict_event_stale_base_version_rejects_whole_batch(tmp_path: Path) -> None:
    _prepare_workspace(tmp_path)
    _insert_draft(tmp_path, state_version=2)
    engine = create_workspace_engine(tmp_path)

    result = apply(
        [{"event": "BriefUpdated", "draft_id": "draft_1", "payload": {"brief": {"goal": "new"}}}],
        engine=engine,
        base_version=1,
        actor="agent",
        created_at=NOW,
    )

    assert result.status == "version_conflict"
    assert _event_log_count(tmp_path) == 0
    with begin_immediate(engine) as connection:
        case = DraftsRepository(connection).get("draft_1")
    assert case is not None
    assert case["state_version"] == 2
    assert case["brief"]["goal"] == "test"


def test_draft_lifecycle_merge_events_update_structural_state(tmp_path: Path) -> None:
    _prepare_workspace(tmp_path)
    _insert_draft(tmp_path)
    engine = create_workspace_engine(tmp_path)

    result = apply(
        [
            {"event": "DraftCreated", "draft_id": "draft_new", "payload": {"name": "New Draft"}},
            {"event": "DraftRenamed", "draft_id": "draft_1", "name": "Renamed Draft"},
            {
                "event": "DraftCopied",
                "draft_id": "draft_2",
                "source_draft_id": "draft_1",
                "payload": {"name": "Copied Draft"},
            },
            {"event": "DraftTrashed", "draft_id": "draft_1"},
        ],
        engine=engine,
        base_version=None,
        actor="user",
        created_at=NOW,
    )

    with begin_immediate(engine) as connection:
        new_draft = DraftsRepository(connection).get("draft_new")
        renamed = DraftsRepository(connection).get("draft_1")
        copied = DraftsRepository(connection).get("draft_2")

    assert result.status == "applied"
    assert new_draft is not None
    assert new_draft["name"] == "New Draft"
    assert new_draft["status"] == "active"
    assert renamed is not None
    assert renamed["name"] == "Renamed Draft"
    assert renamed["status"] == "trashed"
    assert renamed["state_version"] == 1
    assert copied is not None
    assert copied["name"] == "Copied Draft"
    assert copied["state_version"] == 0


def test_draft_created_copies_defaults_from_payload(tmp_path: Path) -> None:
    _prepare_workspace(tmp_path)
    engine = create_workspace_engine(tmp_path)

    result = apply(
        [
            {
                "event": "DraftCreated",
                "draft_id": "draft_1",
                "payload": {
                    "name": "7月7日",
                    "defaults": {"aspect_ratio": "16:9", "fps": 24},
                },
            }
        ],
        engine=engine,
        base_version=None,
        actor="user",
        created_at=NOW,
    )

    with begin_immediate(engine) as connection:
        row = DraftsRepository(connection).get("draft_1")

    assert result.status == "applied"
    assert row is not None
    assert row["name"] == "7月7日"
    assert row["defaults"]["aspect_ratio"] == "16:9"
    assert row["defaults"]["fps"] == 24
    assert row["created_at"] == NOW
    assert row["updated_at"] == NOW


def test_draft_copied_deep_copies_owned_references_and_asset_links(tmp_path: Path) -> None:
    _prepare_workspace(tmp_path)
    _insert_draft(tmp_path, timeline_current_version=1)
    _insert_timeline(tmp_path, version=1)
    engine = create_workspace_engine(tmp_path)
    with begin_immediate(engine) as connection:
        connection.execute(
            schema.objects.insert().values(
                hash="hash_preview",
                rel_path="objects/hash_preview",
                size=0,
                created_at=NOW,
            )
        )
        connection.execute(
            schema.previews.insert().values(
                preview_id="preview_1",
                draft_id="draft_1",
                timeline_version=1,
                object_hash="hash_preview",
                quality=dump_json({}),
                created_at=NOW,
            )
        )
        connection.execute(
            schema.assets.insert().values(
                asset_id="asset_1",
                storage_mode="reference",
                object_hash=None,
                reference_path="/tmp/asset_1.mp4",
                kind="video",
                source="local_path",
                filename="a.mp4",
                hash="hash_a",
                mtime=1,
                size=1,
                probe=None,
                proxy_object_hash=None,
                ingest_status="imported",
                usable=True,
                failure=None,
            )
        )
        connection.execute(
            schema.draft_asset_links.insert().values(
                draft_id="draft_1",
                asset_id="asset_1",
                linked_at=NOW,
                note="keep",
                rel_dir="clips",
            )
        )
        connection.execute(
            schema.drafts.update()
            .where(schema.drafts.c.draft_id == "draft_1")
            .values(preview_current_id="preview_1")
        )

    result = apply(
        [
            {
                "event": "DraftCopied",
                "draft_id": "draft_2",
                "source_draft_id": "draft_1",
                "payload": {"name": "Copied Draft"},
            }
        ],
        engine=engine,
        base_version=None,
        actor="user",
        created_at=NOW,
    )

    with begin_immediate(engine) as connection:
        copied = DraftsRepository(connection).get("draft_2")
        copied_timeline = connection.execute(
            select(schema.timeline_versions).where(
                schema.timeline_versions.c.draft_id == "draft_2",
                schema.timeline_versions.c.version == 1,
            )
        ).one()
        copied_preview = connection.execute(
            select(schema.previews).where(schema.previews.c.preview_id == "draft_2:preview_1")
        ).one()
        copied_link = connection.execute(
            select(schema.draft_asset_links).where(
                schema.draft_asset_links.c.draft_id == "draft_2",
                schema.draft_asset_links.c.asset_id == "asset_1",
            )
        ).one()

    assert result.status == "applied"
    assert copied is not None
    assert copied["timeline_current_version"] == 1
    assert copied["preview_current_id"] == "draft_2:preview_1"
    assert copied_timeline._mapping["draft_id"] == "draft_2"
    assert load_json(copied_timeline._mapping["document_json"])["draft_id"] == "draft_2"
    assert copied_preview._mapping["draft_id"] == "draft_2"
    assert copied_link._mapping["note"] == "keep"
    assert copied_link._mapping["rel_dir"] == "clips"


def test_asset_and_asset_link_merge_events_update_records(tmp_path: Path) -> None:
    _prepare_workspace(tmp_path)
    _insert_draft(tmp_path)
    engine = create_workspace_engine(tmp_path)

    result = apply(
        [
            {
                "event": "AssetImported",
                "asset_id": "asset_1",
                "job_id": "job_import_1",
                "payload": {
                    "object_hash": "obj_asset_1",
                    "reference_path": "/tmp/source.mp4",
                    "size": 10,
                },
            },
            {
                "event": "AssetProbed",
                "asset_id": "asset_1",
                "job_id": "job_probe_1",
                "payload": {"probe": {"duration_sec": 3}, "ingest_status": "probed"},
            },
            {
                "event": "ProxyGenerated",
                "asset_id": "asset_1",
                "job_id": "job_proxy_1",
                "payload": {"proxy_object_hash": "obj_proxy_1", "ingest_status": "ready"},
            },
            {
                "event": "AssetInvalidated",
                "asset_id": "asset_1",
                "job_id": "job_invalid_1",
                "payload": {"failure": {"message": "bad media"}},
            },
            {
                "event": "AssetImported",
                "asset_id": "asset_2",
                "job_id": "job_import_2",
                "payload": {"reference_path": "/tmp/bad.mp4"},
            },
            {
                "event": "AssetIndexFailed",
                "asset_id": "asset_2",
                "payload": {"failure": {"message": "no speech"}},
            },
            {
                "event": "AssetLinked",
                "draft_id": "draft_1",
                "asset_id": "asset_1",
                "payload": {"note": "first link", "rel_dir": "clips"},
            },
            {
                "event": "AssetLinked",
                "draft_id": "draft_1",
                "asset_id": "asset_1",
                "payload": {"note": "second link"},
            },
            {"event": "AssetUnlinked", "draft_id": "draft_1", "asset_id": "asset_1"},
        ],
        engine=engine,
        base_version=None,
        actor="job",
        created_at=NOW,
    )

    with begin_immediate(engine) as connection:
        asset_1 = connection.execute(
            select(schema.assets).where(schema.assets.c.asset_id == "asset_1")
        ).one()
        asset_2 = connection.execute(
            select(schema.assets).where(schema.assets.c.asset_id == "asset_2")
        ).one()
        link_count = connection.execute(
            select(func.count()).select_from(schema.draft_asset_links)
        ).scalar_one()
        object_count = connection.execute(
            select(func.count())
            .select_from(schema.objects)
            .where(schema.objects.c.hash.in_(["obj_asset_1", "obj_proxy_1"]))
        ).scalar_one()

    asset_1_values = asset_1._mapping
    asset_2_values = asset_2._mapping
    assert result.status == "applied"
    assert load_json(asset_1_values["probe"]) == {"duration_sec": 3}
    assert asset_1_values["proxy_object_hash"] == "obj_proxy_1"
    assert asset_1_values["usable"] is False
    assert load_json(asset_1_values["failure"]) == {"message": "bad media"}
    assert load_json(asset_2_values["failure"]) == {"message": "no speech"}
    assert object_count == 2
    assert link_count == 0


def test_asset_index_and_understanding_events_update_asset_columns(tmp_path: Path) -> None:
    _prepare_workspace(tmp_path)
    engine = create_workspace_engine(tmp_path)

    result = apply(
        [
            {
                "event": "AssetImported",
                "asset_id": "asset_1",
                "job_id": "job_import_1",
                "payload": {"reference_path": "/tmp/source.mp4", "size": 10},
            },
            {
                "event": "AssetIndexReady",
                "asset_id": "asset_1",
                "payload": {
                    "index_json": {"shots": [{"start_s": 0.0, "end_s": 2.0}]},
                    "thumbnail_object_hash": "thumb_hash_1",
                    "ingest_status": "indexed",
                },
            },
            {
                "event": "MaterialUnderstandingStarted",
                "asset_id": "asset_1",
                "payload": {"version": 1},
            },
            {
                "event": "MaterialUnderstandingCompleted",
                "asset_id": "asset_1",
                "payload": {"summary_id": "sum_1", "version": 1},
            },
            {
                "event": "AssetImported",
                "asset_id": "asset_2",
                "job_id": "job_import_2",
                "payload": {"reference_path": "/tmp/bad.mp4"},
            },
            {
                "event": "AssetIndexFailed",
                "asset_id": "asset_2",
                "payload": {"failure": {"message": "scenedetect crashed"}},
            },
            {
                "event": "MaterialUnderstandingFailed",
                "asset_id": "asset_2",
                "payload": {"failure": {"message": "subagent timeout"}},
            },
        ],
        engine=engine,
        base_version=None,
        actor="job",
        created_at=NOW,
    )

    with begin_immediate(engine) as connection:
        asset_1 = (
            connection.execute(select(schema.assets).where(schema.assets.c.asset_id == "asset_1"))
            .one()
            ._mapping
        )
        asset_2 = (
            connection.execute(select(schema.assets).where(schema.assets.c.asset_id == "asset_2"))
            .one()
            ._mapping
        )
        thumbnail_object = connection.execute(
            select(schema.objects).where(schema.objects.c.hash == "thumb_hash_1")
        ).first()

    assert result.status == "applied"
    # AssetIndexReady：写便宜索引 JSON、缩略图哈希，并把摄入状态推到 indexed
    assert load_json(asset_1["index_json"]) == {"shots": [{"start_s": 0.0, "end_s": 2.0}]}
    assert asset_1["thumbnail_object_hash"] == "thumb_hash_1"
    assert asset_1["ingest_status"] == "indexed"
    # 缩略图哈希被登记进 objects（满足外键）
    assert thumbnail_object is not None
    # MaterialUnderstanding* 顺序推进 understanding_status
    assert asset_1["understanding_status"] == "ready"
    # asset_2：AssetIndexFailed 记录索引失败，MaterialUnderstandingFailed 只置理解状态
    assert asset_2["understanding_status"] == "failed"
    assert load_json(asset_2["failure"]) == {"message": "scenedetect crashed"}


def test_asset_understanding_status_defaults_to_none(tmp_path: Path) -> None:
    _prepare_workspace(tmp_path)
    engine = create_workspace_engine(tmp_path)

    apply(
        [
            {
                "event": "AssetImported",
                "asset_id": "asset_1",
                "job_id": "job_import_1",
                "payload": {"reference_path": "/tmp/source.mp4", "size": 10},
            }
        ],
        engine=engine,
        base_version=None,
        actor="job",
        created_at=NOW,
    )

    with begin_immediate(engine) as connection:
        asset = (
            connection.execute(select(schema.assets).where(schema.assets.c.asset_id == "asset_1"))
            .one()
            ._mapping
        )

    # server_default：新导入素材理解状态为 none，索引列为空
    assert asset["understanding_status"] == "none"
    assert asset["index_json"] is None
    assert asset["thumbnail_object_hash"] is None


def test_asset_hash_computed_updates_hash_mtime_size(tmp_path: Path) -> None:
    _prepare_workspace(tmp_path)
    engine = create_workspace_engine(tmp_path)

    result = apply(
        [
            {
                "event": "AssetImported",
                "asset_id": "asset_1",
                "job_id": "job_import_1",
                "payload": {
                    "reference_path": "/tmp/source.mp4",
                    "size": 10,
                    "mtime": 5,
                    "hash": "pending:10:5",
                    "ingest_status": "imported",
                },
            },
            {
                "event": "AssetHashComputed",
                "asset_id": "asset_1",
                "job_id": "job_hash_1",
                # 同刻快照：hash 连同 mtime/size 一起落库（描述改写后的新文件）。
                "payload": {"hash": "b" * 64, "mtime": 99, "size": 42},
            },
        ],
        engine=engine,
        base_version=None,
        actor="job",
        created_at=NOW,
    )

    with begin_immediate(engine) as connection:
        asset = (
            connection.execute(select(schema.assets).where(schema.assets.c.asset_id == "asset_1"))
            .one()
            ._mapping
        )

    assert result.status == "applied"
    # canonical sha256 覆盖 pending 占位，且三列同刻一致——其余保持 AssetImported 的落库值。
    assert asset["hash"] == "b" * 64
    assert asset["mtime"] == 99
    assert asset["size"] == 42
    assert asset["ingest_status"] == "imported"
    assert asset["reference_path"] == "/tmp/source.mp4"


def _asset_mapping(engine: Any, asset_id: str) -> Any:
    with begin_immediate(engine) as connection:
        return (
            connection.execute(select(schema.assets).where(schema.assets.c.asset_id == asset_id))
            .one()
            ._mapping
        )


def test_asset_index_ready_merges_index_json_by_key_either_order(tmp_path: Path) -> None:
    _prepare_workspace(tmp_path)
    engine = create_workspace_engine(tmp_path)
    apply(
        [
            {
                "event": "AssetImported",
                "asset_id": "asset_1",
                "job_id": "job_import_1",
                "payload": {"reference_path": "/tmp/a.mp4", "size": 1},
            }
        ],
        engine=engine,
        base_version=None,
        actor="job",
        created_at=NOW,
    )

    def _index_ready(payload: dict[str, Any]) -> dict[str, Any]:
        return {"event": "AssetIndexReady", "asset_id": "asset_1", "payload": payload}

    # 顺序：按需分镜回填先到（只带 shots），index job 后到（只带 duration_sec + 缩略图）。
    apply(
        [
            _index_ready(
                {"index_json": {"shots": [{"shot_id": "s1", "start_sec": 0.0, "end_sec": 2.0}]}}
            )
        ],
        engine=engine,
        base_version=None,
        actor="job",
        created_at=NOW,
    )
    apply(
        [
            _index_ready(
                {
                    "index_json": {"duration_sec": 12.0},
                    "thumbnail_object_hash": "thumb_index",
                    "ingest_status": "indexed",
                }
            )
        ],
        engine=engine,
        base_version=None,
        actor="job",
        created_at=NOW,
    )

    asset = _asset_mapping(engine, "asset_1")
    # index job 带整份 index_json 也不再覆盖已回填的 shots：两键并存（消掉乱序竞态）。
    assert load_json(asset["index_json"]) == {
        "shots": [{"shot_id": "s1", "start_sec": 0.0, "end_sec": 2.0}],
        "duration_sec": 12.0,
    }
    assert asset["thumbnail_object_hash"] == "thumb_index"
    assert asset["ingest_status"] == "indexed"


def test_asset_index_ready_backfill_keeps_poster_thumbnail(tmp_path: Path) -> None:
    _prepare_workspace(tmp_path)
    engine = create_workspace_engine(tmp_path)
    apply(
        [
            {
                "event": "AssetImported",
                "asset_id": "asset_1",
                "job_id": "job_import_1",
                "payload": {"reference_path": "/tmp/a.mp4", "size": 1},
            }
        ],
        engine=engine,
        base_version=None,
        actor="job",
        created_at=NOW,
    )
    # poster 先补写缩略图（只带 thumbnail、无 index_json）。
    apply(
        [
            {
                "event": "AssetIndexReady",
                "asset_id": "asset_1",
                "payload": {"thumbnail_object_hash": "thumb_poster"},
            }
        ],
        engine=engine,
        base_version=None,
        actor="job",
        created_at=NOW,
    )
    # 回填期间（F8）只带 shots：既不清缩略图、也不覆盖并发写入。
    apply(
        [
            {
                "event": "AssetIndexReady",
                "asset_id": "asset_1",
                "payload": {
                    "index_json": {"shots": [{"shot_id": "s1", "start_sec": 0.0, "end_sec": 2.0}]}
                },
            }
        ],
        engine=engine,
        base_version=None,
        actor="job",
        created_at=NOW,
    )

    asset = _asset_mapping(engine, "asset_1")
    assert asset["thumbnail_object_hash"] == "thumb_poster"
    assert load_json(asset["index_json"]) == {
        "shots": [{"shot_id": "s1", "start_sec": 0.0, "end_sec": 2.0}]
    }


def test_merge_preview_result_records_artifact_without_stale_version_conflict(
    tmp_path: Path,
) -> None:
    _prepare_workspace(tmp_path)
    _insert_draft(tmp_path, state_version=12, timeline_current_version=3)
    _insert_timeline(tmp_path, version=2)
    _insert_timeline(tmp_path, version=3)
    engine = create_workspace_engine(tmp_path)

    result = apply(
        [
            {
                "event": "PreviewRendered",
                "draft_id": "draft_1",
                "timeline_version": 2,
                "artifact_id": "preview_old",
            }
        ],
        engine=engine,
        base_version=None,
        actor="job",
        created_at=NOW,
    )

    assert result.status == "applied"
    with begin_immediate(engine) as connection:
        case = DraftsRepository(connection).get("draft_1")
        preview = connection.execute(
            select(schema.previews).where(schema.previews.c.preview_id == "preview_old")
        ).first()
    assert preview is not None
    assert case is not None
    assert case["preview_current_id"] is None
    assert case["state_version"] == 12


def test_preview_and_export_merge_events_keep_history_and_update_current_conditionally(
    tmp_path: Path,
) -> None:
    _prepare_workspace(tmp_path)
    _insert_draft(tmp_path, timeline_current_version=2)
    _insert_timeline(tmp_path, version=1)
    _insert_timeline(tmp_path, version=2)
    engine = create_workspace_engine(tmp_path)

    result = apply(
        [
            {
                "event": "PreviewRendered",
                "draft_id": "draft_1",
                "timeline_version": 1,
                "artifact_id": "preview_history",
            },
            {
                "event": "PreviewRendered",
                "draft_id": "draft_1",
                "timeline_version": 2,
                "artifact_id": "preview_current",
            },
            {
                "event": "ExportCompleted",
                "draft_id": "draft_1",
                "timeline_version": 1,
                "artifact_id": "export_history",
            },
            {
                "event": "ExportCompleted",
                "draft_id": "draft_1",
                "timeline_version": 2,
                "artifact_id": "export_current",
            },
        ],
        engine=engine,
        base_version=None,
        actor="job",
        created_at=NOW,
    )

    with begin_immediate(engine) as connection:
        case = DraftsRepository(connection).get("draft_1")
        preview_count = connection.execute(
            select(func.count()).select_from(schema.previews)
        ).scalar_one()
        export_count = connection.execute(
            select(func.count()).select_from(schema.exports)
        ).scalar_one()

    assert result.status == "applied"
    assert case is not None
    assert case["preview_current_id"] == "preview_current"
    assert case["export_current_id"] == "export_current"
    assert case["state_version"] == 1
    assert preview_count == 2
    assert export_count == 2


def test_preview_viewed_only_updates_when_preview_belongs_to_case(tmp_path: Path) -> None:
    _prepare_workspace(tmp_path)
    _insert_draft(tmp_path)
    _insert_extra_draft(tmp_path, "draft_2")
    engine = create_workspace_engine(tmp_path)
    with begin_immediate(engine) as connection:
        for object_hash in ("hash_preview_1", "hash_preview_2"):
            connection.execute(
                schema.objects.insert().values(
                    hash=object_hash,
                    rel_path=f"objects/{object_hash}",
                    size=0,
                    created_at=NOW,
                )
            )
        connection.execute(
            schema.previews.insert().values(
                preview_id="preview_other",
                draft_id="draft_2",
                timeline_version=1,
                object_hash="hash_preview_1",
                quality=dump_json({}),
                created_at=NOW,
            )
        )
        connection.execute(
            schema.previews.insert().values(
                preview_id="preview_own",
                draft_id="draft_1",
                timeline_version=1,
                object_hash="hash_preview_2",
                quality=dump_json({}),
                created_at=NOW,
            )
        )

    result = apply(
        [
            {"event": "PreviewViewed", "draft_id": "draft_1", "preview_id": "preview_other"},
            {"event": "PreviewViewed", "draft_id": "draft_1", "preview_id": "preview_own"},
        ],
        engine=engine,
        base_version=None,
        actor="user",
        created_at=NOW,
    )

    with begin_immediate(engine) as connection:
        case = DraftsRepository(connection).get("draft_1")

    assert result.status == "applied"
    assert case is not None
    assert case["last_viewed_preview_id"] == "preview_own"
    assert case["state_version"] == 1


def test_merge_event_is_idempotent_by_merge_key(tmp_path: Path) -> None:
    _prepare_workspace(tmp_path)
    _insert_draft(tmp_path, timeline_current_version=2)
    _insert_timeline(tmp_path, version=2)
    engine = create_workspace_engine(tmp_path)
    event = {
        "event": "PreviewRendered",
        "draft_id": "draft_1",
        "timeline_version": 2,
        "artifact_id": "preview_1",
    }

    first = apply([event], engine=engine, base_version=None, actor="job", created_at=NOW)
    second = apply([event], engine=engine, base_version=None, actor="job", created_at=NOW)

    with begin_immediate(engine) as connection:
        preview_count = connection.execute(
            select(func.count()).select_from(schema.previews)
        ).scalar_one()
        case = DraftsRepository(connection).get("draft_1")
    assert first.status == "applied"
    assert second.status == "applied"
    assert second.skipped_events == 1
    assert preview_count == 1
    assert _event_log_count(tmp_path) == 1
    assert case is not None
    assert case["preview_current_id"] == "preview_1"
    assert case["state_version"] == 1


def test_rough_cut_approval_state_machine_across_timeline_changes(tmp_path: Path) -> None:
    _prepare_workspace(tmp_path)
    _insert_draft(tmp_path, timeline_current_version=1)
    _insert_timeline(tmp_path, version=1)
    engine = create_workspace_engine(tmp_path)

    created = apply(
        [
            {
                "event": "DecisionCreated",
                "decision_id": "dec_rough",
                "scope_type": "draft",
                "draft_id": "draft_1",
                "payload": {
                    "decision": {
                        "decision_id": "dec_rough",
                        "scope_type": "draft",
                        "draft_id": "draft_1",
                        "type": "approve_rough_cut",
                        "question": "ok?",
                        "blocking": True,
                    }
                },
            }
        ],
        engine=engine,
        base_version=0,
        actor="agent",
        created_at=NOW,
    )
    answered = apply(
        [
            {
                "event": "DecisionAnswered",
                "decision_id": "dec_rough",
                "scope_type": "draft",
                "draft_id": "draft_1",
                "payload": {
                    "answer": {
                        "option_id": "approve",
                        "answered_via": "button",
                        "payload": {"timeline_version": 1},
                    }
                },
            }
        ],
        engine=engine,
        base_version=1,
        actor="user",
        created_at=NOW,
    )
    changed = apply(
        [
            {
                "event": "TimelineVersionCreated",
                "draft_id": "draft_1",
                "timeline_version": 2,
                "parent_version": 1,
                "patch_id": "patch_visual",
                "payload": {
                    "document_json": _timeline_doc(2),
                    "changed_track_ids": ["visual_base"],
                },
            }
        ],
        engine=engine,
        base_version=2,
        actor="agent",
        created_at=NOW,
    )
    restored = apply(
        [
            {
                "event": "TimelineVersionRestored",
                "draft_id": "draft_1",
                "timeline_version": 1,
            }
        ],
        engine=engine,
        base_version=3,
        actor="user",
        created_at=NOW,
    )

    with begin_immediate(engine) as connection:
        case = DraftsRepository(connection).get("draft_1")
    assert created.status == answered.status == changed.status == restored.status == "applied"
    assert case is not None
    assert case["rough_cut_approved"] is True
    assert case["rough_cut_approved_version"] == 1
    assert case["timeline_current_version"] == 1
    assert case["state_version"] == 4


def test_timeline_created_validated_failed_and_restored_update_draft_state(
    tmp_path: Path,
) -> None:
    _prepare_workspace(tmp_path)
    _insert_draft(
        tmp_path,
        timeline_current_version=1,
        rough_cut_approved=True,
        rough_cut_approved_version=1,
    )
    _insert_timeline(tmp_path, version=1)
    engine = create_workspace_engine(tmp_path)

    subtitles_only = apply(
        [
            {
                "event": "TimelineVersionCreated",
                "draft_id": "draft_1",
                "timeline_version": 2,
                "parent_version": 1,
                "patch_id": "patch_subtitles",
                "payload": {"changed_track_ids": ["subtitles"]},
            }
        ],
        engine=engine,
        base_version=0,
        actor="agent",
        created_at=NOW,
    )
    reset_by_default = apply(
        [
            {
                "event": "TimelineVersionCreated",
                "draft_id": "draft_1",
                "timeline_version": 3,
                "parent_version": 2,
                "patch_id": "patch_unknown",
            }
        ],
        engine=engine,
        base_version=1,
        actor="agent",
        created_at=NOW,
    )
    validated = apply(
        [
            {
                "event": "TimelineValidated",
                "draft_id": "draft_1",
                "timeline_version": 3,
                "payload": {"validation_report": {"valid": True, "checks": []}},
            }
        ],
        engine=engine,
        base_version=2,
        actor="agent",
        created_at=NOW,
    )
    validation_failed = apply(
        [
            {
                "event": "TimelineValidationFailed",
                "draft_id": "draft_1",
                "timeline_version": 3,
                "payload": {"validation_report": {"valid": False, "checks": [{"code": "gap"}]}},
            }
        ],
        engine=engine,
        base_version=3,
        actor="agent",
        created_at=NOW,
    )
    restored_hit = apply(
        [{"event": "TimelineVersionRestored", "draft_id": "draft_1", "timeline_version": 1}],
        engine=engine,
        base_version=4,
        actor="user",
        created_at=NOW,
    )
    restored_miss = apply(
        [{"event": "TimelineVersionRestored", "draft_id": "draft_1", "timeline_version": 2}],
        engine=engine,
        base_version=5,
        actor="user",
        created_at=NOW,
    )

    with begin_immediate(engine) as connection:
        case = DraftsRepository(connection).get("draft_1")
        timeline_3 = connection.execute(
            select(schema.timeline_versions).where(
                schema.timeline_versions.c.draft_id == "draft_1",
                schema.timeline_versions.c.version == 3,
            )
        ).one()

    assert subtitles_only.status == "applied"
    assert reset_by_default.status == "applied"
    assert validated.status == "applied"
    assert validation_failed.status == "applied"
    assert restored_hit.status == "applied"
    assert restored_miss.status == "applied"
    assert case is not None
    assert case["timeline_current_version"] == 2
    assert case["timeline_validated"] is False
    assert case["rough_cut_approved"] is False
    assert case["rough_cut_approved_version"] == 1
    assert case["state_version"] == 6
    assert load_json(timeline_3._mapping["validation_report"]) == {
        "valid": False,
        "checks": [{"code": "gap"}],
    }


def test_pending_tool_call_followup_is_returned_but_not_executed(tmp_path: Path) -> None:
    _prepare_workspace(tmp_path)
    _insert_draft(tmp_path)
    engine = create_workspace_engine(tmp_path)
    create_result = apply(
        [
            {
                "event": "DecisionCreated",
                "decision_id": "dec_export",
                "scope_type": "draft",
                "draft_id": "draft_1",
                "payload": {
                    "decision": {
                        "decision_id": "dec_export",
                        "scope_type": "draft",
                        "draft_id": "draft_1",
                        "type": "export",
                        "question": "export?",
                        "blocking": True,
                        "pending_tool_call": {
                            "tool_name": "render.final_mp4",
                            "arguments": {"draft_id": "draft_1"},
                            "idempotency_key": "dec_export",
                            "argument_fingerprint": "fp",
                        },
                        "pending_tool_call_status": "pending",
                    }
                },
            }
        ],
        engine=engine,
        base_version=0,
        actor="agent",
        created_at=NOW,
    )
    answer_result = apply(
        [
            {
                "event": "DecisionAnswered",
                "decision_id": "dec_export",
                "scope_type": "draft",
                "draft_id": "draft_1",
                "payload": {
                    "answer": {
                        "option_id": "approve",
                        "answered_via": "button",
                        "payload": {},
                    }
                },
            }
        ],
        engine=engine,
        base_version=1,
        actor="user",
        created_at=NOW,
    )

    with begin_immediate(engine) as connection:
        job_count = connection.execute(select(func.count()).select_from(schema.jobs)).scalar_one()
        decision = connection.execute(
            select(schema.decisions).where(schema.decisions.c.decision_id == "dec_export")
        ).first()
    assert create_result.status == "applied"
    assert answer_result.status == "applied"
    assert answer_result.followups[0].kind == "replay_pending_tool_call"
    assert job_count == 0
    assert decision is not None
    assert decision._mapping["pending_tool_call_status"] == "approved"


def test_plan_updates_patch_draft_state(tmp_path: Path) -> None:
    _prepare_workspace(tmp_path)
    _insert_draft(tmp_path)
    engine = create_workspace_engine(tmp_path)

    result = apply(
        [
            {
                "event": "BriefUpdated",
                "draft_id": "draft_1",
                "payload": {"brief": {"goal": "new goal", "confirmed_facts": ["fast"]}},
            },
            {
                "event": "ContentPlanUpdated",
                "draft_id": "draft_1",
                "payload": {"content_plan": {"outline": ["hook"]}},
            },
            {
                "event": "AudioPlanUpdated",
                "draft_id": "draft_1",
                "payload": {"audio_plan": {"mode": "silent"}},
            },
            {
                "event": "CutPlanUpdated",
                "draft_id": "draft_1",
                "payload": {
                    "cut_plan": {
                        "schema": "CutPlan.v1",
                        "slots": [],
                        "total_target_duration_sec": 12.0,
                    }
                },
            },
            {
                "event": "PostprocessPlanUpdated",
                "draft_id": "draft_1",
                "payload": {
                    "postprocess_plan": {
                        "subtitle": {"enabled": True, "style_template_id": "large"}
                    }
                },
            },
        ],
        engine=engine,
        base_version=0,
        actor="agent",
        created_at=NOW,
    )

    with begin_immediate(engine) as connection:
        case = DraftsRepository(connection).get("draft_1")

    assert result.status == "applied"
    assert case is not None
    assert case["brief"]["goal"] == "new goal"
    assert case["content_plan"] == {"outline": ["hook"]}
    assert case["audio_plan"]["mode"] == "silent"
    assert case["cut_plan"]["total_target_duration_sec"] == 12.0
    assert case["postprocess_plan"]["subtitle"]["style_template_id"] == "large"
    assert case["state_version"] == 1


def test_case_decision_answer_applies_effect_and_logs_followup_event(tmp_path: Path) -> None:
    _prepare_workspace(tmp_path)
    _insert_draft(tmp_path)
    engine = create_workspace_engine(tmp_path)

    created = apply(
        [
            {
                "event": "DecisionCreated",
                "decision_id": "dec_fact",
                "scope_type": "draft",
                "draft_id": "draft_1",
                "payload": {
                    "decision": {
                        "decision_id": "dec_fact",
                        "scope_type": "draft",
                        "draft_id": "draft_1",
                        "type": "generic",
                        "question": "fact?",
                        "blocking": True,
                    }
                },
            }
        ],
        engine=engine,
        base_version=0,
        actor="agent",
        created_at=NOW,
    )
    answered = apply(
        [
            {
                "event": "DecisionAnswered",
                "decision_id": "dec_fact",
                "scope_type": "draft",
                "draft_id": "draft_1",
                "payload": {
                    "answer": {
                        "option_id": None,
                        "free_text": "use a calm tone",
                        "answered_via": "natural_language",
                        "payload": {"reduce_target": "brief.confirmed_facts"},
                    }
                },
            }
        ],
        engine=engine,
        base_version=1,
        actor="user",
        created_at=NOW,
    )

    with begin_immediate(engine) as connection:
        case = DraftsRepository(connection).get("draft_1")
        events = EventLogRepository(connection).read_after(0)

    assert created.status == "applied"
    assert answered.status == "applied"
    assert case is not None
    assert case["pending_decision_id"] is None
    assert case["brief"]["confirmed_facts"] == ["use a calm tone"]
    assert [event.event_type for event in events] == [
        "DecisionCreated",
        "DecisionAnswered",
        "BriefUpdated",
    ]


def test_workspace_decisions_only_update_decision_rows(tmp_path: Path) -> None:
    _prepare_workspace(tmp_path)
    _insert_draft(tmp_path)
    engine = create_workspace_engine(tmp_path)

    result = apply(
        [
            {
                "event": "DecisionCreated",
                "decision_id": "dec_answered",
                "scope_type": "workspace",
                "payload": {
                    "decision": {
                        "decision_id": "dec_answered",
                        "scope_type": "workspace",
                        "type": "generic",
                        "question": "answered?",
                    }
                },
            },
            {
                "event": "DecisionAnswered",
                "decision_id": "dec_answered",
                "scope_type": "workspace",
                "payload": {
                    "answer": {
                        "option_id": "ok",
                        "answered_via": "button",
                        "payload": {"reduce_target": "scratch_memory", "value": "ignored"},
                    }
                },
            },
            {
                "event": "DecisionCreated",
                "decision_id": "dec_cancelled",
                "scope_type": "workspace",
                "payload": {
                    "decision": {
                        "decision_id": "dec_cancelled",
                        "scope_type": "workspace",
                        "type": "generic",
                        "question": "cancelled?",
                    }
                },
            },
            {
                "event": "DecisionCancelled",
                "decision_id": "dec_cancelled",
                "scope_type": "workspace",
            },
        ],
        engine=engine,
        base_version=None,
        actor="user",
        created_at=NOW,
    )

    with begin_immediate(engine) as connection:
        draft = DraftsRepository(connection).get("draft_1")
        answered_decision = connection.execute(
            select(schema.decisions).where(schema.decisions.c.decision_id == "dec_answered")
        ).one()
        cancelled_decision = connection.execute(
            select(schema.decisions).where(schema.decisions.c.decision_id == "dec_cancelled")
        ).one()

    assert result.status == "applied"
    assert draft is not None
    # workspace 域决策不阻塞草稿：state_version 不动、pending_decision_id 保持空。
    assert draft["state_version"] == 0
    assert draft["pending_decision_id"] is None
    assert answered_decision._mapping["status"] == "answered"
    assert answered_decision._mapping["draft_id"] is None
    assert cancelled_decision._mapping["status"] == "cancelled"


def test_memory_events_update_memory_tables_without_draft_state_changes(tmp_path: Path) -> None:
    _prepare_workspace(tmp_path)
    _insert_draft(tmp_path)
    engine = create_workspace_engine(tmp_path)

    result = apply(
        [
            {
                "event": "MemoryCandidateExtracted",
                "candidate_id": "mem_candidate_1",
                "draft_id": "draft_1",
                "payload": {"content": "User likes concise edits", "suggested_scope": "user"},
            },
            {
                "event": "MemoryCandidateDiscarded",
                "candidate_id": "mem_candidate_1",
                "draft_id": "draft_1",
            },
            {
                "event": "MemorySaved",
                "memory_id": "memory_1",
                "candidate_id": "mem_candidate_1",
                "payload": {
                    "scope": "user",
                    "content": "User likes concise edits",
                    "tags": ["style"],
                    "created_from_draft_id": "draft_1",
                },
            },
        ],
        engine=engine,
        base_version=None,
        actor="agent",
        created_at=NOW,
    )

    with begin_immediate(engine) as connection:
        case = DraftsRepository(connection).get("draft_1")
        candidate = connection.execute(
            select(schema.memory_candidates).where(
                schema.memory_candidates.c.candidate_id == "mem_candidate_1"
            )
        ).one()
        memory = connection.execute(
            select(schema.memories).where(schema.memories.c.memory_id == "memory_1")
        ).one()

    assert result.status == "applied"
    assert case is not None
    assert case["state_version"] == 0
    assert candidate._mapping["status"] == "saved"
    assert candidate._mapping["saved_memory_id"] == "memory_1"
    assert memory._mapping["scope"] == "user"
    assert load_json(memory._mapping["tags"]) == ["style"]


def test_job_events_maintain_running_jobs_until_terminal_status(tmp_path: Path) -> None:
    _prepare_workspace(tmp_path)
    _insert_draft(tmp_path)
    engine = create_workspace_engine(tmp_path)

    result = apply(
        [
            {
                "event": "JobEnqueued",
                "job_id": "job_success",
                "requested_by_draft_id": "draft_1",
                "payload": {"kind": "render.preview"},
            },
            {
                "event": "JobProgress",
                "job_id": "job_success",
                "requested_by_draft_id": "draft_1",
                "progress": 0.4,
            },
            {
                "event": "JobSucceeded",
                "job_id": "job_success",
                "requested_by_draft_id": "draft_1",
            },
            {
                "event": "JobEnqueued",
                "job_id": "job_failed",
                "requested_by_draft_id": "draft_1",
                "payload": {"kind": "annotate.asset"},
            },
            {
                "event": "JobFailed",
                "job_id": "job_failed",
                "requested_by_draft_id": "draft_1",
            },
            {
                "event": "JobEnqueued",
                "job_id": "job_cancelled",
                "requested_by_draft_id": "draft_1",
                "payload": {"kind": "import.url"},
            },
            {
                "event": "JobCancelled",
                "job_id": "job_cancelled",
                "requested_by_draft_id": "draft_1",
            },
        ],
        engine=engine,
        base_version=None,
        actor="job",
        created_at=NOW,
    )

    with begin_immediate(engine) as connection:
        case = DraftsRepository(connection).get("draft_1")
        rows = connection.execute(select(schema.jobs.c.job_id, schema.jobs.c.status)).all()

    statuses = {row._mapping["job_id"]: row._mapping["status"] for row in rows}
    assert result.status == "applied"
    assert case is not None
    assert case["running_jobs"] == []
    assert case["state_version"] == 1
    assert statuses == {
        "job_success": "succeeded",
        "job_failed": "failed",
        "job_cancelled": "cancelled",
    }


def test_record_only_events_are_logged_without_draft_state_mutation(tmp_path: Path) -> None:
    _prepare_workspace(tmp_path)
    _insert_draft(tmp_path)
    engine = create_workspace_engine(tmp_path)

    result = apply(
        [
            {"event": "PolicyRefusal", "refusal_id": "refusal_1"},
            {"event": "ProviderCallRecorded", "provider_call_id": "provider_call_1"},
            {"event": "ContextCompacted", "compaction_id": "compaction_1"},
            {"event": "TurnEnded", "turn_id": "turn_1", "draft_id": "draft_1"},
            {
                "event": "CapabilityDegraded",
                "degradation_id": "degradation_1",
                "capability": "render.preview",
                "reason": "provider unavailable",
                "fallback": "local",
                "draft_id": "draft_1",
            },
            {
                "event": "SecurityRefusal",
                "security_refusal_id": "security_refusal_1",
                "route": "/api/fs/read",
                "path": "/etc/passwd",
                "reason": "outside workspace",
            },
        ],
        engine=engine,
        base_version=None,
        actor="system",
        created_at=NOW,
    )

    with begin_immediate(engine) as connection:
        case = DraftsRepository(connection).get("draft_1")
        events = EventLogRepository(connection).read_after(0)

    assert result.status == "applied"
    assert result.draft_state_versions == {}
    assert all(event.state_version is None for event in result.applied_events)
    assert case is not None
    assert case["state_version"] == 0
    assert len(events) == 6
