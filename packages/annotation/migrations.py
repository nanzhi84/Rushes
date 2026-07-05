"""Idempotent annotation projection rebuild helpers."""

from __future__ import annotations

from sqlalchemy import select
from sqlalchemy.engine import Connection

from providers import ProviderGateway
from storage import schema

from .projection import rebuild_annotation_projection


async def rebuild_all_annotation_projections(
    connection: Connection,
    *,
    gateway: ProviderGateway | None = None,
) -> int:
    """Rebuild projection rows and FTS for every stored annotation."""

    annotation_ids = [
        str(row[0])
        for row in connection.execute(
            select(schema.annotations_table.c.annotation_id).order_by(
                schema.annotations_table.c.annotation_id
            )
        ).all()
    ]
    for annotation_id in annotation_ids:
        await rebuild_projection_by_annotation_id(
            connection,
            annotation_id,
            gateway=gateway,
        )
    return len(annotation_ids)


async def rebuild_projection_by_annotation_id(
    connection: Connection,
    annotation_id: str,
    *,
    gateway: ProviderGateway | None = None,
) -> None:
    """Drop and rebuild projection rows for one annotation_id."""

    await rebuild_annotation_projection(connection, annotation_id, gateway=gateway)
