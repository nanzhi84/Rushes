# Spec A 实施计划：砍默认素材库 + 导入自动分流收敛

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 删除默认 BGM 库与 AssetSource.DEFAULT_LIBRARY；AssetKind 收敛为 video/audio/image/font 四类；导入零手选（扩展名自动分流，不可识别拒收）；存量数据迁移。

**Architecture:** 契约层先收敛枚举，向外波及 worker/harness/policy_gate/timeline 的 kind 派生逻辑，再改 API 导入入口与前端，最后数据迁移 + PRD 同步。全程 TDD、每任务一提交。

**Tech Stack:** Python 3.12 + FastAPI + SQLAlchemy Core + pydantic v2（`uv run` 驱动）；React + TypeScript + vitest（`pnpm --dir apps/web`）。

**工作目录：** `/Users/yoryon/Projects/Rushes/.worktrees/refactor-a`（分支 `refactor/a-default-assets`，勿动主检出区）

## Global Constraints

- spec 全文见 `docs/superpowers/specs/2026-07-06-a-cut-default-assets-design.md`，验收标准以 spec 为准。
- 面向用户的文案一律简体中文；代码注释仅在表达代码无法自明的约束时才写。
- 每个任务结束跑 `uv run ruff check && uv run ruff format --check && uv run mypy`（Python 改动）或 `pnpm --dir apps/web typecheck`（前端改动），绿了才 commit。
- 保留 6 个字幕样式模板（`packages/domain/subtitle_templates.py` 不动）；时间线轨道角色枚举（voiceover/bgm 轨）不动。
- 提交信息格式沿用仓库惯例（中文、`feat(scope): ...`），并附
  `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>` 与 `Claude-Session: https://claude.ai/code/session_01FJ3rEvooREoJUdT1DbCFH6` 两行尾注。

---

### Task 1: 契约层枚举收敛

**Files:**
- Modify: `packages/contracts/asset.py:14-28`
- Test: 先跑 `grep -rn "SUBTITLE_TEMPLATE\|VOICEOVER\|AssetKind.BGM\|DEFAULT_LIBRARY" tests/` 找到断言这些枚举值的既有测试一并更新

**Interfaces:**
- Produces: `AssetKind = VIDEO|IMAGE|AUDIO|FONT`；`AssetSource = UPLOAD|LOCAL_PATH|URL`。后续所有任务以此为准。

- [ ] **Step 1: 改枚举**

```python
class AssetKind(StrEnum):
    VIDEO = "video"
    IMAGE = "image"
    AUDIO = "audio"
    FONT = "font"


class AssetSource(StrEnum):
    UPLOAD = "upload"
    LOCAL_PATH = "local_path"
    URL = "url"
```

- [ ] **Step 2: 全量跑测试暴露引用面**

Run: `uv run pytest -x -q 2>&1 | tail -30`
Expected: 多处 FAIL/ERROR（AttributeError: BGM/VOICEOVER/SUBTITLE_TEMPLATE/DEFAULT_LIBRARY）。记录失败清单——这是 Task 2-6 的靶子。**本任务只修 tests/ 下纯枚举断言类失败**（如构造 AssetRecord 的夹具把 kind="voiceover" 改 "audio"），涉及生产代码的留给后续任务。

- [ ] **Step 3: Commit**

```bash
git add -A && git commit -m "refactor(contracts): AssetKind 收敛为四类，AssetSource 移除 default_library"
```

（此时全量 pytest 允许红，任务边界以「契约变更+夹具跟进」为限；Task 2-6 逐步修绿，Task 9 全绿。）

### Task 2: worker 与 harness 的 kind 派生逻辑收敛

**Files:**
- Modify: `apps/worker/media_jobs.py:199-204`（`_is_audio_proxy_kind`）
- Modify: `apps/worker/audio_jobs.py:775`（TTS 配音资产 kind）
- Modify: `packages/agent_harness/loop.py:806`、`loop.py:838-874`
- Modify: `packages/domain/preconditions.py:35-52`
- Modify: `packages/agent_harness/policy_gate.py:643`
- Test: `tests/agent_harness/`、`tests/worker/` 中相应用例

**Interfaces:**
- Produces: `ProjectAudioAsset`（原 `ProjectBgmAsset` 更名）；`PreconditionContext.project_audio_assets`（原 `project_bgm_assets`）；`loop._load_project_audio_assets`（kind=="audio"，按 `mtime` 倒序）。Task 3 直接消费。
- 语义：`voiceover_asset_ids` 收敛为「全部 usable 的 audio 素材」（`voiceover_asset_exists` 谓词继续用它校验 `audio_plan.voiceover_asset_id`，不改谓词本身）。

