"""Projection of AnnotationDocument.v1 into query tables and FTS."""

from __future__ import annotations

from array import array
from collections.abc import Mapping, Sequence
from dataclasses import dataclass
from datetime import UTC, datetime
from typing import Any

from sqlalchemy import delete, select
from sqlalchemy.engine import Connection

from contracts.annotation import AnnotationClip, AnnotationDocument
from providers import EMBEDDING_TEXT, ProviderGateway, ProviderRequest
from storage import schema
from storage.repositories._json import dump_json, load_json


@dataclass(frozen=True, slots=True)
class ClipProjectionRow:
    clip_id: str
    annotation_id: str
    asset_id: str
    start_frame: int
    end_frame: int
    role: str
    summary: str
    keywords_json: str
    quality_score: float | None
    usable: bool
    embedding: bytes | None
    retrieval_sentence: str
    ocr_text: str


@dataclass(frozen=True, slots=True)
class SignalProjectionRow:
    signal_id: str
    clip_id: str
    namespace: str
    field: str
    value_text: str | None
    value_number: float | None
    confidence: float | None = None


@dataclass(frozen=True, slots=True)
class AnnotationProjection:
    clips: tuple[ClipProjectionRow, ...]
    signals: tuple[SignalProjectionRow, ...]


async def build_annotation_projection(
    document: AnnotationDocument,
    *,
    gateway: ProviderGateway | None = None,
    job_id: str | None = None,
    case_id: str | None = None,
    embedding_model: str | None = "text-embedding-v4",
) -> AnnotationProjection:
    clip_rows: list[ClipProjectionRow] = []
    signal_rows: list[SignalProjectionRow] = []
    for clip in document.clips:
        retrieval_sentence = _retrieval_sentence(clip)
        embedding = (
            await _embedding_blob(
                gateway,
                retrieval_sentence,
                job_id=job_id,
                case_id=case_id,
                model=embedding_model,
            )
            if gateway is not None
            else None
        )
        clip_rows.append(
            ClipProjectionRow(
                clip_id=clip.clip_id,
                annotation_id=document.annotation_id,
                asset_id=document.asset_id,
                start_frame=clip.source_start_frame,
                end_frame=clip.source_end_frame,
                role=clip.role,
                summary=clip.summary,
                keywords_json=dump_json(clip.keywords),
                quality_score=clip.quality_score,
                usable=clip.role != "avoid" and not clip.hard_quality_event_ids,
                embedding=embedding,
                retrieval_sentence=retrieval_sentence,
                ocr_text=_ocr_text(clip),
            )
        )
        signal_rows.extend(_signal_rows_for_clip(clip))
    return AnnotationProjection(clips=tuple(clip_rows), signals=tuple(signal_rows))


def persist_annotation_projection(
    connection: Connection,
    document: AnnotationDocument,
    projection: AnnotationProjection,
    *,
    updated_at: str | None = None,
) -> None:
    """Persist full annotation JSON and rebuild projections for one annotation."""

    timestamp = updated_at or datetime.now(UTC).isoformat()
    delete_projection_for_annotation(connection, document.annotation_id)
    values = {
        "annotation_id": document.annotation_id,
        "asset_id": document.asset_id,
        "schema": document.schema_,
        "status": document.status,
        "document_json": document.model_dump_json(by_alias=True),
        "created_at": document.created_at,
        "updated_at": timestamp,
    }
    existing = connection.execute(
        select(schema.annotations_table.c.annotation_id).where(
            schema.annotations_table.c.annotation_id == document.annotation_id
        )
    ).first()
    if existing is None:
        connection.execute(schema.annotations_table.insert().values(**values))
    else:
        connection.execute(
            schema.annotations_table.update()
            .where(schema.annotations_table.c.annotation_id == document.annotation_id)
            .values(**values)
        )
    for clip_row in projection.clips:
        connection.execute(
            schema.annotation_clip_projection.insert().values(
                clip_id=clip_row.clip_id,
                annotation_id=clip_row.annotation_id,
                asset_id=clip_row.asset_id,
                start_frame=clip_row.start_frame,
                end_frame=clip_row.end_frame,
                role=clip_row.role,
                summary=clip_row.summary,
                keywords_json=clip_row.keywords_json,
                quality_score=clip_row.quality_score,
                usable=clip_row.usable,
                embedding=clip_row.embedding,
            )
        )
        _insert_fts_row(connection, clip_row)
    for signal_row in projection.signals:
        connection.execute(
            schema.annotation_signal_projection.insert().values(
                signal_id=signal_row.signal_id,
                clip_id=signal_row.clip_id,
                namespace=signal_row.namespace,
                field=signal_row.field,
                value_text=signal_row.value_text,
                value_number=signal_row.value_number,
                confidence=signal_row.confidence,
            )
        )


async def project_annotation_document(
    connection: Connection,
    document: AnnotationDocument,
    *,
    gateway: ProviderGateway | None = None,
    job_id: str | None = None,
    case_id: str | None = None,
) -> None:
    projection = await build_annotation_projection(
        document,
        gateway=gateway,
        job_id=job_id,
        case_id=case_id,
    )
    persist_annotation_projection(connection, document, projection)


