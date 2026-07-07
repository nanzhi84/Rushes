# PR-A：单级草稿（Draft）模型后端化 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 Project/Case 两级领域模型合并为单级 Draft（草稿），全链路（PRD→contracts→storage→domain→tools→agent_harness→api→worker→web 机械适配→e2e）一次切换，合并即全绿可用。

**Architecture:** 保留现 Case 实体为 Draft 本体（state_version 乐观并发锚、对话/计划/时间线全在其上），Project 实体/事件/工具/端点整体退场；素材链接表改挂 draft 并做全局 reference_path 去重；无数据迁移（工作区库 0 行，删库重建）。

**Tech Stack:** Python 3.12 + FastAPI + SQLite（sqlalchemy core schema）/ Vite7 + React19 + TanStack Router/Query / pytest + vitest + Playwright。

**Spec:** `docs/superpowers/specs/2026-07-07-draft-model-ui-refactor-design.md`（本计划一切语义以它为准）

## Global Constraints

- 面向用户文案一律简体中文；PRD 是唯一实现依据，**T1 改 PRD 先于所有代码任务**。
- 代码标识符全量改名 `case_id → draft_id`，**不留 case/project 双轨术语**（含测试、golden、脚本、前端）。
- 覆盖率门槛 90%（pyproject `--cov-fail-under=90`）；`uv run pytest -q`、`uv run ruff check`、`uv run ruff format --check`、`uv run mypy`、`uv run python scripts/check_contracts.py` 全绿才算层收口。
- web 命令用 `npx -y pnpm@10.13.1 --dir apps/web <cmd>`；改 API 后必须跑 `bash scripts/gen_web_types.sh`。
- 导入决策不可破坏：唯一导入入口 = `POST /api/fs/pick` 原生选择框 + reference 零拷贝 + `rel_dir` 层级 + fs_roots 符号链接校验；**不引入任何 copy/上传路径**。
- 时间线保持只读；本 PR 不动工作台布局（布局重排属 PR-B）。
- 冗余/陈旧测试授权删除：凡覆盖「被删除能力」（project 生命周期、move/close、selected/disabled、link/unlink、uploads、project-tree）的测试**直接删**，不要改造保留。
- httpx 连国内 API 强制 IPv4 + `trust_env=False` 的既有约定不动。
- 提交信息中文、频繁小提交；分支 `feat/draft-model-refactor`。

## 命名映射总表（所有任务的公共接口契约）

**实体/表/字段**

| 旧 | 新 |
|---|---|
| `cases` 表 / `CaseState` / `contracts/case.py` / `repositories/cases.py` | `drafts` 表 / `DraftState` / `contracts/draft.py` / `repositories/drafts.py` |
| `projects` 表 / `ProjectState` / `contracts/project.py` / `repositories/projects.py` | **删除** |
| `project_asset_links`（PK (project_id,asset_id)，enabled/note/rel_dir） | `draft_asset_links`（PK `(draft_id, asset_id)`，仅 `note`/`rel_dir`，**无 enabled**） |
| 所有表/事件/payload 中 `case_id` | `draft_id` |
| `jobs.project_id` | **删列**；`jobs.case_id→draft_id`、`jobs.requested_by_case_id→requested_by_draft_id` |
| `memories.project_id` | **删列**；`created_from_case_id→created_from_draft_id`；`Memory.scope` 收敛 `Literal["user"]` |
| `decisions.project_id` | **删列**；`scope_type ∈ {"workspace","draft"}`（draft=strict+可阻塞，workspace=merge+非阻塞） |
| `event_log.project_id`+`event_log.case_id` 两列 | 单列 `draft_id`（索引同步重建） |
| `DomainEventBase.project_id`+`.case_id` | 单字段 `draft_id: str \| None` |
| `WorkspaceConfig.project_ids/project_refs`（`ProjectRef`） | `draft_ids/draft_refs`（`DraftRef`） |
| `ProjectDefaults`（aspect_ratio/fps/质量） | `DraftDefaults`，字段并入 `DraftState.defaults`，`POST /drafts` 时从 workspace defaults 拷贝 |
| `CaseState.project_id / selected_asset_ids / disabled_asset_ids` | **删字段** |

