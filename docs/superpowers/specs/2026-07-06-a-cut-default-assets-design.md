# Spec A：砍掉默认素材库 + 导入自动分流收敛

日期：2026-07-06 ｜ 状态：已定稿（用户授权免评审） ｜ 实施顺序：三份重构 spec 中第 1 个（A → B → C）

## 背景与目标

产品决策：Rushes 只使用用户上传/导入的素材，像剪映一样批量导入后自动按格式分流，导入过程零手选。为此：

1. **删除默认 BGM 库**（`default_bgm_calm/upbeat/ambient` 三条 ffmpeg 合成音及其懒合成链路）。
2. **AssetKind 从 7 类收敛为 4 类**：`video / audio / image / font`。`bgm`、`voiceover`、`subtitle_template` 退役——「这段音频是配乐还是口播配音」属于语义判断，交给 Spec C 的理解层在使用时判定，不再在导入时定死。
3. **导入零手选**：删除前端类型选择器，扩展名自动分流；不可识别的文件明确拒收，不再兜底成 video。

**保留**：6 个内置字幕样式模板（`packages/domain/subtitle_templates.py`）——它们是渲染参数预设（字号/颜色/描边/位置，引用系统字体名），不是素材；用户上传的 font 素材可覆盖模板中的字体引用（现状已支持的部分不动）。

**非目标**：不动标注/索引链路（Spec C 负责）；不动对话链路（Spec B 负责）；MaterialsTable 的标注状态徽标与「开始标注」按钮本期不动（Spec C 会整体替换，避免二次返工）。

## 现状要点（探索结论）

- 默认 BGM 不是文件，是首次使用时 `ffmpeg lavfi` 实时合成：`packages/media/bgm_library.py:40-147`（`_DEFAULT_BGM_TRACKS`、`synthesize_default_bgm`、`ensure_default_bgm_asset`）。
- 引用点：`packages/agent_harness/policy_gate.py:631-653`（`_bgm_confirmation_options`，含「使用默认无版权 BGM」选项）；`packages/tools/timeline_tools/handlers.py:169,229-270`（`_ensure_default_bgm_patch`，`default_bgm_` 前缀特判）；`scripts/e2e_paths/client.py:253-271`；测试 `tests/media/test_bgm_library.py`、`tests/agent_harness/test_m7_postprocess_gate.py`。
- 枚举：`packages/contracts/asset.py:14-22`（AssetKind 7 类）、`:24-28`（AssetSource 含 `default_library`）；前端镜像 `apps/web/src/api/client.ts:16-24`。
- **工作区有一份未提交的自动分流半成品**（`_MATERIAL_KIND_BY_SUFFIX` 等，改动散布在 `apps/api/main.py` 与 Materials 组件）：方向一致但不彻底（保留 7 类、手选、未知兜底 VIDEO）。本 spec 的实现**在干净分支上重新实现并超越它**，不依赖那份脏工作区。

## 设计

### A1. 删除默认 BGM

- 删除 `packages/media/bgm_library.py` 与 `tests/media/test_bgm_library.py`。
- `timeline_tools/handlers.py`：删除 `_ensure_default_bgm_patch` 与所有 `default_bgm_` 前缀逻辑；`add_bgm` op 的 `asset_id` 必须指向项目内真实存在的 audio 素材，否则返回工具错误（`asset_not_found`）。
- `policy_gate.py` `_bgm_confirmation_options` 改为动态生成：
  - 列出当前 project 已 link 的 audio 素材（每个一项，label 用文件名；Spec C 落地后可按语义角色排序，本期按导入时间倒序）；
  - 固定项「上传新的 BGM」（引导用户去素材页上传）；
  - 固定项「跳过 BGM」。
  - 项目内无 audio 素材时只出后两项（对应 PRD「无 BGM 素材时三选项」场景改为两选项，PRD 同步修订）。
- `AssetSource` 枚举删除 `DEFAULT_LIBRARY`，清理 reducer / schema / 序列化中的引用。

### A2. AssetKind 收敛为 4 类

- `packages/contracts/asset.py`：`AssetKind = video | audio | image | font`。
- 全仓清理 `AssetKind.BGM / VOICEOVER / SUBTITLE_TEMPLATE` 引用：
  - TTS 产物、上传配音、对齐链路中 kind 一律改为 `audio`（时间线轨道角色枚举 `voiceover/bgm` 不动——那是使用位置的语义，不是文件类型）。
  - 标注 dispatch（`apps/worker/annotation_jobs.py` 按 kind 分派）中 audio 类分支合并为 `audio`（本期仅做编译层面的收敛，pipeline 本体 Spec C 处理）。
  - 工具 spec / PolicyGate 谓词 / Context Builder 中涉及 kind 枚举值的地方同步。