- [ ] **Step 1: 写失败测试（新增/改造既有）**

在 `tests/agent_harness/` 既有 loop 状态装载测试里（grep `project_bgm_assets` 定位）改断言：kind="audio" 的素材进入 `project_audio_assets` 与 `voiceover_asset_ids`；不存在 bgm/voiceover kind。

- [ ] **Step 2: 实现**

```python
# media_jobs.py
def _is_audio_proxy_kind(kind: str) -> bool:
    return kind == AssetKind.AUDIO.value

# audio_jobs.py:775
"kind": AssetKind.AUDIO.value,

# loop.py:806
if str(values.get("kind")) == "audio":
    voiceover_asset_ids.add(asset_id)

# loop.py _asset_has_audio
if str(asset.get("kind")) == "audio":
    return True

# loop.py _load_project_bgm_assets → _load_project_audio_assets
#   .where(schema.assets.c.kind == "audio")
#   .order_by(schema.assets.c.mtime.desc())
# preconditions.py: ProjectBgmAsset → ProjectAudioAsset（docstring 同步），
#   PreconditionContext.project_bgm_assets → project_audio_assets
# 全仓 grep project_bgm_assets / ProjectBgmAsset / _load_project_bgm_assets 一次性改名
```

- [ ] **Step 3: 跑相关测试**

Run: `uv run pytest tests/agent_harness tests/worker -q 2>&1 | tail -15`
Expected: 除依赖默认 BGM/policy 选项的用例（Task 3-5 处理）外全绿。

- [ ] **Step 4: Commit** `refactor(harness,worker): kind 派生逻辑收敛为 audio 单一音频类`

### Task 3: BGM 决策选项动态化（去默认项）

**Files:**
- Modify: `packages/agent_harness/policy_gate.py:631-668`（`_bgm_confirmation_options`）
- Test: `tests/agent_harness/test_m7_postprocess_gate.py`

**Interfaces:**
- Consumes: `context.preconditions.project_audio_assets`（Task 2）。
- Produces: BGM decision 选项 = 素材项*≤5 + 「上传 BGM 素材」+「跳过 BGM」；无素材时仅后两项。option payload 结构不变（`{"enabled": True, "asset_id", "gain_db": -12.0, "duck": True}`）。

- [ ] **Step 1: 改测试**：test_m7_postprocess_gate 中断言默认项（option_id="default_bgm"）的用例改为断言两种场景的新选项集合；跑测试确认按预期失败。
- [ ] **Step 2: 实现**

```python
def _bgm_confirmation_options(context: PolicyContext) -> list[DecisionOption]:
    upload_option = DecisionOption(
        option_id="upload_bgm",
        label="上传 BGM 素材",
        payload={"enabled": True, "action": "upload"},
    )
    skip_option = DecisionOption(option_id="skip", label="跳过 BGM", payload={"enabled": False})
    project_assets = context.preconditions.project_audio_assets[:5]
    options = [
        DecisionOption(
            option_id=asset.asset_id,
            label=f"使用素材：{asset.filename}",
            payload={"enabled": True, "asset_id": asset.asset_id, "gain_db": -12.0, "duck": True},
        )
        for asset in project_assets
    ]
    options.extend((upload_option, skip_option))
    return options
```

- [ ] **Step 3: 跑测试绿 → Commit** `feat(policy): BGM 决策选项只列用户素材，移除默认 BGM`

### Task 4: timeline add_bgm 校验真实素材

**Files:**
- Modify: `packages/tools/timeline_tools/handlers.py:169-171,229-270`
- Test: `tests/tools/` 中 timeline apply_patch 相关（grep `default_bgm` 定位）

**Interfaces:**
- Produces: `AddBgmOp.asset_id` 必须是项目内 kind=="audio" 的存量资产，否则 ToolResult failed，`error_code="asset_not_found"`。

- [ ] **Step 1: 写失败测试**：apply_patch 提交 AddBgmOp 指向不存在 asset → 期望 failed + asset_not_found；指向 audio 资产 → 正常走 apply_timeline_patch。
- [ ] **Step 2: 实现**：删除 `_ensure_default_bgm_patch` 整个函数与 `apply_patch` 中 169-171 的调用，替换为：

