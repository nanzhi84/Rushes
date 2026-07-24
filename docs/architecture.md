# Rushes Go 核心架构

## 运行时主线

1. API 将本地文件登记为 `AssetImported + AssetLinked + JobEnqueued(ingest)`。
2. worker 原子 claim job，生成 probe、thumbnail、proxy，再经 Reducer 写 `AssetProbed / ProxyGenerated / JobProgress / JobSucceeded`。
3. 用户消息以 202 入 TurnQueue。Eino ReAct 读取草稿消息，调用理解、时间线、决策或渲染工具；过程通过 turn-stream 实时推送。
4. 渲染工具只入队。worker 完成后写终态事件，job observation bridge 再唤醒同一草稿的 Agent 回合。
5. 前端领域 SSE 只做 query invalidation；媒体由带 query token 的 Range/HEAD 端点直接播放。

模型工具目录按当前草稿状态和任务阶段动态披露；长期记忆写入使用可逆的 `memory.set`，用户明确要求忘记时使用破坏性的 `memory.remove` 并先走确认。

素材理解以 `asset hash + 分析参数 + prompt version` 作为持久化 fingerprint；`media.detect_shots` 每次只为一个可用视频建立或刷新镜头证据，相同输入直接复用 SQLite 结果。Agent 每回合常驻读取精简 `material_catalog`，逐镜头语义与精确源帧由只读的 `shot.search` 按创作意图检索，再以稳定 `shot_id` 交给时间线工具执行。

口播是同一套“目录常驻、证据按需检索”模式：`material_catalog` 只常驻 `a_roll/b_roll`、`speech_searchable` 和逐句数量；`speech.transcribe` 单独建立或刷新 SRT/ASR 与 VAD 索引，`speech.search` 严格只从 SQLite 按台词语义、稳定 ID 或源帧范围读取气口及相似文本证据。模型自行判断口误、重复和配画面语义，再用 `timeline.delete`、`timeline.insert`、`timeline.update` 等原子操作执行源区间正确的修改。完整转写和重复 cut 数据都不进入长期 Context。

## Agent 上下文窗口

上下文采用与 Codex `ContextManager + WorldState + replacement compaction` 相同的分层原则：

1. 系统提示只保存稳定的能力、安全和工具使用规则。
2. `ContextBuilder` 从 SQLite 生成带稳定 section ID 的客观 `WorldState`；窗口首次保存完整参考快照，后续只注入 RFC 7396 Merge Patch。
3. 可见消息与模型窗口分开持久化。模型历史只保留 user 指令和 assistant 最终回复，UI 工具折叠记录、observation 和 reset marker 不进入下一轮。
4. assistant 历史被标记为可能过期的叙述，不能覆盖最新 WorldState。工具调用与结果只在当前 Eino ReAct 回合内成对存在。
5. 历史超过软预算时，用结构化交接摘要替换旧消息；当前待执行 user 消息留在摘要之外，排在它后面尚未执行的队列消息不会提前泄漏进本轮。清空对话只删除 context checkpoint，素材、理解结果和当前时间线保持不变。

`agent_context_checkpoints` 只保存当前窗口的参考快照、hash、压缩交接和替换边界，不保存历史时间线版本。

## 写入不变量

- 业务表禁止绕过 `reducer.Apply`；工具结果侧行也必须通过 `ResultRows` 同事务提交。
- strict 事件必须携带正确 base version；merge 事件必须拥有完整 merge key。
- 一批事件的 preflight、apply、状态校验、CAS、event_log append 是一个 immediate transaction。
- draft 内 turn FIFO；跨 draft 并行。取消只传播 context，不删除已经完成的合法结果。
- job terminal event 只能由 worker 写；API/Agent 只负责 `JobEnqueued`。

## 流式契约

领域 SSE：

```text
id: <event_id>
event: <EventType>
data: {"event_id":...,"event":{...}}
```

支持 `Last-Event-ID` header 和 `last_event_id` query。workspace 与 draft 各自使用明确路由谓词。

turn-stream 固定 `event: turn_stream`，data.type 为：`turn_started`、`text_delta`、`message_completed`、`tool_step_started`、`tool_step_finished`、`subagent_progress`、`turn_ended`、`turn_error`。`message_completed` 带全文，用于修复中间 delta 丢失。

## 依赖方向

depguard 将允许的导入方向固化在 `go/.golangci.yml`：contracts/storage/reducer 位于底层；providers、media、timeline 不反向依赖 Agent；tools 是能力聚合层；Agent 是统一工具执行入口；API 与 worker 只在最外层装配。违反分层会在 CI 直接失败。各规则的 allow 列表按最小实际依赖维护，只列真正被源码 import 的内部包；新增跨包依赖时须同步更新该列表并说明理由。
