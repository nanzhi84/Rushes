# Spec C 实施计划：agentic 素材理解（替换离线标注与检索）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 上传只做便宜本地索引（缩略图/分镜/波形/VAD）；素材理解改为 turn 内并行的 VLM 子代理按需执行，产出带时间戳的 MaterialSummary 沉淀复用；离线标注与检索基建（annotation/indexing/candidate_pack/FTS/embedding）全删；时间线改为直接按摘要时间戳组装。

**Architecture:** 十个任务四个阶段：地基（迁移守卫→schema/契约→索引 job）→ 能力（async 工具执行→理解子代理）→ 拆除（标注/检索删除→timeline 重构）→ 集成（context 注入→前端→golden/e2e/PRD）。新链路先立、旧链路后拆，中间每个任务全量绿。

**Tech Stack:** Python 3.12 + FastAPI + SQLAlchemy Core + httpx + PySceneDetect + Silero VAD (onnx) + ffmpeg + fonttools(新增)；React + TS + vitest；`uv run` / `npx -y pnpm@10.13.1 --dir apps/web`。

**工作目录：** `/Users/yoryon/Projects/Rushes/.worktrees/refactor-c`（分支 `refactor/c-agentic-understanding`）

## Global Constraints

- Spec：`docs/superpowers/specs/2026-07-06-c-agentic-material-understanding-design.md`（验收 6 条为准）。MaterialSummary 契约字段以 spec C3 的 JSON 为准，时间戳一律秒（float）。
- transcripts 表、ASR provider、`audio.asr_original`、`media.view_frames`（重构后）保留；`usable` 列保留但语义收窄为「文件未失效」（invalidation 链路），不再是标注门。
- 每任务提交前：`uv run pytest -q`（90% 覆盖率门槛）+ `uv run ruff check && uv run ruff format --check && uv run mypy && uv run python scripts/check_contracts.py` 全绿（前端任务另加 web 三连）。
- 面向用户文案简体中文；注释只写代码不能自明的约束。
- 提交信息中文 + 两行尾注（Co-Authored-By: Claude Fable 5 <noreply@anthropic.com> / Claude-Session: https://claude.ai/code/session_01FJ3rEvooREoJUdT1DbCFH6）。
- 基线：pytest 581 passed / 覆盖率 90.43%；web 37 passed；Playwright 2 passed。

---

### Task 1: data_migrations 表存在守卫（阻塞项）

**Files:** Modify `packages/storage/data_migrations.py`；Test `tests/storage/test_data_migrations.py`

背景事实：`_collapse_asset_kinds`（:42-64）对 clip_fts/annotation_* 等表做无守卫 DELETE；本 PR 后新库不再建这些表，api/worker 启动即 `no such table` 崩溃。

- [ ] Step 1: 测试先行——用「不含 annotation 表族的新 schema」建库跑 `apply_data_migrations` 断言不抛错且幂等（RED）
- [ ] Step 2: 新增 `_table_exists(connection, name)`（查 sqlite_master）；`_collapse_asset_kinds` 里每条涉及可能不存在表的语句套守卫；`_DOOMED_*` 子查询同样处理（表不在则跳过对应 DELETE）
- [ ] Step 3: 全量验证 → Commit `fix(storage): 数据迁移按表存在守卫，兼容删表后的新库`

### Task 2: schema/契约/事件基建（纯加法）

**Files:** Modify `packages/storage/schema.py`（assets 加列 + material_summaries 表）、`packages/storage/data_migrations.py`（幂等 ALTER 加列）、`packages/contracts/`（新 understanding.py + events.py 新事件）、`packages/agent_harness/reducer.py`、`packages/agent_harness/state_validator.py`；Create `packages/storage/repositories/material_summaries.py`；Test tests/storage、tests/contracts、tests/agent_harness

