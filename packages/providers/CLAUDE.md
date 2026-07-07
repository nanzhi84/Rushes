# packages/providers — 云端能力网关

- **`gateway.py` 的 `ProviderGateway`** 负责选路、调用、归一化、记账（`ProviderCallRecorded`）、降级（`CapabilityDegraded`）。能力常量在 `capabilities.py`：`llm.chat / vlm.annotation / asr.transcribe / tts.speech / rerank.text`。
- **流式经 `on_delta`**：gateway 的 `on_delta` 收 `{"type": "text", "text": chunk}` 结构化增量；只有**首个可流式尝试**拿实时 delta，一旦失败故障转移，后续 provider 一律整段回放（`_replay_content_delta`），避免多路 partial 交错。`planner.py` 的 `_text_delta_forwarder` 把它降到纯字符串喂给前端。
- **OpenAI 兼容 provider 是双 capability**：`openai_compatible/llm.py`（`llm.chat`）与 `openai_compatible/vlm.py`（`vlm.annotation`）各自一个 descriptor。国内 provider（`aliyun/`ASR、`volcengine/`TTS、以及 openai_compatible）**都强制 IPv4 + `trust_env=False`**（`force_ipv4=True` → `httpx.AsyncHTTPTransport(local_address="0.0.0.0")`），别去掉。
- **`MockProvider`（`mock/scripted.py`）是测试/golden 的脚本化范式**：按 `capability → 预置响应队列` 出结果，队列耗尽返回 `mock_script_exhausted` 错误。golden 回放和单测都靠它免真网。
- **本层禁止 import agent_harness/tools**（只依赖 contracts，见根 `ALLOWED_IMPORTS`）。planner adapter 刻意不 import harness，只返回结构兼容的 mapping。
- 环境变量：`RUSHES_LLM_{API_KEY,BASE_URL,MODEL}`、`RUSHES_VLM_{API_KEY,MODEL}`、`RUSHES_DASHSCOPE_API_KEY`（ASR/阿里系，也作 LLM key 的兜底来源）。
