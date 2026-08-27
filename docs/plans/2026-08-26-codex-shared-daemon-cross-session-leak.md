# Codex shared daemon 跨 session 串消息问题记录

**日期：** 2026-08-26
**状态：** Adapter fix implemented / Automated tests passed / Live daemon production gate open
**严重度：** P0（跨会话、跨群内容泄露）
**影响分支：** `ws/feat/codex-app-server-daemon`
**相关提交：** `5185a8b feat(codex): support shared app-server daemon`

## 修复结果（2026-08-27）

代码修复和自动化回归已经完成：

- adapter 现在显式区分 thread-scoped 与 global notification；所有 thread-scoped
  notification 都在修改状态或产生 `core.Event` 前精确校验 `threadId`，缺失、空值或
  malformed identity 均 fail closed。账户级 rate-limit notification 保持可用。
- resume 在请求发出前预绑定 expected thread；start 在 response 返回前使用最多 64 条、
  最长 5 秒的缓冲，并只回放与最终 thread ID 匹配的 notification。
- approval、permissions、request-user-input、dynamic tool call 和 MCP elicitation request
  均要求同时匹配当前 thread、由本连接 `turn/start` response 确认的 locally-owned turn 和
  writer owner；daemon transport 对未知 server request 不再抢答。
- daemon endpoint + thread ID 现在拥有进程内 single-writer lease。第二个 writer 会收到
  `ErrAgentSessionWriterBusy`；Core 会保留原 session ID，且不会静默 fallback 到新 thread。
- 已覆盖 100 轮 A/B 交错事件、全部已处理 notification、malformed identity、初始化乱序、
  双客户端 fake daemon、全部 server request 类型、双群 single-writer CUJ 和 race detector。
  `go test ./...`、`go build ./...` 与 `go vet ./...` 均通过。

尚未完成真实 Codex daemon 双客户端冒烟：当前验证环境未配置
`CC_CONNECT_CODEX_APP_SERVER_SOCKET`。在完成真实 daemon 的断连、重连、resume 和重启
验证前，下面的生产临时规避仍然有效，不应将 shared daemon 标记为已完成多 session
生产验收。

进程内 writer lease 只能阻止同一个 cc-connect 进程中的重复群绑定，不能约束另一个
cc-connect 进程或 Codex TUI。已检查的 Codex CLI `0.150.0` schema 没有提供原子的
`start-if-idle`/ownership precondition；因此本次已封堵跨 thread 事件泄露和旁路
server-request 响应，但“外部客户端与 cc-connect 同时写同一 thread”仍是 shared-write
生产门禁，不能用本地 lease 宣称全局 single writer。

## 结论

修复前的 shared app-server daemon transport 缺少 thread 级事件隔离。一个
`appServerSession` 会处理同一 daemon 连接上其他 Codex thread 的通知，并将它们转换
成不再携带 `threadId` 的 `core.Event`。Engine 随后把这些事件当作当前 cc-connect
session 的输出，发送到该 session 绑定的飞书群。

这是已确认并已由本文“修复结果”所述改动封堵的实现缺陷，不是
`share_session_in_channel`、`thread_isolation`、`group_reply_all` 或 multi-workspace
路由本身造成的。

原始 app-server adapter 从提交 `9aea457` 起就没有在 `handleNotification` 中校验
`threadId`，但 `process` transport 为每个 `AgentSession` 启动独立 app-server 子进程，
进程边界使这个缺口通常不可见。提交 `5185a8b` 引入 shared daemon transport 后，多个
thread 共用 daemon，原有缺口变成了可观察的跨 session 回归。

## 用户可见现象

- 一个飞书群绑定 Codex thread A，却收到 thread B 的助手回复。
- 两个不同 cc-connect/Codex session 的输出出现在同一个群中。
- 被错误接收的通知还可能覆盖本 session 的 `currentTurn`、`pendingMsgs`、上下文用量
  和完成状态，使后续消息、状态卡片或历史记录继续出现错乱。
- 现场确认过一个群目标 thread 为 `01a03bd3-7ab2-...`，实际收到的内容来自另一条
  `01a03d43-...` session；完整现场 session 数据只保留在部署机，不提交到仓库。

## 根因链路

```text
Codex shared app-server daemon
        │
        │ thread B notification（params.threadId = B）
        ▼
thread A 的 appServerSession WebSocket reader
        │
        │ handleNotification 未比较 B 与 CurrentSessionID() = A
        ▼
thread A 的 session state / core.Event
        │ core.Event 已不携带 threadId
        ▼
Engine foreground reader 或 unsolicited reader
        │
        ▼
thread A 所绑定的飞书群
```