**Interfaces（后续任务消费，固定）:**
- assets 新列：`thumbnail_object_hash Text FK objects.hash nullable`、`index_json Text nullable`、`understanding_status Text nullable=False server_default "none"`（none/running/ready/failed）。
- `material_summaries` 表：`summary_id Text PK, asset_id Text FK, version Integer, focus Text nullable, status Text (running/ready/failed), summary_json Text, model Text nullable, created_at Text`；唯一索引 (asset_id, version)。
- `packages/contracts/understanding.py`：pydantic `MaterialSummary`（spec C3 JSON：asset_id/version/focus/semantic_role/overall/language?/segments[{start_s,end_s,description,transcript?,tags,quality,notes?}]/generated_at/model/spent{frames_viewed,asr_seconds}）与 `SummarySegment`；`semantic_role` Literal 与 spec 一致。
- 事件（contracts/events.py，进 union+__all__+reducer dispatch+state_validator 白名单）：`AssetIndexReady{asset_id, payload:{index_json, thumbnail_object_hash}}`、`AssetIndexFailed{asset_id, payload:{failure}}`、`MaterialUnderstandingStarted/Completed/Failed{asset_id, payload:{summary_id?, version?, failure?}}`。reducer：IndexReady 写 index_json/thumbnail_object_hash + `ingest_status="indexed"`；Understanding* 写 understanding_status（running/ready/failed）。
- `MaterialSummariesRepository`：`insert(values)`、`latest_ready(asset_id) -> dict|None`、`list_latest_for_assets(asset_ids) -> dict[str, dict]`、`mark_status(summary_id, status)`。

- [ ] Step 1: TDD（契约校验、reducer 三事件状态写、repo 往返、ALTER 幂等迁移测试）RED→GREEN
- [ ] Step 2: 全量验证 → Commit `feat(storage,contracts): 素材索引与理解摘要基建`

### Task 3: 便宜本地索引 job + 缩略图服务 + worker 小并发

**Files:** Create `packages/media/shots.py`（从 packages/annotation/shot_split.py 迁移 PySceneDetect 主路径，删 TransNetV2 适配器与 CapabilityDegraded 回退——直接空列表回退）、`packages/media/thumbnails.py`（视频封面帧/任意时间点帧 + 图片缩略，ffmpeg 管道复用 media_tools `_extract_frame_data_uri` 的 scale 参数思路但落文件产 jpg bytes）、`packages/media/waveform.py`（ffmpeg 解 PCM + numpy 降采样 min/max peaks）、`packages/media/font_meta.py`（fonttools 读 family/style）、`apps/worker/index_jobs.py`；Modify `pyproject.toml`（+fonttools）、`apps/worker/media_jobs.py`（proxy 成功后追加 `JobEnqueued(kind="index")`，照 `_proxy_job_event` :217-233 范式，幂等键 `asset:{id}:index`）、`apps/worker/job_registry.py`（注册 index）、`apps/worker/main.py`（起 N 个 JobRunner，默认 2，env `RUSHES_WORKER_CONCURRENCY`；claim 已多 worker 安全）、`apps/api/main.py`（新端点 `GET /api/media/{asset_id}/thumbnail` 仿 proxy 端点 :1381-1388 + `_require_proxy_path` :2140 范式；素材列表 JSON :1880-1899 增 `thumbnail_ready`、`duration_sec`（读 probe）、`understanding_status`）；Test tests/media、tests/worker（apps/worker 若在 coverage omit 则给纯函数部分建测试）、tests/api

**index job 行为**：按 kind 分派——video：封面帧（1s 或 duration/10 处）+ PySceneDetect shots（秒边界数组）；audio：Silero VAD 区间（复用 `packages/media/vad.py:42` `run_silero_vad`，模型缺失时优雅降级空数组）+ peaks（512 桶 min/max）；image：缩略图；font：family/style 元数据。产物 jpg 写 object store，`AssetIndexReady(payload={index_json:{shots|vad|peaks|font_meta, duration_sec}, thumbnail_object_hash})`；任何一步失败 → `AssetIndexFailed`（不影响素材可用）。

- [ ] Step 1: 纯函数 TDD（shots 用合成视频 fixture——tests/media 现有 ffmpeg fixture 范式；waveform 用合成正弦 wav；font 用系统字体文件或最小 ttf fixture）
- [ ] Step 2: handler + 链接 + 端点 + 并发；索引对 e2e 夹具素材真实可跑
- [ ] Step 3: 全量验证 → Commit `feat(media,worker,api): 上传便宜本地索引与缩略图服务`

### Task 4: async 工具执行路径 + 进度通道

