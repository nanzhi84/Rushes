from __future__ import annotations

import shutil
import subprocess
from collections.abc import Callable
from pathlib import Path

import pytest

import media.hwaccel as hwaccel_module
import media.proxy as proxy_module
from media.probe import probe_media, probe_video_stream_format
from media.proxy import generate_proxy
from storage.workspace_paths import WorkspacePaths

FFMPEG_AVAILABLE = shutil.which("ffmpeg") is not None and shutil.which("ffprobe") is not None


@pytest.fixture(autouse=True)
def _clear_encoder_cache() -> object:
    # 编码/硬解探测结果都是进程级缓存，用例间必须清干净，避免相互污染。硬解默认置 False：让
    # 编码器选择类用例的命令断言保持稳定，且不真的去起 ffmpeg -hwaccels 子进程（保持 hermetic）。
    proxy_module._encoder_cache.clear()
    hwaccel_module._decode_available_cache.clear()
    hwaccel_module._decode_available_cache["ffmpeg"] = False
    yield
    proxy_module._encoder_cache.clear()
    hwaccel_module._decode_available_cache.clear()


def _fake_ffmpeg_runner(
    *,
    encoders_stdout: str,
    fail_encoders: frozenset[str] = frozenset(),
    fail_hwaccel_decode: bool = False,
) -> tuple[Callable[..., subprocess.CompletedProcess[str]], list[list[str]]]:
    """伪 subprocess.run：`-encoders` 返回给定清单；转码按 -c:v 编码器判成败，
    成功则给目标文件写真字节（供 ObjectStore.put_file 读取）。
    ``fail_hwaccel_decode`` 时，命令含 `-hwaccel` 的转码判失败（模拟输入硬解运行期失败）。"""
    calls: list[list[str]] = []

    def run(
        command: list[str],
        **_kwargs: object,
    ) -> subprocess.CompletedProcess[str]:
        calls.append(list(command))
        if "-encoders" in command:
            return subprocess.CompletedProcess(command, 0, encoders_stdout, "")
        encoder = command[command.index("-c:v") + 1]
        if encoder in fail_encoders or (fail_hwaccel_decode and "-hwaccel" in command):
            return subprocess.CompletedProcess(command, 1, "", f"{encoder} runtime failure")
        Path(command[-1]).write_bytes(b"proxy-bytes")
        return subprocess.CompletedProcess(command, 0, "", "")

    return run, calls


def _source_and_paths(tmp_path: Path) -> tuple[Path, WorkspacePaths]:
    source = tmp_path / "src.mp4"
    source.write_bytes(b"x")  # ffmpeg 被 mock，内容无所谓，只需文件存在（strict resolve）。
    paths = WorkspacePaths.from_root(tmp_path / "workspace").initialize()
    return source, paths


def _transcodes(calls: list[list[str]]) -> list[list[str]]:
    return [command for command in calls if "-encoders" not in command]


@pytest.mark.skipif(not FFMPEG_AVAILABLE, reason="ffmpeg/ffprobe not installed")
@pytest.mark.ffmpeg
def test_probe_media_reads_lavfi_fixture(tmp_path: Path) -> None:
    video = _make_test_video(tmp_path)

    probe = probe_media(video)

    assert probe.duration_sec > 0
    assert probe.fps is not None
    assert probe.width == 128
    assert probe.height == 128


@pytest.mark.skipif(not FFMPEG_AVAILABLE, reason="ffmpeg/ffprobe not installed")
@pytest.mark.ffmpeg
def test_generate_proxy_writes_object_store_proxy(tmp_path: Path) -> None:
    video = _make_test_video(tmp_path)
    paths = WorkspacePaths.from_root(tmp_path / "workspace").initialize()

    proxy_ref = generate_proxy(video, paths=paths)

    proxy_path = paths.object_path(proxy_ref.object_hash)
    assert proxy_path.exists()
    assert proxy_path.stat().st_size == proxy_ref.size
    assert proxy_ref.size > 0


def test_proxy_prefers_videotoolbox_on_macos(tmp_path: Path, monkeypatch) -> None:
    monkeypatch.setattr(proxy_module, "_is_macos", lambda: True)
    run, calls = _fake_ffmpeg_runner(encoders_stdout="V..... h264_videotoolbox HW H.264")
    monkeypatch.setattr(proxy_module, "run_media_command", run)
    source, paths = _source_and_paths(tmp_path)

    ref = generate_proxy(source, paths=paths)

    assert ref.size > 0
    transcode = _transcodes(calls)[0]
    assert "h264_videotoolbox" in transcode
    assert "libx264" not in transcode


def test_proxy_uses_libx264_when_videotoolbox_absent(tmp_path: Path, monkeypatch) -> None:
    monkeypatch.setattr(proxy_module, "_is_macos", lambda: True)
    run, calls = _fake_ffmpeg_runner(encoders_stdout="V..... libx264 H.264")
    monkeypatch.setattr(proxy_module, "run_media_command", run)
    source, paths = _source_and_paths(tmp_path)

    generate_proxy(source, paths=paths)

    transcode = _transcodes(calls)[0]
    assert "libx264" in transcode
    assert "h264_videotoolbox" not in transcode


