from __future__ import annotations

import shutil
import subprocess
from pathlib import Path
from types import SimpleNamespace

import pytest

from media import preview_inspection
from media.preview_inspection import PreviewMediaInfo, PreviewSnapshot, inspect_preview_file

FFMPEG_AVAILABLE = shutil.which("ffmpeg") is not None and shutil.which("ffprobe") is not None


@pytest.mark.skipif(not FFMPEG_AVAILABLE, reason="ffmpeg/ffprobe not installed")
@pytest.mark.ffmpeg
def test_synthetic_preview_detects_black_freeze_silence_and_clipping_with_anchors(
    tmp_path: Path,
) -> None:
    preview = tmp_path / "defects.mp4"
    _make_defect_fixture(preview)

    result = inspect_preview_file(
        preview,
        expected=PreviewSnapshot(width=320, height=180, fps=30, duration_sec=3.0),
    )
    by_category = {issue.category: issue for issue in result.issues}

    black = by_category["black_frame"]
    assert black.at_sec == pytest.approx(1.0, abs=0.5)
    assert black.end_sec == pytest.approx(2.0, abs=0.5)
    freeze = by_category["freeze_frame"]
    assert freeze.at_sec == pytest.approx(1.0, abs=0.5)
    silence = by_category["silence"]
    assert silence.at_sec == pytest.approx(1.0, abs=0.5)
    assert silence.end_sec == pytest.approx(2.0, abs=0.5)
    assert by_category["clipping_risk"].severity == "error"
    assert "render_snapshot_mismatch" not in by_category


@pytest.mark.skipif(not FFMPEG_AVAILABLE, reason="ffmpeg/ffprobe not installed")
@pytest.mark.ffmpeg
def test_corrupted_preview_reports_bad_stream_or_decode_integrity(tmp_path: Path) -> None:
    valid = tmp_path / "valid.mp4"
    corrupt = tmp_path / "corrupt.mp4"
    _make_short_fixture(valid)
    payload = valid.read_bytes()
    # faststart 把 moov 放在文件头；保留头部并截断媒体负载，使 ffprobe 可读但全片解码失败。
    corrupt.write_bytes(payload[: max(1024, len(payload) * 2 // 3)])

    result = inspect_preview_file(corrupt, expected=PreviewSnapshot(), checks=["streams", "decode"])

    assert {issue.category for issue in result.issues} & {
        "stream_probe_failed",
        "decode_integrity",
    }


def test_nonzero_ffmpeg_checks_are_reported_as_errors(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    preview = tmp_path / "preview.mp4"
    preview.write_bytes(b"fixture")
    monkeypatch.setattr(
        preview_inspection,
        "_probe",
        lambda *_args, **_kwargs: PreviewMediaInfo(
            width=320,
            height=180,
            fps=30.0,
            duration_sec=2.0,
            has_video=True,
            has_audio=True,
            channels=2,
        ),
    )
    monkeypatch.setattr(
        preview_inspection,
        "_ffmpeg",
        lambda _command: SimpleNamespace(returncode=1, stderr="filter failed"),
    )

    result = inspect_preview_file(
        preview,
        expected=PreviewSnapshot(),
        checks=["black", "freeze", "silence", "loudness"],
    )

    assert {issue.category for issue in result.issues} == {
        "black_check_failed",
        "freeze_check_failed",
        "silence_check_failed",
        "loudness_check_failed",
    }
    assert all(issue.severity == "error" for issue in result.issues)


def _make_defect_fixture(path: Path) -> None:
    subprocess.run(
        [
            "ffmpeg",
            "-hide_banner",
            "-loglevel",
            "error",
            "-f",
            "lavfi",
            "-i",
            "testsrc2=size=320x180:rate=30:duration=1",
            "-f",
            "lavfi",
            "-i",
            "color=black:size=320x180:rate=30:duration=1",
            "-f",
            "lavfi",
            "-i",
            "testsrc2=size=320x180:rate=30:duration=1",
            "-f",
            "lavfi",
            "-i",
            "sine=frequency=440:sample_rate=48000:duration=1",
            "-f",
            "lavfi",
            "-i",
            "anullsrc=r=48000:cl=stereo:d=1",
            "-f",
            "lavfi",
            "-i",
            "sine=frequency=880:sample_rate=48000:duration=1",
            "-filter_complex",
            (
                "[0:v][1:v][2:v]concat=n=3:v=1:a=0[v];"
                "[3:a]aformat=channel_layouts=stereo[a0];"
                "[4:a]aformat=channel_layouts=stereo[a1];"
                "[5:a]volume=20,aformat=channel_layouts=stereo[a2];"
                "[a0][a1][a2]concat=n=3:v=0:a=1[a]"
            ),
            "-map",
            "[v]",
            "-map",
            "[a]",
            "-c:v",
            "libx264",
            "-pix_fmt",
            "yuv420p",
            "-c:a",
            "aac",
            "-movflags",
            "+faststart",
            str(path),
        ],
        check=True,
        timeout=60,
    )


def _make_short_fixture(path: Path) -> None:
    subprocess.run(
        [
            "ffmpeg",
            "-hide_banner",
            "-loglevel",
            "error",
            "-f",
            "lavfi",
            "-i",
            "testsrc2=size=160x90:rate=24:duration=2",
            "-c:v",
            "libx264",
            "-pix_fmt",
            "yuv420p",
            "-movflags",
            "+faststart",
            str(path),
        ],
        check=True,
        timeout=30,
    )
