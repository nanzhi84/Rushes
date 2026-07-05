# Rushes M-1 POC Results

## M-1.1 云端 ASR 契约

### 目的

验证 DashScope Paraformer-v2 在关闭顺滑/ITN 后，是否能保留「呃/嗯/啊/哦」等口癖，并输出可归一化到 `TranscriptDocument.v1` 的 utterance + word 毫秒级时间戳。

### 运行命令

```bash
python scripts/poc/make_fixture.py
python scripts/poc/asr_contract.py --audio scripts/poc/fixtures/filler_speech.wav
```

### 结论

**PASS（2026-07-05 实测，合成语音 fixture）**：

- `disfluency_removal_enabled=false` 生效，口癖完整保留（命中：呃、啊、嗯；"然后然后"重复句原样保留）。
- 字级时间戳单调递增、区间半开，共 72 个 word；与参照文本字符级对齐 **100%**。
- 归一化到 `TranscriptDocument.v1`（raw_preserved=true）无字段缺口。
- 原始响应样本：`research/asr_samples/paraformer_v2_20260705T072712Z.json`。
- 链路事实：录音文件识别只接受公网 URL，本地文件经阿里 OSS 预签名 URL（上传→识别→删除）可行；异步任务约 4s 完成（35s 音频）。

**遗留**：本次用 macOS `say` 合成语音；真实口播（语速/连读/噪声）待用户提供素材后复跑同一脚本确认（脚本支持 `--audio` 任意文件）。

### 风险对照

- R2：云端 ASR 顺滑开关实际行为与文档不符 / 口癖仍被吞。→ **合成语音场景下已排除**：开关行为与文档一致。

## M-1.2 端到端画质假设

### 目的

验证真实素材经 VLM cheap 标注后，能否用简单贪心策略拼出一条约 30s 的 9:16 时间线，并按 PRD §10.3 的分段渲染 + concat 路径导出可人工评估的视频。

### 运行命令

```bash
python scripts/poc/e2e_cut.py --footage-dir /path/to/real/footage
```

### 结论

【待真实素材后运行填写】脚本已就绪（`--footage-dir` 指向素材目录即可）；无素材时退出码 2 并给出指引。

### 风险对照

- R1：VLM 标注 + 检索的候选拼不出「能看」的片子。
- R4：filter_complex 复杂化失控；本 POC 只做单段小 filter + concat。

## M-1.3 火山 TTS 时间戳链路

### 目的

验证火山引擎 TTS 在 `with_timestamp=1` 下能同时返回可解码 MP3 与可归一化到 `TranscriptDocument.v1` utterances/words 语义的句级 + 字/词级毫秒时间戳。

MiniMax 已被用户决策弃用：M-1.3 只验证火山链路，不保留 MiniMax 兼容路径。

### 运行命令

```bash
python scripts/poc/tts_timestamps.py
```

### 结论

【待火山实跑后填写】脚本缺少 `RUSHES_VOLC_TTS_AKSK` / `RUSHES_VOLC_TTS_APPID` / `RUSHES_VOLC_TTS_CLUSTER` 任一配置时 SKIP（退出码 2）；有配置时会签发/复用数据面 API key、合成音频、ffprobe 校验 MP3、断言时间戳单调与全文覆盖，并保存样本到 `research/tts_samples/volcano_<ts>.{mp3,json}`。

### 风险对照

- R3：火山 TTS/ASR 文档字段不一致；M-1.3 以真实响应确认数据面鉴权、`with_timestamp`/frontend 类参数与返回时间戳字段。
