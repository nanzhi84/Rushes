#!/usr/bin/env bash
set -euo pipefail

export RUSHES_API_PORT="${RUSHES_API_PORT:-8000}"
export RUSHES_WORKSPACE_PATH="${RUSHES_WORKSPACE_PATH:-$PWD/.rushes}"

exec uv run uvicorn apps.api.main:create_app_from_env \
  --factory \
  --host 127.0.0.1 \
  --port "$RUSHES_API_PORT" \
  --no-access-log