**事件（50 → 43，四处清单同步：`EVENT_CLASSES`/`EventName`/`EVENT_UNION`/`REDUCER_DISPATCH_EVENTS`）**

- 删除 7：`ProjectCreated / ProjectRenamed / ProjectTrashed / ProjectCopied / CaseMoved / CaseClosed / CaseAssetScopeChanged`
- 改名 4：`CaseCreated→DraftCreated`、`CaseRenamed→DraftRenamed`、`CaseCopied→DraftCopied`、`CaseTrashed→DraftTrashed`
- `AssetLinked / AssetUnlinked` 按 `(draft_id, asset_id)` 定键，payload `project_id→draft_id`，`AssetLinked` 携带 `rel_dir`/`note` 不变
- 其余事件 `case_id→draft_id`、`requested_by_case_id→requested_by_draft_id` 字面改名，strict/merge 语义一律不变

**工具（47 → 32，specs+handlers+`PRECONDITION_REGISTRY` 三处配对）**

- `project.*` 8 个整族删除（目录 `packages/tools/project/` 整个删）；**不新增** draft 生命周期工具（草稿建/改名/复制/删除仅 UI/REST）
- `asset.*` 10 → 3：保留 `asset.import_local_file`、`asset.import_url`（直挂当前草稿）；新 `asset.list_assets`（列当前草稿全部链接素材：asset_id/kind/rel_dir/usable/摘要有无，合并原 list_project_assets+list_case_scope）；删除 `link_to_project / unlink_from_project / list_project_assets / select_for_case / disable_for_case / list_case_scope / upload_complete`
- `ToolExecutionContext`：删 `project_state`，`case_state→draft_state`
- `ToolSpec`：`allowed_scopes` 全部收敛为 `["draft_editor"]`；`requires_active_project`/`requires_active_case` 双旗合并为 `requires_active_draft`；`side_effects` 字面量 `"project"/"case"` → `"draft"`
- 前置条件谓词：`active_case→active_draft`；`usable_asset_exists` 判定收敛为「链接存在且引用有效」（无 enabled/disabled 维度）

**REST（约 45 → 29）**

| 处置 | 端点 |
|---|---|
| 新形态 | `GET/POST /api/drafts`；`GET/PATCH/DELETE /api/drafts/{draft_id}`；`POST /api/drafts/{draft_id}/copy` |
| 扁平化（原 /projects/{pid}/cases/{cid}/…） | `messages` GET/POST、`events` SSE、`turn-stream` SSE、timeline 族、`previews/{id}/viewed`、`costs`、`decisions/current`、`decisions/pending` → 全部 `/api/drafts/{draft_id}/…` |
| 素材族（原挂 project） | `GET /api/drafts/{id}/materials`、`GET …/materials/{asset_id}/summary`、`POST …/materials/import-local\|import-url\|revalidate`、`DELETE …/materials/{asset_id}` |
| 删除 | `/api/project-tree`、`/api/uploads/init\|parts\|complete` 三条、`PATCH …/materials/{asset_id}`、materials `link`/`unlink` 两条 |
| 不动 | `/api/fs/*`、`/api/media/*`（query-token 双通道）、`/api/events`、`/api/decisions/{id}/answer`、jobs cancel |

**关键行为契约**

