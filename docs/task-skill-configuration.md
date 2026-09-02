# Task Skill 配置过程

本文介绍如何把仓库中的 `skills/task` 配置为同时可被本地 Codex 和 cc-connect 使用。
日常命令和阶段语义见 [Task Skill 使用工作流](./task-skill-workflow.md)。

## 同步基线与部署形态

本次仓库副本按以下现有部署同步：

- cc-connect `main`：`v1.3.3`，commit `5235f13`；
- Codex CLI / App Server：`0.152.1`；
- Task：用户级目录 `~/.agents/skills/task`；
- cc-connect：Codex daemon transport、multi-workspace、Feishu-only；
- Task mode hook：Codex 全局 `UserPromptSubmit` hook。

Task 仍是独立的用户级 Skill，不编进 cc-connect。飞书中的 `$task` 由 cc-connect 原样交给
同一个 Codex App Server，并由 Codex 按原生 Skill 语法解析；直接打开 Codex 时使用完全相同
的命令。这里的“本地可用”是指不依赖 cc-connect 作为调用入口，但完整 `init` 生命周期仍会
使用 cc-connect 和飞书资源。

Codex collaboration-mode 控制属于实验性 App Server 接口。当前兼容基线是 `0.152.x`；
升级 Codex 后必须重新运行测试和一次端到端验证，不能只按版本号假设兼容。

## 1. 前置条件

- Python 3.10 或更高版本；
- `git`、`codex`、`cc-connect`、`lark-cli` 均在承载 Codex 的进程 `PATH` 中；
- cc-connect 和 Codex 由同一系统用户运行；
- Codex Remote Control/App Server Unix Socket 可用；
- Git 仓库使用 `<workspace>/<repo>/<repo>` 稳定 checkout，任务 worktree 放在
  `<workspace>/<repo>/worktrees/`；
- lark-cli 已配置 Bot 和 User 两种身份；可用 `lark-cli auth status --verify` 检查；
- 已安装 `lark-im` Skill；若启用任务文档，还需 `lark-doc` 和 `lark-drive` Skills。

只使用 Codex 和飞书时，可从 `main` 构建精简二进制，无需维护 cc-connect 分叉：

```bash
make build AGENTS=codex PLATFORMS_INCLUDE=feishu NO_WEB=1
```

Task 不需要 cloud-web、Web Search 或其他消息平台能力；上面的 Lark 能力仍是完整生命周期的
必要依赖。

## 2. 安装到用户级 Skills

推荐把仓库目录软链到 Codex 的用户级共享目录：

```bash
mkdir -p "$HOME/.agents/skills"
ln -s /absolute/path/to/cc-connect/skills/task "$HOME/.agents/skills/task"
chmod +x /absolute/path/to/cc-connect/skills/task/scripts/*
```

软链源必须位于长期保留的稳定 checkout 或独立安装目录，不能指向某个
`worktrees/ws_*` 任务目录；`task finish` 可能安全移除任务 worktree，使这种软链失效。

创建软链前先检查目标；若已存在，不要直接覆盖，先确认它是旧副本、软链还是含本地修改的
安装版：

```bash
ls -ld "$HOME/.agents/skills/task"
readlink "$HOME/.agents/skills/task"
```

也可以复制整个目录，但升级时必须同步 `SKILL.md`、`references/`、`scripts/` 和 `tests/`，
避免只替换主文件而遗留旧控制脚本。不要同时保留 `~/.codex/skills/task` 和
`~/.agents/skills/task` 两个同名副本。

Codex 会从 `$HOME/.agents/skills` 发现用户级 Skill，也支持指向 Skill 目录的软链。

## 3. 配置 Task 环境变量

Task 不包含固定用户名、主目录、open_id、App ID、Git host 或文档目录。所有部署字段均由
环境变量提供：

