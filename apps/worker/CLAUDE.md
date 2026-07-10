# apps/worker — SQLite Job 轮询 worker

- **Job 注册表 `job_registry.py`**：`kind → async handler` 一一映射。新增一类 Job 只需在 `build_default_job_registry` 里 `registry.register(kind, handler)`。内置 kind：`noop / poster / proxy / index / hash / asr / tts / align / import_url / render_preview / render_final`。`poster` 是缩略图/时长秒出的快任务（claim 优先级最高，见 `packages/storage/repositories/jobs.py` 的 `CLAIM_SQL`），与 `proxy` 一同在导入时入队；`hash` 是 REFERENCE 素材 canonical sha256 的后台补算（claim 优先级最低，不与推进素材可用性的加工抢占）。handler 只在 SQLite 写事务**之外**执行重活，成功返回 `JobExecutionResult`，失败抛 `JobExecutionError(retryable=...)`。
- **单 runner → 并发池**（`main.py`）：`resolve_concurrency` 读 `RUSHES_WORKER_CONCURRENCY`（缺省 2），起 N 个 `JobRunner`，每个拿独立 `worker_id` 才能各自认领/续心跳。
- **链式入队靠事件**：handler 不直接调下一步，而是发 `JobEnqueued` 事件（幂等 key）。链路是 `import_url → proxy → index`（`media_jobs.py` 的 `_enqueue_index` / `_proxy_job_event`），reducer 落库后 runner 再认领。字体没有可转码代理，proxy handler 直接跳到 index。
- **认领/心跳/租约**（`job_runner.py` + `heartbeat.py`）：`claim_next` 用 `begin_immediate` 事务 + `worker_id` 保证多 worker 安全；认领后 `heartbeat_until_done` 定时续 `heartbeat`；`run_forever` 启动时先 `recover_stale_running`（`reset_stale_running`）把心跳超时（崩溃遗留）的 running Job 重新排队。可重试且 `attempts <= max_retries` 时 `_schedule_retry`（退避见 `_retry_delay_seconds`）。
- `main.py` 启动也会 `create_all + apply_data_migrations`（worker 可能先于 API 碰到全新 workspace）。
- index Job 是 best-effort：失败发 `AssetIndexFailed` 但 Job 仍成功，不阻塞素材可用性。
