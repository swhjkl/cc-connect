# Linux 服务器部署：Codex daemon + cc-connect + 飞书

本文档用于在一台 Linux 服务器上部署当前分支中的 Codex shared app-server
daemon 支持，并通过飞书使用 Codex。完成后数据流如下：

```text
飞书客户端
    │  WebSocket 长连接（仅需服务器主动出站）
    ▼
cc-connect systemd 用户服务
    │  WebSocket over Unix socket
    ▼
Codex app-server daemon
    │
    ▼
目标项目目录
```

本文档对应分支：`ws/feat/codex-daemon-feishu-ready`。在功能进入上游版本前，
服务器必须从该分支编译，不能用 npm/Homebrew 中尚未包含此功能的 cc-connect
正式版替代。

该方案连接的是 Codex 的实验性 app-server 本地控制 Socket。首周测试期间应固定
已验证的 Codex CLI 版本；升级 Codex 后先做本页第 7 节的前台冒烟测试，再重启
后台服务。

## 1. 部署前检查

准备以下内容：

- 一台 Linux 服务器；以下命令以 systemd 用户服务为例。
- Node.js/npm，用于安装 Codex CLI。
- Go 1.25 或更高版本，用于编译本分支。
- 一个飞书企业自建应用的管理权限。
- 一个服务器上的非 root 部署用户，以及该用户可读写的项目目录。
- 服务器能够主动访问 OpenAI/ChatGPT 和飞书开放平台的 HTTPS/WSS 地址。

Codex daemon 和 cc-connect 必须由同一个 Linux 用户运行。不要用普通用户启动
Codex daemon、再用 `sudo` 启动 cc-connect，否则 Unix Socket 和 Codex 登录凭据
可能不可访问。

本文中的 `/home/your-user`、`/home/your-user/projects/your-project` 和凭证占位符
都需要替换为服务器上的真实值。不要把 App Secret、Codex Token、
`~/.codex/auth.json` 或部署用 `config.toml` 提交到 Git。

## 2. 安装和认证 Codex

### 2.1 安装 CLI

以部署用户登录服务器：

```bash
npm install -g @openai/codex@0.149.1
codex --version
```

本文档在 `codex-cli 0.149.1` 上验证过。完成一周测试后，可以有计划地升级到
`@openai/codex@latest`。新版本如果调整了实验性 app-server 命令，请先运行
`codex remote-control --help` 和 `codex app-server daemon --help` 核对命令。

### 2.2 在无桌面服务器上登录

优先使用设备码登录：

```bash
codex login --device-auth
codex login status
```

按终端提示在自己的浏览器中打开地址并输入设备码。`codex login status` 返回成功
后再继续。

如果使用 OpenAI Platform API Key，也可以通过标准输入登录，避免密钥进入命令行
参数：

```bash
read -rsp "OPENAI_API_KEY: " OPENAI_API_KEY
printf '%s' "$OPENAI_API_KEY" | codex login --with-api-key
unset OPENAI_API_KEY
codex login status
```

### 2.3 Codex 配置

Codex 的用户级配置位于 `~/.codex/config.toml`。服务器最小配置可以是：

```toml
# Headless Linux 上使用文件存储，便于 systemd 用户服务读取。
# ~/.codex/auth.json 包含敏感凭据，必须保护好。
cli_auth_credentials_store = "file"

# 可选；不填写 model 时使用账号/CLI 的默认模型。
# model = "your-supported-model"
model_reasoning_effort = "high"
```

保护配置和登录缓存：

```bash
chmod 700 ~/.codex
chmod 600 ~/.codex/config.toml
test ! -f ~/.codex/auth.json || chmod 600 ~/.codex/auth.json
```

cc-connect 会在创建 thread 时根据 `mode` 传入 sandbox 和 approval policy，因此
这里不必重复配置 `sandbox_mode` 或 `approval_policy`。Codex daemon 启动后会使用
自己的 `~/.codex/config.toml` 和登录状态；修改 Codex 配置后应重启 daemon。

## 3. 启动 Codex app-server daemon

当前 Codex CLI 的首选命令是：

```bash
codex remote-control start --json
```

检查 daemon 和默认 Unix Socket：

```bash
codex app-server daemon version
test -S ~/.codex/app-server-control/app-server-control.sock
```

