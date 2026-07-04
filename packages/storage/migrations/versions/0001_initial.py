"""initial storage schema

Revision ID: 0001_initial
Revises:
Create Date: 2026-07-04
"""

from __future__ import annotations

from alembic import op

from storage.schema import create_all, drop_all

revision = "0001_initial"
down_revision = None
branch_labels = None
depends_on = None


def upgrade() -> None:
    create_all(op.get_bind())


def downgrade() -> None:
    drop_all(op.get_bind())