| 变量 | 必填 | 说明 |
|---|---:|---|
| `CC_CONNECT_TASK_WORKSPACE_ROOT` | 否 | 工作区根目录，默认 `~/workspace` |
| `CC_CONNECT_TASK_DATA_DIR` | 否 | cc-connect 顶层 `data_dir`；本 bundle 默认 `~/.cc-connect/data` 以匹配同步基线 |
| `CC_CONNECT_TASK_PROJECT` | 否 | cc-connect 项目名，默认 `workspace` |
| `CC_CONNECT_TASK_LARK_CLI_BOT_APP_ID` | 是 | lark-cli 用于创建任务群的 Bot App ID |
| `CC_CONNECT_TASK_USER_OPEN_ID` | 是 | 当前用户在 lark-cli Bot App 下的 open_id；通常不同于 cc-connect `/whoami` |
| `CC_CONNECT_TASK_BOT_APP_ID` | 是 | cc-connect 飞书 Bot App ID |
| `CC_CONNECT_TASK_DOCUMENT_REMOTE_HOSTS` | 否 | 启用任务文档的精确 Git remote host，逗号分隔；空值禁用 |
| `CC_CONNECT_TASK_DOCUMENT_FOLDER_URL` | 条件必填 | 启用文档时使用的飞书文件夹 URL |
| `CC_CONNECT_TASK_DOCUMENT_FOLDER_TOKEN` | 条件必填 | 上述文件夹 token |
| `CODEX_TASK_CONTROL_SOCKET` | 否 | 仅在不用默认 App Server Socket 时设置 |

建议创建权限为 `0600` 的独立文件，例如
`~/.config/cc-connect/task.env`。服务环境不会可靠展开 `~` 或 `$HOME`，因此写绝对路径：

```bash
CC_CONNECT_TASK_WORKSPACE_ROOT=/home/your-user/workspace
CC_CONNECT_TASK_DATA_DIR=/home/your-user/.cc-connect/data
CC_CONNECT_TASK_PROJECT=workspace
CC_CONNECT_TASK_LARK_CLI_BOT_APP_ID=cli_lark_cli_bot
CC_CONNECT_TASK_USER_OPEN_ID=ou_lark_cli_user
CC_CONNECT_TASK_BOT_APP_ID=cli_cc_connect_bot

# 可选：留空即完全禁用任务文档。
CC_CONNECT_TASK_DOCUMENT_REMOTE_HOSTS=git.example.com
CC_CONNECT_TASK_DOCUMENT_FOLDER_URL=https://example.larkoffice.com/drive/folder/folder_token
CC_CONNECT_TASK_DOCUMENT_FOLDER_TOKEN=folder_token
```

```bash
chmod 600 "$HOME/.config/cc-connect/task.env"
```

remote host 使用解析后的 host 精确匹配。例如 `git.example.com` 不匹配
`evilgit.example.com`。跨仓库任务只要一个已验证仓库匹配，就启用同一份共享任务文档。

## 4. 安装 Codex pre-turn hook

把以下 hook 合并到 `~/.codex/hooks.json`。若文件已有其他 hook，不要整文件覆盖：

```json
{
  "description": "Global task lifecycle controls that run before the Agent turn.",
  "hooks": {
    "UserPromptSubmit": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "~/.agents/skills/task/scripts/task-user-prompt-submit",
            "timeout": 20,
            "statusMessage": "Switching task collaboration mode"
          }
        ]
      }
    ]
  }
}
```

Hooks 默认启用。如果 `~/.codex/config.toml` 已显式关闭，需要在已有 `[features]` 表中改为：

```toml
[features]
hooks = true
```

不要复制另一台机器或另一条路径生成的 hook trust hash。启动 Codex 后执行 `/hooks`，审阅并
信任这条命令；脚本内容或路径改变后需要重新审阅。

该 hook 同时识别：

- 本地 Codex 或飞书经 cc-connect 原样提交的完整 `$task execute` / `$task finish`；
- cc-connect 为旧 `/task execute` / `/task finish` 别名生成的精确 Task Skill envelope。

