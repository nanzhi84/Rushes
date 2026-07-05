"""Transcript persistence repository."""

from __future__ import annotations

from typing import Any

from sqlalchemy import select
from sqlalchemy.engine import Connection

from contracts.transcript import TranscriptDocument
from storage import schema

from ._json import decode_json_columns, encode_json_columns
from ._rows import row_to_dict

JSON_COLUMNS = {"utterances", "vad_segments"}


class TranscriptsRepository:
    def __init__(self, connection: Connection) -> None:
        self._connection = connection

    def insert(self, values: dict[str, Any]) -> None:
        self._connection.execute(
            schema.transcripts.insert().values(**encode_json_columns(values, JSON_COLUMNS))
        )

    def insert_document(self, document: TranscriptDocument) -> None:
        self.insert(
            {
                "transcript_id": document.transcript_id,
                "asset_id": document.asset_id,
                "provider_id": document.provider_id,
                "raw_preserved": document.raw_preserved,
                "utterances": [
                    utterance.model_dump(mode="json") for utterance in document.utterances
                ],
                "vad_segments": [
                    segment.model_dump(mode="json") for segment in document.vad_segments
                ],
            }
        )

    def get(self, transcript_id: str) -> dict[str, Any] | None:
        row = self._connection.execute(
            select(schema.transcripts).where(schema.transcripts.c.transcript_id == transcript_id)
        ).first()
        result = row_to_dict(row)
        if result is None:
            return None
        return decode_json_columns(result, JSON_COLUMNS)

    def list_for_asset(self, asset_id: str) -> list[dict[str, Any]]:
        rows = self._connection.execute(
            select(schema.transcripts)
            .where(schema.transcripts.c.asset_id == asset_id)
            .order_by(schema.transcripts.c.transcript_id)
        ).all()
        return [decode_json_columns(dict(row._mapping), JSON_COLUMNS) for row in rows]
