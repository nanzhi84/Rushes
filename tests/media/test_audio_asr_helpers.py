from __future__ import annotations

import os
import shutil
import subprocess
import sys
import types
import wave
from pathlib import Path
from typing import Any

import numpy as np
import pytest

from media.asr_upload import upload_audio_to_oss
from media.audio_extract import extract_audio_to_wav
from media.vad import SileroModelMissing, run_silero_vad
from storage.workspace_paths import WorkspacePaths

FFMPEG_AVAILABLE = shutil.which("ffmpeg") is not None and shutil.which("ffprobe") is not None


def test_silero_vad_missing_model_raises(tmp_path: Path) -> None:
    wav = tmp_path / "input.wav"
    _write_wav(wav, np.zeros(1600, dtype=np.int16))

    with pytest.raises(SileroModelMissing):
        run_silero_vad(wav, paths=WorkspacePaths.from_root(tmp_path / "workspace"))


def test_silero_vad_runs_with_fake_onnxruntime(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    model = tmp_path / "silero.onnx"
    model.write_bytes(b"fake")
    wav = tmp_path / "input.wav"
    samples = np.concatenate([np.zeros(8000, dtype=np.int16), np.full(8000, 8000, dtype=np.int16)])
    _write_wav(wav, samples)
    fake_module = types.SimpleNamespace(InferenceSession=lambda *_args, **_kwargs: _FakeSession())
    monkeypatch.setitem(sys.modules, "onnxruntime", fake_module)

    result = run_silero_vad(wav, model_path=model, min_silence_ms=300)

    assert result.speech_ratio > 0
    assert any(segment.kind == "speech" for segment in result.segments)
    assert any(segment.kind == "silence" for segment in result.segments)


@pytest.mark.skipif(
    not os.environ.get("RUSHES_SILERO_VAD_MODEL")
    or not Path(os.environ.get("RUSHES_SILERO_VAD_MODEL", "")).exists(),
    reason="RUSHES_SILERO_VAD_MODEL is not configured",
)
def test_silero_vad_runs_with_configured_model(tmp_path: Path) -> None:
    wav = tmp_path / "input.wav"
    _write_wav(wav, np.zeros(16000, dtype=np.int16))

    result = run_silero_vad(wav)

    assert isinstance(result.segments, list)


@pytest.mark.skipif(not FFMPEG_AVAILABLE, reason="ffmpeg not installed")
@pytest.mark.ffmpeg
def test_extract_audio_to_wav_with_ffmpeg_fixture(tmp_path: Path) -> None:
    source = tmp_path / "source.mp4"
    subprocess.run(
        [
            "ffmpeg",
            "-y",
            "-f",
            "lavfi",
            "-i",
            "sine=frequency=1000:duration=0.5",
            str(source),
        ],
        check=True,
        capture_output=True,
        text=True,
    )
    paths = WorkspacePaths.from_root(tmp_path / "workspace").initialize()

    extracted = extract_audio_to_wav(source, paths=paths)

    with wave.open(str(extracted.path), "rb") as wav:
        assert wav.getframerate() == 16000
        assert wav.getnchannels() == 1


def test_upload_audio_to_oss_uses_env_and_deletes(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    audio = tmp_path / "audio.wav"
    audio.write_bytes(b"wav")
    deleted: list[str] = []

    class FakeBucket:
        def __init__(self, _auth: object, endpoint: str, bucket: str) -> None:
            assert endpoint == "oss-cn.example.aliyuncs.com"
            assert bucket == "rushes"

        def put_object_from_file(self, key: str, path: str) -> None:
            assert key.startswith("test-prefix/")
            assert path == str(audio)

        def sign_url(self, method: str, key: str, expires: int) -> str:
            assert method == "GET"
            assert expires == 60
            return f"https://oss.example/{key}"

        def delete_object(self, key: str) -> None:
            deleted.append(key)

    fake_oss2 = types.SimpleNamespace(
        Auth=lambda access_key, secret_key: (access_key, secret_key),
        Bucket=FakeBucket,
    )
    monkeypatch.setitem(sys.modules, "oss2", fake_oss2)
    for key, value in {
        "RUSHES_OSS_ENDPOINT": "oss-cn.example.aliyuncs.com",
        "RUSHES_OSS_REGION": "cn-example",
        "RUSHES_OSS_BUCKET": "rushes",
        "RUSHES_OSS_ACCESS_KEY": "ak",
        "RUSHES_OSS_SECRET_KEY": "sk",
    }.items():
        monkeypatch.setenv(key, value)

    upload = upload_audio_to_oss(audio, key_prefix="test-prefix", expires_seconds=60)
    upload.delete()

    assert upload.signed_url.startswith("https://oss.example/test-prefix/")
    assert deleted == [upload.key]


class _FakeInput:
    def __init__(self, name: str) -> None:
        self.name = name


class _FakeSession:
    def get_inputs(self) -> list[_FakeInput]:
        return [_FakeInput("input"), _FakeInput("state"), _FakeInput("sr")]

    def get_outputs(self) -> list[_FakeInput]:
        return [_FakeInput("out"), _FakeInput("stateN")]

    def run(self, _outputs: object, feed: dict[str, Any]) -> list[np.ndarray[Any, Any]]:
        chunk = np.asarray(feed["input"])
        probability = 0.9 if float(np.abs(chunk).mean()) > 0.01 else 0.1
        return [
            np.array([[probability]], dtype=np.float32),
            np.zeros((2, 1, 128), dtype=np.float32),
        ]


def _write_wav(path: Path, samples: np.ndarray[Any, Any]) -> None:
    with wave.open(str(path), "wb") as wav:
        wav.setnchannels(1)
        wav.setsampwidth(2)
        wav.setframerate(16000)
        wav.writeframes(samples.astype(np.int16).tobytes())
