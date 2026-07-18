#!/usr/bin/env bash
set -euo pipefail

# 一条命令拉起 Go API、Go worker 与 Vite。根目录 .env 会被读取，但已 export
# 的同名变量始终优先；端口冲突直接失败，避免前端误连到别的本地服务。
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

load_dotenv() {
  local env_file="$1"
  [[ -f "$env_file" ]] || return 0
  local line key value
  while IFS= read -r line || [[ -n "$line" ]]; do
    line="${line%$'\r'}"
    line="${line#"${line%%[![:space:]]*}"}"
    case "$line" in '' | '#'* ) continue ;; esac
    line="${line#export }"
    case "$line" in *=*) ;; *) continue ;; esac
    key="${line%%=*}"
    value="${line#*=}"
    key="${key%"${key##*[![:space:]]}"}"
    case "$key" in '' | *[!A-Za-z0-9_]*) continue ;; esac
    [[ -n "${!key+x}" ]] && continue
    case "$value" in
      \"*\") value="${value#\"}"; value="${value%\"}" ;;
      \'*\') value="${value#\'}"; value="${value%\'}" ;;
    esac
    export "$key=$value"
  done <"$env_file"
}

port_in_use() {
  local port="$1"
  if command -v lsof >/dev/null 2>&1; then
    lsof -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1
    return
  fi
  if command -v ss >/dev/null 2>&1; then
    ss -ltn "sport = :$port" | tail -n +2 | grep -q .
    return
  fi
  return 1
}

load_dotenv "$ROOT/.env"

persist_generated_token() {
  local env_file="$1"
  local token="$2"
  local temporary old_umask
  old_umask="$(umask)"
  umask 077
  touch "$env_file"
  temporary="$(mktemp "${env_file}.tmp.XXXXXX")"
  awk -v token="$token" '
    BEGIN { replaced = 0 }
    {
      candidate = $0
      sub(/^[[:space:]]*export[[:space:]]+/, "", candidate)
      if (candidate ~ /^[[:space:]]*RUSHES_API_TOKEN[[:space:]]*=/) {
        if (!replaced) {
          print "RUSHES_API_TOKEN=" token
          replaced = 1
        }
        next
      }
      print
    }
    END {
      if (!replaced) print "RUSHES_API_TOKEN=" token
    }
  ' "$env_file" >"$temporary"
  chmod 600 "$temporary"
  mv "$temporary" "$env_file"
  umask "$old_umask"
}

API_PORT="${RUSHES_API_PORT:-8010}"
WEB_PORT="${RUSHES_WEB_PORT:-8011}"
WORKSPACE="${RUSHES_WORKSPACE_PATH:-$ROOT/.rushes}"
case "$WORKSPACE" in
  /*) ;;
  *) WORKSPACE="$ROOT/$WORKSPACE" ;;
esac
if [[ -n "${RUSHES_API_TOKEN:-}" ]]; then
  TOKEN="$RUSHES_API_TOKEN"
else
  TOKEN="$(openssl rand -hex 32)"
  persist_generated_token "$ROOT/.env" "$TOKEN"
  export RUSHES_API_TOKEN="$TOKEN"
  printf '\033[32m已生成本地启动 token 并安全保存到 .env；后续启动将自动复用。\033[0m\n'
fi
BIN_DIR="$WORKSPACE/bin"

for port in "$API_PORT" "$WEB_PORT"; do
  if port_in_use "$port"; then
    echo "错误：端口 $port 已被占用。请设置 RUSHES_API_PORT / RUSHES_WEB_PORT 后重试。" >&2
    exit 1
  fi
done

CHAT_PROVIDER="$(printf '%s' "${RUSHES_CHAT_PROVIDER:-dashscope}" | tr '[:upper:]' '[:lower:]' | tr -d '[:space:]')"
if [[ "$CHAT_PROVIDER" == "dashscope" && -z "${RUSHES_DASHSCOPE_API_KEY:-}" ]]; then
  printf '\033[33m警告：未配置 RUSHES_DASHSCOPE_API_KEY；本地链路可运行，但 Agent 会使用无模型降级回复。\033[0m\n' >&2
elif [[ "$CHAT_PROVIDER" == "ark" && -z "${RUSHES_ARK_API_KEY:-}" && ( -z "${RUSHES_ARK_ACCESS_KEY:-}" || -z "${RUSHES_ARK_SECRET_KEY:-}" ) ]]; then
  printf '\033[33m警告：RUSHES_CHAT_PROVIDER=ark 但未配置 RUSHES_ARK_API_KEY（或 AK/SK）；API 与 worker 会在启动期报错。\033[0m\n' >&2
fi

mkdir -p "$BIN_DIR"
(
  cd "$ROOT/go"
  go build -o "$BIN_DIR/rushes-api" ./cmd/api
  go build -o "$BIN_DIR/rushes-worker" ./cmd/worker
)

pids=()
cleanup() {
  trap - EXIT INT TERM
  for pid in "${pids[@]}"; do
    kill "$pid" 2>/dev/null || true
  done
  for pid in "${pids[@]}"; do
    wait "$pid" 2>/dev/null || true
  done
}
trap cleanup EXIT INT TERM

RUSHES_WORKSPACE_PATH="$WORKSPACE" RUSHES_API_TOKEN="$TOKEN" RUSHES_API_PORT="$API_PORT" \
  "$BIN_DIR/rushes-api" -env-file "$ROOT/.env" -workspace "$WORKSPACE" -port "$API_PORT" -token "$TOKEN" &
pids+=("$!")

ready=0
for _ in $(seq 1 120); do
  if curl --silent --fail "http://127.0.0.1:$API_PORT/healthz" >/dev/null 2>&1; then
    ready=1
    break
  fi
  if ! kill -0 "${pids[0]}" 2>/dev/null; then
    echo "错误：Rushes API 启动失败。" >&2
    exit 1
  fi
  sleep 0.25
done
if [[ "$ready" != 1 ]]; then
  echo "错误：Rushes API 在 30 秒内未就绪。" >&2
  exit 1
fi

RUSHES_WORKSPACE_PATH="$WORKSPACE" \
  "$BIN_DIR/rushes-worker" -env-file "$ROOT/.env" -workspace "$WORKSPACE" &
pids+=("$!")

env -u RUSHES_DASHSCOPE_API_KEY -u RUSHES_API_TOKEN \
  RUSHES_WEB_PROXY_TARGET="http://127.0.0.1:$API_PORT" \
  npx -y pnpm@10.13.1 --dir "$ROOT/apps/web" dev --host 127.0.0.1 --port "$WEB_PORT" --strictPort &
pids+=("$!")

echo
echo "════════════════════════════════════════════════════"
echo "  Rushes Go 全栈已启动："
echo "  日常访问：http://127.0.0.1:$WEB_PORT"
echo "  首次登录：http://127.0.0.1:$WEB_PORT/#t=$TOKEN"
echo "  API :$API_PORT · workspace: $WORKSPACE · Ctrl+C 全停"
echo "════════════════════════════════════════════════════"

while true; do
  for pid in "${pids[@]}"; do
    if ! kill -0 "$pid" 2>/dev/null; then
      status=0
      wait "$pid" || status=$?
      exit "$status"
    fi
  done
  sleep 1
done
