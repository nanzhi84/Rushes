#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
WEB_DIR="${REPO_ROOT}/apps/web"
OPENAPI_JSON="${WEB_DIR}/openapi.json"

if [[ ! -f "${OPENAPI_JSON}" ]]; then
  echo "缺少冻结契约：${OPENAPI_JSON}" >&2
  exit 1
fi

mkdir -p "${WEB_DIR}/src/api/generated"

cd "${REPO_ROOT}/go"
go run ./cmd/specfix -check "${OPENAPI_JSON}"

cd "${WEB_DIR}"
npx -y pnpm@10.13.1 exec openapi-typescript openapi.json -o src/api/generated/schema.d.ts