```python
    invalid = _validate_bgm_asset(input_model, context)
    if invalid is not None:
        return invalid
    outcome = apply_timeline_patch(
        context.readonly_connection, case_state, input_model, created_at=_created_at(context)
    )
```

```python
def _validate_bgm_asset(
    input_model: TimelinePatchRequest,
    context: ToolExecutionContext,
) -> ToolResult | None:
    op = input_model.op
    if not isinstance(op, AddBgmOp):
        return None
    assert context.readonly_connection is not None
    row = context.readonly_connection.execute(
        select(schema.assets.c.kind).where(schema.assets.c.asset_id == op.asset_id)
    ).first()
    if row is None or str(row._mapping["kind"]) != "audio":
        return _failed(
            "timeline.apply_patch",
            context,
            "asset_not_found",
            f"BGM 素材不存在或不是音频：{op.asset_id}",
            details={"asset_id": op.asset_id},
        )
    return None
```

（imports：`from sqlalchemy import select`、`from storage import schema`，若文件尚无则补；同时删除 bgm_library 相关 import 与 `DefaultBgmSynthesisError` 引用。）

- [ ] **Step 3: 跑测试绿 → Commit** `feat(timeline): add_bgm 校验真实音频素材，删除默认 BGM 懒合成`

### Task 5: 删除 bgm_library 与 e2e 客户端默认分支

**Files:**
- Delete: `packages/media/bgm_library.py`、`tests/media/test_bgm_library.py`
- Modify: `scripts/e2e_paths/client.py:240-280`（BGM decision 应答逻辑）
- Test: `tests/scripts/test_e2e_paths_client.py`

- [ ] **Step 1: 删文件** `git rm packages/media/bgm_library.py tests/media/test_bgm_library.py`
- [ ] **Step 2: 改 e2e client**：BGM decision 应答改为——有 `使用素材：` 前缀选项选第一个；否则选「跳过 BGM」；删除「使用默认 BGM」「兜底选择默认 BGM」分支。tests/scripts 同步。
- [ ] **Step 3: 全仓查漏**

Run: `grep -rn "bgm_library\|default_bgm\|DefaultBgmSynthesisError\|ensure_default_bgm_asset" --include="*.py" . | grep -v .worktrees`
Expected: 无输出。

- [ ] **Step 4: 跑 `uv run pytest tests/media tests/scripts -q` 绿 → Commit** `refactor(media,e2e): 删除默认 BGM 库`

### Task 6: API 导入零手选 + 拒收不可识别格式

**Files:**
- Modify: `apps/api/main.py`（请求模型 :205-262；调用点 :602、:1280、:1325、:1910；新增推断助手放 `_reference_relocated_event` 附近 :1900+）
- Test: `tests/api/test_m2_materials.py`

**Interfaces:**
- Produces: `_infer_material_kind(name_or_path: str) -> AssetKind`（未知/字幕后缀 raise HTTPException 400 `unsupported_material_type`）。请求模型不再含 `kind` 字段。

- [ ] **Step 1: 写失败测试**（test_m2_materials 新增）：

```python
@pytest.mark.parametrize(
    ("filename", "expected_kind"),
    [("a.mp4", "video"), ("b.MP3", "audio"), ("c.jpeg", "image"), ("d.ttf", "font")],
)
def test_upload_kind_inferred_from_suffix(client, filename, expected_kind): ...  # init→parts→complete 后查 materials 列表断言 kind

@pytest.mark.parametrize("filename", ["e.srt", "f.xyz", "noext"])
def test_upload_unsupported_suffix_rejected(client, filename):
    resp = client.post("/api/uploads/init", json={...})
    assert resp.status_code == 400
    assert resp.json()["detail"]["error_code"] == "unsupported_material_type"
```

同类用例覆盖 import-local 与 import-url（url 用 `https://example.com/x.srt` 断言决策创建前即 400）。

- [ ] **Step 2: 实现**

