# Codex 外部 turn 统一飞书镜像设计与评审

**日期：** 2026-08-27

**状态：** Implemented — 本地回归通过，待部署冒烟

**目标后端：** Codex app-server daemon

**本地协议基线：** codex-cli `0.150.1` 生成 schema（未使用 Web）

**基线：** [`/track` 与 Codex 权威历史实现设计](./2026-08-27-codex-track-history-design.md)

## 最终决策

外部客户端在共享 Codex thread 中发起的 turn，不再使用一套专用的
tracker 卡片。它和飞书发起的 turn 进入同一套 turn 投递、流式卡片、工具面板、
正文、footer、按钮、截断和更新机制；差异只作为渲染元数据存在：

- 飞书发起的 turn 继续回复原始飞书消息，运行态保持现有蓝色样式；
- TUI 或其他客户端发起的 turn 主动发送一张同结构卡片，运行态使用紫色，标题带
  “外部会话”来源标识；
- 完成、失败和中止仍分别使用绿、红、橙/灰等状态色，来源通过标题或 badge 保留，
  不让颜色同时承担“来源”和“结果”两种语义；
- 外部 turn 没有对应的飞书 prompt 消息，因此卡片额外显示 prompt；飞书来源卡片不
  重复显示已经可见的 prompt；
- “全量 mirror”指和正常飞书对话相同的用户可见事件与展示策略，不是把 daemon 的
  JSON-RPC、reasoning、secret、环境变量和所有原始 tool output 裸转发到群里。

镜像默认开启。`/track on`、`/track off` 和 `/track status` 管理持久开关；裸
`/track` 保留“查看/刷新最新 turn”的兼容语义，但不得为已有主卡再创建一张重复卡。
`/history` 始终从 Codex 读取，cc-connect 不保存可替代 Codex 的对话正文。

方案不再建设第二套 tracker renderer，也不在连接健康时用 1.5 秒 snapshot 轮询驱动
动画。实时路径使用 daemon notification 映射出的规范化 turn 事件；Codex snapshot
只负责初次接管、断线补偿、缺口对账和 `/history`。

## 为什么这样更简单

复用的是完整的卡片生命周期，而不只是复用几段 JSON：

```text
Codex foreground AgentSession events ─┐
                                      ├─> TurnFrame ─> TurnCardDelivery
Codex external observer events ───────┘                   │
                                                          ├─ BuildRichCard
Codex snapshot reconcile ─────────────> 修正同一 TurnFrame │
                                                          ├─ SendPreviewStart
                                                          ├─ Update/CardKit stream
                                                          └─ finish + notification
```

两种来源共用：

- assistant commentary/final response 的累计规则；
- thinking、tool use、tool result、plan 等活动到 `ToolStep` 的转换；
- full/compact/quiet 等现有展示策略；
- rich card 大小预算、Markdown 清洗、图片处理和截断；
- `SendPreviewStart`、`MessageUpdater`、CardKit sequence、降级和 finalize；
- 中止按钮、footer、耗时和终态渲染；
- 更新节流、render hash 和平台失败处理。

两种来源只在以下字段上不同：

```text
TurnPresentation {
  source: foreground | external | recovered
  prompt placement: source_message | in_card
  reply target
  card variant: default | mirror
  terminal notification policy
}
```

实现上应从现有 foreground 流式路径抽出通用 `TurnCardDelivery`，让事件流和 snapshot
都更新同一个规范化 `TurnFrame`。不能把当前 `conversationTracker` 复制成另一个长期
renderer，也不能为了“共用”而伪造缺少 identity 的低层 `EventText`。

`RichCardSupporter.BuildRichCard` 现有 `title` 参数在 Feishu renderer 中没有真正参与
header 构造，且 `CardStatus` 只能表达状态色。实现来源样式时应增加平台无关的 card
variant/render options 能力，由 Feishu 将 `mirror + running` 映射为 purple；core 不得
硬编码 Feishu 名称或具体颜色。当前 `CardStatus`/`ProgressCardState` 也没有 interrupted
语义，不能继续把 interrupted 当作绿色 done；应由统一 render options 增加 warning/
interrupted 状态，再由 Feishu 映射为橙或灰。

