# 设计：单级草稿（Draft）模型 + 剪映式编辑器 UI 重构

- 日期：2026-07-07
- 状态：已与用户逐项确认（重构深度 / 素材复用 / 时间线布局 / 视觉质感四项均选推荐方案；五节设计整体通过）
- 前置事实：工作区 `.rushes/rushes.db` 全部业务表 0 行（2026-07-06 重建），**无数据迁移负担，直接删库重建**。

## 0. 目标与非目标

**目标**

1. 抛弃 Project/Case 两级结构，后端领域模型真合并为单级 **Draft（草稿）**；对齐剪映心智：首页=草稿墙，「开始创作」=新建草稿直进编辑器。
2. 素材文件管理重构：素材导入挂草稿下（含文件夹层级 rel_dir），**全局按 reference_path 去重**——同一文件在第二个草稿导入时秒级建链，复用已有代理/缩略图/分镜/波形/理解摘要。
3. 编辑器布局对齐剪映：左 AI 对话（可拖宽）/ 中素材面板（可拖宽，新增）/ 右播放器（弹性）/ **底部全宽通栏时间线**（上下拖高）。
4. 全面视觉质感升级：令牌补质感层、lucide-react 图标、播放器真控制条、时间线缩略图条+波形、radix-ui 统一弹窗菜单。

**非目标**

- 时间线仍**只读**（编辑一律走对话 → TimelinePatch，PRD §14.1/§18 约束原样保留；本次只动排布与质感，不加 clip 拖拽/裁剪把手）。
- 不做浅色主题、不做回收站页面（删除=trash，数据保留，恢复入口后续再议）。
- 不做剪映工程文件（原「剪映草稿包」非目标）导出；PRD 中该条措辞改为「剪映工程文件导出」以免与本设计的「草稿」撞名。
- 不改导入决策：唯一入口仍是 `POST /api/fs/pick` 原生选择框 + reference 零拷贝原地索引，**不引入任何 copy/上传路径**（PR #44 用户定稿）。

## 1. 领域模型

### 1.1 Draft 实体（Case 升格，Project 消失）

- `CaseState` 改造为 `DraftState`：保留 state_version（乐观并发锚）、pending_decision_id、running_jobs、brief、四计划、timeline 版本链、preview/export 引用、scratch_memory；**删除** `project_id`、`selected_asset_ids`、`disabled_asset_ids`；**并入** 原 ProjectState 的 `defaults`（aspect_ratio/fps/质量，创建时从 workspace defaults 继承拷贝）。
- `ProjectState`、`contracts/project.py` 删除；`WorkspaceConfig.project_refs` → `draft_refs`。
- `DomainEventBase` 的 `project_id`/`case_id` 两字段收敛为单一 `draft_id`；`event_log` 同步为单列。SSE 路由谓词按 draft_id/workspace 两域推导（过滤逻辑仍只写一份）。

### 1.2 事件（51 → 44）

> 修正（2026-07-07 验收核数）：现状事件实为 51 个（含 M0 安全基线后加的 SecurityRefusal，此前侦察少算），删 7 改名 4 后为 44。

| 处置 | 事件 |
|---|---|
| 删除（6） | ProjectCreated / ProjectRenamed / ProjectTrashed / ProjectCopied / CaseMoved / CaseClosed |
| 改名（4） | CaseCreated→DraftCreated、CaseRenamed→DraftRenamed、CaseCopied→DraftCopied、CaseTrashed→DraftTrashed |
| 删除（1） | CaseAssetScopeChanged（选用/禁用概念整体取消，见 1.3，不新增任何替代事件） |
| 改锚 | AssetLinked / AssetUnlinked 按 `(draft_id, asset_id)` 定键；其余 strict/merge 族把 case_id 字面改 draft_id，语义不变 |

草稿只有「存在 / trash」两态（剪映式），无 closed。`EVENT_CLASSES`/`EventName`/`EVENT_UNION`/`REDUCER_DISPATCH_EVENTS` 四处清单同步，check_contracts 卡。

### 1.3 素材：全局资产 + 草稿链接，不用即删

- `assets` 表保持全局（一个物理文件一行）。`project_asset_links` → `draft_asset_links`，复合主键 `(draft_id, asset_id)`，保留 `note`、`rel_dir`（文件夹层级）；**`enabled` 列取消**。「删除素材=断链不删物理文件」语义不变。
- **全局去重**：import-local 按 `reference_path` 查全局 `assets`（不再限定单项目内）：
  - 命中 → 只发 `AssetLinked`（秒完成，不入队任何 job）；若该 asset 缺代理/索引（历史失败），照常补队。
  - 未命中 → `AssetImported` + `AssetLinked` + proxy→index job 链（现状管线，scope 无感）。
  - `hash` 列保留，暂不参与去重（不扩 scope）。
