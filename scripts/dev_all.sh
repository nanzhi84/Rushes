#!/usr/bin/env bash
set -euo pipefail

# 一键起 Rushes 本地全栈：api + worker + web（vite），端口/令牌/代理自动对齐。
# 默认端口刻意避开 8000（常被本机其它项目占用）；端口被占直接报错退出，绝不静默
# 把前端代理挂到别人的后端上——那会表现为「一切操作都失败」而看不出原因。

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

# 自动加载仓库根 .env（本地密钥，已 gitignore）。语义：已 export 的同名变量优先，
# .env 不覆盖——只给「当前环境里尚未设置」的键赋值（临时 KEY=val bash scripts/... 可覆盖）。
# 刻意不用 `set -a; . .env`：那会无条件覆盖已有 env，违背 export 优先。
_load_dotenv() {
  local env_file="$1"
  [ -f "$env_file" ] || return 0
  local line key val
  while IFS= read -r line || [ -n "$line" ]; do
    line="${line%$'\r'}"                       # 去掉可能的 CRLF
    line="${line#"${line%%[![:space:]]*}"}"    # 去掉行首空白
    case "$line" in '' | '#'*) continue ;; esac  # 跳过空行与注释
    line="${line#export }"                       # 容忍可选 export 前缀
    case "$line" in *=*) ;; *) continue ;; esac
    key="${line%%=*}"
    val="${line#*=}"
    key="${key%"${key##*[![:space:]]}"}"         # 去掉 key 尾部空白
    case "$key" in '' | *[!A-Za-z0-9_]*) continue ;; esac  # 只认合法标识符
    [ -n "${!key+x}" ] && continue               # export 优先：已设（含空串）则不覆盖
    case "$val" in                               # 去掉首尾成对引号
      \"*\") val="${val#\"}" && val="${val%\"}" ;;
      \'*\') val="${val#\'}" && val="${val%\'}" ;;
    esac
    export "$key=$val"
  done <"$env_file"
}
_load_dotenv "$ROOT/.env"

API_PORT="${RUSHES_API_PORT:-8010}"
WEB_PORT="${RUSHES_WEB_PORT:-8011}"
WORKSPACE="${RUSHES_WORKSPACE_PATH:-$ROOT/.rushes}"
TOKEN="${RUSHES_API_TOKEN:-$(python3 -c 'import secrets; print(secrets.token_urlsafe(32))')}"

for port in "$API_PORT" "$WEB_PORT"; do
  if lsof -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
    owner="$(lsof -iTCP:"$port" -sTCP:LISTEN | tail -1 | awk '{print $1" (pid "$2")"}')"
    echo "错误：端口 $port 已被 $owner 占用。" >&2
    echo "换端口再来：RUSHES_API_PORT=xxxx RUSHES_WEB_PORT=yyyy bash scripts/dev_all.sh；或先停掉占用进程。" >&2
    exit 1
  fi
done

# 两个模型密钥都缺就黄字警告（不阻断启动）：没密钥剪辑代理无法工作，聊天只会回一句
# 「未配置模型密钥…」。别静默——否则会表现为「发消息没反应」而看不出原因。
if [ -z "${RUSHES_DASHSCOPE_API_KEY:-}" ] && [ -z "${RUSHES_LLM_API_KEY:-}" ]; then
  printf '\033[33m警告：未检测到 RUSHES_DASHSCOPE_API_KEY 或 RUSHES_LLM_API_KEY，剪辑代理将无法工作。\033[0m\n' >&2
  printf '\033[33m      先 export 密钥再启动：export RUSHES_DASHSCOPE_API_KEY=sk-xxxx\033[0m\n' >&2
fi

cleanup() {
  trap - EXIT INT TERM
  kill 0 2>/dev/null || true
}
trap cleanup EXIT INT TERM

RUSHES_WORKSPACE_PATH="$WORKSPACE" RUSHES_API_TOKEN="$TOKEN" RUSHES_API_PORT="$API_PORT" \
  uv run uvicorn apps.api.main:create_app_from_env \
  --factory --host 127.0.0.1 --port "$API_PORT" --no-access-log &

# 等 api 建好工作区库再起 worker，避免首启竞态。
for _ in $(seq 1 60); do
  [ -f "$WORKSPACE/rushes.db" ] && break
  sleep 0.5
done

uv run python -m apps.worker.main "$WORKSPACE" &

RUSHES_WEB_PROXY_TARGET="http://127.0.0.1:$API_PORT" \
  npx -y pnpm@10.13.1 --dir "$ROOT/apps/web" dev --host 127.0.0.1 --port "$WEB_PORT" --strictPort &

sleep 2
echo
echo "════════════════════════════════════════════════════"
echo "  Rushes 全栈已启动，浏览器打开："
echo "  http://127.0.0.1:$WEB_PORT/#t=$TOKEN"
echo "  （API :$API_PORT · workspace: $WORKSPACE · Ctrl+C 一键全停）"
echo "════════════════════════════════════════════════════"
wait