## 开关、默认值与绑定

### 有效开关

每个飞书投递目标保存三态偏好：

```text
unset | on | off
```

- `unset` 继承项目配置，内置项目默认值为 `on`；
- `/track on` 写入显式 `on`；
- `/track off` 写入显式 `off`；
- 显式覆盖跨 cc-connect 重启、`/new`、`/switch` 和 thread 重绑保留；
- 项目级 `enabled = false` 是运维总开关，并覆盖 destination 偏好。

“默认开启”只对已经建立的稳定绑定生效：

```text
(platform instance, channel-scoped destination, workspace binding,
 daemon endpoint, active logical session/thread)
```

它不扫描本机所有 Codex thread，也不会猜测应该把一个无飞书绑定的 TUI 会话发送到
哪个群。一个 daemon thread 默认只能绑定一个自动镜像 destination；第二个群必须被
拒绝，或经过显式 takeover，防止同一份对话静默扩散。同一群若存在多个 participant-
scoped 活跃 session，启动恢复也不会按 map 顺序任选一个；它等待下一条明确绑定的消息
或管理员执行 `/track on`。

### 命令语义

| 命令 | 语义 |
| --- | --- |
| `/track` | 读取并刷新最新 turn。已有 primary 卡时复用它；没有时创建一次性视图，不改变持久开关。 |
| `/track on` | 持久开启当前 destination 的镜像；立即接管当前活动 turn，不补发关闭期间已完成的 turn。 |
| `/track off` | 持久关闭；冻结当前外部镜像卡，不中止 Codex turn，也不影响正常飞书来源回复。 |
| `/track status` | 显示 effective/default/override、当前绑定、最近 turn、恢复缺口和写入能力。 |

首次建立绑定、从 `off` 切到 `on`，或者旧版本升级后首次遇到 external turn 时：

- 已有活动 turn：创建或恢复这一 turn 的主卡；
- 只有已完成 turn：把最新完成 turn 设为 baseline，不补发旧历史；
- 后续处于 enabled 期间的 turn 必须连续投递；若服务离线后恢复，要从持久 watermark
  补齐离线窗口；
- 外部卡标题和 footer 必须明确提示“来自共享 Codex 会话”，使默认开启不是隐形行为。

`/track off` 的切点必须先持久化再停止 watcher。即使进程在回复命令前崩溃，重启后也
不能重新开启。`/track on` 则先持久化 `enabling` 和 baseline，再开始投递，避免 turn
恰好在启用窗口开始或结束造成漏投。

## 统一卡片体验

### 外观

| turn 来源/状态 | header | prompt | 其他区域 |
| --- | --- | --- | --- |
| 飞书、运行中 | 现有蓝色 | 原消息中可见，不重复 | 保持现状 |
| 外部、运行中 | 紫色 + “Codex · 外部会话” | 卡片内展示 | 与飞书来源相同 |
| 完成 | 绿色 + 来源 badge | 按来源规则 | 与飞书来源相同 |
| 失败 | 红色 + 来源 badge | 按来源规则 | 与飞书来源相同 |
| 已请求中止/中止 | 橙或灰 + 来源 badge | 按来源规则 | 与飞书来源相同 |

外部卡不另造 “Prompt/Response tracker” 布局。prompt 只是统一卡片 body 中的可选前置
section；其后的 thinking、tool panels、正文和 footer 与当前正常对话完全一致。

默认展示级别也沿用项目现有 progress 配置：

- `full`：展示正常飞书对话会展示的 thinking/tool/result 摘要和 response；
- `compact`：共用同一套折叠、最近 N 项和截断规则；
- `quiet`：只保留必要状态和最终答复。

因此开镜像不会把原来正常对话刻意隐藏的 reasoning、完整 shell output、环境变量或
secret 暴露出来。若以后提供“查看被省略内容”，它是统一卡片的可选 detail action：
点击时重新鉴权并从 Codex 按 item 读取、脱敏后发独立详情卡；不把原文隐藏在主卡
JSON 中，也不把它作为 MVP 的第二套渲染器。

### 通知

```text
external turn 首次出现
  -> 创建主卡（接受一次飞书通知）
  -> 原地 Patch 更新（不发送新消息）
  -> 写入终态卡
  -> 发送一次短终态通知
```