```python
# POST /api/drafts —— name 缺省时服务端生成剪映式日期名（本地时区，无前导零）
# 「7月7日」；同名已存在（不含 trashed）→「7月7日 (2)」「7月7日 (3)」…
# 响应 = 完整 draft 详情（draft_id/name/status/defaults/created_at/updated_at）

# GET /api/drafts 列表项（聚合封面，消 N+1）：
class DraftListItem(BaseModel):
    draft_id: str
    name: str
    status: str                    # active | trashed（列表默认只返回 active）
    updated_at: datetime
    material_count: int
    cover_asset_ids: list[str]     # ≤4 个 thumbnail_ready 素材，按导入时间倒序；前端自拼缩略图 URL

# import-local 全局去重（对每个展开后的文件）：
# 1) SELECT assets WHERE reference_path = <abs_path>  —— 全局查，不限草稿
# 2) 命中且本草稿已有链接 → 计入 duplicates，跳过
# 3) 命中且无链接 → 仅发 AssetLinked(draft_id, asset_id, rel_dir)；若该 asset 缺代理或索引产物
#    （历史失败），按现有规则补队缺失 job；正常情况不入任何队 —— 秒完成
# 4) 未命中 → 现状链路：AssetImported + AssetLinked + JobEnqueued(proxy)（proxy 完成后链式 index）
```

**任务依赖顺序**（导入分层决定，不可乱序）：T1 → T2 → T3 → T4 → T5 → T6 → T7 → T8 → T9 → T10 → T11 → T12。

---

### Task 1: PRD v2.0 改版

**Files:**
- Modify: `PRD.md`（v1.5 → v2.0，文首变更历史加行）

**Interfaces:**
- Produces: PRD v2.0 = 后续所有代码任务的语义权威；命名映射总表与 spec §2.3 的章节清单是改写依据。

- [ ] **Step 1: 按 spec §2.3 章节清单逐节改写**。全章重写：§0 优先级#2、§1 产品心智（单级草稿叙事：首页=草稿墙、开始创作=新草稿直进编辑器）、§2/§2.1–§2.5（两条路由 `/` 与 `/drafts/:draftId`；编辑器四区：左对话/中素材/右播放器/底部全宽只读时间线；素材面板合一模式）、§3.2 ER 图（DRAFTS/DRAFT_ASSET_LINKS，无 enabled）、§3.4（Draft 规则：建/改名/复制/删除仅 UI/REST；素材不用即删）、§3.6（memory user 单域+scratch）、§4.5 事件表 43 个与 SSE 路由（draft/workspace 两域，过滤逻辑只写一份）、§4.6（删 #4/#5）、§4.9/§4.10（job 的 draft_id/requested_by_draft_id）、§5.1 DraftState、§6.1 删除（工具总数 47→32）、§6.2 asset 三工具、§7.1 删除、§13.1 REST 29 条、§13.2、§14.4、§15 目录结构、§16.1、§17（M1/M2/M8/M9 验收以草稿语义重写）、§19.1、R8（预算挂 workspace）。
- [ ] **Step 2: 局部措辞节**按 spec §2.3「局部措辞修改」列表逐条替换挂靠对象；§13.4 只改挂靠措辞、机制一字不动但**删去分片上传 API 段落**（本 PR 裁撤）；§18「剪映草稿包」改「剪映工程文件导出」。
- [ ] **Step 3: 自检**：`grep -n "case_id\|project_id\|Case\|Project" PRD.md` 残留仅允许出现在变更历史行；全文「草稿」一词只指 Draft 实体。
- [ ] **Step 4: Commit** `docs：PRD v2.0 单级草稿模型改版`

### Task 2: contracts 层

**Files:**
- Modify: `packages/contracts/events.py`、`workspace.py`、`decision.py`、`memory.py`、`jobs.py`、`timeline.py`、`patch.py`、`costs.py`、`tool.py`、`interaction.py`、`asset.py`（凡含 case_id/project_id 处）
- Create: `packages/contracts/draft.py`（由 `case.py` 改造迁移）；Delete: `packages/contracts/case.py`、`packages/contracts/project.py`
- Test: `tests/contracts/`（同步改名断言；删除 project 专属测试）

**Interfaces:**
- Produces: `DraftState`（原 CaseState 字段 − project_id/selected_asset_ids/disabled_asset_ids ＋ `defaults: DraftDefaults`）；`DomainEventBase.draft_id: str | None`；43 事件注册表；`DecisionScopeType = Literal["workspace","draft"]`；`Memory.scope: Literal["user"]`。后续所有层 import 这些名字。

