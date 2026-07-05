"""M-1.3 Volcengine TTS timestamp-chain POC."""

from __future__ import annotations

import argparse
import json
import re
from collections.abc import Iterator, Mapping, Sequence
from dataclasses import dataclass
from pathlib import Path
from typing import Literal, cast

from _common import (
    EXIT_SKIP,
    JsonObject,
    PocError,
    PocSkip,
    compact_text,
    ensure_dir,
    ffprobe_duration_s,
    is_punctuation,
    lcs_ratio,
    load_dotenv,
    require_env,
    timestamp,
    write_json,
)
from _volc import (
    DEFAULT_ENCODING,
    DEFAULT_RESOURCE_ID,
    DEFAULT_VOICE_TYPE,
    VolcCredentials,
    VolcError,
    VolcTTSClient,
)

DEFAULT_SCRIPT_PATH = Path("/Users/yoryon/MyVideo/04-protein-powder-scoop/script.md")
FALLBACK_TEXT = "今天我们用火山引擎语音合成验证字幕时间戳链路。"
COVERAGE_THRESHOLD = 0.98

START_KEYS = (
    "start_ms",
    "startMs",
    "begin_ms",
    "beginMs",
    "start_time",
    "startTime",
    "begin_time",
    "beginTime",
    "offset",
    "start",
    "begin",
)
END_KEYS = (
    "end_ms",
    "endMs",
    "finish_ms",
    "finishMs",
    "end_time",
    "endTime",
    "finish_time",
    "finishTime",
    "stop_time",
    "stopTime",
    "end",
    "finish",
    "stop",
)
DURATION_KEYS = ("duration_ms", "durationMs", "duration", "length_ms", "lengthMs", "length")
SENTENCE_LIST_KEYS = (
    "sentences",
    "sentence",
    "segments",
    "subtitles",
    "utterances",
    "utterance",
    "lines",
)
WORD_LIST_KEYS = (
    "words",
    "word",
    "tokens",
    "token",
    "characters",
    "chars",
    "char",
    "phonemes",
    "phones",
)
SENTENCE_TEXT_KEYS = ("text", "sentence", "content", "subtitle", "utterance")
WORD_TEXT_KEYS = ("w", "word", "text", "char", "value", "content", "phone", "phoneme", "symbol")
FILLERS = frozenset({"呃", "嗯", "啊", "哦", "额"})


@dataclass(frozen=True)
class TtsWord:
    w: str
    start_ms: int
    end_ms: int
    type: Literal["filler", "word", "punct"]


@dataclass(frozen=True)
class TtsSentence:
    text: str
    start_ms: int
    end_ms: int
    words: list[TtsWord]


@dataclass(frozen=True)
class TtsNormalized:
    sentences: list[TtsSentence]

    @property
    def word_count(self) -> int:
        return sum(len(sentence.words) for sentence in self.sentences)

    @property
    def text(self) -> str:
        return "".join(word.w for sentence in self.sentences for word in sentence.words)


@dataclass(frozen=True)
class TimestampAssertions:
    coverage_ratio: float
    reference_chars: int
    timestamped_chars: int


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="验证火山 TTS 音频 + 字/词级时间戳链路。")
    parser.add_argument(
        "--script",
        type=Path,
        default=DEFAULT_SCRIPT_PATH,
        help="Markdown 脚本路径；默认解析其中的 ## 口播稿 小节。",
    )
    parser.add_argument(
        "--voice-type",
        default=DEFAULT_VOICE_TYPE,
        help="火山 audio.voice_type；默认沿用 CutFlow 实测账号里的可用音色。",
    )
    parser.add_argument(
        "--resource-id",
        default=DEFAULT_RESOURCE_ID,
        help="火山数据面 Resource-Id。",
    )
    parser.add_argument("--encoding", default=DEFAULT_ENCODING, help="音频编码，默认 mp3。")
    return parser.parse_args()


def script_text(path: Path) -> str:
    if not path.exists():
        return FALLBACK_TEXT
    raw = path.read_text(encoding="utf-8")
    section = extract_spoken_section(raw)
    if not section:
        return FALLBACK_TEXT
    return section


def extract_spoken_section(markdown: str) -> str:
    lines = markdown.splitlines()
    in_section = False
    in_fence = False
    paragraph_lines: list[str] = []
    paragraphs: list[str] = []
    for line in lines:
        stripped = line.strip()
        if stripped.startswith("```"):
            in_fence = not in_fence
            continue
        if not in_fence and stripped.startswith("## "):
            if in_section:
                break
            title = stripped.removeprefix("## ").strip()
            in_section = title.startswith("口播稿")
            continue
        if not in_section or in_fence:
            continue
        cleaned = clean_script_line(stripped)
        if not cleaned:
            if paragraph_lines:
                paragraphs.append("".join(paragraph_lines))
                paragraph_lines = []
            continue
        paragraph_lines.append(cleaned)
    if paragraph_lines:
        paragraphs.append("".join(paragraph_lines))
    return "\n".join(paragraphs).strip()


