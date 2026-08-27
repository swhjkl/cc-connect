# `/process` 按需查看 Codex 长任务进展设计

**日期：** 2026-08-26
**状态：** Superseded
**目标后端：** Codex app-server daemon
**目标命令：** `/process`
**目标基线：** `origin/ws/feat/codex-app-server-daemon`
**已验证 Codex CLI：** `0.149.1`

> [!WARNING]
> 当前 shared daemon adapter 已确认存在跨 thread notification 串流问题；本设计的 live
> tracker 必须在该问题修复后才能启用。详见
> [Codex shared daemon 跨 session 串消息问题记录](./2026-08-26-codex-shared-daemon-cross-session-leak.md)。

> [!NOTE]
> 本文保留为早期 `/process` 方案记录。最终命令、自动更新卡片、精确中止和
> `/history` 权威数据源以
> [`/track` 与 Codex 权威历史实现设计](./2026-08-27-codex-track-history-design.md)
> 为准；不会注册 `/process` 兼容别名。持续 mirror 和正常对话方案见
> [`Codex 外部 turn 统一飞书镜像设计与评审`](./2026-08-27-codex-continuous-track-mirror-design.md)。
> 后者已用本地 Codex `0.150.1` schema 重新验证：`turn/steer(expectedTurnId)` 已进入
> stable schema；schema 只有 queue 相关通知，没有可由客户端调用的
> `thread/queue/add` / `thread/queue/start` 请求。因此本文基于 `0.149.1` 得出的“没有精确
> steer”已过时，但“没有权威 queue 请求”仍成立。

## 背景

cc-connect 支持连接 Codex app-server daemon。终端 Codex TUI、cc-connect 和其他客户端可以连接同一个 daemon，并使用同一个 Codex thread。

典型使用场景是：用户在终端 TUI 中发起构建、测试、代码修改等长程任务，随后离开电脑，希望从飞书快速确认任务是否仍在执行、当前进行到哪一步、是否正在等待审批，而不必等到整个 turn 完成。

当前 cc-connect 会在观察到外部客户端的最终回复后将结果转发到飞书。产品决策调整为：外部客户端发起的 turn 默认不再自动推送任何输入、过程或最终回复；用户通过 `/process` 查看进行中状态，通过 `/history` 查看已经完成的对话。由飞书在 cc-connect-owned thread 发起的 turn 仍按现有行为正常回复；共享 thread 则必须先满足下文的原子所有权前提。

默认镜像容易产生以下问题：

- 长任务会产生大量飞书消息或频繁更新卡片，干扰正常对话。
- 终端 prompt、命令、文件路径和工具输出可能包含敏感信息。
- app-server 通知没有 replay cursor，断线后无法无损恢复流式 delta。
- 协议只能可靠区分“cc-connect 发起”和“其他客户端发起”，不能证明其他客户端一定是 TUI。

因此，本需求采用“默认不镜像、用户按需查询”的产品形态。

## 命名

- `/process` 是本需求的正式命令，含义是查看当前 Codex turn 的执行进展。
- 首版 `/process` 不接受参数；携带 session ID、thread ID 或其他参数时返回用法提示，不执行查询。
- `/process` 不是操作系统进程列表，也不用于终止或控制进程。
- 现有 `/ps` 语义保持不变，不能用它代替 `/process`。
- `/progress` 可以在以后作为别名讨论，但不属于首版验收范围。

## 现状与问题

### 1. 现有命令看不到当前 turn

- `/status` 展示项目、Agent、工作目录、cc-connect uptime、模式和 session 元数据，不能判断外部 Codex turn 是否正在执行。
- `/current` 展示当前 session 名称和 Agent Session ID，不展示 turn 状态。
- `/history` 面向已经完成的对话，不适合查看尚未完成的工具调用和等待状态。
- `/ps <text>` 是向 cc-connect 自己正在运行的 turn 追加指令的 steer 命令，不是状态查询。它依赖本地 `Session.Busy()`；终端 TUI 发起的外部 turn 不会设置这个标记，因此 `/ps` 会拒绝请求，也不能用于查看或控制外部 turn。

### 2. 发送普通消息询问进度不可靠

“现在做到哪了”这类普通消息会进入 Agent 消息路径，而不是只读状态查询。对于 cc-connect 自己发起的 turn，它可能被排队到任务完成后；对于外部 TUI turn，cc-connect 还可能因没有感知本地 busy 状态而尝试并发 `turn/start`。

查看进度不应该创建新 turn、追加 prompt、改变 session 锁或与正在执行的任务竞争。

### 3. cc-connect 已收到部分事件，但没有可查询状态

当前 Codex app-server adapter 能接收 `turn/started`、`item/started`、`item/completed`、`turn/completed` 等通知。外部 turn 的 unsolicited reader 会记录工具名称，并在 `EventResult` 后转发最终回复，但不会保存一份供命令读取的结构化进度快照。

