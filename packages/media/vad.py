"""Silero ONNX VAD wrapper for 16 kHz mono WAV files."""

from __future__ import annotations

import importlib
import os
import wave
from dataclasses import dataclass
from pathlib import Path
from typing import Any, cast

import numpy as np
from numpy.typing import NDArray

from contracts.transcript import VadSegment
from storage.workspace_paths import WorkspacePaths

SAMPLE_RATE = 16000
WINDOW_SAMPLES = 512
DEFAULT_THRESHOLD = 0.5
DEFAULT_MIN_SILENCE_MS = 600


class SileroModelMissing(RuntimeError):
    """Raised when the configured Silero ONNX model file is absent."""


class VadError(RuntimeError):
    """Raised when VAD input or inference fails."""


@dataclass(frozen=True, slots=True)
class VadResult:
    segments: list[VadSegment]
    speech_ratio: float


def default_model_path(paths: WorkspacePaths) -> Path:
    return paths.root / "config" / "silero_vad.onnx"


def run_silero_vad(
    wav_path: str | Path,
    *,
    paths: WorkspacePaths | None = None,
    model_path: str | Path | None = None,
    threshold: float = DEFAULT_THRESHOLD,
    min_silence_ms: int = DEFAULT_MIN_SILENCE_MS,
) -> VadResult:
    """Run Silero ONNX VAD and return speech plus cuttable silence segments."""

    resolved_model = _resolve_model_path(paths=paths, model_path=model_path)
    if not resolved_model.exists():
        raise SileroModelMissing(f"Silero VAD model not found: {resolved_model}")
    samples = _read_wav_16k_mono(Path(wav_path))
    if samples.size == 0:
        return VadResult(segments=[], speech_ratio=0.0)
    ort = cast(Any, importlib.import_module("onnxruntime"))
    session = ort.InferenceSession(str(resolved_model), providers=["CPUExecutionProvider"])
    probabilities = _infer_probabilities(session, samples)
    segments = _segments_from_probabilities(
        probabilities,
        total_samples=int(samples.size),
        threshold=threshold,
        min_silence_ms=min_silence_ms,
    )
    speech_ms = sum(
        segment.end_ms - segment.start_ms for segment in segments if segment.kind == "speech"
    )
    total_ms = round(samples.size / SAMPLE_RATE * 1000)
    return VadResult(
        segments=segments,
        speech_ratio=0.0 if total_ms <= 0 else min(1.0, speech_ms / total_ms),
    )


def _resolve_model_path(
    *,
    paths: WorkspacePaths | None,
    model_path: str | Path | None,
) -> Path:
    if model_path is not None:
        return Path(model_path).expanduser().resolve(strict=False)
    env_path = os.environ.get("RUSHES_SILERO_VAD_MODEL")
    if env_path:
        return Path(env_path).expanduser().resolve(strict=False)
    if paths is None:
        return Path("config/silero_vad.onnx").expanduser().resolve(strict=False)
    return default_model_path(paths).resolve(strict=False)


def _read_wav_16k_mono(path: Path) -> NDArray[np.float32]:
    try:
        with wave.open(str(path.expanduser().resolve(strict=True)), "rb") as wav:
            channels = wav.getnchannels()
            sample_rate = wav.getframerate()
            sample_width = wav.getsampwidth()
            frames = wav.getnframes()
            data = wav.readframes(frames)
    except wave.Error as exc:
        raise VadError(f"invalid WAV input: {path}") from exc
    if channels != 1 or sample_rate != SAMPLE_RATE or sample_width != 2:
        raise VadError("Silero VAD requires 16 kHz mono 16-bit PCM WAV input")
    pcm = np.frombuffer(data, dtype=np.int16)
    return (pcm.astype(np.float32) / 32768.0).copy()


CONTEXT_SAMPLES = 64


def _infer_probabilities(session: Any, samples: NDArray[np.float32]) -> list[float]:
    input_names = [item.name for item in session.get_inputs()]
    output_names = [item.name for item in session.get_outputs()]
    state = np.zeros((2, 1, 128), dtype=np.float32)
    sr = np.array(SAMPLE_RATE, dtype=np.int64)
    # Silero v5+ 约定：每个 512 样本窗前须拼接上一窗末尾 64 样本上下文
    # （官方 python 包装内部行为；缺它推理概率恒近 0——真实语音实测踩坑）。
    context = np.zeros(CONTEXT_SAMPLES, dtype=np.float32)
    probabilities: list[float] = []
    for offset in range(0, int(samples.size), WINDOW_SAMPLES):
        chunk = samples[offset : offset + WINDOW_SAMPLES]
        if chunk.size < WINDOW_SAMPLES:
            chunk = np.pad(chunk, (0, WINDOW_SAMPLES - chunk.size))
        windowed = np.concatenate([context, chunk])
        feed: dict[str, Any] = {}
        if input_names:
            feed[input_names[0]] = windowed.reshape(1, -1)
        if len(input_names) > 1:
            feed[input_names[1]] = state
        if len(input_names) > 2:
            feed[input_names[2]] = sr
        outputs = session.run(output_names or None, feed)
        probabilities.append(_probability_from_output(outputs[0]))
        if len(outputs) > 1:
            state = cast(NDArray[np.float32], outputs[1])
        context = chunk[-CONTEXT_SAMPLES:]
    return probabilities


def _probability_from_output(value: object) -> float:
    array = np.asarray(value, dtype=np.float32)
    if array.size == 0:
        return 0.0
    return float(array.reshape(-1)[0])


def _segments_from_probabilities(
    probabilities: list[float],
    *,
    total_samples: int,
    threshold: float,
    min_silence_ms: int,
) -> list[VadSegment]:
    total_ms = round(total_samples / SAMPLE_RATE * 1000)
    speech_segments = _speech_segments(probabilities, total_ms=total_ms, threshold=threshold)
    if not speech_segments:
        return [VadSegment(start_ms=0, end_ms=max(1, total_ms), kind="silence")]
    segments: list[VadSegment] = []
    cursor = 0
    for speech in speech_segments:
        if speech.start_ms - cursor >= min_silence_ms:
            segments.append(VadSegment(start_ms=cursor, end_ms=speech.start_ms, kind="silence"))
        segments.append(speech)
        cursor = speech.end_ms
    if total_ms - cursor >= min_silence_ms:
        segments.append(VadSegment(start_ms=cursor, end_ms=total_ms, kind="silence"))
    return segments


def _speech_segments(
    probabilities: list[float],
    *,
    total_ms: int,
    threshold: float,
) -> list[VadSegment]:
    segments: list[VadSegment] = []
    current_start: int | None = None
    window_ms = WINDOW_SAMPLES / SAMPLE_RATE * 1000
    for index, probability in enumerate(probabilities):
        start_ms = round(index * window_ms)
        end_ms = min(total_ms, round((index + 1) * window_ms))
        if probability >= threshold and current_start is None:
            current_start = start_ms
        elif probability < threshold and current_start is not None:
            if current_start < start_ms:
                segments.append(VadSegment(start_ms=current_start, end_ms=start_ms, kind="speech"))
            current_start = None
        if index == len(probabilities) - 1 and current_start is not None and current_start < end_ms:
            segments.append(VadSegment(start_ms=current_start, end_ms=end_ms, kind="speech"))
    return segments
