# Research: 子 Agent 对话归属修复

## 结论

- Codex 0.147 的 V2 collaboration 工具默认 namespace 是 `collaboration`；自定义 provider 只有使用网关返回的静态模型 catalog 时，`multi_agent_version=v2` 才稳定参与工具规划。
- `ResponseItem::AgentMessage` 是 Codex 多 agent 通信的模型输入 item；明文内容承载 `NEW_TASK` / `MESSAGE` / `FINAL_ANSWER`。
- Codex 源码中 `InterAgentMessage::role()` 与 `InterAgentCompletionMessage::role()` 均固定返回 `assistant`，且 history 视 `agent_message` 为回合边界。
- openai-go v3.50.0 不能识别 `type=agent_message`。实测出现该 item 后 SDK input 列表为空，需要从 raw JSON 恢复。
- 修复前网关把 `agent_message` 文本并入最后一个 `function_call_output`。真实 app-server 复现中，子 agent `CHILD_FINAL_A` 被拼进 `wait_agent` 的 `tool_result`，父 agent 历史没有独立 assistant 回复。
- 正确修复是按 raw 位置重建 assistant 消息，不修改任何工具输出；重建后按最终 raw/index 对齐修复后续 assistant `output_text`。
- 2026-08-18 用户会话 `01a01505-41f2-75f1-b497-659e9b1a2757` 进一步暴露 r 路径缺口：a/c 解码恢复只影响日志与预检，`ResponsesBackend.PrepareUpstreamBody` 仍把原始 `agent_message` 发给 GLM，上游丢掉 `NEW_TASK` 后子 agent 回复“没有具体任务”。r 路径需把完全明文的 `agent_message` 原位折为 assistant `message`；含 `encrypted_content` 的 item 保持原生，交原生支持上游处理。
- 2026-08-18 用户会话 `01a0151b-3386-7ed2-b647-ff914295e144`（chat 协议）复现第二类回归：父会话 follow-up 已出现在 `item/completed`，但 Chat 出站请求最后一条仍是旧父 agent 回复，模型因此把新输入当作“复读”。根因是 `restoreAssistantOutputTextFromRaw` 先于 `restoreAgentMessageFromRaw` 执行：SDK 丢 `agent_message` 后列表少 1，raw 索引错位把后续 `output_text` 写进真正的 user 消息/丢尾。修复改为 `agent_message` 从 raw 整体重建 items（普通 item 逐个解、agent 原位转 assistant），`output_text` 恢复随后按同序执行。

## 证据

1. Codex 0.147 tag：`rust-v0.147.0`。
2. `core/src/tools/router.rs`：只有 `namespace=collaboration` 且 `spawn_agent` / `send_message` / `followup_task` 的 `encrypted_function_args=[]` 才走 `DirectPlaintextMessage`。
3. `core/src/context/inter_agent_message.rs` 与 `inter_agent_completion_message.rs`：inter-agent 消息 role 为 `assistant`。
4. 真实端到端测试修复前失败：父 agent 最终 Anthropic 请求中 `CHILD_FINAL_A` 位于 `toolu_wait_a` 的 `tool_result.content` 内。
5. 同一测试修复后通过：`wait_agent` 输出保持 `{"message":"Wait completed.","timed_out":false}`，`CHILD_FINAL_A` 作为后续 assistant 文本保留，父最终答复为 `PARENT_FINAL_A`。
6. 用户会话子 rollout 明确包含 `NEW_TASK` 与任务文本；生产 `gateway.log` 显示请求进入 r 源 `glm`，outbound 仍含 unknown 扩展语义。
7. 真实运行进程为 `/home/mapleafgo/.local/bin/codex-api-gateway`，文件时间 14:20，且日志仍输出旧实现文案；仓库新构建产物是 `./codex-api-gateway`。部署时必须替换/重启用户自管进程后才能生效。
8. `TestRestoreAgentMessageKeepsLaterUserTurn` 修复前失败：`user FOLLOWUP` 被清空/覆盖；修复后 4 个 item 的 role/content 与 raw 完全一致。
9. `APP_SERVER_E2E=1 go test ./internal/server -run TestAppServerChatBackendPreservesFollowUpQuestion` 修复前 mock 上游收到不含 `FOLLOWUP_C` 的出站 body，修复后最后一条为 `role=user` 且回答对应 follow-up。

## 方案取舍

**Decision**: 从 raw JSON 按 input 原始位置重建 `agent_message` 为 assistant 消息。

**Rationale**: 保留父子通信的 role、顺序和回合边界；不污染工具调用配对；同时修复 SDK 丢 item 后的索引错位。

**Alternatives considered**:

- 并入前置工具结果：已在真实 app-server 中证明会造成子 agent 回复归属错误。
- 追加到 input 末尾：多条子 agent 或交错的 `NEW_TASK` / `FINAL_ANSWER` 会失去原始顺序。
- 引入 session store：违反产品边界，且 Codex 客户端已经完整回灌历史。