同时，当前初始化主动关闭了 agent message delta、reasoning delta 和部分工具输出 delta。依赖流式 delta 会扩大改动范围，并且仍然无法解决断线重放问题。

### 4. `/history` 不能作为 `/process` 的数据源

当前 `/history` 只有在 cc-connect 本地历史为空时才回退读取 Codex transcript；一旦本地已有飞书消息，外部 TUI turn 可能不会被合并进结果。此外，Codex JSONL reader 的单行上限为 256 KiB，遇到更大的工具记录时会停止扫描，后续消息可能缺失。

这些问题需要独立修复，但 `/process` 不应依赖 `/history` 或 JSONL 扫描结果。

## 目标

首版 `/process` 必须满足：

1. 即使当前 session 正在执行长任务，也能立即进入命令处理路径，不等待 turn 完成。
2. 只读取当前映射的 Codex thread，不创建 turn、不追加输入、不审批、不取消任务。
3. 展示当前状态、运行时长和最后更新时间；在协议数据可用且内容符合固定安全白名单时展示最近动作类型和审批等待状态，否则明确标记内容已隐藏、不可用或未知。
4. 同时支持 cc-connect 发起的 turn 和同一 daemon/thread 上由外部客户端发起的 turn。
5. daemon 短暂断连时返回带有“可能过期”标记的最近缓存，而不是伪装成实时数据。
6. 外部客户端发起的 turn 默认不向飞书自动推送输入、工具进展、流式文本或最终回复；飞书在 cc-connect-owned thread 发起的 turn 正常回复。
7. 对自由文本采用 fail-closed 策略，不展示 raw reasoning、prompt 正文、命令参数、完整工具输出、密钥或路径。

## 非目标

- 不提供终端输入和输出的自动镜像开关；首版固定为按需查询。
- 不提供逐 token 或逐字流式文本回放。
- 不展示模型内部 reasoning。
- 不通过 `/process` 批准、拒绝、取消或 steer 当前 turn。
- 不保证识别外部客户端的具体类型；只能显示“外部客户端”。
- 不替代 `/history`；任务完成后的完整对话仍由 `/history` 负责。
- 首版不支持通过任意 thread ID 查询其他 session。

## 用户体验

### 运行中

```text
⏳ Codex 任务运行中 · 12m34s
来源：外部客户端
当前请求：有新请求（内容已隐藏）
最近动作：Bash
动作状态：运行中
最后更新：18 秒前
等待审批：否
数据状态：实时
```

### 等待审批

```text
⏸️ Codex 任务等待审批 · 3m12s
来源：飞书
当前请求：有新请求（内容已隐藏）
审批动作：Bash
最后更新：5 秒前

请在发起该 turn 的客户端完成审批。
```

`/process` 只报告等待状态，不代表观察连接能够接收或处理原始审批请求。

### 空闲

```text
✅ 当前没有运行中的 Codex 任务
最近完成：2 分钟前
最后结果：已完成（内容请使用 /history 查看）

使用 /history 查看已完成对话。
```

### 数据暂时不可用

```text
⚠️ 无法连接 Codex daemon
以下是 47 秒前的缓存状态，可能已过期：
状态：运行中
最近动作：Patch
```

如果从未取得过有效快照，则返回明确的“暂时无法获取进度”，不能把未知状态显示为空闲。

## 状态模型

状态拆成三个正交维度，避免“失败后已经空闲”和“运行中正在等审批”互相覆盖。

| 字段 | 取值 | 含义 |
| --- | --- | --- |
| `activity_state` | `idle` | 当前没有活跃 turn |
|  | `running` | 当前存在一个活跃 turn |
|  | `waiting_approval` | 当前 turn 明确处于等待审批；显示优先级高于 `running` |
|  | `waiting_input` | 当前 turn 明确等待发起客户端继续输入 |
|  | `not_loaded` | thread 当前未加载 |
|  | `system_error` | daemon 报告 thread system error |
|  | `unknown` | 无法可靠判断当前是否活跃 |
| `last_outcome` | `completed` | 最近 turn 正常完成 |
|  | `failed` | 最近 turn 失败 |
|  | `interrupted` | 最近 turn 被中断或取消 |
|  | `unknown` | 没有可信的最近结果 |
| `freshness` | `live` | 已有 snapshot 基线，现有 AgentSession 通知连接在线且未检测到丢事件 |
|  | `snapshot` | 本次持久化快照读取成功，但没有实时事件覆盖 |
|  | `stale` | 本次无法刷新，只能返回上次成功的数据 |
|  | `unknown` | 从未取得可信数据 |

审批状态另存为 `none`、`waiting` 或 `unknown`。只有 daemon 的 `thread/status/changed.activeFlags` 明确提供 `waitingOnApproval` 时，才把 `activity_state` 提升为 `waiting_approval`；`waitingOnUserInput` 映射为 `waiting_input`。相关标志不可用时保持 `running`，并显示“审批状态：未知”。进度 tracker 不根据工具长时间无更新来猜测审批或等待输入状态。