def clean_script_line(line: str) -> str:
    if not line or line.startswith("#"):
        return ""
    cleaned = re.sub(r"^\s*(?:[-*+]|\d+[.)])\s+", "", line)
    cleaned = re.sub(r"\*\*([^*]+)\*\*", r"\1", cleaned)
    cleaned = re.sub(r"`([^`]+)`", r"\1", cleaned)
    return cleaned.strip()


def normalize_timestamps(raw: JsonObject, reference_text: str) -> TtsNormalized:
    sentence_mappings = collect_list_mappings(raw, SENTENCE_LIST_KEYS)
    sentences: list[TtsSentence] = []
    for mapping in sentence_mappings:
        sentence = sentence_from_mapping(mapping)
        if sentence.words:
            sentences.append(sentence)
    if sentences:
        return TtsNormalized(sentences=sentences)

    word_mappings = collect_word_mappings(raw)
    if not word_mappings:
        raise PocError("火山响应中没有可识别的字/词级时间戳结构。")
    return TtsNormalized(sentences=[sentence_from_words(word_mappings, reference_text)])


def collect_word_mappings(raw: JsonObject) -> list[Mapping[str, object]]:
    candidates = collect_list_mappings(raw, WORD_LIST_KEYS)
    if candidates:
        return candidates
    loose: list[Mapping[str, object]] = []
    for mapping in iter_json_objects(raw):
        if first_string(mapping, WORD_TEXT_KEYS) is None:
            continue
        if mapping_range(mapping) is None:
            continue
        loose.append(mapping)
    return loose


def collect_list_mappings(raw: JsonObject, keys: Sequence[str]) -> list[Mapping[str, object]]:
    candidates: list[Mapping[str, object]] = []
    for mapping in iter_json_objects(raw):
        for key in keys:
            value = mapping.get(key)
            if isinstance(value, list):
                candidates.extend(
                    cast(Mapping[str, object], item) for item in value if isinstance(item, Mapping)
                )
            elif isinstance(value, Mapping):
                candidates.append(cast(Mapping[str, object], value))
    return candidates


def iter_json_objects(value: object) -> Iterator[Mapping[str, object]]:
    for item in iter_json_values(value):
        if isinstance(item, Mapping):
            yield cast(Mapping[str, object], item)


def iter_json_values(value: object) -> Iterator[object]:
    yield value
    if isinstance(value, Mapping):
        for child in value.values():
            yield from iter_json_values(child)
    elif isinstance(value, list):
        for child in value:
            yield from iter_json_values(child)
    elif isinstance(value, str):
        stripped = value.strip()
        if stripped.startswith(("{", "[")) and len(stripped) < 2_000_000:
            try:
                parsed = json.loads(stripped)
            except json.JSONDecodeError:
                return
            yield from iter_json_values(parsed)


def sentence_from_mapping(mapping: Mapping[str, object]) -> TtsSentence:
    word_mappings = first_child_word_mappings(mapping)
    fallback_text = first_string(mapping, SENTENCE_TEXT_KEYS) or "".join(
        text_of_words(word_mappings)
    )
    if not word_mappings:
        return TtsSentence(text=fallback_text, start_ms=0, end_ms=0, words=[])
    return sentence_from_words(word_mappings, fallback_text)


def first_child_word_mappings(mapping: Mapping[str, object]) -> list[Mapping[str, object]]:
    for key in WORD_LIST_KEYS:
        value = mapping.get(key)
        if isinstance(value, list):
            return [cast(Mapping[str, object], item) for item in value if isinstance(item, Mapping)]
    return []


def sentence_from_words(
    word_mappings: Sequence[Mapping[str, object]],
    fallback_text: str,
) -> TtsSentence:
    words: list[TtsWord] = []
    for mapping in word_mappings:
        text = first_string(mapping, WORD_TEXT_KEYS)
        time_range = mapping_range(mapping)
        if text is None or time_range is None:
            raise PocError(f"字/词级时间戳字段不完整：{mapping}")
        start_ms, end_ms = time_range
        if start_ms >= end_ms:
            raise PocError(f"字/词级时间戳非法：{mapping}")
        words.append(
            TtsWord(
                w=text,
                start_ms=start_ms,
                end_ms=end_ms,
                type=word_type(text),
            )
        )
    if not words:
        raise PocError("火山响应缺少字/词级时间戳。")
    return TtsSentence(
        text=fallback_text or "".join(word.w for word in words),
        start_ms=min(word.start_ms for word in words),
        end_ms=max(word.end_ms for word in words),
        words=words,
    )