- 首卡通知不做额外规避；
- 过程中只能更新同一张卡，不发送“进度消息”；
- 默认 `notify = on_finish`，外部 turn 完成/失败/中止后发送一条短通知；
- 飞书 foreground turn 沿用当前完成 reaction/回复策略，mirror 不能叠加终态通知；
- 终态卡更新失败超过有限重试后，仍发送带“卡片更新失败”说明的终态消息，不能因卡片
  API 故障让用户永远不知道任务结束。

## 实时事件与 Codex 真值

### 实时主路径

observer 连接接收 external turn 的 daemon notification，但只把当前 foreground renderer
已识别的事件转换成带 identity 的规范化事件：

```text
TurnEventEnvelope {
  thread ID
  turn ID
  client user message ID, if present
  item ID / event sequence evidence
  event kind + sanitized structured payload
}
```

未知事件不展示 raw JSON，只触发 snapshot reconcile。事件进入 delivery registry 后：

1. 在项目独立的状态账本中按 `(threadID, turnID, destination)` 找到或 claim 唯一
   primary delivery；
2. 更新和 foreground 相同的 `TurnFrame`；
3. 仅当 frame/render hash 改变时调用统一 card updater；
4. 终态后冻结 delivery，不接受迟到的非终态事件回退状态。

不能直接把现有 `RelayUnsolicitedEvents` 设为 `true` 后无条件转发。原始事件流没有投递
目标、来源 owner、持久 cursor 和去重语义；它只能作为带 thread/turn identity 的输入，
必须先经过统一 registry。

observer 只订阅/读取，不调用 `thread/resume` 或 `turn/start`，不响应 thread-scoped
approval/server request，因此不会取得或冒充 foreground writer owner。`steer` 和
interrupt 是用户显式动作，由对应控制能力执行，不属于 observer 的被动读路径。

这里的 owner 只是“哪条投递链负责这张卡、哪条客户端连接负责 server request”的路由
身份，不是 daemon 的全局排他 writer lease。多个 TUI 仍可同时连接同一 thread；本方案
不增加一把把其他客户端挡在外面的写锁。

### snapshot 对账

Codex snapshot 仍是内容和终态的唯一权威来源，但不再是健康连接下每 1.5 秒刷卡的主
路径。以下情况必须读取 snapshot：

- watcher 首次绑定或 `/track on`；
- daemon 连接重建、事件缓冲溢出、发现未知/乱序事件；
- 活动 turn 长时间没有事件的 terminal watchdog；
- 服务重启后恢复 delivery；
- `/track`、`/history` 和可选详情读取。

由于 notification 没有可靠 replay cursor，enabled 状态要保存 turn watermark，并在重连
后从新到旧分页直到 watermark，再按旧到新补齐。只读取 latest turn 会漏掉断线期间快速
完成的多个 turn。

- watermark 使用稳定排序键和有界 recent turn ID set，不能只用秒级时间戳；
- 没找到 watermark 时继续分页；达到安全上限后标记 `gap/degraded` 并暂停推进，不能把
  最新 ID 直接写成新 watermark；
- 只有 turn 已被 foreground owner 或 mirror delivery 原子 claim 后才能推进 watermark；
- `thread/turns/list` 不可用时，只有 `thread/read(includeTurns=true)` 确认覆盖 watermark
  才能保持连续保证，否则 `/track status` 明确报告降级。

健康事件流下可以保留低频 watchdog/reconcile，但它用于发现缺口，不用于制作动画。

## 一 turn 一 owner 一主卡

复用卡片机制并不能自动解决重复回显；必须先解决 turn 来源分类。

### 飞书发起 turn

1. 调用 Codex 前创建 `pending_foreground` reservation，生成稳定
   `clientUserMessageId`，并关联源飞书消息、destination 和待创建/已创建的回复卡；
2. adapter 把 marker 传给 `turn/start`；响应中的确切 turn ID 必须返回
   core，reservation 原子升级为 `foreground_reply` owner；
3. observer 更早收到同一 turn 事件时先放入有界 pending buffer，不能因为暂时没有 turn
   ID 就创建 external 卡；