如果快照异常地显示同一 thread 有多个活跃 turn，返回 `activity_state=unknown` 并触发 resync，不能任意挑选一个 turn 展示。

每份进度快照至少包含：

- thread ID、turn ID 和 item ID，仅供内部去重，不默认展示。
- activity、last outcome、approval 和 freshness 状态及其原因。
- 来源：`cc-connect`、`external_client` 或 `unknown`。
- 当前用户输入是否存在；正文默认不展示。
- turn 开始时间、完成时间和计算出的运行时长。
- 最近 item 的固定安全类型、状态和更新时间；自由文本参数默认不展示。
- 最近一条 agent message 是否已经完成；正文由 `/history` 的受控私聊入口负责。
- 是否等待审批，以及可安全展示的固定工具类型。
- 最后成功刷新时间和最后活动时间。数据年龄按最后成功刷新时间计算；长工具调用没有新事件时，不能仅因此把在线数据标记为 stale。

来源判定规则：cc-connect 为自己发起的 turn 写入可持久化 client marker（优先使用协议的 `clientUserMessageId`/user message client ID），并保存近期 turn ID 和发起平台。只有存在正向证据时才标记为 cc-connect 或 `external_client`；通知与 `turn/start` response 乱序、进程重启或外部 steer 导致证据不足时标记为 `unknown`。不能使用“未记录为本地 turn”这一排除法，也不能将 `external_client` 展示成“终端 TUI”。

## 前置条件

当前部署分支 `origin/ws/feat/codex-app-server-daemon` 已实现通过 Unix Socket 连接共享 app-server daemon；上游 `main` 仍会为每个 Codex AgentSession 启动独立 app-server 进程。因此：

- 本设计以当前部署分支为基线；若向 `main` 单独移植，必须先合入 shared daemon transport。
- 终端 TUI 和 cc-connect 必须连接同一个 daemon endpoint，并打开同一个 thread。不同 app-server 进程之间没有实时可见性。
- daemon WebSocket/remote-control 仍是实验能力，升级 Codex CLI 时必须重新执行双客户端和审批路由测试。
- 保留任何共享 thread 的订阅连接前，必须验证 approval、elicitation 和 dynamic tool 等 server request 只路由给 turn owner。owner routing 是订阅模式的硬前提，而不只是 live tracker 的优化项。
- 在共享 thread 上允许飞书普通消息前，daemon 必须提供原子的 `start-if-idle`、ownership lease 或语义等价能力：当另一客户端已经抢先开始 turn 时，请求必须失败，绝不能变成 steer。Codex CLI `0.149.1` 的 `turn/start` 会进入 `start_or_steer_turn`，且响应不区分 started/steered；仅在发送前读取 ThreadStatus 不能消除 TOCTOU，因此不能作为安全保证。

如果 owner routing 测试失败，cc-connect 必须主动关闭并取消该共享 thread 的所有被动 `thread/resume` 连接，只能使用 initialize-only 的非订阅 snapshot 查询。此模式只允许 `/process` 等只读操作；普通飞书消息必须拒绝，或由 `/new` 明确创建独立的 cc-connect-owned thread，不能在被观察的 thread 上发送。不能只关闭 tracker、却继续保留可能抢答 server request 的订阅连接。

如果 owner routing 安全、但 daemon 没有原子写入所有权能力，可以保留只读 live 观察，但被观察的共享 thread 仍必须处于 observer-only 模式。完整的“同一 thread 可查询也可从飞书继续对话”只有在两项能力都通过集成测试后才能启用。

| 运行模式 | owner routing | 原子 start/lease | 共享订阅 | `/process` | 共享 thread 普通消息 |
| --- | --- | --- | --- | --- | --- |
| full shared | 已验证安全 | 已验证可用 | 允许 | live + snapshot | 允许，冲突时原子失败 |
| observer-only | 已验证安全 | 不可用 | 允许只读观察 | live + snapshot | 拒绝；`/new` 创建独立 thread |
| snapshot-only | 不安全或未知 | 任意 | 禁止并主动关闭 | initialize-only snapshot | 拒绝；`/new` 创建独立 thread |

部署前提和当前配置见 [Codex daemon + Feishu server](../codex-daemon-feishu-server.md)。

## 方案

采用“非订阅持久化快照为事实源 + 现有 AgentSession 通知入口提供实时覆盖”的混合方案。首版不新建独立 observer，也不额外调用 `thread/resume` 订阅 thread。

```text
Codex TUI / cc-connect / 其他客户端
                  │
                  ▼
        同一个 app-server daemon thread
                  │
        ┌─────────┴────────────────┐
        │ 持久化 thread/turn/items │ 现有 AgentSession notifications
        ▼                          ▼
 non-subscribing snapshot     ingress progress tracker
        │                          │
        └────────────┬─────────────┘
                  ▼
          per-thread progress cache
                  │
       /process ──┴──> 文本或状态卡片
```

