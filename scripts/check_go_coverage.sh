#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RAW="$ROOT/go/coverage.raw.out"
MERGED="$ROOT/go/coverage.out"

(
  cd "$ROOT/go"
  go test -race -covermode=atomic -coverpkg=./internal/... -coverprofile="$RAW" ./internal/...
)

# 多 package 的 -coverpkg 会为同一源码块输出多份计数。按块取最大执行次数，
# 得到跨包单测的并集；机器生成的 oapi-codegen 文件不计入手写后端覆盖率。
awk '
  NR == 1 { mode = $0; next }
  $1 !~ /openapi\.gen\.go/ {
    key = $1 FS $2
    if (!(key in count) || $3 > count[key]) count[key] = $3
    block[key] = $1 FS $2
  }
  END {
    print mode
    for (key in block) print block[key], count[key]
  }
' "$RAW" > "$MERGED"

TOTAL="$(cd "$ROOT/go" && go tool cover -func=coverage.out | awk '/^total:/ {gsub(/%/, "", $3); print $3}')"
printf 'Rushes Go handwritten coverage: %.1f%%\n' "$TOTAL"
awk -v total="$TOTAL" 'BEGIN { if (total + 0 < 90) exit 1 }'