它不会根据 `permission_mode` 猜测 Plan 状态，而是对每条合法 transition 做幂等 Default
更新并验证回执。控制失败时 hook 会阻止命令进入 Agent，避免在 Plan 中误执行。

## 5. 启动 Codex App Server

先让启动 Codex/Remote Control 的环境加载 Task 配置：

```bash
set -a
. "$HOME/.config/cc-connect/task.env"
set +a
codex login status
codex remote-control start --json
test -S "$HOME/.codex/app-server-control/app-server-control.sock"
```

当前部署使用 daemon transport。已经运行的 daemon 不会因为修改 cc-connect
`[projects.agent.options.env]` 自动获得新环境；修改 `task.env` 后，应停止并从已加载该文件的
环境重新启动 Remote Control。若由 systemd 管理 Codex，则把同一个文件作为该用户服务的
`EnvironmentFile`。

更新环境后的重启顺序为：

```bash
codex remote-control stop --json
set -a
. "$HOME/.config/cc-connect/task.env"
set +a
codex remote-control start --json
```

旧 Codex 版本可能使用：

```bash
codex app-server daemon enable-remote-control
codex app-server daemon start
```

但本 bundle 以 `0.152.x` 协议为基线，不建议仅为了保留旧命令而降级。

直接运行 Codex TUI 时也要加载同一个 `task.env`。如果希望每个终端自动加载，可在自己的
shell 启动配置中引用它，但不要把 token 或个人 ID 提交到仓库。

## 6. 配置 cc-connect（Codex + Feishu）

下面是与当前部署形态一致的最小相关配置。multi-workspace 模式必须使用 `base_dir`，不要再
设置冲突的 `work_dir`：

```toml
data_dir = "/home/your-user/.cc-connect/data"
language = "zh"

[[projects]]
name = "workspace"
mode = "multi-workspace"
base_dir = "/home/your-user/workspace"
workspace_init_allow_local_paths = false
filter_external_sessions = false
reset_on_idle_mins = 0
agent_session_idle_timeout_mins = 0
admin_from = "ou_cc_connect_user"
inject_sender = false

[projects.agent]
type = "codex"

[projects.agent.options]
backend = "app_server"
app_server_transport = "daemon"
app_server_socket = "/home/your-user/.codex/app-server-control/app-server-control.sock"
mode = "suggest"

[[projects.platforms]]
type = "feishu"

[projects.platforms.options]
app_id = "${FEISHU_APP_ID}"
app_secret = "${FEISHU_APP_SECRET}"
allow_from = "ou_cc_connect_user"
enable_feishu_card = true
progress_style = "card"
group_reply_all = true
share_session_in_channel = true
thread_isolation = false
reaction_emoji = "OnIt"
done_emoji = "Done"
```

必须保持以下对应关系：

- 顶层 `data_dir` 与 `CC_CONNECT_TASK_DATA_DIR` 完全相同；
- `projects.name` 与 `CC_CONNECT_TASK_PROJECT` 完全相同；
- `base_dir` 与 `CC_CONNECT_TASK_WORKSPACE_ROOT` 完全相同；
- `app_server_socket` 指向 hook 控制的同一 Socket；使用非默认路径时，同时设置
  `CODEX_TASK_CONTROL_SOCKET`；
- `app_id` 的实际值与 `CC_CONNECT_TASK_BOT_APP_ID` 相同；
- `admin_from` 和 `allow_from` 使用 cc-connect Bot 的 `/whoami` 返回值；不要把它们等同于
  lark-cli App 下的 `CC_CONNECT_TASK_USER_OPEN_ID`；
- `inject_sender = false`，保证当前 hook 收到原始 `$task` prompt（或旧别名的标准 Skill
  envelope），而不是带 sender 前缀的变体；