**Files:** Modify `packages/tools/registry.py`（ToolHandler 类型放宽为可返回 Awaitable）、`packages/tools/tool_router.py`、`packages/agent_harness/loop.py`（`_execute_tool` async 化：`inspect.iscoroutinefunction(handler)` → await；同步 handler 行为不变；调用点 :659 加 await）、`packages/tools/context.py`（不动结构——进度回调经 `metadata["turn_progress"]` 注入，loop `_tool_context_metadata` :1932 填充：包一层永不抛错的 `Callable[[Mapping], None]`，转发为 turn-stream `{"type":"subagent_progress", ...payload}` 事件）；Test tests/agent_harness（async handler 全链路：执行/异常/进度事件序列）、tests/tools

**Interfaces:** 工具 handler 可为 `async def handler(input_model, context) -> ToolResult`；`context.metadata["turn_progress"]` 是 `Callable[[Mapping[str, Any]], None]`（无 listener 时为 no-op 函数，永不为 None）。Task 5 消费。

- [ ] Step 1: TDD（注册一个 async 测试工具：断言被 await、result 正常入 accumulator/trace、progress 回调产生 turn-stream 事件、异常路径与同步 handler 等价）
- [ ] Step 2: 实现（注意 `_maybe_answer_pending_decision_from_user_message` 与 defer/replay 路径同样经过执行点的要一并适配）
- [ ] Step 3: 全量验证 → Commit `feat(harness,tools): 工具执行支持 async handler 与进度通道`

### Task 5: 理解子代理 + understand.materials / asset.read_summary 工具

**Files:** Create `packages/tools/understand/{__init__.py,handlers.py,subagent.py}`；Modify `packages/tools/specs.py`（两个新 ToolSpec + handler 注册）、`packages/providers/tool_gateway.py:48`（VLM model 可经 `RUSHES_VLM_MODEL` 覆盖）、`packages/tools/media_tools/handlers.py`（抽出 `extract_frame_data_uri` 供 subagent 复用——公共函数化，view_frames 行为不变）；Test tests/tools/understand（脚本化 VLM：MockProvider 喂 JSON 动作序列）

**子代理协议（subagent.py）**：多模态 mini-loop，直接经 `context.metadata["provider_gateway"]` 调 `ProviderRequest(capability=VLM_ANNOTATION)`（照 media_tools/handlers.py:149-161 范式，json_object 强制）。每步给 VLM：system（素材理解员职责+输出契约）+ 素材便宜索引摘要（index_json）+ 已看帧图（content 数组 image_url）+ 已得转写片段 + 动作菜单。VLM 返回 JSON 动作之一：
- `{"action":"view_frames","timestamps_s":[...]}`（≤6 帧/次，走抽帧函数，图进下一步上下文）
- `{"action":"transcribe","start_s","end_s"}`（仅音频/含音轨视频；直接调 ASR 链路函数——读 apps/worker/audio_jobs.py 的 VAD+上传+paraformer 流程抽成可复用函数或直接调用既有函数，结果写 transcripts 表并回灌子代理）
- `{"action":"emit_summary","summary":{...}}`（终结；schema 校验失败给一次纠错重试）
步数预算 12、单素材超时 `RUSHES_UNDERSTAND_TIMEOUT_S`（默认 300）、非法 JSON 连续 3 次判失败。每步经 `turn_progress({"asset_id", "note": "正在查看 02:10 画面"})` 推进度。

**understand.materials（async handler）**：入参 `{asset_ids: list[str], focus: str|None}`；spec `requires_active_project=True`、无人工确认。缓存：asset 有 `latest_ready` 且 focus 空 → 直接返回；有 focus → 子代理带旧摘要增量深挖存 version+1。并发 `asyncio.gather` + `Semaphore(RUSHES_UNDERSTAND_CONCURRENCY 默认 3)`。事件：每 asset 发 MaterialUnderstandingStarted → Completed/Failed（经 ToolResult.events 走 reducer）。observation 返回每 asset 的摘要全文或失败原因；工具永不整体失败（部分失败在 data 里逐 asset 报告）。

**asset.read_summary（同步只读）**：入参 asset_ids，返回 latest_ready 摘要全文集合。

- [ ] Step 1: TDD——脚本化 VLM 动作序列驱动子代理（view→transcribe(打桩)→emit）；缓存命中；focus 增量 version+1；超时/非法 JSON 失败路径；并发上限；进度事件序列
- [ ] Step 2: 实现 + specs 注册（谓词按需新增进 PRECONDITION_REGISTRY）
- [ ] Step 3: 全量验证 → Commit `feat(tools): 素材理解子代理与 understand 工具`