def word_type(text: str) -> Literal["filler", "word", "punct"]:
    if is_punctuation(text):
        return "punct"
    if text in FILLERS:
        return "filler"
    return "word"


def text_of_words(word_mappings: Sequence[Mapping[str, object]]) -> list[str]:
    values: list[str] = []
    for mapping in word_mappings:
        text = first_string(mapping, WORD_TEXT_KEYS)
        if text is not None:
            values.append(text)
    return values


def first_string(mapping: Mapping[str, object], keys: Sequence[str]) -> str | None:
    for key in keys:
        value = mapping.get(key)
        if isinstance(value, str) and value:
            return value
        if isinstance(value, int | float) and not isinstance(value, bool):
            return str(value)
    return None


def mapping_range(mapping: Mapping[str, object]) -> tuple[int, int] | None:
    start_ms = first_ms(mapping, START_KEYS)
    end_ms = first_ms(mapping, END_KEYS)
    if start_ms is None:
        return None
    if end_ms is None:
        duration_ms = first_ms(mapping, DURATION_KEYS)
        if duration_ms is None:
            return None
        end_ms = start_ms + duration_ms
    return start_ms, end_ms


def first_ms(mapping: Mapping[str, object], keys: Sequence[str]) -> int | None:
    for key in keys:
        if key not in mapping:
            continue
        parsed = parse_time_ms(key, mapping[key])
        if parsed is not None:
            return parsed
    return None


def parse_time_ms(key: str, value: object) -> int | None:
    if value is None or isinstance(value, bool):
        return None
    if isinstance(value, str):
        stripped = value.strip()
        if not stripped:
            return None
        try:
            number = float(stripped)
        except ValueError:
            return None
    elif isinstance(value, int | float):
        number = float(value)
    else:
        return None

    lower_key = key.lower()
    if "ms" in lower_key or "millisecond" in lower_key:
        return round(number)
    if "sec" in lower_key or lower_key.endswith("_s") or not number.is_integer():
        return round(number * 1000)
    return round(number)


def timestamps_via_asr_fallback(audio_path: Path) -> TtsNormalized:
    """PRD §9.5 兜底：provider 无原生时间戳时，TTS 音频回送云端 ASR 取字级时间戳。

    实测背景（2026-07-05）：本账号为火山声音复刻 ICL（cluster=volcano_icl），
    v1 /api/v1/tts 无论 with_timestamp/with_frontend 参数均只回 duration；
    v3 unidirectional 与 megatts.default/concurr 资源均无授权（45000010）。
    因此 timestamps_source=asr_fallback 是当前账号下的唯一时间戳通道。
    """
    import asr_contract
    from _common import DashScopeClient, run_command

    wav_path = audio_path.with_suffix(".16k.wav")
    run_command(
        ["ffmpeg", "-y", "-i", str(audio_path), "-ar", "16000", "-ac", "1", str(wav_path)],
        description="TTS 音频转 16k 单声道 wav",
    )
    upload = asr_contract.upload_to_oss(wav_path, "rushes-poc/tts-fallback")
    try:
        api_key = require_env("RUSHES_DASHSCOPE_API_KEY")
        with DashScopeClient(api_key) as client:
            _, _, transcription = asr_contract.run_asr(client, upload.signed_url)
    finally:
        upload.delete()
        print(f"已清理 OSS 对象：{upload.key}")
    document = asr_contract.normalize_asr_response(transcription, asset_id="tts_asr_fallback")
    sentences: list[TtsSentence] = []
    for utterance in document.utterances:
        words = [
            TtsWord(w=word.w, start_ms=word.start_ms, end_ms=word.end_ms, type=word.type)
            for word in utterance.words
        ]
        sentences.append(
            TtsSentence(
                text=utterance.text,
                start_ms=utterance.start_ms,
                end_ms=utterance.end_ms,
                words=words,
            )
        )
    if not sentences:
        raise PocError("ASR 兜底未产出任何句级时间戳。")
    return TtsNormalized(sentences=sentences)


def assert_audio_decodable(path: Path) -> float:
    if path.stat().st_size <= 0:
        raise PocError("火山 TTS 音频为空。")
    duration_s = ffprobe_duration_s(path)
    if duration_s <= 0:
        raise PocError(f"ffprobe 返回非正音频时长：{duration_s}")
    return duration_s