```python
_MATERIAL_KIND_BY_SUFFIX: dict[str, AssetKind] = {
    # video
    ".mp4": AssetKind.VIDEO, ".mov": AssetKind.VIDEO, ".mkv": AssetKind.VIDEO,
    ".webm": AssetKind.VIDEO, ".avi": AssetKind.VIDEO, ".m4v": AssetKind.VIDEO,
    ".mpg": AssetKind.VIDEO, ".mpeg": AssetKind.VIDEO, ".3gp": AssetKind.VIDEO,
    ".wmv": AssetKind.VIDEO,
    # audio
    ".mp3": AssetKind.AUDIO, ".wav": AssetKind.AUDIO, ".m4a": AssetKind.AUDIO,
    ".aac": AssetKind.AUDIO, ".flac": AssetKind.AUDIO, ".ogg": AssetKind.AUDIO,
    ".opus": AssetKind.AUDIO, ".aiff": AssetKind.AUDIO, ".aif": AssetKind.AUDIO,
    ".ape": AssetKind.AUDIO,
    # image
    ".jpg": AssetKind.IMAGE, ".jpeg": AssetKind.IMAGE, ".png": AssetKind.IMAGE,
    ".gif": AssetKind.IMAGE, ".webp": AssetKind.IMAGE, ".bmp": AssetKind.IMAGE,
    ".tif": AssetKind.IMAGE, ".tiff": AssetKind.IMAGE, ".heic": AssetKind.IMAGE,
    ".heif": AssetKind.IMAGE, ".svg": AssetKind.IMAGE,
    # font
    ".ttf": AssetKind.FONT, ".otf": AssetKind.FONT, ".woff": AssetKind.FONT,
    ".woff2": AssetKind.FONT,
}


def _infer_material_kind(name_or_path: str) -> AssetKind:
    suffix = Path(name_or_path).suffix.lower()
    kind = _MATERIAL_KIND_BY_SUFFIX.get(suffix)
    if kind is None:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "error_code": "unsupported_material_type",
                "message": f"不支持的素材格式：{suffix or '（无扩展名）'}。"
                "支持常见视频/音频/图片/字体格式。",
            },
        )
    return kind
```

调用点：
- `import-local`（:602）：`kind=_infer_material_kind(str(source))`
- `uploads/init`（:1280）：manifest 存 `_infer_material_kind(payload.filename).value`（在写盘前推断，未知即 400）
- `uploads/complete`（:1325）：`kind = AssetKind(str(manifest["kind"]))`
- `import-url`（:1910 决策 payload）：文件名取 `payload.filename or Path(urlsplit(payload.url).path).name`，`_infer_material_kind(...)`（缺文件名/未知后缀即 400）；`from urllib.parse import urlsplit` 补 import。
- 四个请求模型删除 `kind` 字段。

- [ ] **Step 3: 跑 `uv run pytest tests/api -q` 绿 → Commit** `feat(api): 导入按扩展名自动分流，不可识别格式拒收`

### Task 7: 存量数据迁移

**Files:**
- Create: `packages/storage/data_migrations.py`
- Modify: `apps/api/main.py:283-285`（create_all 后调用）、`apps/worker/main.py:27` 附近（engine 创建后调用）
- Test: `tests/storage/test_data_migrations.py`（新建）

**Interfaces:**
- Produces: `apply_data_migrations(connection: Connection) -> None`（幂等，可重复执行）。

- [ ] **Step 1: 写失败测试**：内存 SQLite 建 schema，插入 kind∈{bgm,voiceover,subtitle_template}、source=default_library（被/不被 timeline_versions.document_json 引用两种）的资产与 link 行，断言迁移后：bgm/voiceover→audio；subtitle_template 资产与 link 消失；被引用的 default_library → source=upload/kind=audio；未被引用的被删；重复执行不变。
- [ ] **Step 2: 实现**

```python
"""Idempotent data migrations applied at workspace startup."""

from sqlalchemy.engine import Connection


def apply_data_migrations(connection: Connection) -> None:
    _collapse_asset_kinds(connection)


def _collapse_asset_kinds(connection: Connection) -> None:
    connection.exec_driver_sql(
        "UPDATE assets SET kind='audio' WHERE kind IN ('bgm','voiceover')"
    )
    connection.exec_driver_sql(
        "DELETE FROM project_asset_links WHERE asset_id IN "
        "(SELECT asset_id FROM assets WHERE kind='subtitle_template')"
    )
    connection.exec_driver_sql("DELETE FROM assets WHERE kind='subtitle_template'")
    connection.exec_driver_sql(
        "UPDATE assets SET source='upload', kind='audio' WHERE source='default_library' "
        "AND EXISTS (SELECT 1 FROM timeline_versions t "
        "WHERE t.document_json LIKE '%' || assets.asset_id || '%')"
    )
    connection.exec_driver_sql(
        "DELETE FROM project_asset_links WHERE asset_id IN "
        "(SELECT asset_id FROM assets WHERE source='default_library')"
    )
    connection.exec_driver_sql("DELETE FROM assets WHERE source='default_library'")
```

