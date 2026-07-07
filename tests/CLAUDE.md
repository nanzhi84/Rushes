# tests — Python 测试

- 结构镜像被测代码：`tests/<group>/` 一一对应 `packages/<group>` 与 `apps/<group>`（`agent_harness / api / apps / contracts / domain / events / media / providers / storage / timeline / tools / worker / scripts`）。
- **`tests/golden/` 是协议级回放**：`framework.py` 用 `ScriptedPlanner` / `MockProvider` 固定脚本驱动 `agent_harness.run_turn`，断言工具轨迹与事件序列；`test_cases.py` 是各回合场景。改主循环/工具/事件后先看它。
- **覆盖率门槛 90%**（`pyproject.toml` 的 `--cov-fail-under=90`，`--cov=packages --cov=apps`）。默认 `-m "not external"` 跳过打真实 provider 的用例。
- 标记：`external`（调外部 provider API）、`ffmpeg`（需本地 ffmpeg/ffprobe）。跑全量：`uv run pytest -q`。
