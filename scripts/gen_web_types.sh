#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
WEB_DIR="${REPO_ROOT}/apps/web"
OPENAPI_JSON="${WEB_DIR}/openapi.json"

mkdir -p "${WEB_DIR}/src/api/generated"

PYTHONPATH="${REPO_ROOT}/packages:${REPO_ROOT}" uv run python - <<'PY' > "${OPENAPI_JSON}"
import json
import tempfile
from pathlib import Path

from apps.api.main import create_app

with tempfile.TemporaryDirectory(prefix="rushes-openapi-") as tmpdir:
    app = create_app(Path(tmpdir) / "workspace", token="openapi-typegen-token", startup_port=8000)
    print(json.dumps(app.openapi(), ensure_ascii=False, indent=2, sort_keys=True))
PY

cd "${WEB_DIR}"
pnpm openapi-typescript openapi.json -o src/api/generated/schema.d.ts