- **不用即删，无禁用开关**：「不参与剪辑」没有独立状态——不想用某素材就直接删除引用（断链，`AssetUnlinked`）；物理文件与全局索引/摘要保留，之后重导同一文件或文件夹秒级回链。原 case 级 selected/disabled 两层、StateValidator 不变量 #4/#5（case 选用 ⊆ project 池）随之删除；前置条件谓词的 usable 判定收敛为「链接存在且引用有效」，不再有 enabled 维度。
- `revalidate`（失效重检）与素材重定位流程保留，改挂 draft。

### 1.4 Scope 收敛

- **Decision**：`scope_type ∈ {workspace, draft}`。draft 域=strict+可阻塞（原 case 语义）；workspace 域=merge+非阻塞（原语义）。project 域及其校验器分支删除。
- **Memory**：`scope` 收敛为 `user` 单域（原 project 级「品牌/系列偏好」由 user 域承接）；`memories.project_id` 删除、`created_from_case_id` → `created_from_draft_id`；`MemoryCandidate.case_id` → `draft_id`；memory 工具的 scope 询问相应简化。草稿内 `scratch_memory` 保留。
- **Job**：`jobs` 表 `project_id` 删除，`case_id`/`requested_by_case_id` → `draft_id`/`requested_by_draft_id`。proxy/index job 本质挂 asset，`draft_id` 仅记录发起草稿用于 SSE 路由与 observation 桥（`_job_observation_bridge` 逻辑不变，字段跟随）。
- **成本**：`provider_calls`/`agent_traces` 挂 draft；per-draft 小计（`GET /api/drafts/{id}/costs`，编辑器顶栏展示，本次接入）+ 全局汇总进首页设置；R8 预算暂停挂 workspace。

### 1.5 存储与迁移

- `schema.py` 直接写终态：删 `projects` 表，`cases` → `drafts`，各表外键列 `case_id` → `draft_id`（**代码标识符全量改名，不留 case 双轨术语**，与仓库「术语精确化」风格一致）。
- **不写数据迁移**：`data_migrations.py` 清空历史迁移、保留机制骨架；本地 `.rushes` 目录删除重建（dev 环境唯一存量，0 行）。

## 2. 后端 API 与工具

### 2.1 REST（约 45 条 → 约 29 条）

- 草稿族：`GET/POST /api/drafts`、`GET/PATCH/DELETE /api/drafts/{id}`、`POST /api/drafts/{id}/copy`。
  - `GET /api/drafts` 列表项内嵌 `cover_thumbnails`（前 4 个 thumbnail_ready 素材的缩略图 URL）+ 素材数/更新时间，**消掉首页每卡一请求的 N+1**。
  - `POST /api/drafts` 默认名剪映式日期名（「7月7日」，重名追加序号），请求体可带 name/goal。
- 素材族（挂 draft）：`GET materials`、`GET materials/{asset_id}/summary`、`POST materials/import-local|import-url|revalidate`、`DELETE materials/{asset_id}`。原 link/unlink 两条 REST 删除（import 即链接、DELETE 即断链）；`PATCH materials/{asset_id}` 随 enabled 取消一并删除。
- 会话/产物族：messages GET/POST、`events` SSE、`turn-stream` SSE、timeline、previews viewed、costs、decisions current/pending → 全部 `/api/drafts/{id}/…` 扁平化。
- 删除：`/api/project-tree`、`/api/uploads/*` 三条（分片上传自 PR #44 后无任何 UI 调用方，连同 `asset.upload_complete` 工具与 UploadCompleteRequest 一起裁撤）。
- 保留不动：`/api/fs/*`（pick/roots/list）、`/api/media/*`（query-token 鉴权双通道）、`/api/events`、`/api/decisions/{id}/answer`、jobs cancel。
- 改完重跑 `scripts/gen_web_types.sh`，并把 `api/client.ts` 中手写类型与 generated schema 同步核对。

### 2.2 工具（注册 46 → 31；PRD 契约面 47 → 32）

> 修正（2026-07-07 验收核数，二次修订）：注册目标 31 = 46 − 8（project 族）− 8（asset 七个域工具 + read_summary）+ 1（asset.list_assets）。`asset.read_summary` 并入 `understand.materials` 的缓存命中路径（该路径已存在：cached 状态直接返回摘要全文，不派 VLM 子代理）；`memory.ask_scope` **保留**——记忆保存确认流程需要它创建 memory_scope decision，单域后只问「存为 user 记忆/跳过」；`memory.extract_from_case` 改名 `memory.extract_from_draft`。PRD §6 的「共 32」是契约面计数（含 content.extract_transcript_plan、timeline.insert_clip 两个规划未实现工具，不含子代理内部的 media.view_frames），与 v1.5「47 vs 注册 46」同构，属既有惯例非漂移。

