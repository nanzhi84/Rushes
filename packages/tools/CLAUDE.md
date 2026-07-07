# packages/tools — 工具注册表与内置工具

- **注册纪律**（`specs.py` + `registry.py`）：`tool_specs()` 声明所有 `ToolSpec`，`build_default_tool_registry()` 用一个 `name → handler` 的 dict 逐个 `registry.register(spec, handlers[spec.name])`——**spec 与 handler 按名字一一配对**，缺一个直接 KeyError。工具按子目录分组：`asset / audio / content / interaction / media_tools / memory_tools / project / render_tools / timeline_tools / understand / builtin`。
- **`requires_artifacts` 谓词必须已知**：`registry.register` 会 `assert_known_preconditions(spec.requires_artifacts)`，谓词得先登记在 `domain.preconditions.PRECONDITION_REGISTRY`（`audio_mode_in(...)` 这类带参谓词特判）。`emits_events` 也会校验都在事件注册表里。这两条 `scripts/check_contracts.py` 会兜。
- **handler 可同步可 async**：`ToolHandler = (input, ToolExecutionContext) -> ToolResult | Awaitable[ToolResult]`，执行侧（`agent_harness`）统一 `await` 兼容两者。
- **这是唯一能同时 import `media` / `providers` / `timeline` 的实现聚合层**（见根 `ALLOWED_IMPORTS`）；别把这三者的调用漏到 agent_harness 或 apps。
- **`understand/` 是素材理解子代理**（`subagent.py`）：一个复用 harness 设施的多模态 mini-loop，子代理本身就是 VLM，每步回一个 JSON 动作 `view_frames / transcribe / emit_summary`，直到产出 `MaterialSummary` 或耗尽预算/超时。**它不碰网络/DB/provider 报文**——`vlm`/`extract_frame`/`transcribe` 全由 `handlers.py` 注入（测试直接喂脚本化动作）。
- understand 环境变量（`handlers.py`）：`RUSHES_UNDERSTAND_CONCURRENCY`（缺省 3）、`RUSHES_UNDERSTAND_TIMEOUT_S`（缺省 300）、`RUSHES_VLM_MODEL`。
