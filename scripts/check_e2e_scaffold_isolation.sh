#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

(
  cd "$ROOT/go"
  go build -trimpath -tags='' -o "$TMP/rushes-api-production" ./cmd/api
  go build -trimpath -tags=e2e_scaffold -o "$TMP/rushes-api-e2e" ./cmd/api
)
LC_ALL=C strings "$TMP/rushes-api-production" > "$TMP/production.strings"
LC_ALL=C strings "$TMP/rushes-api-e2e" > "$TMP/e2e.strings"

markers=(
  E2E_BLOCK_UNTIL_CANCEL
  E2E_CANCEL_UNDERSTANDING
  E2E_FULL_MAINLINE
)
for marker in "${markers[@]}"; do
  if grep -Fq "$marker" "$TMP/production.strings"; then
    echo "生产 API 二进制仍包含 E2E scaffold 标记: $marker" >&2
    exit 1
  fi
  if ! grep -Fq "$marker" "$TMP/e2e.strings"; then
    echo "e2e_scaffold API 二进制缺少测试标记: $marker" >&2
    exit 1
  fi
done

if grep -Fq "e2e_cancel" "$TMP/production.strings"; then
  echo "生产 API 二进制仍包含素材理解测试延时钩子" >&2
  exit 1
fi

echo "生产 API 二进制不含 E2E scaffold，tagged 构建包含全部测试入口。"
