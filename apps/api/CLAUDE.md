# apps/api — 本地单进程 FastAPI

- **`main.py` 是单文件路由集**：所有路由都在 `create_app()` 里用 `@app.<method>(...)` 就地注册（`_register_routes`），没有 APIRouter 拆分。`create_app_from_env()` 从环境变量装配（`RUSHES_WORKSPACE_PATH` / `RUSHES_API_TOKEN` / `RUSHES_API_PORT` / `RUSHES_FS_ROOTS`）。
- **启动即建库**：`create_app` 里 `schema.create_all(connection)` + `apply_data_migrations(connection)`（幂等，见 packages/storage）。启动钩子还拉起 `_job_observation_bridge`——把独立 worker 完成的 Job 事件转成 job_observation turn 唤醒 Agent（缺它 Agent 永远等不到 ASR/渲染结果）。
- **鉴权在 `deps.py` 的 `security_baseline_middleware`**（只管 `/api/` 前缀）：依次校验 Host=`127.0.0.1:<port>`、Origin、Bearer token、mutation 的 `Content-Type: application/json`（上传分片放行 `application/octet-stream`）。拒绝会落一条 `SecurityRefusal` 事件。
- **query-token 白名单**（`_allows_query_token`）：SSE 端点与 GET 媒体流（`/api/media/...`）允许 `?token=`，因为浏览器原生 `EventSource`/`<img>`/`<video>` 设不了 header。其余一律走 `Authorization: Bearer`。
- **两条 SSE 通道，机制不同**：
  - 领域事件流（`/api/events`、`.../cases/{id}/events`）——轮询尾随 `event_log`，按 `Last-Event-ID` 续传，`route_workspace`/`route_case` 谓词过滤。
  - `.../turn-stream`——进程内 `TurnStreamHub`（`turn_stream.py`）：先回放当前 turn 快照，再实时排空队列；慢订阅者超 `SUBSCRIBER_QUEUE_LIMIT` 直接踢下线。
- 测试用 `sse_max_events` 让无限流主动收尾（同步 TestClient 消费不了无限流）。
- 改任何请求/响应模型后，跑 `scripts/gen_web_types.sh` 重新生成前端类型。
