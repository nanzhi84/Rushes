# Rushes Go 核心架构

## 运行时主线

1. API 将本地文件登记为 `AssetImported + AssetLinked + JobEnqueued(ingest)`。
2. worker 原子 claim job，生成 probe、thumbnail、proxy，再经 Reducer 写 `AssetProbed / ProxyGenerated / JobProgress / JobSucceeded`。
3. 用户消息以 202 入 TurnQueue。Eino ReAct 读取草稿消息，调用理解、时间线、决策或渲染工具；过程通过 turn-stream 实时推送。
4. 渲染工具只入队。worker 完成后写终态事件，job observation bridge 再唤醒同一草稿的 Agent 回合。
5. 前端领域 SSE 只做 query invalidation；媒体由带 query token 的 Range/HEAD 端点直接播放。

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

depguard 将允许的导入方向固化在 `go/.golangci.yml`：contracts/storage/reducer 位于底层；providers、media、timeline 不反向依赖 Agent；tools 是能力聚合层；Agent 是统一工具执行入口；API 与 worker 只在最外层装配。违反分层会在 CI 直接失败。
