#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

# 自动加载仓库根 .env（本地密钥，已 gitignore），与 dev_all.sh 同一套语义：
# 已 export 的同名变量优先，.env 只填当前环境里尚未设置的键（不用 `set -a; . .env`，
# 那会无条件覆盖已有 env）。
_load_dotenv() {
  local env_file="$1"
  [ -f "$env_file" ] || return 0
  local line key val
  while IFS= read -r line || [ -n "$line" ]; do
    line="${line%$'\r'}"
    line="${line#"${line%%[![:space:]]*}"}"
    case "$line" in '' | '#'*) continue ;; esac
    line="${line#export }"
    case "$line" in *=*) ;; *) continue ;; esac
    key="${line%%=*}"
    val="${line#*=}"
    key="${key%"${key##*[![:space:]]}"}"
    case "$key" in '' | *[!A-Za-z0-9_]*) continue ;; esac
    [ -n "${!key+x}" ] && continue
    case "$val" in
      \"*\") val="${val#\"}" && val="${val%\"}" ;;
      \'*\') val="${val#\'}" && val="${val%\'}" ;;
    esac
    export "$key=$val"
  done <"$env_file"
}
_load_dotenv "$ROOT/.env"

export RUSHES_API_PORT="${RUSHES_API_PORT:-8000}"
export RUSHES_WORKSPACE_PATH="${RUSHES_WORKSPACE_PATH:-$PWD/.rushes}"

exec uv run uvicorn apps.api.main:create_app_from_env \
  --factory \
  --host 127.0.0.1 \
  --port "$RUSHES_API_PORT" \
  --no-access-log
