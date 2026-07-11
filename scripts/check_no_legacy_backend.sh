#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

legacy_paths=(
  apps/api
  apps/worker
  packages
  tests
  pyproject.toml
  uv.lock
)

found=()
for path in "${legacy_paths[@]}"; do
  tracked="$(git ls-files -- "$path" "$path/**")"
  if [[ -n "$tracked" ]]; then
    found+=("$path")
  fi
done

python_files="$(git ls-files -- '*.py' '*.pyi' 'requirements*.txt' 'Pipfile*' 'poetry.lock' 'setup.py' 'setup.cfg' 'tox.ini' '.python-version')"
if [[ -n "$python_files" ]]; then
  while IFS= read -r path; do
    found+=("$path")
  done <<<"$python_files"
fi

if (( ${#found[@]} > 0 )); then
  printf '检测到已废弃的 Python 后端残留：\n' >&2
  printf '  - %s\n' "${found[@]}" >&2
  exit 1
fi

echo "旧 Python 后端残留检查通过。"