### 1. Core 使用可选能力接口

在 `core/interfaces.go` 定义 Agent 可选能力，例如：

```go
type ThreadProgressProvider interface {
	GetThreadProgress(ctx context.Context, binding ProgressBinding) (*ThreadProgress, error)
}
```

`ThreadProgress` 使用通用的 turn/item/status 字段，不能包含 Codex 专属 JSON。这样 `core` 不需要判断 Agent 名称，其他 Agent 以后也可以实现相同能力。

`ProgressBinding` 由 Engine 在管理员鉴权成功后，从当前 session 解析生成，包含不可由聊天参数覆盖的 Agent Session ID、daemon endpoint identity、标准化 workspace 和内部 session binding。cwd 校验只是纵深防御，不能作为授权依据。鉴权必须先于 session/thread 解析，避免未授权用户通过不同错误文案探测 thread 是否存在。

不支持该接口的 Agent 返回本地化的“不支持实时进度查询”，而不是把 `/process` 当作普通 prompt 传给 Agent。

### 2. `/process` 在 session lock 之前执行

`core/engine.go` 已在 `Session.TryLock()` 之前分发已识别 slash command。将 `/process` 注册为内置命令并沿用该路径，可以保证：

- 长任务运行时仍能处理 `/process`。
- 不进入普通消息队列。
- 不修改 `Session.Busy()`。
- 不调用 `turn/start`。

前台命令总预算最多 2 秒：有 fresh cache 时立即返回；需要同步读取 snapshot 时，超过预算就返回 stale cache 或 `unknown`。5 秒超时只用于后台 refresh/resync，不能阻塞 `/process` 前台回复。

### 3. 非订阅 Codex 快照读取

每次执行 `/process` 时，Codex provider 尝试读取当前 thread 的持久化状态：

1. 先使用便宜的 `thread/read(includeTurns=false)` 读取 ThreadStatus，确定 `notLoaded`、`idle`、`systemError` 或 `active(activeFlags)`。
2. 再 best-effort 调用 `thread/turns/list`，只取最近一个 turn；需要最近工具详情时再调用 `thread/items/list`。这些分页接口属于 experimental API，收到 method-not-found 时降级为 status-only。
3. 只有 legacy daemon 明确支持时才调用 `thread/read(includeTurns=true)`，并设置响应大小和时间上限；分页 history 模式可能拒绝该调用。
4. 只读取 Engine 提供的当前 session binding，不接受用户提供任意 thread ID。
5. 对 cwd 做 clean、符号链接解析和项目边界校验；不一致时返回 `unknown` 并记录不含内容的诊断日志。

snapshot reader 复用现有健康 RPC 连接，或建立只完成 `initialize` 的短连接。短连接禁止调用 `thread/start`、`thread/resume` 或响应任何 thread-scoped server request；收到意外 server request 时立即关闭并降级。这样读取快照不会新增订阅，也不会参与 TUI 的审批。

持久化快照负责 activity 和 turn 终态的最终一致性。ThreadItems 是有损投影，可能没有断线期间或尚未完成的工具详情，因此最近动作类型、prompt 是否存在和 agent message 是否完成均是 best-effort 字段；自由文本仍按 fail-closed 策略隐藏，不可用时明确显示“无可用详情”，不能把详情列为状态收敛保证。

provider 在 `initialize` 后记录 daemon 能力矩阵，并对 method-not-found 做运行时降级：分页 turn/item API 可用时优先使用；否则尝试 legacy `thread/read`；审批状态标志不可用时将审批状态设为 `unknown`。不能根据 Codex CLI 版本字符串直接假定能力。

### 4. 在现有通知入口更新 live cache

daemon transport 的 `appServerSession` 已因正常 `thread/start`/`thread/resume` 订阅当前 thread。实时 tracker 直接在 `handleNotification` 入口解析通知，并在事件转成 `core.Event` 之前更新独立、并发安全的 progress cache：

1. `thread/status/changed` 更新 activity 和 active flags。
2. `turn/started`/`turn/completed` 更新 turn ID、时间和终态。
3. `item/started`/`item/completed` 解析 `userMessage` 是否存在，以及 command execution、file change、MCP 和 web search 的固定白名单类型与状态；不缓存自由文本参数。
4. notification ingress 只写 cache；`/process` 只读 cache，不消费 `AgentSession.Events()`，因此不会与 foreground event loop 或 unsolicited reader 抢事件。
5. 首版继续关闭 agent message delta、reasoning delta 和工具 output delta；completed `agentMessage` 只用于记录“已有最终回复”和终态，不缓存或展示正文。

progress cache 归 Agent/daemon endpoint 所有，并按 binding/thread 隔离。首次 snapshot 基线尚未建立时只能返回 `snapshot` 或 `unknown`，不能声称 `live`。容量和生命周期规则如下：