4. snapshot 中 `userMessage.clientId` 命中 reservation，或 turn ID 命中 foreground
   owner 后，事件只更新原回复卡；
5. 只有 marker/turn identity 明确不属于本地 reservation 时，才能 claim external owner。

本地 `0.150.1` schema 已显示：

- `turn/start`、`turn/steer` 接受 `clientUserMessageId`；
- 完整 turn 的 `userMessage` item 有可选 `clientId`；
- `turn/start` 响应包含确切 turn；schema 没有客户端可调用的 queue add/start 请求。

当前实现已把飞书 `MessageID` 作为 `clientUserMessageId` 传入 `turn/start`，并从 snapshot
中的 `userMessage.clientId` 与持久 foreground reservation 进行来源分类；事件先于 RPC
确认到达时也不会按 prompt 文本或固定延迟猜测来源。marker 的 daemon 重启 round-trip
仍应保留为部署冒烟项；无法回读时来源分类 fail closed，不创建重复 external 卡。

### 外部 turn

没有 foreground reservation 的 turn 以 `external + mirror` 原子 claim。多个 watcher、
重复 notification、snapshot reconcile 和重启恢复都只能拿到同一个 delivery。存储层必须
为 primary key 建唯一约束，不能仅依赖单进程 mutex。

裸 `/track` 的一次性视图标记为 `purpose=on_demand`；它可以和 primary 共存，但不得发送
自动终态通知，也不能抢走 primary owner。若 primary 已存在，优先刷新/复用 primary。

## 跟踪期间继续从飞书对话

镜像是读路径；正常对话需要额外的精确写入语义。

### 回复活动卡

回复活动卡表示 steer 当前卡绑定的 turn：

1. Feishu 把被回复的 card message ID 作为结构化字段交给 core；
2. core 从 delivery 解出精确 `(sessionID, threadID, turnID)`；
3. 重新读取 Codex，确认它仍是当前活动 turn；
4. 调用 `turn/steer(threadId, expectedTurnId, input, clientUserMessageId)`；
5. `expectedTurnId` 不匹配、turn 已终态或不可 steer 时明确失败，绝不降级成新
   `turn/start`。

本地 stable schema 已具备带 `expectedTurnId` 的 `turn/steer`。当前缺口是 adapter 接入和
平台把 parent/card identity 传给 core。

### 独立普通消息

独立普通消息表示“下一 turn”，不能悄悄变成当前 turn 的 steer。本地 `0.150.1` schema
只有 queue 相关通知，没有客户端可调用的权威 queue add/start 请求，因此当前实现固定为
`observer_only` 安全语义：

- 回复活动卡仍可用 stable exact steer；
- 活动 external turn 期间，独立普通消息明确提示未发送；
- 读取 thread 状态失败时同样 fail closed，不发送；
- thread 空闲时沿用正常飞书对话路径，但不宣称具备跨 TUI 的原子 queue 保证；
- 不把普通 `turn/start` 当作 queue，也不保存一份 cc-connect 私有 prompt 队列充当真值。

未来只有 Codex 暴露并验证了原子 queue 请求、竞争失败不丢 item、附件生命周期以及
approval/server-request owner routing 后，才可实现并启用 `daemon_queue`。

## 中止与其他可操作按钮

external 主卡和 foreground 主卡共用 button renderer，但 action 必须绑定精确 delivery。
中止 token 使用短期 opaque/signed 值，服务端点击时重新验证：

- 操作者权限；
- destination、workspace、logical session generation；
- card message ID 与 delivery 的绑定；
- thread ID、turn ID 和当前 Codex 状态。

验证后调用 `turn/interrupt(threadId, turnId)`。卡片先显示“已请求中止”，最终状态仍以
Codex snapshot/terminal event 为准。旧卡、终态卡或切换前 generation 的按钮不能影响新
turn。

`turn/interrupt` 表示请求中止 Codex turn，不承诺硬杀 turn 启动后脱离生命周期的所有
子进程；UI 和文档必须如实区分 `turn interrupted` 与 `child process exited`。

external turn 的 permission/approval 仍属于启动它的客户端。mirror 可以显示“等待原
客户端处理”，但在 daemon 没有跨客户端安全响应能力前，不复刻可点击的 approval 按钮。
由飞书正常发起的 foreground turn 则继续使用现有 writable session 处理请求。