- [ ] **Step 1**: 按命名映射总表改写 contracts；`EVENT_CLASSES`/`EventName`/`EVENT_UNION` 三清单收敛 43。
- [ ] **Step 2**: 更新 `tests/contracts/` 与 `tests/test_contracts_import.py`；删除被删事件/实体的测试。
- [ ] **Step 3**: `uv run pytest tests/contracts tests/test_contracts_import.py -q` 绿（此时其余层预期红，不跑全量）。
- [ ] **Step 4: Commit** `refactor：contracts 单级草稿化（事件 43/DraftState/scope 收敛）`

### Task 3: storage + events 包

**Files:**
- Modify: `packages/storage/schema.py`、`data_migrations.py`（清空历史迁移，保留 `apply_data_migrations` 骨架与守卫式注释样板）、`db.py`、`workspace_paths.py`（如含 project 措辞）、`packages/storage/repositories/*.py`（cases.py→drafts.py，projects.py 删除，其余字段跟随）、`packages/events/`（event_log 写读路径 draft_id 单列）
- Test: `tests/storage/`、`tests/events/`

**Interfaces:**
- Consumes: Task 2 的 contracts 名字。
- Produces: `drafts`/`draft_asset_links` 表结构；`repositories/drafts.py` 的 CRUD 签名（原 cases.py 同形，字段改名）；`event_log(draft_id)` 单列路由基础。

- [ ] **Step 1**: schema 终态改写（删 projects；drafts；draft_asset_links `(draft_id,asset_id)` PK + note + rel_dir；各 FK 列改名；event_log 单 draft_id 列+索引；jobs/memories/decisions 列变更见总表）。
- [ ] **Step 2**: repositories 与 events 包字段跟随；`data_migrations.py` 清空为骨架。
- [ ] **Step 3**: `uv run pytest tests/storage tests/events -q` 绿。
- [ ] **Step 4: Commit** `refactor：storage/events 单级草稿 schema（删 projects，链接表挂 draft）`

### Task 4: domain 包

**Files:**
- Modify: `packages/domain/preconditions.py`（谓词更名 active_draft/usable 判定去 enabled 维度）、`decision_effects.py`（DraftState patch 锚）、其余含 case/project 措辞文件
- Test: `tests/domain/`

**Interfaces:**
- Consumes: Task 2/3。Produces: `PreconditionContext` 基于 draft_state 的谓词名（后续 tools 的 `requires_artifacts` 引用）。

- [ ] **Step 1**: 改写 + 删除 project 域 decision 分支。
- [ ] **Step 2**: `uv run pytest tests/domain -q` 绿。**Commit** `refactor：domain 谓词与决策效果草稿化`

### Task 5: tools 层

**Files:**
- Delete: `packages/tools/project/`（整目录）、asset 手册中 7 个被删工具的 spec+handler、`UploadCompleteRequest` 相关
- Modify: `packages/tools/specs.py`、`registry.py`、`context.py`（draft_state）、`packages/tools/asset/handlers.py`（import_local_file/import_url 改挂 draft + `list_assets` 新实现）、audio/content/timeline_tools/render_tools/memory_tools/understand 各包字段跟随（memory_tools 停止询问 scope，固定 user）
- Test: `tests/tools/`（删除被删工具测试；`asset.list_assets` 新测试）

**Interfaces:**
- Consumes: Task 2–4。Produces: 32 个 ToolSpec（`allowed_scopes=["draft_editor"]`、`requires_active_draft`）；`asset.list_assets` 返回 `[{asset_id, kind, rel_dir, usable, has_summary}]`。

- [ ] **Step 1**: 按总表裁撤与改名；`PRECONDITION_REGISTRY` 同步。
- [ ] **Step 2**: `uv run pytest tests/tools -q` 绿。**Commit** `refactor：工具族收敛 47→32（project 族退场，asset 三工具）`

### Task 6: agent_harness

