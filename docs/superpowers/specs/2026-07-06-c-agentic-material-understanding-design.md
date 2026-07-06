# Spec C：agentic 素材理解（替换离线标注与检索）

日期：2026-07-06 ｜ 状态：已定稿（用户授权免评审） ｜ 实施顺序：三份重构 spec 中第 3 个（A → B → C），依赖 A 的 kind 收敛与 B 的协议/流式通道

## 背景与目标

现状：素材上传后需要显式触发一条重离线标注流水线（分镜 → 抽帧 → 逐镜头 VLM → embedding，单并发串行队列），产物喂给 FTS+向量+RRF 检索出候选包再进时间线规划。问题：用户等待长、不 agentic、音频类 pipeline 是未实现 stub、算力花在可能用不到的素材上。

目标（用户拍板）：

1. **上传只做便宜本地索引**：技术元数据 + 缩略图 + 关键帧/分镜边界 + 音频波形，秒级~十秒级，素材几乎立即可用。
2. **理解按需、agentic**：主代理通过工具派出「素材理解子代理」（turn 内同步并行），子代理用受限工具集（看帧/听音频/读索引）理解素材，产出**带时间戳的结构化摘要**沉淀落库，返回主代理用于后续推理；重复使用直接命中缓存。
3. **离线检索基建完全删除**：FTS、embedding、RRF、candidate_pack 及其上下游全部移除；选材主路径 = 主代理读摘要直接推理。
4. **「标注失败素材不能参与剪辑」硬门取消**：改为理解失败可重试、可绕过（明确告知用户），由主代理自行处置。

## 现状要点（探索结论）

- 上传只自动触发 proxy job（ffprobe+转码，`apps/worker/media_jobs.py:32`）；标注显式触发（`annotation.enqueue/retry` 工具或素材页按钮）。
- 标注链路：`apps/worker/annotation_jobs.py` → `packages/annotation/pipelines/{video,image}.py`（audio/bgm/voiceover 是 NotImplementedError stub）→ `packages/annotation/projection.py` 写 4 处（annotations / annotation_clip_projection / annotation_signal_projection / clip_fts）。
- 检索：`retrieval.search_candidates` → `packages/indexing/candidate_pack.py`（BM25+余弦+RRF）→ candidate_packs 表 → `timeline.plan_from_candidates`。
- `media.view_frames`（`packages/tools/media_tools/handlers.py:58`）已是实时 ffmpeg 抽帧 + VLM，是理解子代理「看」能力的现成地基。
- ASR 独立链路：`audio.asr_original` → asr job → transcripts 表（词级时间戳），与标注互不融合。
- harness 无任何子代理机制；worker 是单并发串行 job 队列。
- clip 时间戳目前用帧号；本 spec 的新契约统一用**秒（float）**，渲染层负责换算。

## 设计

### C1. 上传时便宜本地索引

- proxy job 完成后自动链入新的 **index job**（本地、无网络）：
  - 视频：封面帧 + PySceneDetect 分镜边界（秒）+ 每镜头首帧缩略图（小尺寸 jpg，存 object store）；
  - 音频：波形峰值（peaks json）+ 波形缩略图 + VAD 语音区间（本地）；
  - 图片：缩略图；
  - 字体：家族名/样式元数据解析。
- 产物：assets 表新增 `index_json`（shots/peaks/vad/thumbnail hashes 等）+ `index_status`（none/running/ready/failed，语义重定义为「便宜索引」状态）。索引失败不阻塞任何流程，仅记录。
- 前端素材列表立即显示缩略图与时长（显著改善「上传后干等」的观感）。
- worker 并发：job runner 从单并发提升为小并发池（默认 2-3，配置项），index/proxy 短活不被长活饿死。

### C2. 删除离线标注与检索

删除（代码、表、工具、测试、PRD 全链）：

- `packages/annotation/` 整包（pipelines、projection、质量事件等）与 `apps/worker/annotation_jobs.py`；
- `packages/indexing/` 中 candidate_pack/keyword/vector/rrf 及 embedding 相关 provider 依赖；
- 工具：`annotation.enqueue / annotation.retry / annotation.status / annotation.inspect`、`retrieval.search_candidates`、`timeline.plan_from_candidates`；
- 表：annotations、annotation_clip_projection、annotation_signal_projection、clip_fts、candidate_packs（迁移时 DROP）；
- assets 表 `annotation_status / annotation_pass / usable` 列退役（迁移删除或忽略）；
- API：retry-annotation 端点；前端：标注状态徽标、「开始标注」按钮；
- `scripts/check_contracts.py` 分层表同步（annotation/indexing 组移除）。

保留：transcripts 表 + ASR provider + `audio.asr_original`（口播粗剪与字幕的词级时间戳刚需）；`media.view_frames` 保留并重构为理解子代理与主代理共用的内部能力。

### C3. 理解子代理与 understand 工具

**工具契约**：`understand.materials(asset_ids: list[str], focus: str | None)`

- 主代理在需要素材信息时调用；PolicyGate 无需人工确认（非破坏、成本有界）。
- 执行：**turn 内同步并行**——每个 asset 一个理解子代理，`asyncio.gather` 并发（上限配置，默认 3），单素材超时（默认 300s）。
- 缓存语义：asset 已有 `ready` 摘要且 `focus` 为空 → 直接返回缓存不起子代理；有新 focus → 子代理带着已有摘要增量深挖，产出合并后的新版本（version+1）。
- 返回：每个 asset 的 MaterialSummary（或失败原因），作为 observation 回灌主代理，同 turn 继续推理。