调用点（api `create_app` 与 worker `main`）：

```python
with engine.begin() as connection:
    schema.create_all(connection)
    apply_data_migrations(connection)
```

- [ ] **Step 3: 跑测试绿 → Commit** `feat(storage): 启动期幂等数据迁移收敛存量素材 kind`

### Task 8: 前端零手选 + 批量结果报告

**Files:**
- Delete: `apps/web/src/components/Materials/MaterialKindSelect.tsx`
- Modify: `apps/web/src/api/client.ts:16-23,106-156`、`UploadDropzone.tsx`、`LocalImportPanel.tsx`、`UrlImportPanel.tsx`、`MaterialsTable.tsx:131-141`、`ProjectMaterialsPage.tsx:61-71,165`
- Test: `apps/web/src/routes/ProjectMaterialsPage.test.tsx`

**Interfaces:**
- Produces: `MaterialKind = "video"|"audio"|"image"|"font"`；请求类型均无 kind；`LocalImportPanel.onImport(path: string)`；UploadDropzone 内部收集 `{filename, message}[]` 拒收列表并渲染。

- [ ] **Step 1: 改测试**：ProjectMaterialsPage.test.tsx 断言不再渲染类型选择器；新增「上传 .srt 被拒收时显示逐文件错误」用例（mock api 400 detail）。
- [ ] **Step 2: 实现**：删 MaterialKindSelect 及全部引用；三面板去 kind state/prop/传参；UploadDropzone `uploadFiles` 循环里 try/catch 收集失败 `{filename, message}`（message 取 API detail.message，网络错误用通用文案），循环后 setState 渲染「拒收 N 个」列表；`kindLabel` 收敛四项（视频/音频/图片/字体）。
- [ ] **Step 3: 验证**

Run: `pnpm --dir apps/web typecheck && pnpm --dir apps/web test -- --run && pnpm --dir apps/web build`
Expected: 全绿。

- [ ] **Step 4: Commit** `feat(web): 素材导入零手选，批量拒收逐文件报告`

### Task 9: PRD 修订 + 全量绿

**Files:**
- Modify: `chat_first_editing_agent_prd_v1_2.md`（修订点见 spec「PRD 修订清单」；grep `默认 BGM|default_bgm|default_library|voiceover|bgm|subtitle_template` 逐处核对，只改属于 Spec A 范围的：kind 枚举、AssetSource、bgm decision 选项、默认资产约束、Gherkin 场景；**轨道角色/audio_plan/postprocess_plan 中的 voiceover/bgm 字样不属于 kind，不改**）
- Modify: `.gitignore`（追加 `.rushes/`、`.models/`、`.worktrees/`）

- [ ] **Step 1: PRD 逐项修订**（对照 spec 清单五条）
- [ ] **Step 2: 全量验证**

Run: `uv run ruff check && uv run ruff format --check && uv run mypy && uv run pytest -q && uv run python scripts/check_contracts.py && pnpm --dir apps/web typecheck && pnpm --dir apps/web test -- --run && pnpm --dir apps/web build`
Expected: 全部通过。

- [ ] **Step 3: 验收 grep**

Run: `grep -rn "default_bgm\|DEFAULT_LIBRARY\|AssetKind.BGM\|AssetKind.VOICEOVER\|AssetKind.SUBTITLE_TEMPLATE\|MaterialKindSelect" --include="*.py" --include="*.ts" --include="*.tsx" apps packages scripts tests`
Expected: 无输出。

- [ ] **Step 4: Commit** `docs(prd): 同步 Spec A 契约修订` + `chore: gitignore 运行时目录`

### Task 10: e2e 回归

**Files:**
- Modify: `e2e/`（Playwright 用例若断言素材类型选择器/默认 BGM 选项则更新；grep `MaterialKind|默认|BGM` 定位）

- [ ] **Step 1:** `grep -rn "kind\|默认\|BGM" e2e/tests/ | head -20` 定位受影响用例并更新断言。
- [ ] **Step 2:** 本地跑得动的话 `pnpm --dir e2e exec playwright test --project=chromium`（起 API+web 的驱动脚本见 e2e/README 或 playwright.config）；跑不动就留给 CI，但必须完成 Step 1 的静态更新。
- [ ] **Step 3: Commit** `test(e2e): 适配导入零手选与 BGM 选项变更`