## 必要的额外保证

下面是统一卡片之外不可省略的最小可靠性集合。

### 1. 持久投递账本

只保存控制元数据：

```text
TrackPreference {
  destination + workspace binding
  override: unset | on | off
  binding generation
}

TurnDelivery {
  destination + thread ID + turn ID + purpose
  source + owner
  source Feishu message ID / client marker
  card message ID / opaque recoverable handle
  stable platform idempotency keys
  render hash + terminal status
  notification state
  watermark evidence
}
```

不保存 prompt、response、reasoning、命令和 tool output。`/history`、恢复渲染和详情读取
都回到 Codex。

### 2. 平台幂等与 outbox

- 主卡创建 UUID 由项目状态命名空间内的 `(thread, turn, destination, primary)` 稳定派生；
- 终态通知 UUID 由同一 identity 稳定派生；
- 持久 mirror 卡使用带稳定 UUID 的 inline card，持久化 message handle，并通过完整
  Message.Patch 恢复更新；这样平台返回 UUID 已创建消息时不会误绑到新建的 CardKit entity；
- 普通 foreground 卡仍可使用 CardKit 流式 sequence，它不承担跨进程幂等恢复；
- 终态先持久化、再更新卡、再写 notification pending、发送、最后标记 sent；
- Feishu 幂等键与 outbox 一起覆盖“平台成功、本地尚未记账就崩溃”的窗口。

只能承诺平台能力范围内的 effective-once；平台不支持幂等创建时要明确降级为
at-least-once，不能在文档中宣称严格 exactly-once。

### 3. 乱序、背压与恢复

- 每个 delivery 的 terminal 状态单调，迟到 delta 不能把 completed 改回 running；
- 事件按 turn/item identity 合并，不依赖到达顺序拼接重复全文；
- 更新复用当前节流和大小限制，同一 turn 同时最多一个 in-flight card update；
- 缓冲区溢出、未知序列或持续更新失败时停止猜测，转 snapshot reconcile；
- watcher 读取按 endpoint/thread 合并，不为每个用户启动无限 goroutine；
- snapshot、平台调用和恢复循环均有 timeout、有限重试和指数退避。

### 4. 权限与防串会话

- destination 必须是 chat/thread-root 等频道级稳定 ID，不能把操作者 ID 混进投递 key；
- 操作者 ID 只用于授权和审计；群卡内容对群成员可见，折叠不是权限边界；
- 沿用当前权限边界：`/track`、`on/off/status` 和卡片控制只允许 `admin_from`；默认自动
  镜像不因普通群成员发言而改变绑定或偏好；
- 用户不能通过命令传任意 Codex thread ID；thread 只来自当前受控 session 绑定；
- `/new`、`/switch`、workspace 重绑先递增 generation、冻结旧 external 卡，再原子切换；
- 同一 daemon thread 默认只允许一个自动镜像 destination；
- 日志只写短 ID、状态、延迟和错误类型，不记录对话、marker、queue input、路径或 output。

### 5. 明确降级

`/track status` 必须分别报告：

- mirror 是否有效开启以及 override 来源；
- realtime event、paged reconcile、client marker round-trip；
- exact steer、daemon queue、interrupt；
- delivery/card 恢复和当前 gap 状态。

缺少某项能力时只关闭依赖它的交互，不伪装成成功。例如 queue 不安全时仍可观察、刷新、
中止和 exact steer，但不能承诺活动 turn 期间独立发起下一 turn。

## 配置建议

项目配置定义默认策略，`/track on/off` 的 destination override 单独持久化：

```toml
[projects.track]
enabled = true
default_enabled = true
notify = "on_finish"           # never | on_finish | on_failure
shared_write = "observer_only" # current safe mode; daemon_queue is reserved for a future verified capability
```

来源颜色属于平台对通用 `mirror` card variant 的映射。Feishu 首版固定为 purple；不在
core 配置中加入 Feishu 专用颜色字段。若以后需要自定义，再放到 Feishu platform options。

## Core 能力边界

core 定义通用 identity、状态机和可选能力，不识别 `feishu` 或 `codex` 名称：

