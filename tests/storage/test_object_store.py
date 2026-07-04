from pathlib import Path

from storage import schema
from storage.db import begin_immediate, create_workspace_engine
from storage.object_store import ObjectStore
from storage.repositories.objects import ObjectsRepository
from storage.workspace_paths import WorkspacePaths


def test_cas_write_dedup_atomic_tmp_cleanup_and_gc(tmp_path: Path) -> None:
    paths = WorkspacePaths.from_root(tmp_path).initialize()
    engine = create_workspace_engine(paths)
    with engine.begin() as connection:
        schema.create_all(connection)

    with begin_immediate(engine) as connection:
        repository = ObjectsRepository(connection)
        store = ObjectStore(paths, repository)
        first = store.put_bytes(b"same")
        duplicate = store.put_bytes(b"same")
        second = store.put_bytes(b"delete-me")

        assert first == duplicate
        assert store.exists(first.object_hash)
        assert store.exists(second.object_hash)
        assert list(paths.tmp_dir.iterdir()) == []
        assert repository.get(first.object_hash) is not None

        result = store.gc({first.object_hash})

        assert result.deleted_hashes == (second.object_hash,)
        assert store.exists(first.object_hash)
        assert not store.exists(second.object_hash)
        assert repository.get(first.object_hash) is not None
        assert repository.get(second.object_hash) is None