- `project.*` 8 个工具**整族删除**，不新增 draft 生命周期工具——沿用 PRD 硬规则：草稿的建/改名/复制/删除仅 UI/REST，Agent 无权（防误删）。
- `asset.*` 10 → 2＋新 1：保留 `import_local_file`、`import_url`（直挂当前草稿）+ 新形态 `list_assets`（列本草稿链接素材）；删除 link_to_project/unlink_from_project/list_project_assets/select_for_case/disable_for_case/list_case_scope/upload_complete/**read_summary**（摘要读取由 `understand.materials` 缓存命中路径承接）。Agent 不需要排除素材的工具——不用某素材就是不在计划里引用它；用户要排除则在 UI 删除引用。
- `ToolSpec`：`allowed_scopes` 收敛为 `draft_editor` 单值；`requires_active_project`/`requires_active_case` 双旗合并为 `requires_active_draft`；`side_effects` 字面量同步。`PRECONDITION_REGISTRY` 谓词把 case/project 措辞换 draft，逻辑基本不动。
- 其余 audio/content/timeline/render/memory/understand 工具族只做锚字段改名。

### 2.3 PRD 改版（先于代码）

PRD 升 **v2.0**，变更历史加行。按侦察清单执行：§0/§1/§2 全章、§2.1–§2.5、§3.2 ER 图、§3.4（Draft 规则重写）、§3.6（memory 单域）、§4.5 事件表与 SSE 路由、§4.6 #4/#5、§4.9/§4.10 job scope、§5.1 DraftState、§6.1/§6.2 工具族、§7.1 删除、§7.6/§7.9、§8.2、§11、§12 规则 2/3/7、§13.1/§13.2、§14.4、§15 目录、§16.1、§17（M1/M2/M8/M9 验收重写）、§19.1、R8；§13.4 导入机制只改挂靠措辞；§18「剪映草稿包」改名「剪映工程文件导出」。工具总数与 check_contracts 断言联动更新。

## 3. 前端信息架构与编辑器

### 3.1 路由（4 → 2）

| 路径 | 页面 |
|---|---|
| `/` | DraftsHome 草稿墙 |
| `/drafts/$draftId` | DraftEditor 编辑器 |

ProjectDetailPage、ProjectMaterialsPage 及 materials redirect 删除。

### 3.2 草稿墙

- 顶栏：Rushes 字标 + 连接状态 + 右上齿轮 → 全局设置弹窗（设置彻底移出编辑器；内容维持现状占位水平——workspace 默认值展示 + 成本汇总占位，本次不扩展设置功能本身）。
- 主区：「开始创作」主按钮（点击 = `POST /drafts` → 直接 navigate 进编辑器，不弹表单）+ 草稿卡片网格。
- DraftCard：2×2 缩略图拼贴封面（数据来自列表接口 cover_thumbnails，搬现 ProjectCard 拼贴布局）、名称、更新时间 + 素材数；hover「⋯」与右键 → ContextMenu（重命名/复制/删除）。空态大虚线框。
- EntityActionDialog 收敛为 renameDraft/copyDraft/deleteDraft 三种（radix Dialog 重做，删除保留勾选确认）。

### 3.3 编辑器（DraftEditor，改造自 CaseAgentConsole）

```
┌────────────────────────────────────────────────────┐
│ TopBar: ←草稿墙  草稿名(内联改名)     成本小计 · 导出 │
├─────────────┬─────────────────┬────────────────────┤
│ AI 对话      ‖ 素材面板         ‖ 播放器              │
│ chatWidth   ‖ materialsWidth  ‖ (flex-1)           │
│ (拖,已有)    ‖ (拖,新增)        ‖                    │
├─────────────┴─────────────────┴────────────────────┤
│ 时间线 · 全宽通栏  timelineHeight (上下拖,已有)        │
└────────────────────────────────────────────────────┘
```

- **flex 重排**：外层改纵向三段（TopBar / 三栏行 / 时间线），时间线提出到与三栏行同级 → 全宽通栏，左聊天不再通高。
- **中栏可拖宽**：`ui_store` 新增 `materialsPanelWidth`（localStorage 持久化 + 钳制，同 chatPanelWidth 模式），中/右之间加 ResizeHandle。
- **素材面板合一模式**：AssetsPanel 取消 management/工作台双模式——中栏即完全体：导入（文件/文件夹，原生选择框）、rel_dir 文件夹下钻 + 面包屑、瓦片右键 ContextMenu（重新定位、删除引用、查看理解摘要）、摘要抽屉与 FsBrowserDialog（重定位）保留于编辑器内。点瓦片 → 右侧播放器试看（现有联动）。
- **顶栏**：左「←」返回草稿墙 + 草稿名内联改名；右侧本草稿成本小计（接 `GET costs`，把 PRD「后续接入」落掉）+「导出」按钮（沿用发固定话术走对话）。设置按钮从编辑器移除。
- 预览↔时间线联动（playhead 双向、clip 点击滚动聊天锚点、ClipDetailBar）原样保留。
- 三处 SSE 事件名硬编码列表（CASE_EVENT_TYPES 31 / WORKSPACE_EVENT_TYPES 15 / MATERIAL_EVENT_TYPES 17）随事件改名同步重建，并在 PR-A 里收敛为单一常量来源，避免三处漂移。

## 4. 视觉系统升级

1. **令牌补质感层**（index.css `@theme`）：radius 体系（sm/md/lg/xl 语义档）、elevation 阴影 3 档、边框双层次、hover/active/focus 态令牌、动效时长与缓动令牌；沿用 ink 深色 + accent 橙体系，但把 accent 职责减负（用户气泡改低饱和底、accent 只留主按钮/播放头/选中/focus）；清掉 TimelineViewer 内两处硬编码色值（#3d3d47/#ff5c38），字号档收敛（清理散落的 text-[10px]/text-[11px] 任意值）。
2. **图标**：引入 `lucide-react`，全仓 emoji/文字字符图标（⋯ ＋ − ✓ ✗ •）与 4 个手写 glyph 全部替换。
3. **播放器**：基于 vidstack 组件补全控制条——可拖进度条（scrub）、播放/暂停、±1 帧、时间码、音量、全屏；素材试看与成片预览共用同一控制条形态。
4. **时间线**：视频 clip 内平铺素材缩略图条（filmstrip，有分镜帧按时间取帧、否则重复 poster 缩略图）；音频 clip 内嵌波形（peaks 数据已有）；轨道头图标化；播放头/刻度尺细节打磨。只读约束不破。
5. **弹窗/菜单**：引入 `@radix-ui`（Dialog / DropdownMenu / ContextMenu），废掉 `window.confirm` 与手搓 hover 弹层，统一深色令牌皮肤。
6. 顺手清理：删除 `@assistant-ui/react` 死依赖。

## 5. 交付切分（三个 PR 串行，每个合并即全绿）

| PR | 内容 | 验收 |
|---|---|---|
| **PR-A 后端 draft 化** | PRD v2.0 → contracts/storage/reducer/state_validator/tools/domain/api/worker 全链改名与裁撤 + 全局去重导入 + drafts 列表聚合封面 + gen_web_types + **前端机械适配**（路由压平为两条、双 ID→draftId、页面沿用现样式最小改动）+ 后端/前端测试、golden 回放、e2e 两条 spec 修绿 | pytest（覆盖率≥90%）/ ruff / mypy / check_contracts / web typecheck+test+build / e2e 全绿；手工冒烟：建草稿→导入文件夹→索引→第二草稿重复导入秒完成 |
| **PR-B 编辑器结构** | 草稿墙（开始创作直进、DraftCard、设置入口）+ 编辑器 flex 重排（全宽时间线、中栏可拖宽、素材管理合一、顶栏成本小计/导出）+ EntityActionDialog 收敛 | web 测试更新 + e2e 补「开始创作→导入→对话」路径；布局手工核对对齐目标图 |
| **PR-C 视觉质感** | 令牌质感层 + lucide 图标 + 播放器控制条 + 时间线 filmstrip/波形 + radix 弹窗菜单 + 死依赖清理 | 视觉走查（截图对照）；无功能回归（现有测试全绿） |

**测试推进顺序**（PR-A 内）：contracts → storage → reducer → tools → api → web → e2e，逐层修绿再进下一层；约 55 个后端测试文件与 golden 回放受波及，属机械改名为主。

## 6. 风险与对策

- **改动面大、半成品状态无法过 CI**（check_contracts 三重一致性门）：PR-A 内按层原子推进，每层跑 check_contracts 定位漂移。
- **SSE 事件名三处硬编码静默失灵**：PR-A 收敛为单一常量来源 + e2e 里保「导入后素材列表自动刷新」断言。
- **api/client.ts 手写类型与 generated schema 双来源漏改**：PR-A 时逐端点核对，凡 generated 已覆盖的手写类型删除改引 generated。
- **全局去重的边界**：同一路径文件被外部替换内容后二次导入会复用旧索引——revalidate 机制可救；hash 去重留后续（已记非目标）。
- **e2e 真实 LLM 路径（scripts/e2e_paths run_path1/2/scenery）在 07-06 重构后仍未跑过**：本次 PR-A 合并后属最佳真机验证窗口，列为交付后跟进项。
