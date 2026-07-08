#!/usr/bin/env bash
set -euo pipefail

# 一键起 Rushes 本地全栈：api + worker + web（vite），端口/令牌/代理自动对齐。
# 默认端口刻意避开 8000（常被本机其它项目占用）；端口被占直接报错退出，绝不静默
# 把前端代理挂到别人的后端上——那会表现为「一切操作都失败」而看不出原因。

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
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