def test_proxy_falls_back_to_libx264_when_hardware_transcode_fails(
    tmp_path: Path, monkeypatch
) -> None:
    monkeypatch.setattr(proxy_module, "_is_macos", lambda: True)
    run, calls = _fake_ffmpeg_runner(
        encoders_stdout="V..... h264_videotoolbox HW H.264",
        fail_encoders=frozenset({"h264_videotoolbox"}),
    )
    monkeypatch.setattr(proxy_module, "run_media_command", run)
    source, paths = _source_and_paths(tmp_path)

    ref = generate_proxy(source, paths=paths)

    assert ref.size > 0
    transcodes = _transcodes(calls)
    assert "h264_videotoolbox" in transcodes[0]
    assert "libx264" in transcodes[1]
    # 硬件运行期失败后，本进程后续探测结果被降级为软件编码。
    assert proxy_module._encoder_cache["ffmpeg"] == "libx264"


def test_proxy_probes_encoder_once_and_caches(tmp_path: Path, monkeypatch) -> None:
    monkeypatch.setattr(proxy_module, "_is_macos", lambda: True)
    run, calls = _fake_ffmpeg_runner(encoders_stdout="V..... h264_videotoolbox HW H.264")
    monkeypatch.setattr(proxy_module, "run_media_command", run)
    source, paths = _source_and_paths(tmp_path)

    generate_proxy(source, paths=paths)
    generate_proxy(source, paths=paths)

    probes = [command for command in calls if "-encoders" in command]
    assert len(probes) == 1  # 探测只跑一次，第二个 job 复用缓存


def test_proxy_uses_libx264_without_probe_off_macos(tmp_path: Path, monkeypatch) -> None:
    monkeypatch.setattr(proxy_module, "_is_macos", lambda: False)
    run, calls = _fake_ffmpeg_runner(encoders_stdout="V..... h264_videotoolbox HW H.264")
    monkeypatch.setattr(proxy_module, "run_media_command", run)
    source, paths = _source_and_paths(tmp_path)

    generate_proxy(source, paths=paths)

    assert not any("-encoders" in command for command in calls)  # 非 macOS 不探测硬件编码
    transcode = _transcodes(calls)[0]
    assert "libx264" in transcode
    assert transcode[transcode.index("-pix_fmt") + 1] == "yuv420p"


@pytest.mark.skipif(not FFMPEG_AVAILABLE, reason="ffmpeg/ffprobe not installed")
@pytest.mark.ffmpeg
def test_software_proxy_converts_422_source_to_browser_playable_420(
    tmp_path: Path, monkeypatch
) -> None:
    source = tmp_path / "source-422.mp4"
    subprocess.run(
        [
            "ffmpeg",
            "-y",
            "-f",
            "lavfi",
            "-i",
            "testsrc=duration=0.25:size=64x64:rate=24",
            "-c:v",
            "libx264",
            "-pix_fmt",
            "yuv422p",
            str(source),
        ],
        check=True,
        capture_output=True,
    )
    monkeypatch.setattr(proxy_module, "_is_macos", lambda: False)
    paths = WorkspacePaths.from_root(tmp_path / "workspace").initialize()

    ref = generate_proxy(source, paths=paths)

    assert probe_video_stream_format(paths.object_path(ref.object_hash)) == ("h264", "yuv420p")


def test_proxy_uses_hwaccel_decode_when_available(tmp_path: Path, monkeypatch) -> None:
    monkeypatch.setattr(proxy_module, "_is_macos", lambda: True)
    hwaccel_module._decode_available_cache["ffmpeg"] = True  # 输入侧硬解可用
    run, calls = _fake_ffmpeg_runner(encoders_stdout="V..... h264_videotoolbox HW H.264")
    monkeypatch.setattr(proxy_module, "run_media_command", run)
    source, paths = _source_and_paths(tmp_path)

    generate_proxy(source, paths=paths)

    transcode = _transcodes(calls)[0]
    # -hwaccel videotoolbox 放在 -i 之前作为输入解码参数。
    assert "-hwaccel" in transcode
    assert transcode.index("-hwaccel") < transcode.index("-i")
    assert transcode[transcode.index("-hwaccel") + 1] == "videotoolbox"


def test_proxy_falls_back_to_software_when_hwaccel_decode_fails(
    tmp_path: Path, monkeypatch
) -> None:
    monkeypatch.setattr(proxy_module, "_is_macos", lambda: True)
    hwaccel_module._decode_available_cache["ffmpeg"] = True
    run, calls = _fake_ffmpeg_runner(
        encoders_stdout="V..... h264_videotoolbox HW H.264",
        fail_hwaccel_decode=True,
    )
    monkeypatch.setattr(proxy_module, "run_media_command", run)
    source, paths = _source_and_paths(tmp_path)

    ref = generate_proxy(source, paths=paths)

    assert ref.size > 0
    transcodes = _transcodes(calls)
    assert "-hwaccel" in transcodes[0]  # 先试硬解
    # 硬件路径失败后整条回落软件路径：无 -hwaccel、软件编码器。
    assert "-hwaccel" not in transcodes[1]
    assert "libx264" in transcodes[1]


def _make_test_video(tmp_path: Path) -> Path:
    video = tmp_path / "fixture.mp4"
    subprocess.run(
        [
            "ffmpeg",
            "-y",
            "-f",
            "lavfi",
            "-i",
            "testsrc=duration=1:size=128x128:rate=30",
            "-pix_fmt",
            "yuv420p",
            str(video),
        ],
        check=True,
        capture_output=True,
        text=True,
    )
    return video