def delete_projection_for_annotation(connection: Connection, annotation_id: str) -> None:
    clip_ids = [
        str(row[0])
        for row in connection.execute(
            select(schema.annotation_clip_projection.c.clip_id).where(
                schema.annotation_clip_projection.c.annotation_id == annotation_id
            )
        ).all()
    ]
    for clip_id in clip_ids:
        connection.exec_driver_sql("DELETE FROM clip_fts WHERE clip_id = ?", (clip_id,))
    if clip_ids:
        connection.execute(
            delete(schema.annotation_signal_projection).where(
                schema.annotation_signal_projection.c.clip_id.in_(clip_ids)
            )
        )
    else:
        connection.execute(
            delete(schema.annotation_signal_projection).where(
                schema.annotation_signal_projection.c.clip_id == "__none__"
            )
        )
    connection.execute(
        delete(schema.annotation_clip_projection).where(
            schema.annotation_clip_projection.c.annotation_id == annotation_id
        )
    )


async def rebuild_annotation_projection(
    connection: Connection,
    annotation_id: str,
    *,
    gateway: ProviderGateway | None = None,
) -> None:
    row = connection.execute(
        select(schema.annotations_table.c.document_json).where(
            schema.annotations_table.c.annotation_id == annotation_id
        )
    ).first()
    if row is None:
        raise KeyError(f"annotation not found: {annotation_id}")
    raw = load_json(str(row._mapping["document_json"]))
    document = AnnotationDocument.model_validate(raw)
    await project_annotation_document(connection, document, gateway=gateway)


def _insert_fts_row(connection: Connection, row: ClipProjectionRow) -> None:
    connection.exec_driver_sql("DELETE FROM clip_fts WHERE clip_id = ?", (row.clip_id,))
    connection.exec_driver_sql(
        (
            "INSERT INTO clip_fts "
            "(clip_id, summary, keywords, retrieval_sentence, ocr_text) "
            "VALUES (?, ?, ?, ?, ?)"
        ),
        (
            row.clip_id,
            row.summary,
            " ".join(_keywords_from_json(row.keywords_json)),
            row.retrieval_sentence,
            row.ocr_text,
        ),
    )


def _retrieval_sentence(clip: AnnotationClip) -> str:
    pieces = [clip.summary, " ".join(clip.keywords)]
    return " ".join(piece.strip() for piece in pieces if piece and piece.strip())


async def _embedding_blob(
    gateway: ProviderGateway,
    text: str,
    *,
    job_id: str | None,
    case_id: str | None,
    model: str | None,
) -> bytes | None:
    result = await gateway.call(
        ProviderRequest(
            capability=EMBEDDING_TEXT,
            model=model,
            job_id=job_id,
            case_id=case_id,
            payload={"input": text, "retrieval_sentence": text},
        )
    )
    if result.result.error is not None:
        return None
    vector = _embedding_vector(result.result.normalized_output)
    if vector is None:
        return None
    return array("f", vector).tobytes()


def _embedding_vector(output: Mapping[str, Any]) -> list[float] | None:
    direct = output.get("embedding") or output.get("vector")
    if isinstance(direct, Sequence) and not isinstance(direct, str | bytes | bytearray):
        return [float(item) for item in direct]
    data = output.get("data")
    if isinstance(data, Sequence) and not isinstance(data, str | bytes | bytearray) and data:
        first = data[0]
        if isinstance(first, Mapping):
            embedding = first.get("embedding")
            if isinstance(embedding, Sequence) and not isinstance(
                embedding, str | bytes | bytearray
            ):
                return [float(item) for item in embedding]
    return None


def _signal_rows_for_clip(clip: AnnotationClip) -> list[SignalProjectionRow]:
    rows: list[SignalProjectionRow] = []
    extensions = clip.extensions.model_dump(mode="json", by_alias=True, exclude_none=True)
    for namespace, payload in extensions.items():
        if not isinstance(payload, Mapping):
            continue
        for field, value in _flatten_payload(payload):
            value_text, value_number = _signal_value(value)
            rows.append(
                SignalProjectionRow(
                    signal_id=f"{clip.clip_id}:{namespace}:{field}",
                    clip_id=clip.clip_id,
                    namespace=namespace,
                    field=field,
                    value_text=value_text,
                    value_number=value_number,
                )
            )
    return rows


def _flatten_payload(
    payload: Mapping[str, Any],
    *,
    prefix: str = "",
) -> list[tuple[str, Any]]:
    rows: list[tuple[str, Any]] = []
    for key, value in payload.items():
        field = f"{prefix}.{key}" if prefix else str(key)
        if isinstance(value, Mapping):
            rows.extend(_flatten_payload(value, prefix=field))
        else:
            rows.append((field, value))
    return rows


def _signal_value(value: Any) -> tuple[str | None, float | None]:
    if isinstance(value, bool):
        return ("true" if value else "false"), 1.0 if value else 0.0
    if isinstance(value, int | float):
        return None, float(value)
    if isinstance(value, str):
        return value, None
    if value is None:
        return None, None
    return dump_json(value), None


def _ocr_text(clip: AnnotationClip) -> str:
    extension = clip.extensions.text_ocr_v1
    if extension is None:
        return ""
    parts: list[str] = []
    if extension.full_text:
        parts.append(extension.full_text)
    parts.extend(text.text for text in extension.texts)
    return " ".join(parts)


def _keywords_from_json(raw: str) -> list[str]:
    parsed = load_json(raw)
    if not isinstance(parsed, list):
        return []
    return [str(item) for item in parsed]