**Files:**
- Modify: `packages/agent_harness/reducer.py`（删 project/move/close/scope apply 约 300 行；Draft 族 apply；AssetLinked 挂 draft；`REDUCER_DISPATCH_EVENTS`=43）、`state_validator.py`（删不变量 #4/#5；引用完整性锚 draft）、`loop.py`（约 25 处：memory owner 固定 user、url 导入哈希改 draft_id、decision replay 按 draft）、`policy_gate.py`（规则 2/3/7 草稿化）及包内其余文件
- Test: `tests/agent_harness/`、`tests/golden/`（framework.py + test_cases.py 固定序列全量改名；删除 move/close/scope 用例）

**Interfaces:**
- Consumes: Task 2–5。Produces: reducer 对 43 事件的 dispatch；strict 预检按 `drafts.state_version` 不变。

- [ ] **Step 1**: reducer/state_validator/loop/policy_gate 改写。
- [ ] **Step 2**: golden 固定轨迹重铸（事件名/字段名批量替换 + 删除死场景）。
- [ ] **Step 3**: `uv run pytest tests/agent_harness tests/golden -q` 绿。**Commit** `refactor：agent_harness 单级草稿化（reducer 43 事件）`

### Task 7: apps/api

**Files:**
- Modify: `apps/api/main.py`（路由全集按 REST 总表重排；`_job_observation_bridge` 字段跟随；SSE 两条挂 draft）、`schemas.py`（DraftListItem/DraftDetail/默认日期名逻辑）、`turn_stream.py`、`deps.py`；`scripts/e2e_paths/*.py`（API 路径跟随）
- Test: `tests/api/`（`test_m1_project_case.py` 重写为 `test_m1_drafts.py`；删 uploads/project-tree 测试；新增：全局去重两场景、日期名生成、列表聚合封面）

**Interfaces:**
- Consumes: Task 2–6。Produces: OpenAPI 终态（Task 10 依赖 `gen_web_types.sh` 产物）。

- [ ] **Step 1**: 路由/schemas 改写；实现 `POST /drafts` 日期名与 `GET /drafts` 聚合封面（单条 SQL join thumbnail-ready 素材，禁止 per-draft 循环查询）；import-local 全局去重按「关键行为契约」实现。
- [ ] **Step 2**: 新测试：①同一文件第二草稿导入 → 秒链接、0 新 job、duplicates 语义正确；②同草稿重复导入 → duplicates；③「7月7日」与「7月7日 (2)」；④列表 cover_asset_ids ≤4 且倒序。
- [ ] **Step 3**: `uv run pytest tests/api -q` 绿。**Commit** `refactor：API 扁平化 /api/drafts + 全局去重导入 + 聚合封面`

### Task 8: apps/worker

**Files:**
- Modify: `apps/worker/job_runner.py`、`index_jobs.py`、`media_jobs.py`、`audio_jobs.py`、`render_jobs.py`、`heartbeat.py`、`job_registry.py`（字段跟随：draft_id/requested_by_draft_id，删 project_id 透传）
- Test: `tests/worker/`

- [ ] **Step 1**: 字段跟随改名。`uv run pytest tests/worker -q` 绿。**Commit** `refactor：worker 字段跟随草稿化`

### Task 9: 后端收口

**Files:**
- Modify: `scripts/check_contracts.py`（事件/工具注册表断言、允许导入表中 project 相关残留、工具总数 32）、`tests/scripts/`、`tests/apps/`；删除空壳死目录 `packages/annotation`、`packages/indexing`、`packages/tools/annotation`、`packages/tools/retrieval`（确认无 import 后）
- Verify: 全量后端门禁

- [ ] **Step 1**: check_contracts 与残余测试修绿；全仓 `grep -rn "case_id\|project_id" packages apps tests scripts --include="*.py"` 清零（golden 数据内允许的除外——原则上也应清零）。
- [ ] **Step 2**: `uv run pytest -q`（覆盖率≥90）+ `uv run ruff check` + `uv run ruff format --check` + `uv run mypy` + `uv run python scripts/check_contracts.py` 全绿。
- [ ] **Step 3: Commit** `chore：后端收口（check_contracts 对齐 43 事件/32 工具，清死目录）`

