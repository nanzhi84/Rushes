"""M-1.3 MiniMax TTS timestamp-chain POC."""

from __future__ import annotations

import argparse
import base64
import binascii
import json
from collections.abc import Mapping
from dataclasses import dataclass
from pathlib import Path
from typing import cast

import httpx
from _common import (
    EXIT_SKIP,
    JsonObject,
    PocError,
    PocSkip,
    checked_response_json,
    ensure_dir,
    first_mapping_list,
    first_ms,
    first_string,
    http_get_bytes,
    iter_json_objects,
    load_dotenv,
    require_env,
    timestamp,
    write_json,
)

DEFAULT_TEXT = "呃，今天我们用一段短旁白验证 MiniMax 的字幕时间戳链路。"
MINIMAX_ENDPOINTS = (
    "https://api.minimax.io/v1/t2a_v2",
    "https://api.minimaxi.com/v1/t2a_v2",
)


@dataclass(frozen=True)
class TtsChar:
    text: str
    start_ms: int
    end_ms: int


@dataclass(frozen=True)
class TtsSentence:
    text: str
    start_ms: int
    end_ms: int
    chars: list[TtsChar]


@dataclass(frozen=True)
class TtsNormalized:
    sentences: list[TtsSentence]

    @property
    def char_count(self) -> int:
        return sum(len(sentence.chars) for sentence in self.sentences)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="验证 MiniMax T2A v2 subtitle_file 时间戳链路。")
    parser.add_argument("--text", default=DEFAULT_TEXT, help="TTS 文本。")
    parser.add_argument("--voice-id", default="male-qn-qingse", help="MiniMax voice_id。")
    parser.add_argument("--model", default="speech-02-turbo", help="MiniMax T2A 模型。")
    return parser.parse_args()


def call_minimax(api_key: str, payload: JsonObject) -> tuple[str, JsonObject]:
    errors: list[str] = []
    for endpoint in MINIMAX_ENDPOINTS:
        try:
            with httpx.Client(timeout=httpx.Timeout(120.0, connect=10.0)) as client:
                response = client.post(
                    endpoint,
                    headers={
                        "Authorization": f"Bearer {api_key}",
                        "Content-Type": "application/json",
                    },
                    json=payload,
                )
            return endpoint, checked_response_json(response, context=f"MiniMax T2A {endpoint}")
        except (PocError, httpx.HTTPError) as exc:
            errors.append(f"{endpoint}: {exc}")
    raise PocError("MiniMax 两个 endpoint 都失败：\n" + "\n".join(errors))


def build_payload(args: argparse.Namespace) -> JsonObject:
    return {
        "model": args.model,
        "text": args.text,
        "stream": False,
        "subtitle_enable": True,
        "voice_setting": {
            "voice_id": args.voice_id,
            "speed": 1.0,
            "vol": 1.0,
            "pitch": 0,
        },
        "audio_setting": {
            "sample_rate": 32000,
            "bitrate": 128000,
            "format": "mp3",
            "channel": 1,
        },
    }


def extract_subtitle_url(response: JsonObject) -> str:
    for mapping in iter_json_objects(response):
        value = first_string(mapping, ("subtitle_file", "subtitleFile", "subtitle_url"))
        if value is not None:
            return value
    raise PocError(f"MiniMax 响应缺少 extra_info.subtitle_file：{response}")


def extract_audio_bytes(response: JsonObject) -> bytes | None:
    for mapping in iter_json_objects(response):
        audio_url = first_string(mapping, ("audio_url", "audioUrl", "url"))
        if audio_url is not None and audio_url.startswith("http"):
            return http_get_bytes(audio_url, context="下载 MiniMax 音频")
        encoded = first_string(mapping, ("audio", "audio_data", "audioData"))
        if encoded is not None:
            decoded = decode_audio_field(encoded)
            if decoded:
                return decoded
    return None