关键代码位置：

1. `turnNotification`、`itemNotification` 和 request-user-input 参数都已经包含
   `threadId`：`agent/codex/appserver_session.go`。
2. `handleIncomingMessage` 将 daemon notification 直接交给
   `handleNotification`。
3. `handleNotification` 解析 `threadId`，但在处理 `turn/started`、
   `item/started`、`item/completed`、`turn/completed`、
   `thread/status/changed` 和 `thread/tokenUsage/updated` 前均未与当前
   `appServerSession.threadID` 比较。
4. `handleItemStarted`、`handleItemCompleted` 和 `completeTurn` 生成的
   `core.Event` 不携带 thread identity，进入 Core 后已无法恢复正确归属。
5. `core.Engine.runUnsolicitedReader` 在收到 `EventResult` 后使用当前
   `interactiveState` 的 platform/reply context 发送结果，因此错误事件最终进入当前群。

## 独立但相关的重复绑定问题

现场 session store 中还存在同一个 Codex thread 被两个飞书群引用的情况。当前
`SessionManager.SwitchToAgentSession` 只在单个 `userKey` 范围内复用
`AgentSessionID`；开启 `share_session_in_channel=true` 后，每个群拥有不同
`userKey`，因此全局上没有阻止同一个 thread 被多个群同时绑定。

重复绑定不能解释“thread A 收到完全不同的 thread B 内容”，因此不是本次跨 thread
泄露的主因；但即使 notification filter 修复，它仍可能造成：

- 同一个 thread 的结果被两个群同时接收。
- 两个群或群与本地 TUI 同时向同一 thread 写入。
- 审批、用户输入请求和 turn ownership 归属不明确。

shared daemon 要进入稳定使用，必须同时定义“一条 thread 一个 writer”的所有权规则。
其他绑定只能是显式只读 observer，或通过明确的 takeover 流程接管。

## 风险范围

### 已确认

- 助手文本和工具事件可以跨 thread 进入错误的 cc-connect session。
- 错误的 `turn/completed` 或 idle 状态可以结束另一条 session 正在收集的 turn。
- 错误内容会被写入接收群对应的 cc-connect history，形成持久化污染。

### 修复中已验证

- 当前 Codex schema 中 command/file-change approval、permissions approval、
  request-user-input、dynamic tool 和 MCP elicitation server request 均携带 thread
  identity；除允许 nullable `turnId` 的 MCP elicitation 外，其余类型也携带 turn identity。
- fake daemon 会把 server request 广播给多个 WebSocket client，回归测试确认只有同时匹配
  thread、locally-owned turn 和 writer lease 的 owner 才展示或响应请求；同 thread 但由
  外部通知建立的 active turn 也不会被当作本连接所有。
- `thread/start` response 之前的 notification 进入有界缓冲；绑定返回的 thread ID 后只回放
  identity 匹配的事件。resume 则在请求发出前预绑定 expected thread ID。

server request 路由在未验证前必须 fail closed。不能因为尚未复现跨 thread 审批，就
假设它和普通 notification 一样安全。

## 最小复现

1. 启动同一个 Codex app-server daemon。
2. 将 cc-connect 配置为 `app_server_transport = "daemon"`。
3. 创建两个 cc-connect session，使它们分别 resume 不同的 thread A 和 B，并保持两个
   `appServerSession` WebSocket reader 存活。
4. 在 B 上执行会产生助手文本和 `turn/completed` 的任务。
5. 观察 A 的 `Events()`；修复前，A 可能收到由 B 的 notification 生成的事件。
6. 若 A 的 unsolicited reader 已启动，B 的最终文本会被发送到 A 绑定的平台上下文。

自动化复现应使用 fake daemon 在同一连接集合上交错发送 A、B 两条 thread 的通知，
避免依赖真实模型时序。

## 临时规避

在真实 Codex daemon 双客户端冒烟和断连/重连验收完成前，生产环境仍应切回独立进程
transport：

```toml
[projects.agent.options]
backend = "app_server"
app_server_transport = "process"
```

随后重启 cc-connect。`process` 模式保留按 session ID resume 的能力，但不支持 shared
daemon 的实时共享状态。