- 前端 `apps/web/src/api/client.ts`：`MaterialKind` 收敛为 4 类；删除 `MaterialKindInput`/`"auto"` 概念。

### A3. 导入零手选 + 拒收不可识别格式

- 后端 `apps/api/main.py`：
  - 建立扩展名 → kind 映射（video/audio/image/font 四类常见扩展名，覆盖 mp4/mov/mkv/webm/avi/m4v/mpg/mpeg/3gp/wmv、mp3/wav/m4a/aac/flac/ogg/opus/aiff/ape、jpg/jpeg/png/gif/webp/bmp/tiff/heic/heif/svg、ttf/otf/woff/woff2）。
  - `.srt/.vtt/.ass/.ssa` 与一切未知扩展名：HTTP 400，错误码 `unsupported_material_type`，message 说明支持的格式族。**不再兜底成 video。**
  - 三个导入入口（uploads/init、import-local、import-url）的请求模型删除 `kind` 字段（不是改 optional，是删除；API 契约变更同步进 PRD）。URL 导入按 URL 路径的文件名后缀推断，无后缀或未知后缀在决策确认前即拒收。
  - 批量语义：前端逐文件调用现有单文件接口，被拒文件收集错误信息汇总展示（后端无需新增批量端点）。
- 前端：
  - 删除 `MaterialKindSelect.tsx`；`UploadDropzone` / `LocalImportPanel` / `UrlImportPanel` 移除 kind 选择与传参。
  - `UploadDropzone` 确保支持多文件选择/拖拽（若现状单文件则升级），批量导入结束后展示逐文件结果（成功 N 个；拒收列表：文件名 + 原因）。
  - `MaterialsTable` 的 kind 展示映射收敛为 4 类。

### A4. 存量数据迁移

- 存量 SQLite workspace 打开不崩：启动时轻量迁移（跟随仓库现有 schema 初始化机制的模式）：
  - `assets.kind`：`bgm`→`audio`，`voiceover`→`audio`；
  - `kind='subtitle_template'` 的资产行删除（连带 link 行；object store 内容寻址无引用即为孤儿，不强制回收）；
  - `source='default_library'` 的资产：未被任何 timeline 引用的删除；被引用的转为 `source='upload'`、kind=`audio` 保留（避免打断既有时间线）。
- 项目处于开发期、无真实外部用户，迁移允许粗粒度，但**不允许启动崩溃或静默丢用户上传的素材**。

### A5. 仓库卫生（顺带）

- `.gitignore` 增加 `.rushes/`、`.models/`、`.worktrees/`（当前为未跟踪噪音）。

## PRD 修订清单（随本 PR 提交）

- 硬约束节：删除「默认资产约束：字幕模板 ≤10 / 默认 BGM ≤10」中 BGM 部分；字幕模板约束保留。
- §3.5 / §7.3 AssetRecord：kind 枚举收敛为 4 类；AssetSource 删 default_library。
- §7.6.1 bgm DecisionAnswer 与「无 BGM 素材时三选项」场景：改为动态素材列表 + 上传 + 跳过（无素材时两选项）。
- §6.2 asset.* 工具契约中涉及 kind 的入参说明同步。
- Gherkin 场景（约 L2323-2326）同步改写。

## 验收标准

1. 全仓 grep 无 `default_bgm`、`DEFAULT_LIBRARY`、`AssetKind.BGM`、`AssetKind.VOICEOVER`、`AssetKind.SUBTITLE_TEMPLATE`、`MaterialKindSelect`。
2. API 行为：上传 `.mp4/.mp3/.jpg/.ttf` 分别自动落 `video/audio/image/font`；上传 `.srt` 与 `.xyz` 返回 400 `unsupported_material_type`；三个导入入口请求体不再接受 kind 字段。
3. BGM 决策选项动态化：有/无 audio 素材两种场景选项正确；`add_bgm` 指向不存在素材时报 `asset_not_found`。
4. 含 `bgm/voiceover/subtitle_template/default_library` 存量数据的 workspace 启动迁移成功。
5. CI 全绿（ruff / mypy / pytest / check_contracts / web typecheck+test+build / Playwright e2e），受影响的 e2e path driver 与测试同步更新。