- 已确认 active 的 entry 由通知、成功 snapshot 或无断点的健康 live 连接持续确认；健康连接本身算持续确认，因此长工具调用没有新 item 事件时不会仅因静默而过期。只要最近 5 分钟内仍有上述活跃确认，就不按普通 inactivity TTL 淘汰。
- active entry 在没有健康 live 连接、通知或 snapshot 的情况下连续 10 分钟未确认时，先降级为 `stale/unknown`，然后允许按最近确认时间淘汰，不能无限保留旧 `running`。
- idle/completed entry 的 TTL 为 1 小时；session 删除或解绑时立即清理。
- 每个 endpoint 硬上限为 128 个 entry，任何情况下都不能突破。如果达到上限，先淘汰 idle LRU，再淘汰 stale 且最久未确认的 entry；如果全部都是近期确认的 active entry，对新 thread 只返回无详情的 capacity/unknown 状态并触发 resync，不能继续缓存正文或无界增长。

“健康 live 连接”必须是可测量状态：WebSocket 仍打开，最近 60 秒内 transport ping/pong 或等价轻量 RPC 成功，并且没有 notification overflow、解析错误或 daemon epoch 变化。仅仅“尚未读到 socket error”不能视为健康。

### 5. 事件归并与断线恢复

app-server 通知没有 sequence、cursor 或 replay token，不能假设 exactly-once。缓存按 `(threadID, turnID, itemID)` 幂等 upsert：

- `item/completed` 是 item 的最终权威状态，不能被迟到的 `item/started` 回退。
- `turn/completed` 是 turn 的最终权威状态。
- snapshot 开始前记录 cache generation；读取期间到达的 notification 在归并时优先，避免旧 snapshot 覆盖新事件。
- 收到 `turn/completed` 后再做一次异步快照校验。
- AgentSession 连接断开时立即取消 `live`；本次 snapshot 成功则标记 `snapshot`，只有无法刷新、使用旧缓存时才标记 `stale`。
- 如果检测到事件通道溢出或无法解析的状态转换，触发 snapshot resync，不能静默丢失后继续声称“实时”。

缓存 key 至少包含 daemon endpoint identity、workspace/session binding 和 thread ID。daemon 重启后，如果磁盘记录仍是 in-progress，但当前 daemon epoch 没有活跃状态确认，则显示 `activity_state=unknown`，不能永久沿用旧的 `running`。

首版不新增常驻 reconnect supervisor，也不承诺 daemon 恢复后在无人查询时自动收敛。下一次 `/process` 会在 2 秒前台预算内尝试刷新；若未完成，则先返回 stale/unknown 并启动单次最长 5 秒的后台 snapshot。后台成功后，下一次查询必须返回新状态。

### 6. 请求所有权和外部活跃 turn 保护

共享 daemon 后，本地 `Session.Busy()` 不能代表 thread 是否被外部客户端占用。实现 `/process` 时必须同步修正以下行为：

- cc-connect 发起 turn 时传递并持久化 client marker，记录返回的 turn ID、平台和 session generation。
- ThreadStatus preflight 只用于反馈更清楚的错误，不能充当并发保护。共享 thread 的普通消息必须通过 daemon 的原子 `start-if-idle`/ownership lease 提交；所有权冲突时明确失败并提示使用 `/process`，不能调用可能 steer 的 `turn/start`。
- daemon 不具备原子能力时，将共享 thread 标记为 observer-only：拒绝其普通消息，并引导用户使用 `/new` 创建独立的 cc-connect-owned thread。不能用本地 mutex 解决跨 TUI 进程的竞态，也不能接受“先检查再发送”的时间窗口。
- `/ps` 保持“向 cc-connect 自己正在运行的 foreground turn 追加指令”的现有语义；外部 turn 不允许使用 `/ps`。
- Codex adapter 必须在通知入口为 Text、Tool、Result、Error、permission 等全部 `core.Event` 固化不可变的 thread ID、turn ID、origin 和 session generation，随后一路传递到平台发送决策；不能在消费事件时查询可变 cache 推断来源。只有确定属于 cc-connect-owned turn 的 permission event 才能处理；external/unknown request 不响应。
- 集成测试必须证明 daemon 将 server request 定向给 turn owner。如果请求会广播，立即关闭/取消共享 thread 的订阅 AgentSession，只提供 initialize-only 的非订阅 snapshot；该 thread 的普通消息禁止或分叉到独立 thread，不能靠“忽略请求”冒险上线。

`/process` 在 session lock 外执行，可能与 `/switch`/`/new` 并发。handler 捕获 session generation 和 binding，发送回复前再次核对；绑定已变化时丢弃旧结果并重试一次，不能把旧 thread 状态回复到新 session。

### 7. 输出渲染

Engine 先构造平台无关的状态内容：

- 支持结构化卡片的平台显示状态卡片。
- 其他平台使用紧凑纯文本。
- 卡片只在用户调用 `/process` 时发送；不会在后台持续更新。
- `/process` 可以配置到飞书机器人悬浮菜单，点击后仍走相同只读命令路径。