**子代理 = 复用 harness 设施的小循环**：独立 system prompt（素材理解员：任务是产出忠实、带时间戳、可直接用于剪辑决策的摘要）、独立步数预算（默认 12）、多模态模型 profile（qwen-vl 系，配置项），工具白名单：

| 子代理工具 | 说明 |
|---|---|
| `read_index(asset_id)` | 读 C1 便宜索引：元数据、分镜表、VAD、波形概要 |
| `view_frames(asset_id, timestamps_s)` | 指定时间点抽帧，图像直接进子代理多模态上下文（复用现 view_frames 的 ffmpeg 抽帧，去掉其内嵌 VLM 调用——子代理自己就是 VLM） |
| `transcribe(asset_id, range_s?)` | 触发/复用 ASR（写 transcripts 表），返回带时间戳 utterances；VAD 显示无语音时直接报告无语音 |
| `emit_summary(summary)` | 终结动作：提交结构化 MaterialSummary（schema 校验，不合格重试） |

- 子代理进度经 Spec B 的 turn-stream 推送 `subagent_progress`（「正在查看 xxx.mp4 02:10 画面」「正在转写 00:00-01:30」），用户全程可见。
- 子代理每步 trace 照记（复用 TraceRecorder，标注 subagent 归属），便于回放与审计。

**MaterialSummary 契约**（`packages/contracts/understanding.py`，新）：

```jsonc
{
  "asset_id": "...",
  "version": 2,
  "focus": null,                    // 或本次深挖的关注点
  "semantic_role": "speech_footage | footage | music | voiceover | ambient | photo | font | other",
  "overall": "整体一句话概述",
  "language": "zh",                 // 有语音时
  "segments": [                      // 时间戳分段，图片/字体可为空数组
    {"start_s": 0.0, "end_s": 12.4, "description": "…", "transcript": "…或省略",
     "tags": ["产品特写"], "quality": "good | usable | avoid", "notes": "手抖/过曝等"}
  ],
  "generated_at": "...", "model": "...",
  "spent": {"frames_viewed": 9, "asr_seconds": 84.0}
}
```

- 存 `material_summaries` 表（asset_id, version, status: running/ready/failed, summary_json, focus, created_at）；assets 表加 `understanding_status` 冗余列供列表页展示（none/running/ready/failed）。
- 领域事件：`MaterialUnderstandingStarted / Completed / Failed`（reducer 更新状态列；领域事件 SSE 照常驱动 UI 刷新）。

### C4. 主代理消费与时间线规划

- Context Builder 注入**素材摘要索引**：每个已 link 素材一行（文件名、kind、时长、understanding_status、semantic_role、overall 截断）。完整 segments 不进常驻上下文，主代理需要时调 `understand.materials`（命中缓存即取全文）或新增只读工具 `asset.read_summary(asset_ids)`。
- 时间线规划改为主代理直接产出：基于摘要的时间戳段落，用现有 `timeline.apply_patch` 组装初剪（若现有 op 集不足以从零组装轨道 clips，按需补最小 op，如批量 `add_clips`；以 plan 阶段代码核对为准）。`timeline.plan_from_candidates` 删除。
- 理解失败素材：主代理收到失败 observation，可重试（`understand.materials` 再调）、可跳过并在回复中告知用户，不再有「不能参与剪辑」硬门。

### C5. 前端素材页

- 标注徽标/按钮 → 理解状态徽标（未理解/理解中/已理解/失败）+ 缩略图列 + 时长列。理解不设任何手动按钮（含失败重试）：触发与重试都由用户在对话里表达、agent 调工具完成，UI 不加旁路入口，保持 chat-first。
- 素材详情可查看摘要全文（segments 时间戳表）。

## PRD 修订清单（随本 PR 提交）

- 硬约束节：「标注失败素材不能参与剪辑」→「理解失败素材需重试或经用户知情后绕过」。
- §3.1 架构图、§3.2 ER 图：annotation/indexing 相关节点替换为 index job + material_summaries。
- §4.10 job 类型表：annotation job 删除，新增 index job；worker 并发说明更新。
- §5.2 工具前置条件表：annotation/retrieval 行删除，新增 understand.materials。
- §6.3 annotation.*、§6.7 retrieval.* 删除；新增 §6.x understand.*；§6.8 timeline.* 中 plan_from_candidates 删除。
- §7.4 AnnotationDocument 整节替换为 MaterialSummary 契约；§7.5 TranscriptDocument 保留不动。
- 子代理机制新增小节（挂 §4 Agent Harness 下）：执行模型、并发、超时、trace 归属。

## 验收标准

1. 上传视频后 ~10s 内素材列表出现缩略图/时长（index job 自动跑，无需任何手动触发）；全程无 annotation 概念。
2. 对话中让 agent 剪辑：可观察到 understand 子代理并行理解素材（turn-stream 进度可见），摘要落库；同素材二次使用命中缓存不重复理解。
3. 摘要 → 时间线：主代理基于摘要时间戳产出初剪 patch 并渲染预览成功（e2e 用脚本化 planner 驱动）。
4. 全仓 grep 无 candidate_pack、clip_fts、annotation_clip、search_candidates、plan_from_candidates；相关表被迁移 DROP。
5. 理解失败路径：子代理超时/报错后主代理获得失败 observation，可重试或告知用户，流程不死锁。
6. CI 全绿（含 check_contracts 分层更新、golden trace、e2e path drivers 更新）。