### Task 6: 删除离线标注与检索链路

**Files:** Delete `packages/annotation/`、`packages/indexing/`、`packages/tools/annotation/`、`packages/tools/retrieval/`、`apps/worker/annotation_jobs.py`；Modify `packages/tools/specs.py`（annotation.enqueue/status/retry/inspect :958-1012、retrieval.search_candidates :1038-1043 的 spec+import+handler 注册删除）、`packages/storage/schema.py`（annotations/annotation_clip_projection/annotation_signal_projection/candidate_packs 表定义与 clip_fts DDL/create_fts 删除；assets 删 `annotation_status/annotation_pass` 列定义）、`packages/storage/data_migrations.py`（新增守卫式 `DROP TABLE IF EXISTS` 上述表 + `DROP TABLE IF EXISTS clip_fts`；老库 assets 旧列留存无害不删）、`packages/agent_harness/reducer.py`（explorer 清单：:52-53、:281-288、:921-944、:637-665、:1186-1213 annotation 特判、:1502-1510）、`packages/agent_harness/state_validator.py`、`packages/contracts/events.py`（AnnotationCompleted/AnnotationFailed/CandidatePackCreated）、`packages/contracts/candidate.py`（删）、`packages/contracts/case.py`（candidate_pack_id 等字段）、`packages/domain/preconditions.py`（candidate_pack_valid/相关谓词）、`apps/worker/job_registry.py`（annotation kind）、`apps/api/main.py`（retry-annotation 端点 :749-764 与 AnnotationRetryInput）、`apps/api/schemas.py`、`scripts/check_contracts.py`（分层表 annotation/indexing 组移除）、受影响测试全部删/迁

**注意**：`timeline.plan_from_candidates`、`insert_candidate` op、timeline 内 candidate 依赖留给 Task 7（本任务保持 timeline 编译通过的最小交集处理——若 timeline_tools 引用被删符号，与 Task 7 协调：本任务可先删 spec 暴露（specs.py 中 plan_from_candidates 一并摘除）но保留 timeline 内部代码由 Task 7 清）。依赖清理：`scenedetect`/`opencv` 已被 media/shots.py 接管（保留）；`numpy` 保留（vad/waveform）；检查 `text-embedding` 相关 provider 配置残留一并删。

- [ ] Step 1: 按清单删除 + 全仓 grep `annotation_clip\|clip_fts\|candidate_pack\|search_candidates\|AnnotationCompleted\|CandidatePackCreated` 归零（timeline 内 candidate 引用除外，列给 Task 7）
- [ ] Step 2: 老库升级测试：含标注数据的旧 schema 库跑迁移 → 表被 DROP、启动不崩
- [ ] Step 3: 全量验证 → Commit `refactor!: 删除离线标注与检索基建`

### Task 7: timeline 从零组装

**Files:** Modify `packages/timeline/materializer.py`（新增 `materialize_from_clips(clips, case_state, fps, ...) -> TimelineState`：复用 6 轨骨架 :439-444 与角色/轨道映射，入参直接给 asset_id+源秒区间+role，不经 candidate pack/cut_plan）、`packages/timeline/patch_apply.py`（新 op `insert_clip`：asset_id/source_start_s/source_end_s/role/track_id?/position_s?，替代 `insert_candidate` :383-395 并删除之；删除 candidate 解析 :925 一带）、`packages/contracts/patch.py`（InsertClipOp 定义、InsertCandidateOp 删除）、`packages/tools/timeline_tools/handlers.py`（删 plan_from_candidates :52-150；新工具 `timeline.compose_initial`：入参 clips 列表 + 可选 voiceover_asset_id，调 materialize_from_clips 落 timeline v1）、`packages/tools/specs.py`（compose_initial spec，前置 `active_case`+`audio_plan_confirmed`+`usable_asset_exists`；EXPECTED_PATCH_OPS 在 `scripts/check_contracts.py:62-77` 同步 insert_candidate→insert_clip）、preconditions（`candidate_pack_exists` 谓词删除）；Test tests/timeline、tests/tools

**Interfaces:** `ComposeInitialInput.clips: list[{asset_id, source_start_s: float, source_end_s: float, role: "a_roll"|"b_roll"|"image"}]`；秒→帧换算用 timeline fps（读 materializer 现有换算函数）。validator 全部不变量（轨道类型/重叠/时长）继续生效。

