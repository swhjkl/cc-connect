# Feishu 原生任务卡增强 TODO

状态：本期增强已实现并通过回归，待线上体验验证。

## 目标

- 原生任务卡在执行中提供精确的“中止当前任务”操作，替代用户手动输入 `/stop` 的主要场景。
- 用户回复一张仍在执行的原生任务卡时，将内容精确 steer 到该卡片对应的 turn。
- 所有任务级操作都绑定权威 `thread ID`、`turn ID` 和卡片消息身份；旧卡片不得影响后续 turn。
- 后端确认终态后在原过程卡中就地展示 `Done`、`Error` 或 `Interrupted`，保留已有 reasoning 和工具调用；response 继续由独立结果消息承载，避免重复回显。

## 实现约束

- 原生卡片控制不依赖 `/track on/off`、mirror binding 或 mirror delivery。
- 仅持久化 fail-closed 所需的最小身份元数据，不存储 prompt、response、reasoning 或工具内容；Codex 仍是对话内容和 turn 状态的唯一真值。
- 活跃卡片身份同时绑定平台、会话、workspace/interactive key、thread ID、turn ID、执行代次、卡片 message ID 和不可猜测 action token。
- 服务重启后不恢复对旧原生卡片的控制权；已持久化的身份只用于把旧按钮和旧卡片回复判定为 stale。
- 中止成功不发送独立确认消息；失败或 stale 可以发送错误提示。卡片只在收到权威终态后移除按钮并切换终态。

## 待实现

- [x] 只在后端支持 `ConversationTurnController` 且已获得精确 turn 身份时显示“中止当前任务”按钮。
- [x] 中止操作校验 session、thread、turn、当前执行代次和卡片消息 ID；失效或转发的卡片只返回 stale，不执行兜底 `/stop`。
- [x] 中止成功时不发送独立飞书消息，等待后端终态事件驱动原卡片更新并移除按钮。
- [x] 根据 `EventResult.Metadata["turn_status"]` 区分 completed、failed 和 interrupted，避免中止后仍显示 `Done`。
- [x] 将对执行中原生卡片的直接回复路由为带 `expectedTurnID` 的精确 steer。
- [x] 原生卡片 steer 失败或卡片过期时 fail closed，不排队为新 turn。
- [x] 在执行中卡片增加“回复此卡片可追加指令”的轻量提示。
- [x] 为正常中止、旧卡片误点、新 turn 已开始、并发点击、终态按钮移除和精确 steer 添加回归测试；涉及用户完整流程时补 CUJ。

## 验证

- `go test ./...`
- `go test ./core -run TestCUJ -count=1`
- `go test -race ./core -run 'Test(NativeTurnCard|TurnCardStore|CompactProgressWriter_Controls|CUJ_I8)' -count=1`
- `go vet ./core ./platform/feishu`
- `go build -buildvcs=false ./...`

## 后续只读增强

- [ ] 按需查看因长度限制省略的 reasoning 和工具输入/输出，详情从 Codex 权威 turn 数据重建。
- [ ] 仅在卡片暂停、断连或恢复场景提供“刷新状态”。
- [ ] Failed/Interrupted 后提供“继续处理”入口，引导用户补充指令，不做可能重复副作用的一键重试。
- [ ] 只有获得 per-turn 变更归属后再考虑“查看本任务变更”；当前工作区级 `/diff` 不直接绑定任务卡。

## 明确不纳入

- 不提供伪暂停/恢复；后端没有可靠的暂停语义。
- 不把模型、reasoning、权限模式、compact、会话切换等会话级命令放进任务卡。
- 不提供一键撤销/回滚。
- 在后端提供权威持久队列前，不提供“排队下一轮”按钮。
- 权限批准和 Agent 提问继续使用独立的一次性交互卡。
