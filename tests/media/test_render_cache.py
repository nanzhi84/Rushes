from __future__ import annotations

import os
from pathlib import Path

from media.render_cache import SegmentRenderCache, segment_cache_key
from storage.workspace_paths import WorkspacePaths


def test_segment_cache_hit_touches_and_reuses_file(tmp_path: Path) -> None:
    cache = SegmentRenderCache(WorkspacePaths.from_root(tmp_path), max_bytes=100)
    key = segment_cache_key({"asset_hash": "a", "source_range": [0, 30]})
    source = tmp_path / "segment.mp4"
    source.write_bytes(b"abcd")

    cached = cache.put_file(key, source)
    hit = cache.get(key)

    assert hit == cached
    assert hit.read_bytes() == b"abcd"


def test_segment_cache_lru_prunes_oldest_file(tmp_path: Path) -> None:
    cache = SegmentRenderCache(WorkspacePaths.from_root(tmp_path), max_bytes=5)
    key1 = segment_cache_key({"segment": 1})
    key2 = segment_cache_key({"segment": 2})
    source1 = tmp_path / "one.mp4"
    source2 = tmp_path / "two.mp4"
    source1.write_bytes(b"1111")
    source2.write_bytes(b"2222")

    first = cache.put_file(key1, source1)
    os.utime(first, (1, 1))
    second = cache.put_file(key2, source2)

    assert not first.exists()
    assert second.exists()