所有用户可见字符串必须进入现有 i18n，覆盖 English、简体中文、繁体中文、日文和西班牙文。

### 8. 关闭外部 turn 的默认镜像

当前 unsolicited reader 会在外部 turn 的 `EventResult` 到达后主动向平台发送最终回复。实现 `/process` 时应同时调整这条路径：

- cc-connect 自己从飞书或其他平台发起的 foreground turn 继续按现有方式发送过程卡片和最终回复。
- 标记为 `external_client` 的 turn 只更新 progress cache 和 Codex 持久化历史，不调用平台 `Send`/`Reply`。
- 来源为 `unknown` 的 turn 同样不自动发送；只有能够证明由当前 foreground 平台发起的 turn 才允许自动回复。
- Text、Tool、Result、Error 等事件必须携带通知入口固化的 thread ID、turn ID、origin 和 session generation；抑制决策只能使用这份不可变元数据，不能在事件消费时读取“当前 turn”进行猜测。
- 不新增 `mirror_external_*` 配置项；首版没有自动镜像模式。
- `/process` 是进行中状态的唯一主动入口，`/history` 是已完成内容的主动入口。关闭外部最终回复镜像前，`/history` 必须先通过以下发布门槛：
  - 合并 daemon 的权威 thread history，可靠包含外部 turn；修复本地历史非空时不合并和 JSONL 大行截断问题。
  - 外部或来源未知的 thread 内容仅允许 `admin_from` 用户读取；完整正文只在可确认的私聊中展示。
  - 群聊或会话类型未知时只返回 turn 时间、状态等无内容元数据，不能导出 prompt、命令或 agent 回复。
  - 使用与 `/process` 相同的边界、脱敏、日志和错误降级策略。

上述任一条件未满足时，不得关闭现有外部最终回复路径；这是一项 rollout gate，而不是可在上线后补做的优化。

不能全局关闭 unsolicited reader 的既有行为，因为其他 Agent 和后台任务仍可能依赖它。抑制逻辑必须基于 Agent 可选能力或可靠的 turn 来源元数据，不能在 `core` 中硬编码 Codex 名称。

## 权限与隐私

首版按敏感诊断命令处理：

- `/process` 仅允许 `admin_from` 用户执行。
- `admin_from` 为空时 `/process` 默认禁用，不能回退为 `allow_from` 或所有允许使用机器人的用户。
- 只能读取当前消息所映射 session 的 thread，禁止指定其他 session/thread ID。
- 群聊中只展示状态、耗时、固定工具类型和更新时间，不展示输入、命令、文件名、URL 或 agent 回复。私聊也遵循下述 fail-closed 内容策略。
- 原始 snapshot/event 必须先经过按 item 类型的 allowlist 和 sanitizer，再写入 cache；cache 只保存已脱敏、有界的结构化字段，不能暂存原始 prompt、agent 文本、命令或工具参数。
- 自由文本默认一律隐藏，包括 prompt、agent reply、shell 参数、HTTP header、URL、位置参数、文件名和文件路径。首版只显示固定白名单枚举，例如 `Bash`、`Patch`、`MCP`、`Web Search` 及其 started/completed 状态；未知类型统一显示“工具调用”，不能回退展示原文。
- 只有安全性可由结构化协议字段和固定 allowlist 证明的值才能进入摘要。长度截断和正则替换不能把任意自由文本变成可信安全内容；即使以后增加摘要，也必须先移除 URL userinfo/query/fragment、环境变量值、header、路径和参数，并在无法证明安全时完全隐藏。
- 不展示完整命令输出、环境变量值、绝对路径、URL 查询凭据、token、secret、password 或 API key。
- 不展示 raw reasoning、reasoning summary delta、系统 prompt 或审批原始参数。
- progress cache 只保存在内存中，不创建新的完整 transcript 副本；每个 entry 最多保存当前 turn 的结构化安全字段和最近一次完成状态。active/stale/idle 的过期规则以及 endpoint 级 128 条硬上限以“在现有通知入口更新 live cache”一节为准。
- 日志只记录 thread/turn 的短 ID、状态、延迟和错误类型，不记录 prompt、命令正文或工具输出。

当前通用 `core.Message` 没有会话类型字段。实现时需要增加平台无关的 conversation scope 元数据，并由飞书等已知会话类型的平台填充；如果平台无法可靠判断私聊与群聊，首版采用更安全的降级输出，只显示不含内容的状态字段。

## 失败与降级行为

