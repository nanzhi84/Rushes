"""M-1.1 Paraformer-v2 ASR contract POC."""

from __future__ import annotations

import argparse
import time
from collections.abc import Mapping
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Literal, cast

from _common import (
    EXIT_SKIP,
    DashScopeClient,
    JsonObject,
    PocError,
    PocSkip,
    Stopwatch,
    compact_text,
    first_list,
    first_mapping_list,
    first_ms,
    first_string,
    http_get_json,
    iter_json_objects,
    lcs_ratio,
    load_dotenv,
    require_env,
    timestamp,
    write_json,
)

from contracts import TranscriptDocument, TranscriptUtterance, TranscriptWord

FILLER_WORDS = frozenset({"呃", "嗯", "啊", "哦"})
ALIGNMENT_PASS_RATIO = 0.60
ASR_POLL_INTERVAL_S = 3.0
ASR_TIMEOUT_S = 600.0
WordKind = Literal["filler", "word", "punct"]


@dataclass(frozen=True)
class OssUpload:
    bucket: Any
    key: str
    signed_url: str

    def delete(self) -> None:
        self.bucket.delete_object(self.key)


@dataclass(frozen=True)
class AsrReport:
    filler_hits: list[str]
    timestamps_ok: bool
    alignment_ratio: float
    word_count: int
    warnings: list[str]

    @property
    def ok(self) -> bool:
        return (
            bool(self.filler_hits)
            and self.timestamps_ok
            and self.word_count > 0
            and self.alignment_ratio >= ALIGNMENT_PASS_RATIO
        )


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="验证 DashScope Paraformer-v2 ASR 契约。")
    parser.add_argument(
        "--audio",
        type=Path,
        default=Path("scripts/poc/fixtures/filler_speech.wav"),
        help="待识别音频，默认使用 scripts/poc/fixtures/filler_speech.wav。",
    )
    parser.add_argument(
        "--reference-text",
        type=Path,
        default=Path("scripts/poc/fixtures/filler_speech.txt"),
        help="粗对齐参考文本路径。",
    )
    return parser.parse_args()


def upload_to_oss(audio_path: Path, key_prefix: str) -> OssUpload:
    import oss2

    endpoint = require_env("RUSHES_OSS_ENDPOINT")
    _region = require_env("RUSHES_OSS_REGION")
    bucket_name = require_env("RUSHES_OSS_BUCKET")
    access_key = require_env("RUSHES_OSS_ACCESS_KEY")
    secret_key = require_env("RUSHES_OSS_SECRET_KEY")
    auth = oss2.Auth(access_key, secret_key)
    bucket = oss2.Bucket(auth, endpoint, bucket_name)
    key = f"{key_prefix.rstrip('/')}/{timestamp()}_{audio_path.name}"
    bucket.put_object_from_file(key, str(audio_path))
    signed_url = cast(str, bucket.sign_url("GET", key, 3600))
    return OssUpload(bucket=bucket, key=key, signed_url=signed_url)


def run_asr(client: DashScopeClient, signed_url: str) -> tuple[JsonObject, JsonObject, JsonObject]:
    submit_response = client.submit_asr_transcription(signed_url)
    task_id = extract_required_string(submit_response, ("task_id", "taskId"), "DashScope task_id")
    print(f"ASR 任务已提交：{task_id}")
    task_response = poll_task(client, task_id)
    transcription_url = extract_transcription_url(task_response)
    transcription_response = http_get_json(transcription_url, context="下载 DashScope 转写 JSON")
    return submit_response, task_response, transcription_response


def poll_task(client: DashScopeClient, task_id: str) -> JsonObject:
    deadline = ASR_TIMEOUT_S
    with Stopwatch("poll_asr") as timer:
        while timer.elapsed_ms / 1000 <= deadline:
            task_response = client.get_task(task_id)
            status = extract_required_string(
                task_response,
                ("task_status", "taskStatus", "status"),
                "DashScope task_status",
            )
            print(f"ASR 任务状态：{status}")
            if status == "SUCCEEDED":
                return task_response
            if status in {"FAILED", "CANCELED", "UNKNOWN"}:
                raise PocError(f"ASR 任务失败：{task_response}")
            time.sleep(ASR_POLL_INTERVAL_S)
    raise PocError(f"ASR 任务超过 {ASR_TIMEOUT_S:.0f}s 未完成。")


def extract_required_string(data: JsonObject, keys: tuple[str, ...], label: str) -> str:
    for mapping in iter_json_objects(data):
        value = first_string(mapping, keys)
        if value is not None:
            return value
    raise PocError(f"DashScope 响应缺少 {label}：{data}")