如果当前版本没有 `codex remote-control start`，但提供旧版 daemon 子命令，使用：

```bash
codex app-server daemon enable-remote-control
codex app-server daemon start
```

如果服务器通过 SSH 使用，且 daemon 退出登录后不能保持运行，可以先安装 durable
daemon 管理：

```bash
codex app-server daemon bootstrap --remote-control
```

停止 daemon：

```bash
codex remote-control stop --json
```

cc-connect 只需要本地 Unix Socket，不需要 `codex remote-control pair`，也不要把
app-server WebSocket 端口直接暴露到公网。

## 4. 编译部署分支

```bash
git clone --branch ws/feat/codex-daemon-feishu-ready --single-branch \
  https://github.com/swhjkl/cc-connect.git
cd cc-connect
go version
node --version
npm --version
make build AGENTS=codex PLATFORMS_INCLUDE=feishu
test -f web/dist/index.html
./cc-connect --version
```

`make build` 会在首次构建时安装 `web/` 下的 npm 依赖，生成 `web/dist`，再把
Web 管理界面嵌入 cc-connect 二进制。`AGENTS` 和 `PLATFORMS_INCLUDE` 仍然只编译
Codex Agent 与飞书平台，不影响 Web 管理界面。

如果明确不需要 Web UI，可以改用精简构建：

```bash
make build-noweb AGENTS=codex PLATFORMS_INCLUDE=feishu
```

`build-noweb` 生成的二进制不包含 Web 管理界面，`/web` 命令也会提示该功能不可用。
安装完整二进制：

```bash
sudo install -m 0755 ./cc-connect /usr/local/bin/cc-connect
```

升级该部署分支时：

```bash
cd /path/to/cc-connect
git pull --rebase
make build AGENTS=codex PLATFORMS_INCLUDE=feishu
sudo install -m 0755 ./cc-connect /usr/local/bin/cc-connect
cc-connect daemon restart
```

## 5. 配置飞书应用

完整平台说明见 [飞书接入指南](./feishu.md)。服务器部署使用 WebSocket 长连接，
不需要公网 IP、域名、Webhook URL 或反向代理。

### 5.1 创建应用和机器人

