# Task Skill 使用工作流

本文介绍 Task Skill 的日常使用。安装和环境配置见
[Task Skill 配置过程](./task-skill-configuration.md)。

Task 是显式触发的编码任务生命周期：它把 Git worktree、原生 Codex thread、飞书任务群、
cc-connect 路由和可选任务文档关联为同一项任务。直接使用 Codex 和通过飞书使用
cc-connect 时，执行的是同一套生命周期，入口命令也完全相同：

| 入口 | 初始化 | 执行 | 收尾 |
|---|---|---|---|
| Codex | `$task init <需求>` | `$task execute` | `$task finish` |
| cc-connect / 飞书 | `$task init <需求>` | `$task execute` | `$task finish` |

`execute` 可缩写为 `exe` 至 `execute`，`finish` 可缩写为 `fin` 至 `finish`。命令必须独占
整条消息；`$task execute 并更新文档` 之类带额外文本的写法不会触发模式切换。

## 完整流程

```text
$task init
  └─ 创建或复用 worktree、任务群、路由和 Codex thread
       └─ 进入 Plan，形成并评审计划
            └─ $task execute
                 └─ pre-turn hook 在同一 turn 切到 Default
                      └─ 文档策略检查、实现、测试和验收
                           └─ 提交并集成到稳定默认分支
                                └─ $task finish
                                     └─ 安全清理并归档原生 thread
```

Task 只在显式输入 `$task` 时触发，不会接管普通编码请求。cc-connect 仍兼容旧的 `/task`
Skill 别名，但不再推荐新任务使用。

## 1. 初始化任务

需求已经完整时直接初始化：

```text
$task init 修复登录态过期后重复跳转的问题
```

飞书中使用相同命令：

```text
$task init 修复登录态过期后重复跳转的问题
```

省略 `init` 也视为初始化，例如 `$task 修复登录态过期问题`。Skill 会先读取当前 Git 和
会话事实，再创建或复用标准资源：

- 分支：`ws/<angular-type>/<lowercase-hyphen-slug>`；
- worktree：`<workspace>/<repo>/worktrees/ws_<type>_<slug>`；
- 原生 Codex thread：名称与分支一致；
- 飞书任务群、cc-connect workspace 路由和 session attachment；
- 适用时的上下文交接文件 `handoff.md`。

初始化不会开始实现，也不会创建任务文档。输入按上下文依赖分为三类：

| 模式 | 何时使用 | 初始化后的行为 |
|---|---|---|
| Direct | 当前消息已包含完整需求 | 原样把有效需求排队到目标 thread，并在 Plan 中开始分析 |
| Contextual | 需求依赖当前长对话、文件选择或隐含约束 | 生成并校验 worktree 根目录下的 `handoff.md`，再排队其路径和摘要 |
| Deferred | 当前没有可安全转交的需求 | 只完成资源初始化，下一条用户消息在 Plan 中继续 |

Task 会按实时资源做幂等协调，因此中断后应重新执行同一命令并让它核对现状，不要手工创建
同名群、重复绑定路由或猜测 thread ID。

## 2. 在 Plan 中评审

初始化后的目标 thread 处于 Codex Plan collaboration mode。此时可以继续补充要求、回答
澄清问题、要求调整计划，直到计划足够明确。Plan 阶段只分析，不应修改项目文件或开始实现。

cc-connect `main` 的跟踪卡片也可能显示通用的“执行计划”按钮。该按钮会以 Default mode
执行上一轮计划，但它不是 Task 生命周期命令，不会运行 Task 的文档策略和资源校验。使用
Task 管理的工作区时，无论在飞书还是直接使用 Codex，都应输入 `$task execute`。

## 3. 从 Plan 直接执行

计划确认后，只发送一条完整命令：

```text
$task execute
```

Codex 在 Agent 收到消息前运行全局 `UserPromptSubmit` hook：

1. 直接 Codex 和飞书中的 `$task` 命令都按原文识别；
2. 旧 `/task` 别名生成的 Task Skill envelope 仅作为向后兼容路径识别；
3. hook 使用事件中的原生 `session_id`，通过 App Server 把该 thread 持久切到 Default；
4. hook 为正在提交的这个 turn 注入 Default collaboration context；
5. Agent 在同一个 turn 继续执行，不再排队第二个 `$task execute`。

因此，Plan → Execute 不需要用户先手动切换模式，也不依赖 cc-connect 特判 Task。若 hook
缺失、未信任、连接不到 App Server 或无法验证回执，它会在 Agent 前阻止这条命令；修复后
重新发送即可，不会在 Plan 中误执行一半。

执行前，Skill 会再次协调缺失的初始化资源。任务文档是否为执行门禁由配置的 Git remote
host 决定：

- 未配置 `CC_CONNECT_TASK_DOCUMENT_REMOTE_HOSTS`，或当前仓库不匹配：文档不适用，不阻塞实现；
- 有精确匹配：必须创建或更新任务文档，并由 cc-connect Bot 在任务群发送规范链接；
- 文档写入或消息发送结果不确定：先查现状，不盲目重试，也不开始实现。

## 4. 实现与验收

实现始终发生在已验证的任务 worktree 和原生 task thread 中。照常完成代码、测试、评审和
验收；Task 不会代替项目自身的 `AGENTS.md`、测试门禁或发布流程。

准备收尾前，需要由用户或正常开发流程完成：

- 提交任务分支上的 tracked/staged 修改；
- 将任务提交合并、rebase、cherry-pick 或 squash 到稳定默认分支；
- 确认稳定默认分支已经包含任务最终内容。

Task 自身不会替用户 commit、merge、rebase 或 push。

## 5. 收尾

用户明确希望结束当前任务工作区和 thread 时输入：

```text
$task finish
```

飞书中也使用 `$task finish`。如果 thread 仍在 Plan，pre-turn hook 会先在同一 turn 切到
Default，再开始清理。

收尾有两种目标：

- **任务 worktree**：拒绝 staged 或 tracked 修改，验证任务内容已进入稳定默认分支；把
  所有未跟踪和 ignored 内容移动到可恢复 archive 后，才解除路由并移除 worktree。分支和
  提交保留。
- **稳定 checkout**：只关闭当前 thread 及其精确 attachment；不要求干净状态，不移动、
  清理或删除稳定目录中的任何文件，也不会选择旁边某个 `ws/*` worktree 代替它。

原生 thread 归档是最后一步。归档之后 Skill 不再执行文件、消息或文档操作。

## 中断与排障原则

- `configuration_missing`：补齐错误列出的环境变量，并重启承载 Codex 的进程。
- Execute 仍收到 Plan 限制：检查 `~/.codex/hooks.json`、`/hooks` 信任状态、App Server
  Socket，以及 hook 是否识别飞书原样提交的 `$task` prompt。
- 找不到 Task：用 `/skills` 检查；目录应为 `~/.agents/skills/task/SKILL.md`，不能多嵌套
  一层 `task/task`。
- 飞书群冲突：同名群的成员或 thread 描述与预期不一致时，Skill 会停止，不会接管或清理。
- 收尾被阻止：按回执处理未提交修改、未集成提交或活动中的其他 thread，再重试原命令。
- 任何写操作结果不确定时先检查实际状态；不要用重复发送、强制删除或手工解绑来“修复”。