def extract_transcription_url(task_response: JsonObject) -> str:
    for mapping in iter_json_objects(task_response):
        value = first_string(
            mapping,
            ("transcription_url", "transcriptionUrl", "result_url", "resultUrl"),
        )
        if value is not None:
            return value
    raise PocError(f"DashScope 任务完成但缺少 transcription_url：{task_response}")


def normalize_asr_response(raw: JsonObject, asset_id: str) -> TranscriptDocument:
    warnings: list[str] = []
    utterances: list[TranscriptUtterance] = []
    sentence_mappings = collect_sentence_mappings(raw)
    if not sentence_mappings:
        warnings.append("未找到 sentences/utterances；尝试用全文字段构造空 word utterance。")
        text = extract_full_text(raw)
        if not text:
            raise PocError("ASR 响应中没有可归一化的 text/sentences/words 字段。")
        utterances.append(
            TranscriptUtterance(
                utterance_id="u_001",
                text=text,
                start_ms=0,
                end_ms=1,
                words=[],
            )
        )
    else:
        for index, sentence in enumerate(sentence_mappings, start=1):
            utterance = normalize_sentence(sentence, index, warnings)
            if utterance is not None:
                utterances.append(utterance)
    full_text = "".join(utterance.text for utterance in utterances)
    filler_hits = sorted({filler for filler in FILLER_WORDS if filler in full_text})
    return TranscriptDocument(
        transcript_id=f"tr_paraformer_v2_{timestamp()}",
        asset_id=asset_id,
        language="zh",
        provider_id="aliyun_paraformer_v2",
        raw_preserved=bool(filler_hits),
        utterances=utterances,
        vad_segments=[],
        warnings=warnings,
    )


def collect_sentence_mappings(raw: JsonObject) -> list[Mapping[str, object]]:
    candidates: list[Mapping[str, object]] = []
    for mapping in iter_json_objects(raw):
        candidates.extend(
            first_mapping_list(
                mapping,
                ("sentences", "sentence", "utterances", "segments", "paragraphs"),
            )
        )
    return [candidate for candidate in candidates if has_text_or_words(candidate)]


def has_text_or_words(mapping: Mapping[str, object]) -> bool:
    return (
        first_string(mapping, ("text", "sentence", "content", "transcript")) is not None
        or first_list(mapping, ("words", "tokens", "characters", "chars")) is not None
    )


def normalize_sentence(
    sentence: Mapping[str, object],
    index: int,
    warnings: list[str],
) -> TranscriptUtterance | None:
    words = normalize_words(sentence, warnings)
    text = first_string(sentence, ("text", "sentence", "content", "transcript"))
    if text is None:
        text = "".join(word.w for word in words)
    start_ms = first_ms(
        sentence,
        ("start_ms", "begin_time", "start_time", "startTime", "beginTime", "start", "begin"),
    )
    end_ms = first_ms(
        sentence,
        ("end_ms", "end_time", "endTime", "finish_time", "finishTime", "end", "finish"),
    )
    if words:
        start_ms = start_ms if start_ms is not None else min(word.start_ms for word in words)
        end_ms = end_ms if end_ms is not None else max(word.end_ms for word in words)
    if start_ms is None or end_ms is None:
        warnings.append(f"utterance {index} 缺少起止时间，已跳过。")
        return None
    if start_ms >= end_ms:
        warnings.append(f"utterance {index} 起止时间非法 {start_ms}-{end_ms}，已跳过。")
        return None
    if not words:
        warnings.append(f"utterance {index} 没有 word/char 时间戳。")
    return TranscriptUtterance(
        utterance_id=f"u_{index:03d}",
        text=text,
        start_ms=start_ms,
        end_ms=end_ms,
        words=words,
    )


def normalize_words(
    sentence: Mapping[str, object],
    warnings: list[str],
) -> list[TranscriptWord]:
    word_mappings = first_mapping_list(sentence, ("words", "tokens", "characters", "chars"))
    normalized: list[TranscriptWord] = []
    for index, word in enumerate(word_mappings, start=1):
        text = first_string(word, ("w", "word", "text", "char", "value", "content"))
        start_ms = first_ms(
            word,
            ("start_ms", "begin_time", "start_time", "startTime", "beginTime", "start", "begin"),
        )
        end_ms = first_ms(
            word,
            ("end_ms", "end_time", "endTime", "finish_time", "finishTime", "end", "finish"),
        )
        if text is None:
            warnings.append(f"word {index} 缺少文本，已跳过。")
            continue
        if start_ms is None or end_ms is None:
            warnings.append(f"word {text!r} 缺少起止时间，已跳过。")
            continue
        if start_ms >= end_ms:
            warnings.append(f"word {text!r} 起止时间非法 {start_ms}-{end_ms}，已跳过。")
            continue
        normalized.append(
            TranscriptWord(
                w=text,
                start_ms=start_ms,
                end_ms=end_ms,
                type=word_type(text),
            )
        )
    return normalized


