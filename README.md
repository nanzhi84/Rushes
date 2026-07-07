# 🎬 Rushes

**和 AI 聊天，把一堆素材聊成一条成片。**

Rushes 是一个跑在你自己电脑上的 AI 视频剪辑 Agent。把视频丢给它，用大白话告诉它你想要什么——它自己看素材、挑镜头、排时间线、渲预览；你说"7 秒那段删掉"，它就删掉。确认满意后，导出 MP4。

> 实测：5 段 4K 素材 + 一首 BGM，从导入到导出成片，全程 AI 自主完成，**4 分 33 秒**。

---

## ✨ 它能做什么

- 🗣️ **对话式剪辑** —— 不用学时间线软件，说人话就行："帮我把这条口播粗剪一下""节奏快一点""BGM 用我上传的那首"
- 👀 **AI 真的看得懂素材** —— 视觉模型逐镜头理解画面内容（"街头吉他手""瀑布航拍"），按你的描述检索匹配镜头
- 🎙️ **口播智能粗剪** —— 云端语音识别 + 自动找出"呃""就是说"这类口癖和停顿，一键剪掉
- 🔇 **无声素材也能剪** —— 风景混剪、产品种草：AI 生成内容规划和镜头编排，配上 TTS 配音或 BGM
- ✂️ **改起来像聊天** —— "删掉 7 秒附近那段"→ 精确定位、自动补齐时间线，不留黑帧不留缝
- ✅ **关键操作你说了算** —— 加字幕、加 BGM、最终导出，AI 必须先问你确认，绝不擅自行动
- 💾 **数据在你手里** —— 素材文件不离开本地磁盘，工作区就是一个 SQLite 文件；只有抽帧和音频会发给云端模型做分析
- ⚡ **增量渲染** —— 改一小段只重渲一小段，预览秒级更新

## 🚀 快速开始

### 需要准备

- macOS / Linux，装好 [ffmpeg](https://ffmpeg.org)（`brew install ffmpeg`）
- Python 3.12+ 与 [uv](https://docs.astral.sh/uv/)、Node 20+ 与 pnpm
- 一个[阿里云百炼（DashScope）API Key](https://bailian.console.aliyun.com/)——负责看片、听音、想剪法

### 三步跑起来

```bash
# 1. 安装
git clone https://github.com/nanzhi84/Rushes.git && cd Rushes
uv sync
cd apps/web && pnpm install && cd ../..

# 2. 配置 Key（写到项目根目录 .env）
echo 'RUSHES_DASHSCOPE_API_KEY=sk-你的key' > .env
echo 'RUSHES_LLM_MODEL=qwen-max' >> .env

# 3. 启动（三个终端）
scripts/dev_api.sh                          # API 服务
uv run python -m apps.worker.main .rushes   # 渲染/分析 worker
cd apps/web && pnpm dev                     # Web 界面
```

打开 `http://127.0.0.1:5173`，新建项目 → 导入素材 → 开始聊天。

### 🎥 你可以这样说

```text
「帮我把这条口播粗剪一下，先用原声，识别口癖后给我粗剪预览。」
「把这些风景素材剪成 30 秒混剪，不要配音，节奏明快一点。」
「7 秒附近那段删掉。」
「预览可以。字幕跳过，BGM 用我上传的那首，导出 MP4。」
```

## 🧭 它是怎么干活的

```text
导入素材 ──▶ AI 逐镜头标注理解 ──▶ 你说需求
                                      │
导出 MP4 ◀── 你确认 ◀── 预览 ◀── 排时间线 ◀── 检索匹配镜头
      ▲                   │
      └── "再改改" ────────┘   （改多少渲多少，秒级预览）
```

每一步都有据可查：AI 的每个动作、每次确认、每版时间线都记录在本地工作区里，随时回滚到任意历史版本。

## 🔑 API Key 一览

| Key | 用途 | 必需？ |
|---|---|---|
| `RUSHES_DASHSCOPE_API_KEY` | 剪辑规划（qwen-max）、看片（qwen-vl）、语音识别、语义检索 | ✅ 必需 |
| `RUSHES_VOLC_TTS_AKSK` / `RUSHES_VOLC_TTS_APPID` | 火山引擎 TTS 配音（做种草视频用） | 可选 |
| `RUSHES_OSS_*` | 语音识别需要的对象存储中转 | 用口播粗剪时需要 |

## ❓ 常见问题

**我的素材会被上传吗？**
视频文件全程留在你的磁盘上。分析时只把抽出的关键帧和音频片段发给云端模型，成片渲染完全在本地 ffmpeg 完成。

**AI 会不会乱动我的东西？**
不会。删项目、加字幕、加 BGM、最终导出这类操作都被"人工确认门"拦住——AI 只能提出，你点头才执行。所有写操作走同一条经过校验的事件管道，坏状态进不了库。

**能换别的大模型吗？**
可以。任何 OpenAI 兼容端点都行：设 `RUSHES_LLM_BASE_URL` / `RUSHES_LLM_MODEL` / `RUSHES_LLM_API_KEY` 即可。多步剪辑规划建议用能力较强的模型（默认推荐 `qwen-max`）。

## 🛠️ 开发者

```bash
uv run pytest && uv run ruff check && uv run mypy   # 后端：510+ 测试，覆盖率门禁 90%
cd apps/web && pnpm typecheck && pnpm test -- --run # 前端
cd e2e && pnpm exec playwright test                 # 端到端
```

完整设计文档见 [`PRD.md`](./PRD.md)，端到端演示脚本见 [`scripts/e2e_paths/`](./scripts/e2e_paths/README.md)。

---

**Rushes** —— 剪片子这件事，说出来就行。⭐️