def assert_timestamp_contract(
    normalized: TtsNormalized,
    reference_text: str,
) -> TimestampAssertions:
    if not normalized.sentences or normalized.word_count == 0:
        raise PocError("归一化结果缺少 sentences/words。")
    previous_sentence_end = -1
    previous_word_end = -1
    for sentence in normalized.sentences:
        if sentence.start_ms < previous_sentence_end or sentence.start_ms >= sentence.end_ms:
            raise PocError(f"句级时间戳不单调：{sentence}")
        previous_sentence_end = sentence.end_ms
        for word in sentence.words:
            if word.start_ms < previous_word_end or word.start_ms >= word.end_ms:
                raise PocError(f"字/词级时间戳不单调：{word}")
            previous_word_end = word.end_ms

    reference_compact = compact_text(reference_text)
    timestamped_compact = compact_text(normalized.text)
    if not reference_compact or not timestamped_compact:
        raise PocError("覆盖率检查文本为空。")
    coverage = lcs_ratio(reference_compact, timestamped_compact)
    if coverage < COVERAGE_THRESHOLD or len(timestamped_compact) < len(reference_compact):
        raise PocError(
            "时间戳文本未覆盖全文："
            f"coverage={coverage:.3f}, reference={len(reference_compact)}, "
            f"timestamped={len(timestamped_compact)}"
        )
    return TimestampAssertions(
        coverage_ratio=coverage,
        reference_chars=len(reference_compact),
        timestamped_chars=len(timestamped_compact),
    )


def normalized_to_json(normalized: TtsNormalized) -> JsonObject:
    return {
        "sentences": [
            {
                "text": sentence.text,
                "start_ms": sentence.start_ms,
                "end_ms": sentence.end_ms,
                "words": [
                    {
                        "w": word.w,
                        "start_ms": word.start_ms,
                        "end_ms": word.end_ms,
                        "type": word.type,
                    }
                    for word in sentence.words
                ],
            }
            for sentence in normalized.sentences
        ]
    }


def main() -> int:
    args = parse_args()
    try:
        load_dotenv()
        credentials = VolcCredentials.from_values(
            aksk=require_env("RUSHES_VOLC_TTS_AKSK", label="火山 TTS AK/SK"),
            appid=require_env("RUSHES_VOLC_TTS_APPID", label="火山 TTS AppID"),
            cluster=require_env("RUSHES_VOLC_TTS_CLUSTER", label="火山 TTS cluster"),
        )
        text = script_text(args.script)
        run_id = timestamp()
        sample_dir = ensure_dir(Path("research/tts_samples"))
        audio_path = sample_dir / f"volcano_{run_id}.{args.encoding}"
        raw_path = sample_dir / f"volcano_{run_id}.json"

        with VolcTTSClient(credentials) as client:
            api_key = client.ensure_api_key()
            result = client.synthesize(
                api_key=api_key,
                text=text,
                voice_type=str(args.voice_type),
                resource_id=str(args.resource_id),
                encoding=str(args.encoding),
            )

        audio_path.write_bytes(result.audio_bytes)
        duration_s = assert_audio_decodable(audio_path)
        try:
            normalized = normalize_timestamps(result.response_json, text)
            timestamps_source = "native"
        except PocError as exc:
            print(f"火山原生时间戳不可用（{exc}），走 ASR 兜底链路……")
            normalized = timestamps_via_asr_fallback(audio_path)
            timestamps_source = "asr_fallback"
        assertions = assert_timestamp_contract(normalized, text)
        write_json(
            raw_path,
            {
                "provider": "volcengine.tts",
                "endpoint": "https://openspeech.bytedance.com/api/v1/tts",
                "script_path": str(args.script),
                "voice_type": str(args.voice_type),
                "resource_id": str(args.resource_id),
                "request": result.request_payload,
                "response": result.response_json,
                "timestamps_source": timestamps_source,
                "normalized": normalized_to_json(normalized),
                "assertions": {
                    "audio_duration_s": duration_s,
                    "coverage_ratio": assertions.coverage_ratio,
                    "reference_chars": assertions.reference_chars,
                    "timestamped_chars": assertions.timestamped_chars,
                },
            },
        )

        print("Volcengine TTS timestamp report")
        print(f"- script: {args.script}")
        print(f"- voice_type: {args.voice_type}")
        print(f"- resource_id: {args.resource_id}")
        print(f"- audio_duration_s: {duration_s:.3f}")
        print(f"- timestamps_source: {timestamps_source}")
        print(f"- sentences: {len(normalized.sentences)}")
        print(f"- words: {normalized.word_count}")
        print(f"- coverage: {assertions.coverage_ratio:.3f}")
        print(f"- audio sample: {audio_path}")
        print(f"- raw sample: {raw_path}")
        return 0
    except PocSkip as exc:
        print(f"SKIP: {exc}")
        return EXIT_SKIP
    except (PocError, VolcError) as exc:
        print(f"ERROR: {exc}")
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