### Task 10: web 机械适配

**Files:**
- Run: `bash scripts/gen_web_types.sh`
- Modify: `apps/web/src/api/client.ts`（draftPath 单参；删 uploads/link/unlink/patch-material；手写类型与 generated 逐一核对，能引 generated 的删手写）、`app/router.tsx`（`/` 与 `/drafts/$draftId` 两条）、`app/query_client.ts`（queryKeys 单 ID 形状）、`state/ui_store.ts`（EntityDialogKind → renameDraft/copyDraft/deleteDraft）、`app/use_workspace_events.ts`
- Create: `apps/web/src/api/event_types.ts`（**SSE 事件名单一常量来源**：DRAFT_EVENT_TYPES / WORKSPACE_EVENT_TYPES / MATERIAL_EVENT_TYPES 全部从此导出）；`routes/DraftsHome.tsx`（由 ProjectsOverview 改造：列 drafts、卡片封面用 cover_asset_ids 拼缩略图、「开始创作」按钮=createDraft→navigate、卡片菜单 重命名/复制/删除）；`routes/DraftEditor.tsx`（由 CaseAgentConsole 改造：单 draftId、布局不动、AssetsPanel 开 management 能力保住重定位/删引用/摘要，**去掉禁用菜单项**、顶栏返回文案「草稿」）
- Delete: `routes/ProjectsOverview.tsx`、`ProjectDetailPage.tsx`、`ProjectMaterialsPage.tsx`、`CaseAgentConsole.tsx` 及对应 test；`components/Materials/` 内禁用相关 UI；EntityActionDialog 收敛
- Test: 对应 `.test.tsx` 重写（DraftsHome/DraftEditor）；`auth.test.tsx`、`client.test.ts` 跟随

- [ ] **Step 1**: 生成类型 → client → 常量 → store → router → 页面 → 测试，顺序推进。
- [ ] **Step 2**: `npx -y pnpm@10.13.1 --dir apps/web typecheck && npx -y pnpm@10.13.1 --dir apps/web test -- --run && npx -y pnpm@10.13.1 --dir apps/web build` 全绿。
- [ ] **Step 3: Commit** `refactor：web 机械适配单级草稿（两条路由/单 ID/SSE 常量单源）`

### Task 11: e2e 与真机冒烟

**Files:**
- Modify: `e2e/` 两条 Playwright spec（路由/选择器/文案跟随草稿模型）
- Verify: 本地起 api+worker+web

- [ ] **Step 1**: `pnpm --dir e2e exec playwright test` 全绿。
- [ ] **Step 2**: 删除本地 `.rushes` 重建；脚本冒烟：POST /drafts → fs 导入文件夹（用 ~/MyVideo 素材）→ 等 index job 完成 → 建第二草稿导入同一文件夹 → 断言秒完成且 0 新 job。
- [ ] **Step 3: Commit** `test：e2e 对齐草稿模型`

### Task 12: PR / review / CI / merge（编排者执行）

- [ ] Push 分支 → `gh pr create`（PR 描述含 spec/计划链接与验收清单）→ 触发 CI。
- [ ] 运行 code-review（多代理），修复确认项。
- [ ] CI 全绿 + review 清零 → squash merge → 删远程分支 → 真机验证窗口（run_path1/2/scenery 真实 LLM 路径）列入交付后跟进。

---

## Self-Review 记录

- Spec 覆盖：§1（T2/T3/T6）、§2.1 REST（T7）、§2.2 工具（T5）、§2.3 PRD（T1）、§3.1 路由与 §3.3 SSE 单源（T10）、§5 PR-A 验收（T9/T11）、风险节的 SSE 单源与手写类型核对（T10）均有任务落点；§3.2/§3.3 其余项与 §4 属 PR-B/PR-C，不在本计划。
- 无 TBD/占位；类型与命名以「命名映射总表”为单一来源，各任务不得自造名。
