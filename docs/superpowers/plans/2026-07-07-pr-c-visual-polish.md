# PR-C：视觉质感升级 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在既有深色 ink/橙 accent 体系上全面补质感：令牌质感层、lucide 图标、播放器真控制条、时间线 filmstrip/波形、radix 统一弹窗菜单——解决「界面像线框稿」的病根。

**Architecture:** 纯 apps/web；新增依赖 lucide-react + @radix-ui/react-dialog + @radix-ui/react-dropdown-menu + @radix-ui/react-context-menu（且仅这四个）。方向是**精修不改向**：PR #42 用户选定的深色剪辑器风格不动，只补层次与细节。

**Spec:** `docs/superpowers/specs/2026-07-07-draft-model-ui-refactor-design.md` §4。

## Global Constraints

- 时间线保持只读（不加编辑交互）；组件不写裸色值（一切经 index.css `@theme` 令牌）；文案简体中文。
- **不加后端端点**：filmstrip 用现有 `/api/media/{aid}/thumbnail` 重复平铺（无分镜帧端点，poster 重复即可）；波形用现有 peaks 数据链路。
- 门禁：web typecheck/test/build 三绿 + e2e 两绿（先删 `.rushes`）+ 门禁代理产出草稿墙/编辑器两张全页截图供编排者目检。
- 病根清单（来自侦察，逐条对账）：①令牌无 radius/shadow/层次体系 ②图标全 emoji/文字字符 ③accent 橙过载（用户气泡整块橙底）④播放器无进度条 ⑤时间线 clip 纯色矩形 ⑥手搓 hover 菜单 + window.confirm ⑦text-[10px]/[11px] 任意值散落 ⑧（字体不动，系统回落可接受）。
- 分支 `feat/visual-polish`（基于 c0418dd）。

---

### Task 1: 令牌质感层 + 依赖引入（地基，其余任务的前置）

**Files:** `apps/web/src/index.css`、`apps/web/package.json`（4 个新依赖）、全局字号档清理波及的组件

- [ ] `@theme` 补：radius 语义档（--radius-sm/md/lg/xl）、elevation 阴影 3 档（--shadow-raised/overlay/pop，深色下用黑色系半透明+细描边组合）、hover/active/focus 态令牌、动效时长与缓动（--ease-out-snappy 等）；accent 减负——用户气泡改低饱和深底（如 accent 10-15% 混 ink），accent 纯色只留主按钮/播放头/选中描边/focus ring。
- [ ] 面板层次：panel/raised 之上用 shadow+border 双层次替代「1px 描边平色」单一手法（关键容器：卡片、弹窗、面板头、时间线轨道）。
- [ ] 清 TimelineViewer 两处硬编码色值（#3d3d47/#ff5c38 → 令牌 CSS 变量）；全仓 text-[10px]/text-[11px] 收敛到 text-xs 或统一的 --text-2xs 档。
- [ ] 安装 lucide-react + radix 三包（npx -y pnpm@10.13.1 --dir apps/web add …）。
- [ ] typecheck + test 绿后 Commit `feat：设计令牌质感层（radius/elevation/动效）+ 视觉依赖`。

### Task 2a: radix 弹窗与菜单统一（可与 2b/2c 并行）

**Files:** `components/Shell/EntityActionDialog.tsx`、`WorkspaceSettingsDialog.tsx`、`routes/DraftsHome.tsx`（卡片菜单）、`components/Materials/AssetsPanel.tsx`（瓦片菜单→ContextMenu+DropdownMenu 双入口）、`FsBrowserDialog.tsx`、相关 tests

- [ ] Dialog：两个手搓弹窗迁 @radix-ui/react-dialog（overlay 模糊/淡入、内容 --shadow-overlay、Esc/点外关闭语义由 radix 接管；dirty 守卫与删除勾选确认逻辑原样保留）。
- [ ] 菜单：DraftsHome 卡片 hover「⋯」→ DropdownMenu，卡片右键 → ContextMenu；AssetsPanel 瓦片同双入口（重新定位/删除引用/查看理解摘要）；删除全部 onMouseLeave 手搓弹层与 window.confirm（确认一律进 Dialog）。
- [ ] 图标：本任务波及组件内的 ⋯/＋/✓/✗ 等字符换 lucide（MoreHorizontal/Plus/Check/X/FolderOpen/Trash2/PencilLine/Copy/Settings…，16px 档，stroke 1.5-1.75）。
- [ ] tests 更新（radix portal 下的查询方式）；Commit `feat：radix 统一弹窗菜单 + lucide 图标（壳层）`。

### Task 2b: 播放器控制条（可与 2a/2c 并行）

**Files:** `components/PreviewPlayer/PreviewPlayer.tsx`（及其 test）

- [ ] 基于 vidstack 组件补全：可拖 scrub 进度条（含缓冲显示）、播放/暂停（lucide Play/Pause）、±1 帧（保留，图标化）、时间码 `mm:ss:ff / 总长`、音量（滑杆+静音）、全屏；素材试看（AssetMediaPreview 的 video 分型）与成片预览共用同一控制条形态。
- [ ] onTimeUpdate→playheadSec、onFirstPlay 上报 PreviewViewed、fit 双模式等既有行为不破坏；控制条样式走令牌（raised 底、overlay 阴影、accent 进度）。
- [ ] tests 更新；Commit `feat：播放器完整控制条（scrub/时间码/音量/全屏）`。

### Task 2c: 时间线质感（可与 2a/2b 并行）

**Files:** `components/TimelineViewer/TimelineViewer.tsx`（及其 test）

- [ ] 视频 clip：内部按 clip 宽度平铺该素材 thumbnail（`/api/media/{aid}/thumbnail?token=`，SVG `<pattern>`/`<image>` 平铺，宽度按固定瓦片宽切片；缩略图未就绪回落现纯色+标签）；文字标签压深色渐变底保可读。
- [ ] 音频 clip：内嵌波形（复用现 peaks 数据源，SVG path 渲染，不再依赖底部独立 wavesurfer 条——该独立条移除或降级为可选）；轨道头 112px 标签列加 lucide 轨道类型图标（Video/AudioLines/Type/Image）。
- [ ] 播放头/刻度尺打磨：播放头 accent 细线+顶部把手、主刻度文字 --text-2xs、当前缩放档高亮；选中 clip 描边走 focus 令牌。**只读不变，不加任何拖拽/裁剪交互**。
- [ ] tests 更新；Commit `feat：时间线 filmstrip/内嵌波形/轨道头图标`。

### Task 3: 门禁 + 截图目检 + PR

- [ ] 全量 web 三门禁 + e2e 两绿（rm -rf .rushes 先行；选择器如因图标替换文字字符而失锚，机械修）。
- [ ] 起真实栈（照 e2e global-setup 的环境变量方式），Playwright 截 `/`（含一张有素材草稿卡）与 `/drafts/$id`（导入素材+seed 时间线后）两张全页图存 scratchpad，交编排者目检。
- [ ] 编排者：目检 → push → PR → review → CI → squash merge。

## Self-Review 记录

- spec §4 六条（令牌/图标/播放器/时间线/弹窗菜单/死依赖）逐条有落点（死依赖 PR-A 已删）；病根清单 ①-⑦ 对账齐（⑧字体明确不做）。
- 2a/2b/2c 文件不相交可并行；令牌与依赖先行为硬前置。