- [ ] Step 1: TDD（compose_initial 从摘要式入参产出合法 6 轨 timeline 且过 validator；insert_clip 各角色/轨道；非法 asset/区间报错）
- [ ] Step 2: 实现 + 删旧 + check_contracts 同步
- [ ] Step 3: 全量验证 → Commit `feat(timeline)!: 摘要时间戳直接组装时间线，删除候选包路径`

### Task 8: Context Builder 素材摘要索引

**Files:** Modify `packages/agent_harness/loop.py`（LoadedState 增 `asset_digest`：join assets+material_summaries latest_ready，每行 {asset_id, filename, kind, duration_sec(probe), understanding_status, semantic_role?, overall 截断 80 字}）、`packages/agent_harness/context_builder.py`（`_render_assets_block` :457-473 重写为逐行素材索引 + 计数头；块预算 assets 提到 2000 并实际截断（行数上限 50，超出显示「另有 N 个素材」））；Test tests/agent_harness

- [ ] Step 1: TDD（有/无摘要素材的渲染、截断、understanding_status 展示）
- [ ] Step 2: 实现 → 全量验证 → Commit `feat(harness): 上下文注入素材摘要索引`

### Task 9: 前端素材页改造

**Files:** Modify `apps/web/src/components/Materials/MaterialsTable.tsx`（缩略图列 `<img src=/api/media/{id}/thumbnail>`（thumbnail_ready 才渲染，带 token query——看 proxy 预览现有取流方式）、时长列、理解状态徽标（未理解/理解中/已理解/失败）；删标注徽标/annotationLabel/pass/「重试标注」按钮）、`StatusBadge.tsx`（annotation 部分删、新增 understanding 映射）、`ProjectMaterialsPage.tsx`（MATERIAL_EVENT_TYPES :246-261 换成 AssetIndexReady/AssetIndexFailed/MaterialUnderstanding*；详情面板显示摘要 segments 时间戳表——active asset 有 ready 摘要时渲染，需要 client.ts 新增 `getAssetSummary` 或素材列表响应内嵌 summary 概要，按后端 Task 3/5 实际暴露形态选最小实现（若无现成端点则加 `GET /api/projects/{pid}/materials/{aid}/summary`，后端同 PR 内补上））、`client.ts`（MaterialAsset 类型同步：删 annotation_status/annotation_pass，加 understanding_status/thumbnail_ready/duration_sec）；Test ProjectMaterialsPage.test.tsx

- [ ] Step 1: TDD（表格渲染新列与徽标、无标注 UI 残留、摘要详情渲染）
- [ ] Step 2: 实现（若需后端 summary 端点则在本任务一并加 + API 测试）
- [ ] Step 3: web 三连 + 全量 Python 验证 → Commit `feat(web): 素材页缩略图/理解状态/摘要视图`

### Task 10: golden/e2e/PRD 收口

**Files:** Modify `tests/golden/`（新 golden case：understand.materials(脚本化 VLM)→asset.read_summary→timeline.compose_initial→content 收尾；删除已不存在工具的旧 case 引用）、`scripts/e2e_paths/`（run_path2/run_scenery 提示词与断言：检索/候选包措辞 → 理解/摘要/compose_initial；client.py 若有 candidate 相关应答逻辑更新）、`e2e/`（Playwright 两用例保绿；素材页断言若涉标注徽标则更新）、`PRD.md`（spec「PRD 修订清单」七条：硬约束、§3.1/§3.2 图、§4.10 job 表+并发、§5.2 前置表、§6.3/§6.7 删+understand 新增+§6.8 变更、§7.4 整节替换为 MaterialSummary、子代理机制新增小节；纪律同前两轮：范围内逐处、报告行号+改前改后）

- [ ] Step 1: golden + e2e 迁移与新用例
- [ ] Step 2: PRD 修订
- [ ] Step 3: 终验：全量 Python + web 三连 + Playwright 本地 + 验收 grep：`grep -rn "candidate_pack\|clip_fts\|annotation_clip\|search_candidates\|plan_from_candidates\|annotation.enqueue" --include="*.py" --include="*.ts" --include="*.tsx" apps packages scripts e2e tests` 归零
- [ ] Step 4: Commit `docs(prd),test: Spec C 契约同步与回归收口`