| 场景 | 行为 |
| --- | --- |
| Agent 不支持进度接口 | 返回“当前 Agent 不支持 `/process`” |
| session 尚未绑定 thread | 返回“当前会话尚无 Codex thread” |
| thread 不属于当前 workspace | 返回安全错误，不展示任何内容 |
| daemon 暂时不可用且有缓存 | 返回缓存并明确标记过期时间 |
| daemon 暂时不可用且无缓存 | 返回“暂时无法获取”，不能显示 idle |
| live AgentSession 断开，本次 snapshot 成功 | 返回 `freshness=snapshot` |
| live AgentSession 断开且 snapshot 失败 | 返回旧缓存并标记 `freshness=stale`，或返回 `unknown` |
| daemon 无可靠审批状态 | 保持 `activity_state=running`，显示“审批状态：未知” |
| TUI 使用独立 app-server 进程 | 说明实时观察不可用，只展示共享 daemon 可见的持久化状态 |

实时观察的前提是终端 TUI 与 cc-connect 确实连接同一个 daemon，并打开同一个 thread。不同 app-server 进程之间不共享实时订阅。

## 改动范围

预计涉及：

- `core/interfaces.go`
  - 新增通用进度 provider 和数据结构。
- `core/message.go` 及相关平台 adapter
  - 增加可选的私聊/群聊/未知 conversation scope 元数据，用于安全降级。
- `core/engine.go`
  - 注册、鉴权和处理 `/process`。
  - 生成纯文本与结构化卡片内容。
  - 使用事件携带的不可变 ownership 元数据抑制外部 Codex turn 的过程和最终回复。
  - 修复 `/history` 的 daemon/local 合并、管理员鉴权、私聊正文限制和群聊降级，作为关闭外部镜像的发布门槛。
- `core/session.go`
  - 持久化有限的 cc-connect turn ID、发起平台和 daemon identity，不持久化 prompt 或工具摘要。
- `core/i18n.go`
  - 增加命令描述、状态、错误和 freshness 文案。
- `agent/codex/`
  - 新增 daemon snapshot reader、notification ingress tracker、缓存和归并逻辑。
  - 增加来源 marker、ThreadStatus 映射、全事件 ownership 元数据和 request owner 保护。
  - 探测原子 start/lease 能力；不具备时强制共享 thread 使用 observer-only 模式。
  - owner routing 不安全时关闭共享订阅连接，并禁止在被观察 thread 上发送普通消息。
- `docs/usage.md`、`docs/usage.zh-CN.md`
  - 增加命令用法、权限、共享 daemon 前提和隐私说明。
- 测试文件
  - 增加 core 命令测试、Codex progress tracker 单元测试和双客户端集成测试。

不需要新增飞书事件或 OpenAPI；现有 slash command 和卡片能力可以承载输出。飞书 adapter 只需把已有的 `chat_type` 映射到通用 conversation scope。

## 实施顺序

1. 在锁定的 Codex CLI 版本上验证双客户端通知、server request owner routing 和原子 start/lease 能力；按结果确定 full shared、observer-only 或 snapshot-only 模式，不能默认假定能力存在。
2. 定义通用 `ThreadProgressProvider`、状态模型和 i18n 文案。
3. 实现 `/process` 分发、权限、2 秒前台预算和 unsupported 分支。
4. 实现非订阅 snapshot reader，先覆盖 activity、last outcome 和 freshness。
5. 在现有 `appServerSession.handleNotification` 入口实现实时 cache、来源 marker 和归并。
6. 为全部事件固化 ownership 元数据；接入原子写入门槛、observer-only 降级和 unsolicited approval 的 ownership 行为。
7. 增加卡片渲染、群聊降级和内容脱敏。
8. 修复 `/history` 外部 turn 完整性、权限和会话范围后，通过发布门槛再关闭外部最终回复自动镜像。
9. 更新用户文档，并将 `/process` 加入可配置的飞书快捷菜单建议。

`/history` 的 JSONL 大行、本地/Agent 历史合并、管理员鉴权和私聊/群聊降级必须作为独立改动完成，不能用 `/process` 的内存缓存替代。`/process` 本身可以独立开发，但在关闭外部最终回复自动镜像之前，上述完整性与隐私测试必须全部通过，否则用户既没有可信的按需查看入口，也可能从共享群聊导出外部内容。

## 测试计划

### Core

- session 已被 foreground turn 锁定时，`/process` 仍立即返回，且不进入消息队列。
- `/process` 携带任何参数时返回用法提示，不解析或探测参数中的 session/thread ID。
- `/process` 不调用 `AgentSession.Send`、`turn/start` 或 permission response。
- 不支持 provider、无 thread、超时、有缓存和无缓存的输出正确。
- 非管理员被拒绝，不能看到任何进度字段。
- 私聊显示白名单结构化字段；群聊或未知聊天类型只显示降级字段。
- `/history` 对外部/unknown 内容执行管理员鉴权；管理员私聊可读取受控正文，非管理员、群聊和未知 scope 只能获得无内容元数据。
- 所有状态和错误文案覆盖全部支持语言。

### Codex provider

