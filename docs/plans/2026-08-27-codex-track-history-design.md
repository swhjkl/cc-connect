# `/track` 与 Codex 权威历史实现设计

**日期：** 2026-08-27

**状态：** Implemented

**目标后端：** Codex app-server daemon

**命令：** `/track`、`/history`

> 本文记录已经实现的一次性 `/track` 基线，其中“默认不镜像”仅描述该基线版本。
> 后续产品决策已改为默认开启外部 turn mirror，并让它复用正常飞书对话的卡片机制；
> `/track on/off`、来源去重、终态通知和恢复保证见
> [`Codex 外部 turn 统一飞书镜像设计与评审`](./2026-08-27-codex-continuous-track-mirror-design.md)。

## 产品语义

- 飞书发起的普通对话继续正常发送 prompt 并接收 response。
- TUI 或其他 daemon 客户端发起的 turn 默认不自动镜像到飞书，也不写入
  cc-connect 的本地 History。
- `/track` 固定追踪调用时最新一个 turn；不会自动跳到后续 turn。运行中的卡片约每
  1.5 秒读取一次 Codex snapshot，只在渲染内容变化时更新，并在
  `completed`、`failed` 或 `interrupted` 后停止。
- `/track` 展示 prompt、assistant commentary/final response、状态、耗时、等待状态，
  以及固定白名单工具类型和状态。它不展示 reasoning、命令、路径、工具参数或输出。
- 运行中的飞书卡片提供“中止当前任务”按钮。按钮绑定卡片创建时的
  `(sessionID, turnID)`；点击时再次校验当前 session、活动 tracker 和 Codex turn
  都仍然匹配，随后才调用 `turn/interrupt`。旧卡片不能中止后续任务。
- `/history` 只展示终态 turn 的 prompt 与 final answer。对 Codex，Codex backend 是
  唯一真值；读取失败时明确报错，不回退到 cc-connect 本地 History。
- 不注册 `/process` 命令或兼容别名；现有 `/ps` steer 语义保持不变。

## 权限

- `/track` 及其中止动作只允许 `admin_from` 用户。
- Codex `/history` 同样只允许 `admin_from` 用户；管理员可以在群聊使用。
- 命令参数不能选择任意 session。`sessionID` 和 `turnID` 只会由卡片生成，并在执行中止
  前与当前绑定及 tracker 逐项核对。
- 帮助卡中的 `/history` 使用正常 command action，禁止通过无用户身份的 card nav
  直接读取内容。

## 数据路径

Core 通过可选能力保持 Agent 无关：

- `ConversationProvider.GetConversation` 返回从旧到新排列的权威 turn snapshot。
- `ConversationTurnController.InterruptConversationTurn` 只控制明确指定的 turn。
- `UnsolicitedEventRelayPolicy` 只影响非前台事件是否发送和写本地 History。
- `RichCardActionSupporter` 给支持的平台添加精确绑定的交互按钮。

Codex daemon 使用一条缓存的 initialize-only 连接：

1. `thread/read(includeTurns=false)` 验证 thread、workspace 和活动状态。
2. `thread/turns/list(sortDirection=desc, itemsView=full)` 读取最近 turn；旧版本降级为
   `thread/read(includeTurns=true)`。
3. reader 不调用 `thread/resume`、`thread/start`，也不响应 thread-scoped server
   request，因此不会取代正常可写 AgentSession，更不会限制多个 TUI。
4. 只有管理员点击仍有效的运行中卡片时，这条控制连接才发送精确的
   `turn/interrupt(threadId, turnId)`。

非 daemon Codex 后端继续从 Codex JSONL transcript 读取；Scanner 上限提升到与
app-server 消息一致的 10 MiB，并显式返回扫描错误，避免大记录后静默丢历史。

## 飞书交互

`/track` 在 session lock 之前执行，不占用或替换正常 AgentSession。用户可以按以下
顺序操作：

1. 在 TUI 启动任务。
2. 在飞书执行 `/track` 查看并持续观察。
3. 继续从飞书发送普通消息；消息仍走原有可写连接。
4. 如果观察到任务方向错误，点击卡片中的中止按钮。
5. 要观察后续新 turn，再执行一次 `/track`。

重复执行 `/track` 会取消当前频道的旧 tracker 并创建新卡片。旧卡片保留最后一次
内容；它只有在仍绑定同一个运行中 turn 时才能中止该 turn，绝不能中止后续 turn。

## 验证重点

- shared daemon 的 thread notification 必须严格按 thread 隔离。
- `/history` 即使本地 History 非空或 backend 读取失败，也不能使用本地内容替代。
- 未授权用户在 backend 查询前即被拒绝。
- `/track` 后发送普通飞书消息仍能收到正常回复。
- 中止 action 必须携带原 session key，并只发送卡片绑定的精确 thread/turn ID。
- external/unknown unsolicited turn 不发送平台消息、不写本地 History；其他 Agent 的
  默认 unsolicited 行为不变。
- prompt/response 受飞书卡片载荷上限约束；超长内容应明确截断，不能导致卡片发送
  失败或泄露被禁止的工具详情。