- `ConversationProvider`：读取 Codex snapshot，继续作为 `/history` 唯一真值；
- paged conversation provider：从 watermark 追到全部未见 turn；
- identified turn event source：输出带 thread/turn/item identity 的外部事件；
- turn identity confirmation：关联 `clientUserMessageId` 和确切 turn ID；
- exact turn steer：对指定 expected turn 追加输入；
- authoritative input queue：预留可选能力；当前 Codex schema 不提供，状态必须报告不可用；
- `ConversationTurnController`：中止精确 turn；
- referenced-message identity：平台把被回复卡片 ID 结构化传入 core；
- restorable/idempotent card delivery：持久 card handle、稳定 create key 和 terminal notifier；
- card variant render options：平台自行把 `default | mirror` 映射为样式。

现有 Agent/Platform 不实现新增能力时保持原行为。所有新增用户可见文案进入
`core/i18n.go`，覆盖 EN、ZH、ZH-TW、JA、ES。

## 测试与验收

### 单元与故障注入

- preference 未设置时 effective=on；`off/on` 跨重启和 session 切换保持；项目总开关优先；
- 从 off 重新 on 只接管活动 turn，不补发 off 期间完成历史；enabled 重启则补齐离线窗口；
- external 和 foreground 使用同一 body/tool/footer renderer，只有 prompt placement、title 和
  running variant 不同；终态仍使用状态色；
- foreground start response 与 observer event 任意乱序、请求超时和响应前崩溃时都只有一张
  主卡；client marker 可从 Codex 持久 turn 回读；
- 两个 watcher、重复 event 和 snapshot reconcile 同时 claim external turn 时只创建一张卡；
- 两次事件/读取间完成多个 turn 时逐个投递；分页追到 watermark，gap 时不静默推进；
- 迟到 delta 不回退 terminal，重复 item 不重复工具行或 response；
- card create、Patch handle、terminal outbox 在各崩溃点复用相同 identity；
- `/new`、`/switch` 和 workspace 重绑后旧 card action 全部失效；
- 同一 thread 第二 destination 默认拒绝；
- exact steer 的 expected turn 不匹配时不启动新 turn；
- 活动 external turn 的独立普通消息明确未发送，且不会进入该活动 turn；
- external turn 的 approval 仍路由原 owner；observer 不响应 thread-scoped server request；
- `go test -race` 覆盖 preference、registry、delivery、outbox 和 card handle。

### CUJ

所有流程经 `ReceiveMessage` 驱动，并断言平台实际可见内容：

1. 默认未设置 override → TUI 启动 turn A → 紫色 external 卡原地更新 → 绿色终态 → 一次
   完成通知 → turn B 自动出现新卡。
2. `/track off` → TUI turn 不回显 → 重启/`/switch` 后仍不回显 → `/track on` → 当前活动
   turn 被接管，已完成历史不补发。
3. 飞书发起 turn → 只出现原蓝色回复卡 → observer 对账不创建 external 卡、不重复终态通知。
4. external turn 活跃 → 回复其卡片 → `turn/steer(expectedTurnId)`；turn 已变更时明确失败。
5. external turn 活跃 → 独立飞书消息明确提示未发送；回复该卡片才会 exact steer。
6. 点击旧卡中止不影响新 turn；点击活动卡只中止其精确 turn，且文案不承诺硬杀子进程。
7. 活动中重启/断连 → 恢复同一 card，无重复首卡和终态通知，并补齐离线完成的 turn。
8. `/history` 在 cc-connect 本地 History 非空、Codex 读取失败等情况下仍只服从 Codex 或
   明确失败。

### 现场协议验收

- 用 TUI 与飞书连接同一 daemon thread，确认 observer 不 resume/start thread，也不接管
  approval owner；