此外应清理 session store 中重复的跨群 thread 绑定。仅清理重复绑定不能修复本问题；
只要 shared daemon 上存在多个 thread，缺少 notification filter 仍可能泄露消息。

## 修复要求

### 1. Adapter 入口强制 thread 过滤

- 为所有 thread-scoped notification 建立显式分类。
- 在修改 `appServerSession` 状态、usage、pending message 或发送 `core.Event` 之前，
  精确比较 notification `threadId` 与当前绑定 thread ID。
- 不匹配的通知直接丢弃并记录不包含正文的 structured debug/warn 日志。
- thread-scoped 方法缺失或无法解析 `threadId` 时 fail closed；账户级 rate-limit 等明确
  global 的通知可以继续处理。
- `thread/tokenUsage/updated` 和 `thread/status/changed` 也必须过滤，不能只过滤
  turn/item 事件。

### 2. 处理初始化竞态

- resume 已知 thread 时，可以从请求参数预先建立 expected thread ID。
- start 新 thread 时，在 response 返回 ID 前不得把任意 thread notification 归给该
  session；如协议确实允许 notification 先于 response，应使用有界缓冲并在绑定后只
  回放匹配 ID 的事件。
- 缓冲必须有大小和时间上限，未知 thread 事件不得无限积累。

### 3. Server request 所有权

- 逐类验证 approval、request-user-input 和 dynamic tool request 是否带 thread/turn
  identity，以及 daemon 是否只向 owner connection 投递。
- 非 owner connection 不得展示、批准、拒绝或用默认响应抢答其他 thread 的 request。
- 无法可靠判定 owner 时，关闭被动订阅并降级为 non-subscribing snapshot observer。

### 4. Core 纵深防御

- 首选在 Codex adapter 转换为 `core.Event` 前完成隔离。
- 可以评估为内部事件附加不可展示的 thread identity，让 Engine 在 session binding
  处做第二次一致性校验；不能依赖 Core 弥补 adapter 的第一层缺失。
- unsolicited reader 只有在 adapter 已提供隔离保证后才能转发外部完成事件。

### 5. Thread writer 所有权

- 建立 daemon endpoint + thread ID 级别的 writer lease/binding registry。
- 第二个群绑定同一 thread 时默认拒绝写入，或进入 observer-only。
- takeover 必须显式、原子，并让旧 writer 停止发送和响应 server request。

## 必需回归测试

1. `appServerSession` 绑定 thread A，输入 thread B 的每一种 thread-scoped
   notification，断言状态不变且 `Events()` 没有输出。
2. A、B notification 交错输入时，各 session 只收到自己的文本、工具和完成事件。
3. B 的 idle/completed 不得调用 A 的 `completeTurn`。
4. B 的 token usage/status 不得覆盖 A 的 runtime state。
5. global rate-limit notification 仍能被正确处理。
6. malformed/missing `threadId` 按 fail-closed 处理。
7. start/resume response 与 notification 乱序的测试。
8. fake shared-daemon 双客户端集成测试，覆盖 notification 和所有 server request 类型。
9. race detector 覆盖关闭连接、切换 session、notification 到达同时发生的情况。
10. Core 级双群测试：不同 thread 不串消息；同一 thread 的第二 writer 被拒绝或只能观察。

修复提交必须包含能够在修复前稳定失败、修复后通过的 regression test。

## 验收标准

- 不同 thread 并发运行至少 100 轮，文本、工具、完成、状态和用量事件均零串流。
- 一个群的 history 中不出现另一个 thread 的内容。
- 一个 thread 在任意时刻最多存在一个可写 owner。
- owner 以外的连接不能看到或响应审批/用户输入请求。
- daemon 断连、重连、resume 和 cc-connect 重启后隔离规则仍成立。
- `go test ./agent/codex ./core` 与相关 `-race` 测试通过。
- 通过双客户端真实 Codex daemon 冒烟测试后，才能重新将 daemon transport 标记为可用于
  多 session 部署。

## 与 `/process` 设计的关系

本问题是 [`/process` 按需查看 Codex 长任务进展设计](./2026-08-26-codex-process-command-design.md)
的阻塞项。任何 live progress cache 都必须在 notification 通过 thread filter 之后更新；
否则 `/process` 会把其他 thread 的状态展示为当前任务状态。

如果 server request owner routing 或原子 writer ownership 无法验证，`/process` 只能使用
non-subscribing snapshot-only 模式，不能以修复状态查询为由继续保留不安全的共享订阅。