1. 登录 [飞书开放平台](https://open.feishu.cn/)，进入开发者后台。
2. 创建“企业自建应用”。
3. 在“凭据与基础信息”中复制 App ID 和 App Secret。
4. 在“应用能力 → 机器人”中启用机器人能力。

App Secret 是密码，只应写入服务器的私有配置或秘密管理系统。

### 5.2 申请权限

在“权限管理”中至少申请：

| 权限标识 | 用途 |
|---|---|
| `contact:user.base:readonly` | 获取发送者基本信息 |
| `im:message.p2p_msg:readonly` | 接收机器人单聊消息 |
| `im:message.group_at_msg:readonly` | 接收群内 @机器人 消息 |
| `im:message:send_as_bot` | 以机器人身份回复消息 |

只有当机器人需要读取群内所有普通消息时，才额外申请敏感权限
`im:message.group_msg`，并在 cc-connect 中设置 `group_reply_all = true`。

### 5.3 配置事件和卡片回调

在“事件与回调”中完成以下配置：

1. 事件配置选择“使用长连接接收事件”。
2. 添加事件 `im.message.receive_v1`。
3. 回调配置也选择“使用长连接接收事件”。
4. 添加卡片回调 `card.action.trigger`。

`card.action.trigger` 用于 Codex 权限审批、会话切换和模式选择等交互按钮。如果
暂时无法配置卡片回调，可以在 cc-connect 配置中设置
`enable_feishu_card = false`，先用纯文本模式完成联调。

### 5.4 发布和可用范围

1. 在“版本管理与发布”中创建并发布版本。
2. 确认新申请的权限、事件和回调已经包含在已发布版本中。
3. 把应用可用范围设置为测试用户或目标组织。
4. 发布后在飞书中搜索机器人，或将机器人添加到目标群聊。

每次修改权限、事件、回调或可用范围后，都要检查是否需要重新发布版本。

## 6. 配置 cc-connect

创建部署目录：

```bash
mkdir -p ~/.cc-connect
chmod 700 ~/.cc-connect
```

创建 `~/.cc-connect/config.toml`：

```toml
language = "zh"
data_dir = "/home/your-user/.cc-connect/data"

[log]
level = "info"

[[projects]]
name = "your-project"
reset_on_idle_mins = 0
agent_session_idle_timeout_mins = 0

# 首次启动可以暂时留空；通过飞书发送 /whoami 获得 ou_xxx 后填入。
admin_from = ""

[projects.agent]
type = "codex"

[projects.agent.options]
work_dir = "/home/your-user/projects/your-project"
mode = "suggest"
backend = "app_server"
app_server_transport = "daemon"
app_server_socket = "/home/your-user/.codex/app-server-control/app-server-control.sock"

# 可选；留空时使用 Codex daemon 当前配置。
# model = "your-supported-model"
# reasoning_effort = "high"

[[projects.platforms]]
type = "feishu"

[projects.platforms.options]
app_id = "cli_xxxxxxxxxxxxxxxx"
app_secret = "replace-with-real-app-secret"

# 首次联调暂时允许所有用户；获取自己的 open_id 后立即收紧。
allow_from = "*"
enable_feishu_card = true
progress_style = "card"
group_reply_all = false
thread_isolation = false
reaction_emoji = "OnIt"
done_emoji = "Done"
```

保护配置文件：

```bash
chmod 600 ~/.cc-connect/config.toml
```

`admin_from` 必须放在 `[[projects]]` 下，而不是
`[projects.platforms.options]` 下。`mode = "suggest"` 是首周测试的推荐值：Codex
使用只读 sandbox，并通过飞书卡片请求需要的权限。`auto-edit` 和 `full-auto` 会减少
审批，`yolo` 会跳过 sandbox，不建议在普通服务器上使用。

### 6.1 启用 Web 管理界面

完整构建包含前端资源，但仍需要在配置中启用 Management API 和 Bridge。使用部署
用户执行：

```bash
cc-connect web --no-browser
```

该命令会更新默认配置文件 `~/.cc-connect/config.toml`，启用 Management 和
Bridge，生成各自的随机 token，并打印 Web UI 地址及登录 token。token 等同于管理
凭据，不要提交到 Git 或发送到聊天中。首次配置完成后，在第 7 节启动 cc-connect；
如果服务已经运行，则需要重启：

```bash
cc-connect daemon restart
```

Web UI 默认使用端口 `9820`，Bridge 默认使用端口 `9810`。不要在服务器防火墙中
直接向公网开放这两个端口。推荐从本机建立 SSH 隧道：

```bash
ssh -N -L 9820:127.0.0.1:9820 your-user@your-server
```

保持该 SSH 连接运行，在本机浏览器打开：

```text
http://localhost:9820
```

使用 `cc-connect web --no-browser` 输出的 Management token 登录。Web UI 可以管理
项目、会话、Provider 和定时任务，也可以直接与 Codex 对话；它与飞书可以同时
使用。

当前 Web UI 尚未提供 `app_server_transport` 和 `app_server_socket` 输入框。因此
这两个 daemon 参数仍应按本节前面的示例直接维护在 `config.toml` 中，不要因为页面
上看不到它们而删除。若确实需要从公网访问 Web UI，应在前面部署 HTTPS 反向代理并
增加访问控制，而不是直接暴露明文 HTTP 端口。

## 7. 首次前台联调

先确认项目目录存在且部署用户可以访问：

```bash
test -d /home/your-user/projects/your-project
test -r /home/your-user/projects/your-project
test -w /home/your-user/projects/your-project
```

确认 Codex daemon 已运行后，前台启动：

```bash
cc-connect --config ~/.cc-connect/config.toml
```

正常日志应包括：

```text
platform ready ... platform=feishu
cc-connect is running
```

在飞书中依次测试：

```text
/whoami
/status
告诉我当前工作目录，不要修改文件
```

第一次普通消息到达 Codex 后，日志还应包括：

```text
codex app-server session connected transport=websocket-unix
codex app-server thread started
turn complete
```

把 `/whoami` 返回的 `ou_xxx` 写入两个位置：

```toml
[[projects]]
admin_from = "ou_xxxxxxxxxxxxxxxx"

[projects.platforms.options]
allow_from = "ou_xxxxxxxxxxxxxxxx"
```

重启 cc-connect，再验证 `/status`、`/dir` 和一次需要审批的只读命令。不要长期保留
`allow_from = "*"`。

## 8. 安装为 systemd 用户服务

先用 `Ctrl+C` 停止前台实例。为保证 SSH 退出后用户服务继续运行，执行一次：

```bash
sudo loginctl enable-linger your-user
```

不要用 `sudo` 执行下面的 cc-connect 安装命令：

```bash
cc-connect daemon install --config /home/your-user/.cc-connect/config.toml
cc-connect daemon status
cc-connect daemon logs -n 100
```

日常维护：

```bash
cc-connect daemon restart
cc-connect daemon logs -f
cc-connect daemon stop
cc-connect daemon start
```

重启服务器后检查两层服务：

```bash
codex app-server daemon version
test -S ~/.codex/app-server-control/app-server-control.sock
cc-connect daemon status
```

## 9. 故障排查

### “无法完成 App Server 初始化”

依次检查：

```bash
codex login status
codex app-server daemon version
test -S ~/.codex/app-server-control/app-server-control.sock
cc-connect daemon logs -n 200
```

常见原因：

- Codex daemon 没有启动。
- `app_server_socket` 路径写错。
- Codex daemon 和 cc-connect 由不同用户运行。
- cc-connect 服务读取到了不同的 `CODEX_HOME`。
- Codex 未登录、登录已失效，或服务器无法访问 OpenAI/ChatGPT。
- Codex CLI 与正在运行的 app-server 版本不一致；用
  `codex app-server daemon version` 检查并重启 daemon。

### 飞书连接成功，但收不到消息

检查：

- 应用版本是否已发布。
- 是否启用了机器人能力，并把机器人加入了会话。
- 是否使用长连接订阅 `im.message.receive_v1`。
- 应用可用范围是否包含当前用户。
- `allow_from` 是否包含 `/whoami` 返回的 open_id。

### 能收到消息，但审批按钮无效

确认回调配置使用长连接并订阅 `card.action.trigger`，然后重新发布应用版本。临时
降级方案：

```toml
[projects.platforms.options]
enable_feishu_card = false
progress_style = "compact"
```

### 需要临时回退 daemon transport

当前实现保留旧用法。停止 cc-connect 后改为私有 app-server 进程：

```toml
[projects.agent.options]
backend = "app_server"
app_server_transport = "process"
app_server_url = "stdio"
```

或者回退到默认 `exec` backend：

```toml
[projects.agent.options]
backend = "exec"
```

## 10. 明日部署检查清单

- [ ] `codex --version` 正常。
- [ ] `codex login status` 成功。
- [ ] `codex remote-control start --json` 成功。
- [ ] 默认 app-server Unix Socket 存在。
- [ ] 从 `ws/feat/codex-daemon-feishu-ready` 编译 cc-connect。
- [ ] `web/dist/index.html` 已生成，安装的是 `make build` 产生的完整二进制。
- [ ] Web UI 已启用，并能通过 SSH 隧道在本机访问 `http://localhost:9820`。
- [ ] 飞书机器人能力、权限、消息事件和卡片回调均已发布。
- [ ] 前台模式完成 `/whoami`、`/status` 和一次 Codex 对话。
- [ ] `allow_from` 和 `admin_from` 已收紧为自己的 `ou_xxx`。
- [ ] systemd linger 已启用。
- [ ] cc-connect daemon 和 Codex daemon 均能在 SSH 退出后保持运行。
- [ ] App Secret、Codex 登录缓存和部署配置没有进入 Git。

## 参考资料

- [OpenAI Codex CLI 命令参考](https://learn.chatgpt.com/docs/developer-commands?surface=cli)
- [OpenAI Codex App Server](https://learn.chatgpt.com/docs/app-server)
- [OpenAI Codex 无桌面设备登录](https://learn.chatgpt.com/docs/auth#login-on-headless-devices)
- [飞书开放平台](https://open.feishu.cn/)
- [飞书事件订阅文档](https://open.feishu.cn/document/ukTMukTMukTM/uUTNz4SN1MjL1UzM)
- [cc-connect 飞书接入指南](./feishu.md)
