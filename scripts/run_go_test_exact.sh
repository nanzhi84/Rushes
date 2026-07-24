#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 2 ]]; then
  echo "usage: $0 <package> <exact-test-name> [go-test-flags...]" >&2
  exit 2
fi

PACKAGE="$1"
TEST_NAME="$2"
shift 2

if [[ ! "$TEST_NAME" =~ ^[A-Za-z0-9_]+$ ]]; then
  echo "invalid exact Go test name: $TEST_NAME" >&2
  exit 2
fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT/go"

LIST_OUTPUT="$(go test "$@" "$PACKAGE" -list "^${TEST_NAME}$")"
MATCH_COUNT="$(printf '%s\n' "$LIST_OUTPUT" | awk -v test_name="$TEST_NAME" '$0 == test_name { count++ } END { print count + 0 }')"
if [[ "$MATCH_COUNT" -ne 1 ]]; then
  echo "expected exactly one Go test named $TEST_NAME in $PACKAGE, found $MATCH_COUNT" >&2
  exit 1
fi

exec go test "$@" "$PACKAGE" -run "^${TEST_NAME}$" -count=1 -v