- 验证 external 首卡会产生预期通知，而后续 Patch 原地更新不会产生额外推送；
- 验证 `clientUserMessageId` 在 start、turn snapshot 和 daemon 重启后 round-trip；
- 人为插入 start response/event 乱序并在响应前重启，确认无重复卡；
- 用第二个 TUI 保持活动 turn，确认独立飞书消息被明确拦截且没有隐式 steer；
- 对 external steer 触发 approval，确认 observer 没有接管原客户端 owner routing；
- 运行 60 秒任务验证实时更新、静默 Patch、终态通知、中止和子进程实际状态；
- 断开 observer、跨多个快速 turn 后恢复，确认 watermark 分页没有漏卡；
- 检查 Feishu Card 2.0 button、约 28 KiB 容量、API 限流、UUID 幂等和日志脱敏。

## 实施顺序

1. **统一展示底座：** 抽出 `TurnFrame`/`TurnCardDelivery`，让 foreground 保持行为不变；
   增加 generic mirror variant，Feishu 映射紫色运行态。
2. **身份与去重：** adapter 回传 turn ID，接入 `clientUserMessageId` round-trip、reservation
   和 primary unique claim。
3. **实时 mirror：** external identified event source、默认开关、绑定 generation、统一卡片
   投递和终态通知。
4. **恢复保证：** 持久 handle/Patch identity、平台幂等、outbox、watermark 分页和 snapshot
   reconcile。
5. **精确交互：** parent card identity、`turn/steer(expectedTurnId)`、signed interrupt action。
6. **正常对话：** queue 协议未来可用后，再实现竞争验证、附件生命周期和
   server-request owner routing；当前保持 observer-only。
7. **可选详情：** 按 item 即时读取、鉴权、脱敏和独立详情卡。

当前实现已完成前五步中的 observer-only 范围：默认开启、同卡样式、实时观察、去重、
恢复、exact steer 和 exact interrupt。活动 external turn 期间的独立下一轮消息会明确
拦截；只有未来第六步具备权威 queue 后，才承诺这一场景自动排队。

## 评审结论

### 通过项

- 复用 foreground card delivery 明显减少两套 renderer 的视觉和行为漂移；
- 事件驱动负责实时体验、snapshot 负责权威对账，职责比固定高频轮询更清楚；
- default-on + persistent override 的用户语义稳定，`off` 不会被重启或切换偷偷撤销；
- 紫色只表达 external running 来源，终态继续使用状态色，信息层次明确；
- 一 turn 一 owner 一 primary 卡能解决 `/track on` 后飞书来源重复回显；
- exact steer 和精确 interrupt 分别覆盖回复和中止；缺少权威 daemon queue 时独立消息
  fail closed，不混用 `Session.Busy()` 猜测。

### 上线前置条件

| 级别 | 条件 | 验收标准 |
| --- | --- | --- |
| P0 | turn identity 可恢复 | start 回传 turn ID；marker 在持久 snapshot 中可回读；乱序和崩溃无重复卡。 |
| P0 | primary delivery 唯一 | 存储层 unique claim；重复 event、双 watcher 和 reconcile 都只产生一张卡。 |
| P0 | 默认开启不串群 | 稳定 destination、binding generation、thread 单目标约束和旧 action 失效。 |
| P0 | 卡片与通知可恢复 | 稳定 Feishu UUID、可恢复 Patch handle、terminal outbox；故障注入无重复通知。 |
| P0 | 断线不静默漏 turn | 分页追到 watermark；无法覆盖时进入 degraded/gap，而不是跳过。 |
| P0 | 无 queue 时 fail closed | 活动 external turn 期间独立消息明确未发送，绝不隐式 steer；状态报告 observer-only。 |
| P1 | external 事件有 identity | 每个可投递事件能归属 thread/turn；未知/溢出时 snapshot 对账。 |
| P1 | 精确 card reply/action | parent card ID进入 core；steer/interrupt 每次重验 turn 与 generation。 |
| P1 | owner routing 不改变 | external approval 留在原客户端；飞书启动 turn 的请求回到 writable session。 |
| P1 | 默认开启可见且可控 | external 来源显著、`/track status` 可诊断、`off` 先落盘后生效。 |

**最终意见：observer-only 范围已实现，待部署冒烟。** 统一卡片机制保留了 foreground
卡片的渲染能力和平台更新路径；来源去重、持久幂等、断线补偿、exact steer 与 exact
interrupt 已进入同一投递底座。权威 queue 仍是协议缺口，因此活动 external turn 期间的
独立下一轮消息保持明确拦截，不伪装成已排队。
