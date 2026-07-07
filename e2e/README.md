# Rushes E2E

本目录是 M9 的独立 Playwright E2E 包，不复用 `apps/web` 的 lockfile 或测试配置。

## 本地运行

先准备依赖：

```bash
uv sync
pnpm --dir apps/web install
pnpm --dir e2e install
pnpm --dir e2e exec playwright install chromium
```

本机还需要 `ffmpeg`/`ffprobe` 可执行文件。macOS 可用 Homebrew 安装：

```bash
brew install ffmpeg
```

运行全部 spec：

```bash
pnpm --dir e2e exec playwright test
```

## 运行方式

`global-setup.ts` 每次都会清空并重建仓库根目录下的 `.e2e-workspace/`，然后启动三个本地进程：

- API：`127.0.0.1:18000`
- worker：绑定同一个 `.e2e-workspace/`
- web dev server：`127.0.0.1:15173`，通过 `RUSHES_WEB_PROXY_TARGET` 代理到 API

测试 token 固定为 `e2e-token`，只用于本地 E2E 进程。`.e2e-workspace/` 会保留到下次运行开始时，方便失败后查看 DB、fixture 和进程日志。

## 覆盖范围

单级草稿（Draft）模型下的两条不依赖真实模型的链路：

- `path3-draft-materials.spec.ts`：开始创作建草稿 → 中栏素材面板导入 fixture（reference 原地索引）→ seed 时间线/导出/user 记忆 → 时间线只读展示 → 第二草稿导入同一文件走全局去重复用（asset_id 相同）→ user 记忆按 goal token 注入第二草稿。
- `streaming-console.spec.ts`：开始创作建草稿 → 发消息 → 空 `ScriptedPlanner` 走 content 散文协议回复 → 助手气泡渲染 → TurnEnded 解锁输入框。

路径 1/2 需要真实 provider 与真实素材，留到后续 PR（见 `scripts/e2e_paths/`）。