- 客户端 A 订阅 thread，客户端 B 发起 turn；A 能观察 user item、tool item 和完成事件。
- approval、elicitation 和 dynamic tool server request 只投递给发起 turn 的客户端；若测试失败，关闭/取消共享 thread 的订阅连接，强制使用 initialize-only snapshot，并禁止该 thread 的普通消息。
- 原子 start/lease 竞态测试中，客户端 B 在客户端 A preflight 后抢先启动 turn；A 的请求必须失败且 B 的 turn 不出现 A 的输入。若 daemon 不支持该语义，共享 thread 强制为 observer-only，普通消息只能拒绝或在明确的新 thread 中执行。
- cc-connect 发起的 turn 标记为对应平台；在线确认的其他 turn 标记为 `external_client`；重启后无法证明来源的 turn 标记为 `unknown`。
- 重复、乱序的 started/completed 事件按 ID 幂等归并，完成态不会回退。
- live AgentSession 在 turn 中途断线后，通过非订阅 snapshot 恢复 activity/终态；工具详情允许明确降级为 unavailable。
- paginated history 和 legacy history 两种 daemon 能力路径均有覆盖。
- 慢消费者或缓存溢出触发 resync。
- cache 达到 128 条硬上限时不再增长；按 idle、stale 顺序淘汰，全为近期 active 时不保存新详情并返回 capacity/unknown。
- external/unknown turn 的审批请求不会被 cc-connect 自动批准或拒绝。
- daemon 不提供可靠审批标记时，任务保持 running 且审批状态为 unknown。
- 健康 live 连接上的静默工具调用持续超过 10 分钟时仍保持 live/running；heartbeat 失败且 snapshot 也失败后才降为 stale/unknown，并进入可淘汰状态。
- daemon 重启后旧的 in-progress 快照不能永久显示 running；下一次 `/process` 在 2 秒内尝试同步刷新，必要时启动最长 5 秒的后台 snapshot，后台成功后的下一次查询显示新状态。
- 外部 turn 活跃时，普通飞书消息和 `/ps` 不会调用 `turn/start` 或 steer；用户收到 `/process` 引导。
- 相邻 turn 和乱序事件中，Text、Tool、Result、Error、permission 均使用入口固化的 thread/turn/origin/generation；不会误抑制飞书 turn，也不会把外部 turn 发到平台。
- `/process` 与 `/switch`/`/new` 并发时，不会把旧 binding 的状态回复到新 session。

### 安全与用户旅程

- prompt 中的 secret、Authorization header、命令位置参数、敏感文件名、环境变量值和 URL 凭据不会出现在回复、cache 或日志；未知类型不会回退展示自由文本。
- 原始 prompt 和工具参数不会写入 cache；session 删除、解绑或 TTL 到期后缓存被清理。
- 用户从 TUI 发起长任务，在飞书执行 `/process` 能看到运行状态和耗时；最近工具动作可用时展示，不可用时明确标记。
- turn 完成后再次执行 `/process` 显示 idle 和最近完成状态，并引导使用 `/history`。
- 整个外部 turn 期间，如果用户没有调用 `/process` 或 `/history`，飞书不会收到输入、过程或最终回复的自动镜像；cc-connect-owned thread 上由飞书发起的正常 turn 回复行为保持不变。
- 关闭外部最终回复镜像前，`/history` 的外部 turn 完整性、管理员私聊正文和群聊无内容降级测试全部通过。

## 验收标准

1. 在同一 daemon/thread 的外部 turn 运行期间，从飞书执行 `/process`，无需等待 turn 完成即可收到状态。
2. 前台命令在 2 秒内返回；超过前台预算时必须使用 stale cache 或明确返回 unknown。后台 refresh/resync 的单次超时为 5 秒。
3. 输出至少包含 activity、已运行时长、来源、最后更新时间和 freshness；最近动作属于 best-effort 字段，不可用时必须明确说明。
4. `/process` 不创建新 turn、不改变当前任务、不处理审批；cc-connect-owned 独立 thread 的正常回复不受影响。共享 thread 只有通过原子 start/lease 测试后才允许普通消息，否则明确使用 observer-only 模式。
5. 外部 turn 默认没有输入、工具进展、流式输出或最终回复的自动镜像，且首版没有开启镜像的配置项；只有 `/history` 完整性和隐私发布门槛全部通过后才能关闭现有最终回复路径。
6. daemon 断线、重启或 live tracker 丢事件时，状态使用明确的 `snapshot`、`stale` 或 `unknown` freshness；恢复后的下一次 `/process` 启动刷新，前台 2 秒和后台 5 秒预算结束后能够在后续查询中收敛 activity 和 turn 终态。
7. 未授权用户、错误 workspace 和其他 thread 无法获得 prompt、命令或路径信息。
8. raw reasoning、完整工具输出和敏感凭据不会出现在 `/process` 回复或日志中。
9. 任意客户端在 preflight 后抢先启动 turn 时，飞书输入不会被 steer 进去；没有原子所有权能力时，共享 thread 的写入功能不会上线。
10. owner routing 不安全时不存在共享 thread 的订阅连接；snapshot-only 客户端不调用 `thread/resume`、不响应 server request，普通消息不会写入被观察 thread。
