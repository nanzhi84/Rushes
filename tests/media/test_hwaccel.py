from __future__ import annotations

import subprocess

import pytest

import media.hwaccel as hwaccel


@pytest.fixture(autouse=True)
def _clear_cache() -> object:
    hwaccel._decode_available_cache.clear()
    yield
    hwaccel._decode_available_cache.clear()


def test_decode_args_empty_when_unavailable(monkeypatch) -> None:
    hwaccel._decode_available_cache["ffmpeg"] = False
    assert hwaccel.hwaccel_decode_args("ffmpeg") == []


def test_decode_args_present_when_available() -> None:
    hwaccel._decode_available_cache["ffmpeg"] = True
    assert hwaccel.hwaccel_decode_args("ffmpeg") == ["-hwaccel", "videotoolbox"]


def test_probe_returns_false_off_macos(monkeypatch) -> None:
    monkeypatch.setattr(hwaccel, "is_macos", lambda: False)
    calls: list[list[str]] = []
    monkeypatch.setattr(
        hwaccel.subprocess, "run", lambda *a, **k: calls.append(a[0]) or _completed(0, "")
    )
    assert hwaccel.videotoolbox_decode_available("ffmpeg") is False
    assert calls == []  # 非 macOS 直接判否，不起子进程


def test_probe_detects_videotoolbox_and_caches(monkeypatch) -> None:
    monkeypatch.setattr(hwaccel, "is_macos", lambda: True)
    runs = 0

    def fake_run(command, **kwargs):
        nonlocal runs
        runs += 1
        return _completed(0, "Hardware acceleration methods:\nvideotoolbox\n")

    monkeypatch.setattr(hwaccel.subprocess, "run", fake_run)
    assert hwaccel.videotoolbox_decode_available("ffmpeg") is True
    assert hwaccel.videotoolbox_decode_available("ffmpeg") is True
    assert runs == 1  # 每个 ffmpeg_bin 只探一次


def test_probe_returns_false_when_videotoolbox_absent(monkeypatch) -> None:
    monkeypatch.setattr(hwaccel, "is_macos", lambda: True)
    monkeypatch.setattr(
        hwaccel.subprocess, "run", lambda *a, **k: _completed(0, "Hardware:\nnvdec\n")
    )
    assert hwaccel.videotoolbox_decode_available("ffmpeg") is False


def test_probe_returns_false_on_subprocess_error(monkeypatch) -> None:
    monkeypatch.setattr(hwaccel, "is_macos", lambda: True)

    def boom(*args, **kwargs):
        raise OSError("no ffmpeg")

    monkeypatch.setattr(hwaccel.subprocess, "run", boom)
    assert hwaccel.videotoolbox_decode_available("ffmpeg") is False


def _completed(returncode: int, stdout: str) -> subprocess.CompletedProcess[str]:
    return subprocess.CompletedProcess(["ffmpeg"], returncode, stdout, "")
