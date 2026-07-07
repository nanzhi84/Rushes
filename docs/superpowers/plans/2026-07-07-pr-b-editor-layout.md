# PR-B：编辑器结构重排（全宽时间线 + 中栏可拖宽 + 顶栏成本小计）实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 编辑器四区布局对齐剪映（左对话/中素材/右播放器均可控宽，底部**全宽通栏**只读时间线可拖高），设置入口彻底移出编辑器，顶栏接入本草稿成本小计。

**Architecture:** 纯前端（apps/web）结构重排，后端零改动；样式令牌沿用现深色体系，质感升级留给 PR-C。

**Tech Stack:** React 19 + zustand + Tailwind v4（现有令牌）；不引入新依赖（radix/lucide 属 PR-C）。

**Spec:** `docs/superpowers/specs/2026-07-07-draft-model-ui-refactor-design.md` §3.2/§3.3/§5 PR-B 行。

## Global Constraints

- 时间线保持只读；不动 TimelineViewer 内部渲染逻辑，只动容器排布。
- 不做视觉质感升级（图标/弹窗组件库/播放器控制条是 PR-C）；新 UI 文案一律简体中文。
- 门禁：`npx -y pnpm@10.13.1 --dir apps/web typecheck && … test -- --run && … build` 三绿；`pnpm --dir e2e exec playwright test` 两绿（先删本地 `.rushes` 保证干净工作区）。
- 分支 `feat/draft-editor-layout`（已建，基于 da000e7）。

---

### Task 1: 编辑器 flex 重排 + 中栏可拖宽

**Files:**
- Modify: `apps/web/src/routes/DraftEditor.tsx`（338-560 布局区）、`apps/web/src/state/ui_store.ts`、`apps/web/src/routes/DraftEditor.test.tsx`

**现状 → 目标**（DraftEditor.tsx 现结构行号）：
```
现：338 纵向[TopBar / 373 横向[聊天(通高,拖宽) | 446 右列[447 上排[素材 w-300 写死 | 预览] / 473 拖高手柄 / 481 时间线(只跨中+右)]]]
目：纵向[TopBar / 三栏行 flex-1[聊天(拖宽) |手柄| 素材(拖宽,新增) |手柄| 预览 flex-1] / 拖高手柄(全宽) / 时间线(全宽, height=timelinePanelHeight)]
```

- [ ] **Step 1**: `ui_store.ts` 增 `materialsPanelWidth`（默认 300，钳制 240–480，localStorage 键 `rushes.materialsPanelWidth`，完全照抄 chatPanelWidth 的持久化/钳制模式）与 `setMaterialsPanelWidth`。
- [ ] **Step 2**: DraftEditor 重排：时间线 section 与其上方的 ResizeHandle（horizontal invert）提升到最外层纵向容器直属子级（TopBar 平级），全宽；原「右列」壳删除；素材 div 宽度改 `style={{ width: materialsPanelWidth }}` 并在素材/预览之间插入 `<ResizeHandle orientation="vertical" value={materialsPanelWidth} onChange=…/>`；聊天 aside 不再通高（自然被三栏行约束）。保留全部现有联动（playhead/seek/clip 点击滚动聊天锚点/ClipDetailBar/试看）。
- [ ] **Step 3**: 测试更新：DraftEditor.test.tsx 补「时间线容器是根纵向布局的直属子级（全宽）」与「素材列拖宽手柄存在、store 值变化生效」两断言；跑 `… test -- --run src/routes/DraftEditor.test.tsx` 绿。
- [ ] **Step 4**: Commit `feat：编辑器全宽时间线 + 素材列可拖宽`。

### Task 2: 顶栏成本小计 + 设置移出编辑器 + 草稿墙设置弹窗

**Files:**
- Modify: `apps/web/src/routes/DraftEditor.tsx`（TopBar trailing）、`apps/web/src/components/Shell/TopBar.tsx`（设置按钮可隐藏/可接管点击）、`apps/web/src/routes/DraftsHome.tsx`、`apps/web/src/routes/DraftsHome.test.tsx`、`apps/web/src/routes/DraftEditor.test.tsx`
- Create: `apps/web/src/components/Shell/WorkspaceSettingsDialog.tsx`

**Interfaces:**
- Consumes: `api.draftCosts(draftId)`（PR-A 已有，`queryKeys.costs(draftId)`）。
- Produces: `WorkspaceSettingsDialog({open, onClose})`——占位内容：「全局默认值」区（画幅/帧率/质量的当前 workspace 默认展示，若无只读接口则静态描述当前默认）与「成本汇总」区（占位文案「后续接入」），样式沿用 EntityActionDialog 的手搓弹窗模式（radix 在 PR-C 统一替换）。

- [ ] **Step 1**: TopBar 组件把内置设置按钮改为可控（如 `showSettings`/`onSettingsClick` props，缺省保持现状不破坏其它调用方）。
- [ ] **Step 2**: DraftEditor 顶栏：隐藏设置按钮；trailing 区在「导出」按钮旁加成本小计徽标（`draftCosts` 数据，格式「¥x.xxxx」或后端返回的币种字段；TurnEnded SSE 到达时失效 `queryKeys.costs(draftId)`——挂进现有 DRAFT_EVENT_TYPES 失效逻辑）。
- [ ] **Step 3**: DraftsHome：齿轮点击打开 `WorkspaceSettingsDialog`。
- [ ] **Step 4**: 测试：DraftsHome.test 补设置弹窗开合；DraftEditor.test 补成本小计渲染（mock costs 响应）与「编辑器无设置按钮」断言。全部 web test 绿。
- [ ] **Step 5**: Commit `feat：顶栏成本小计接入 + 设置移出编辑器`。

### Task 3: 门禁 + e2e + PR

- [ ] **Step 1**: web 三门禁全绿；`rm -rf .rushes` 后 `pnpm --dir e2e exec playwright test` 两绿（布局重排若移动了 e2e 选择器锚点，机械修 spec 选择器，不加用例）。
- [ ] **Step 2**: 推分支、`gh pr create`（标题「编辑器结构重排：全宽时间线 + 中栏可拖宽 + 成本小计（PR-B）」，正文含 spec 链接与验收证据）。
- [ ] **Step 3**: review + CI 绿 → squash merge（编排者执行）。

## Self-Review 记录

- spec §3.2（设置弹窗占位/开始创作直进——后者 PR-A 已落）、§3.3（四区图/中栏拖宽/管理合一——管理能力 PR-A 已并入中栏，本 PR 只重排）逐条有落点；§4 质感项明确不做。
- 无 TBD；ResizeHandle/持久化模式复用现有实现，无新类型引入。