def word_type(text: str) -> WordKind:
    if text in FILLER_WORDS:
        return "filler"
    from _common import is_punctuation

    if is_punctuation(text):
        return "punct"
    return "word"


def extract_full_text(raw: JsonObject) -> str:
    pieces: list[str] = []
    for mapping in iter_json_objects(raw):
        text = first_string(mapping, ("text", "content", "transcript"))
        if text is not None:
            pieces.append(text)
    return max(pieces, key=len, default="")


def build_report(document: TranscriptDocument, reference_text: str) -> AsrReport:
    words = [word for utterance in document.utterances for word in utterance.words]
    filler_hits = sorted({word.w for word in words if word.type == "filler"})
    if not filler_hits:
        full_text = "".join(utterance.text for utterance in document.utterances)
        filler_hits = sorted({filler for filler in FILLER_WORDS if filler in full_text})
    timestamps_ok = words_are_monotonic(words)
    hypothesis = compact_text("".join(utterance.text for utterance in document.utterances))
    reference = compact_text(reference_text)
    alignment_ratio = lcs_ratio(reference, hypothesis)
    return AsrReport(
        filler_hits=filler_hits,
        timestamps_ok=timestamps_ok,
        alignment_ratio=alignment_ratio,
        word_count=len(words),
        warnings=document.warnings,
    )


def words_are_monotonic(words: list[TranscriptWord]) -> bool:
    if not words:
        return False
    previous_end = -1
    for word in words:
        if word.start_ms < previous_end or word.start_ms >= word.end_ms:
            return False
        previous_end = word.end_ms
    return True


def print_report(report: AsrReport, sample_path: Path, document: TranscriptDocument) -> None:
    print("\nASR contract report")
    print(f"- raw_preserved: {document.raw_preserved}")
    print(f"- filler hits: {', '.join(report.filler_hits) if report.filler_hits else '未命中'}")
    print(f"- word timestamps: {'OK' if report.timestamps_ok else 'FAIL'}")
    print(f"- word count: {report.word_count}")
    print(f"- fixture alignment: {report.alignment_ratio:.2%}")
    print(f"- raw sample: {sample_path}")
    if report.warnings:
        print("- warnings:")
        for warning in report.warnings:
            print(f"  - {warning}")
    print(f"- verdict: {'PASS' if report.ok else 'FAIL'}")


def main() -> int:
    args = parse_args()
    try:
        load_dotenv()
        if not args.audio.exists():
            raise PocSkip(f"音频不存在：{args.audio}。请先运行 scripts/poc/make_fixture.py。")
        if not args.reference_text.exists():
            raise PocSkip(
                f"参考文本不存在：{args.reference_text}。请先运行 scripts/poc/make_fixture.py。"
            )
        api_key = require_env("RUSHES_DASHSCOPE_API_KEY")
        upload: OssUpload | None = None
        try:
            upload = upload_to_oss(args.audio, "rushes-poc")
            print(f"已上传 OSS：{upload.key}")
            with DashScopeClient(api_key) as client:
                submit_response, task_response, transcription_response = run_asr(
                    client,
                    upload.signed_url,
                )
            document = normalize_asr_response(transcription_response, args.audio.stem)
            reference_text = args.reference_text.read_text(encoding="utf-8")
            report = build_report(document, reference_text)
            sample_path = Path("research/asr_samples") / f"paraformer_v2_{timestamp()}.json"
            write_json(
                sample_path,
                {
                    "submit_response": submit_response,
                    "task_response": task_response,
                    "transcription_response": transcription_response,
                    "normalized_transcript": document.model_dump(by_alias=True),
                    "report": {
                        "filler_hits": report.filler_hits,
                        "timestamps_ok": report.timestamps_ok,
                        "alignment_ratio": report.alignment_ratio,
                        "word_count": report.word_count,
                        "warnings": report.warnings,
                    },
                },
            )
            print_report(report, sample_path, document)
            return 0 if report.ok else 1
        finally:
            if upload is not None:
                try:
                    upload.delete()
                    print(f"已清理 OSS 对象：{upload.key}")
                except Exception as exc:
                    print(f"WARN: OSS 对象清理失败，请手动删除 {upload.key}：{exc}")
    except PocSkip as exc:
        print(f"SKIP: {exc}")
        return EXIT_SKIP
    except PocError as exc:
        print(f"ERROR: {exc}")
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
