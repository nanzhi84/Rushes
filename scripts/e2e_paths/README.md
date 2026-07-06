# M9 路径 1/2 手动验收脚本

本目录是 PRD §17-M9 的手动驱动脚本，不进 CI，不用 Playwright。脚本直接通过
REST + SSE 推进真实 provider 链路，并对真实 LLM 输出做宽松结构判据。

## 运行顺序

先生成合成素材：

```bash
uv run python scripts/e2e_paths/make_fixtures.py --out-dir .e2e-paths-fixtures
```

自动启动 API + worker 跑路径 1：

```bash
uv run python scripts/e2e_paths/run_path1.py \
  --api-url http://127.0.0.1:8000 \
  --token e2e-token \
  --workspace .e2e-paths-workspace/path1 \
  --autostart \
  --voiceover-video .e2e-paths-fixtures/path1_voiceover_video.mp4 \
  --out-dir .e2e-paths-output/path1
```

自动启动 API + worker 跑路径 2：

```bash
uv run python scripts/e2e_paths/run_path2.py \
  --api-url http://127.0.0.1:8000 \
  --token e2e-token \
  --workspace .e2e-paths-workspace/path2 \
  --autostart \
  --fixture-dir .e2e-paths-fixtures \
  --out-dir .e2e-paths-output/path2
```

如果服务已经由外部启动，不传 `--autostart`。这时请确认 API token 匹配，
并且 API 的 `RUSHES_FS_ROOTS` 覆盖 `.e2e-paths-fixtures/` 或实拍素材目录。

## 期望阶段输出

路径 1 会打印这些阶段：

- 创建 project/case，导入口播视频。
- `audio_mode` 如出现则选择原声粗剪。
- 等 ASR、口癖候选、粗剪 timeline、预览。
- 发送“把 7 秒附近那段删掉”，等待 timeline version 递增。
- 字幕和 BGM decision 选择跳过，确认 export。
- 下载最终 MP4，并用 ffprobe 检查时长在预期区间内。

路径 2 会打印这些阶段：

- 导入 3 段无声 B-roll 和 1 张图。
- 等内容计划确认，进入 TTS。
- 等 TTS、检索、timeline、预览。
- 字幕选择 `clean_bottom`，BGM 从项目已上传的 audio 素材中选、无音频素材则跳过，确认 export。
- 下载最终 MP4，并用 ffprobe 检查时长在预期区间内。

所有阶段日志都有本地时间戳。超时会打印当前 case 摘要和最近 SSE 事件。

## 环境变量

脚本会读取 repo 根目录 `.env`，真实 provider 需要这些配置：

- LLM planner：`RUSHES_DASHSCOPE_API_KEY`，或等价的 `RUSHES_LLM_API_KEY`。
- 路径 1 ASR：`RUSHES_DASHSCOPE_API_KEY`。
- 路径 1 ASR 上传公网音频：`RUSHES_OSS_ENDPOINT`、`RUSHES_OSS_REGION`、
  `RUSHES_OSS_BUCKET`、`RUSHES_OSS_ACCESS_KEY`、`RUSHES_OSS_SECRET_KEY`。
- 路径 1 fixture TTS、路径 2 TTS：`RUSHES_VOLC_TTS_AKSK`、
  `RUSHES_VOLC_TTS_APPID`、`RUSHES_VOLC_TTS_CLUSTER`，可选
  `RUSHES_VOLC_TTS_VOICE_TYPE`。
- 路径 2 图/视频标注和检索按本机配置启用，通常需要 `RUSHES_VLM_API_KEY`；
  embedding 没有配置时会降级到关键词检索，效果可能不稳定。

本机还需要 `ffmpeg` 和 `ffprobe`。

## 常见卡点

- 401/403：token、Host 或 `RUSHES_FS_ROOTS` 不匹配；优先使用
  `http://127.0.0.1:<port>`，不要用 `localhost`。
- ASR 卡住：DashScope 录音识别需要公网可访问 URL。worker 会把本地音频上传到
  OSS 再给 ASR，所以必须配置 OSS 环境变量。
- TTS 或 LLM 超时：脚本默认 LLM 120 秒，ASR/TTS/render 300 秒，可用
  `--llm-timeout`、`--job-timeout`、`--render-timeout` 放大。
- worker 已完成 job 但 agent 没继续：外置 worker 只写 job/event，脚本在 case 空闲时
  会自动发“继续”消息推动下一轮。
- BGM 决策没有预期选项：BGM 只能从项目已上传的 audio 素材中选择，未上传音频素材时
  只有「上传 BGM 素材 / 跳过 BGM」两项，脚本会按提示选择跳过。

## 替换为真实素材

路径 1 可直接替换口播视频：

```bash
uv run python scripts/e2e_paths/run_path1.py \
  --api-url http://127.0.0.1:8000 \
  --token e2e-token \
  --voiceover-video /abs/path/to/voiceover.mp4 \
  --out-dir .e2e-paths-output/path1
```

如使用 `--autostart`，脚本会把该视频所在目录加入 `RUSHES_FS_ROOTS`。

路径 2 可把真实 B-roll/图片放到一个目录，并使用同名文件：
`path2_broll_01.mp4`、`path2_broll_02.mp4`、`path2_broll_03.mp4`、
`path2_product_image.png`、`path2_script.txt`，然后传 `--fixture-dir`。
