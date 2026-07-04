from __future__ import annotations

import sys
from logging.config import fileConfig
from pathlib import Path

from alembic import context
from sqlalchemy import engine_from_config, pool
from sqlalchemy.engine import Connection

PACKAGES_DIR = Path(__file__).resolve().parents[2]
if str(PACKAGES_DIR) not in sys.path:
    sys.path.insert(0, str(PACKAGES_DIR))

from storage.db import create_workspace_engine  # noqa: E402
from storage.schema import metadata  # noqa: E402
from storage.workspace_paths import WorkspacePaths  # noqa: E402

config = context.config
if config.config_file_name is not None:
    fileConfig(config.config_file_name)

target_metadata = metadata


def _db_path_from_args() -> Path | None:
    args = context.get_x_argument(as_dictionary=True)
    db_path = args.get("db_path")
    if db_path is not None:
        return Path(db_path).expanduser().resolve(strict=False)
    workspace = args.get("workspace")
    if workspace is not None:
        return WorkspacePaths.from_root(workspace).initialize().db_path
    return None


def run_migrations_offline() -> None:
    db_path = _db_path_from_args()
    url: str | None = config.get_main_option("sqlalchemy.url")
    if db_path is not None:
        url = f"sqlite+pysqlite:///{db_path}"
    context.configure(
        url=url,
        target_metadata=target_metadata,
        literal_binds=True,
        dialect_opts={"paramstyle": "named"},
        render_as_batch=True,
    )
    with context.begin_transaction():
        context.run_migrations()


def _connect() -> Connection:
    db_path = _db_path_from_args()
    if db_path is not None:
        return create_workspace_engine(db_path).connect()
    connectable = engine_from_config(
        config.get_section(config.config_ini_section, {}),
        prefix="sqlalchemy.",
        poolclass=pool.NullPool,
    )
    return connectable.connect()


def run_migrations_online() -> None:
    with _connect() as connection:
        context.configure(
            connection=connection,
            target_metadata=target_metadata,
            render_as_batch=True,
        )
        with context.begin_transaction():
            context.run_migrations()


if context.is_offline_mode():
    run_migrations_offline()
else:
    run_migrations_online()
