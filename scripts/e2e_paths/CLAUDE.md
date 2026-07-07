# scripts/e2e_paths — 真实 provider 路径驱动（不进 CI）

- PRD §17-M9 的**手动验收脚本**，**不进 CI、不用 Playwright**：直接用 REST + SSE 推进真实 provider 链路，对真实 LLM 输出做宽松结构判据。和 `e2e/`（CI Playwright、纯降级）是两套东西。
- **需要真实 key**：`RUSHES_LLM_API_KEY`/`RUSHES_DASHSCOPE_API_KEY`（LLM/ASR）、`RUSHES_VLM_API_KEY`（VLM）。缺 key 跑不通。
- 流程：先 `make_fixtures.py` 生成合成素材，再 `run_path1.py`（口播原声粗剪链路）/ `run_path2.py`（无声素材规划链路）/ `run_scenery.py`，各自可 `--autostart` 自起 API+worker，或对接外部已起的服务（注意 token 与 `RUSHES_FS_ROOTS` 要覆盖素材目录）。`client.py` 是共用的 REST/SSE 客户端。
- 用途是跑通真实模型的端到端语义（ASR→口癖→粗剪→改剪→字幕/BGM decision→导出→ffprobe 校验时长），改真实链路时手动验收用。
