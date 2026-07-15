#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

bash "$ROOT/scripts/check_no_legacy_backend.sh"
bash "$ROOT/scripts/check_e2e_scaffold_isolation.sh"

cp "$ROOT/go/internal/api/openapi.gen.go" "$TMP/openapi.gen.go"
cp "$ROOT/apps/web/src/api/generated/schema.d.ts" "$TMP/schema.d.ts"

(
  cd "$ROOT/go"
  go generate ./internal/api
)
bash "$ROOT/scripts/gen_web_types.sh"

cmp "$TMP/openapi.gen.go" "$ROOT/go/internal/api/openapi.gen.go"
cmp "$TMP/schema.d.ts" "$ROOT/apps/web/src/api/generated/schema.d.ts"

(
  cd "$ROOT/go"
  go test -tags='' ./internal/agent ./internal/api
  go test -tags=e2e_scaffold ./internal/agent ./internal/api
)

echo "OpenAPI 生成物与两套 SSE 金丝雀均无漂移。"