- `share_session_in_channel = true` 且 `thread_isolation = false`，使任务群 session key 稳定为
  `feishu:<chat-id>`，与生命周期脚本一致。

`group_reply_all = true` 与当前部署一致，允许任务群里不 `@` 机器人直接输入 `$task`；如果
关闭它，用户必须按飞书平台规则 `@` 机器人。`[projects.track]` 不是 Task 的必要配置；若
启用 tracked Plan 卡片，Task 工作区仍应通过 `$task execute` 进入执行阶段。

daemon transport 下，Task 环境以 Codex daemon 的环境为准。只有改用
`app_server_transport = "process"` 时，才可把相同变量放进
`[projects.agent.options.env]`，由 cc-connect 注入新建的 Codex 子进程。

## 7. 配置飞书和 lark-cli 身份

部署涉及三个主体和四个配置标识，不要把它们硬编码进 Skill：

- 当前用户在 lark-cli Bot App 下的 open_id：用于建群成员、文档和 Feed Shortcut；
- 当前用户在 cc-connect Bot App 下的 open_id：由 `/whoami` 获取，用于 `allow_from` 和
  `admin_from`；
- lark-cli Bot App ID：用于创建或核验任务群；
- cc-connect Bot App ID：用于接收用户消息和发送普通任务消息。

飞书 open_id 是 App 维度的标识。同一个用户在两个 App 下的值可能不同，不能只配置一份
open_id 并在两边复用。

飞书应用至少需要机器人能力、WebSocket 长连接的消息事件和实际操作所需权限。先前台启动
cc-connect 后可发送 `/whoami` 获取当前应用可见的用户 ID，再收紧 `allow_from` 和
`admin_from`；生产配置不要长期使用 `*`。

完整的应用权限、事件订阅和服务器部署步骤见 [飞书接入指南](./feishu.md) 与
[Codex daemon + 飞书部署指南](./codex-daemon-feishu-server.md)。

Task 规定 lark-cli Bot 不发送普通任务消息或生命周期命令，以免 cc-connect 把 Bot 消息
误当成用户 turn。普通消息和任务文档链接由 `cc-connect send` 经本地控制面发出。

## 8. 启动与验证

安装或更新 cc-connect 用户服务：

```bash
cc-connect daemon install --config /home/your-user/.cc-connect/config.toml --force
cc-connect daemon status
cc-connect daemon logs -n 100
```

服务的 `PATH` 必须解析到与交互终端相同的 Codex 版本。Codex 升级后检查 service unit 中
是否仍固定到旧版本目录，必要时重新安装服务并重启两层进程。

基础检查：

```bash
codex --version
cc-connect --version
~/.agents/skills/task/scripts/codex-task-control --version
~/.agents/skills/task/scripts/task-lifecycle-control --version
python3 -m unittest discover -s ~/.agents/skills/task/tests -p 'test_*.py' -v
```

在 Codex 中执行：

```text
/skills
/hooks
```

再用一个可丢弃的小任务做端到端验证：在 Codex 或飞书中用 `$task init` 产生 Plan 后，只
发送 `$task execute`，确认同一 turn 已进入 Default 且没有重复 queued execute；最后满足
安全门禁后执行 `$task finish`。

升级任一组件后至少重新检查：

- Codex CLI 与运行中的 Remote Control 版本一致；
- hook 仍受信任，且 `$task` 原始 prompt 与旧别名 envelope 的测试均通过；
- App Server 仍提供 `collaborationMode/list` 和 `thread/settings/update`；
- `data_dir`、project、workspace，以及三类主体对应的四个飞书标识仍一一对应；
- 文档 remote host 策略仍是精确匹配且不含个人固定值。

## 官方参考

- [Codex Skills](https://developers.openai.com/codex/skills)
- [Codex Hooks](https://developers.openai.com/codex/hooks)
- [Codex App Server](https://developers.openai.com/codex/app-server)
- [Codex configuration reference](https://developers.openai.com/codex/config-reference)