def fetch_subtitle_json(subtitle_url: str) -> JsonObject:
    raw_bytes = http_get_bytes(subtitle_url, context="下载 MiniMax subtitle_file")
    try:
        data = json.loads(raw_bytes.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise PocError("MiniMax subtitle_file 不是有效 UTF-8 JSON。") from exc
    if isinstance(data, dict):
        return cast(JsonObject, data)
    if isinstance(data, list):
        return {"items": data}
    raise PocError("MiniMax subtitle_file JSON 顶层既不是 object 也不是 array。")


def decode_audio_field(value: str) -> bytes | None:
    stripped = value.strip()
    if not stripped:
        return None
    if len(stripped) % 2 == 0:
        try:
            return bytes.fromhex(stripped)
        except ValueError:
            pass
    try:
        return base64.b64decode(stripped, validate=True)
    except (ValueError, binascii.Error):
        return None


def normalize_subtitle(raw: JsonObject) -> TtsNormalized:
    sentence_mappings = collect_sentence_mappings(raw)
    if not sentence_mappings:
        word_mappings = collect_word_mappings(raw)
        if not word_mappings:
            raise PocError("字幕 JSON 中没有可识别的句级或字级时间戳字段。")
        sentence = sentence_from_words(word_mappings, "".join(text_of_words(word_mappings)))
        return TtsNormalized(sentences=[sentence])
    sentences = [sentence_from_mapping(mapping) for mapping in sentence_mappings]
    return TtsNormalized(sentences=sentences)


def collect_sentence_mappings(raw: JsonObject) -> list[Mapping[str, object]]:
    candidates: list[Mapping[str, object]] = []
    for mapping in iter_json_objects(raw):
        candidates.extend(
            first_mapping_list(mapping, ("sentences", "sentence", "subtitles", "segments"))
        )
    return [candidate for candidate in candidates if first_ms(candidate, START_KEYS) is not None]


def collect_word_mappings(raw: JsonObject) -> list[Mapping[str, object]]:
    candidates: list[Mapping[str, object]] = []
    for mapping in iter_json_objects(raw):
        candidates.extend(first_mapping_list(mapping, ("words", "tokens", "characters", "chars")))
    return candidates


START_KEYS = ("start_ms", "begin_time", "start_time", "startTime", "beginTime", "start", "begin")
END_KEYS = ("end_ms", "end_time", "endTime", "finish_time", "finishTime", "end", "finish")


def sentence_from_mapping(mapping: Mapping[str, object]) -> TtsSentence:
    word_mappings = first_mapping_list(mapping, ("words", "tokens", "characters", "chars"))
    text = first_string(mapping, ("text", "sentence", "content", "subtitle")) or "".join(
        text_of_words(word_mappings)
    )
    if word_mappings:
        return sentence_from_words(word_mappings, text)
    start_ms = first_ms(mapping, START_KEYS)
    end_ms = first_ms(mapping, END_KEYS)
    if start_ms is None or end_ms is None or start_ms >= end_ms:
        raise PocError(f"句级字幕时间戳非法：{mapping}")
    return TtsSentence(text=text, start_ms=start_ms, end_ms=end_ms, chars=[])


def sentence_from_words(
    word_mappings: list[Mapping[str, object]],
    fallback_text: str,
) -> TtsSentence:
    chars: list[TtsChar] = []
    for mapping in word_mappings:
        text = first_string(mapping, ("w", "word", "text", "char", "value", "content"))
        start_ms = first_ms(mapping, START_KEYS)
        end_ms = first_ms(mapping, END_KEYS)
        if text is None or start_ms is None or end_ms is None or start_ms >= end_ms:
            raise PocError(f"字级字幕时间戳非法：{mapping}")
        chars.append(TtsChar(text=text, start_ms=start_ms, end_ms=end_ms))
    if not chars:
        raise PocError("句子缺少字级字幕时间戳。")
    return TtsSentence(
        text=fallback_text or "".join(char.text for char in chars),
        start_ms=min(char.start_ms for char in chars),
        end_ms=max(char.end_ms for char in chars),
        chars=chars,
    )


def text_of_words(word_mappings: list[Mapping[str, object]]) -> list[str]:
    values: list[str] = []
    for mapping in word_mappings:
        text = first_string(mapping, ("w", "word", "text", "char", "value", "content"))
        if text is not None:
            values.append(text)
    return values


def timestamps_are_monotonic(normalized: TtsNormalized) -> bool:
    previous_sentence_end = -1
    previous_char_end = -1
    for sentence in normalized.sentences:
        if sentence.start_ms < previous_sentence_end or sentence.start_ms >= sentence.end_ms:
            return False
        previous_sentence_end = sentence.end_ms
        for char in sentence.chars:
            if char.start_ms < previous_char_end or char.start_ms >= char.end_ms:
                return False
            previous_char_end = char.end_ms
    return normalized.char_count > 0


def normalized_to_json(normalized: TtsNormalized) -> JsonObject:
    return {
        "sentences": [
            {
                "text": sentence.text,
                "start_ms": sentence.start_ms,
                "end_ms": sentence.end_ms,
                "chars": [
                    {
                        "text": char.text,
                        "start_ms": char.start_ms,
                        "end_ms": char.end_ms,
                    }
                    for char in sentence.chars
                ],
            }
            for sentence in normalized.sentences
        ]
    }


def main() -> int:
    args = parse_args()
    try:
        load_dotenv()
        api_key = require_env("RUSHES_MINIMAX_API_KEY", label="MiniMax key")
        run_id = timestamp()
        payload = build_payload(args)
        endpoint, response = call_minimax(api_key, payload)
        subtitle_url = extract_subtitle_url(response)
        subtitle = fetch_subtitle_json(subtitle_url)
        normalized = normalize_subtitle(subtitle)
        monotonic = timestamps_are_monotonic(normalized)
        sample_dir = ensure_dir(Path("research/tts_samples"))
        raw_path = sample_dir / f"minimax_t2a_v2_{run_id}.json"
        subtitle_path = sample_dir / f"minimax_t2a_v2_{run_id}_subtitle.json"
        normalized_path = sample_dir / f"minimax_t2a_v2_{run_id}_normalized.json"
        audio_path = sample_dir / f"minimax_t2a_v2_{run_id}.mp3"
        write_json(raw_path, {"endpoint": endpoint, "request": payload, "response": response})
        write_json(subtitle_path, subtitle)
        write_json(normalized_path, normalized_to_json(normalized))
        audio_bytes = extract_audio_bytes(response)
        if audio_bytes is not None:
            audio_path.write_bytes(audio_bytes)
        print("MiniMax TTS timestamp report")
        print(f"- endpoint: {endpoint}")
        print(f"- subtitle_file: {subtitle_url}")
        print(f"- sentences: {len(normalized.sentences)}")
        print(f"- chars: {normalized.char_count}")
        print(f"- timestamps: {'OK' if monotonic else 'FAIL'}")
        print(f"- raw sample: {raw_path}")
        print(f"- subtitle sample: {subtitle_path}")
        print(f"- normalized sample: {normalized_path}")
        if audio_bytes is not None:
            print(f"- audio sample: {audio_path}")
        else:
            print("- audio sample: MiniMax 响应中未找到 audio/audio_url 字段")
        return 0 if monotonic else 1
    except PocSkip:
        print("SKIP: 缺 MiniMax key")
        return EXIT_SKIP
    except PocError as exc:
        print(f"ERROR: {exc}")
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
