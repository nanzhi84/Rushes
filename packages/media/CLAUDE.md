# packages/media — 纯本地媒体能力

- 全是**本地**媒体处理，无云端调用：`probe`（时长/流信息）、`proxy`（转码代理）、`shots`（镜头切分）、`thumbnails`（封面/缩略图）、`waveform`（波形峰值）、`vad`（Silero 静音检测，模型缺失优雅降级）、`font_meta`（字体元数据）、`subtitles_ass`（字幕 ASS）、`preview`/`final_mp4`/`segment_render`（分段渲染 + `render_cache` 缓存键）、`audio_extract`/`url_import`/`align`/`concat`/`rough_cut` 等。
- **禁止 `import providers`**（分层：media 只能依赖 `storage`/`contracts`，见根 `ALLOWED_IMPORTS`）。要用云端能力的活儿归 `packages/tools` 接线。
- **ffmpeg/ffprobe 子进程范式**：`subprocess.run(command, capture_output=True, check=False, text=True)`，靠 `returncode` 判成败并抛领域异常（如 `MediaProxyError`）。**已知跟进项：这些 ffmpeg 子进程目前没有 `timeout=`**，卡死的 ffmpeg 会挂住 worker——改到相关代码时留意，别照抄成新的无超时调用。（唯一带超时的是 `url_import.py` 的 httpx 下载。）
- 需要真 ffmpeg 的测试打了 `@pytest.mark.ffmpeg`。
