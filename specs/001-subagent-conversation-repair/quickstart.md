# Quickstart: 子 Agent 对话归属修复

## 前置条件

- 本机可执行 `codex --version`，版本为 0.147.0。
- Go 1.26.5。
- 不需要真实上游 API key；端到端测试使用进程内 mock Anthropic SSE。

## 常跑单元测试

```bash
go test ./internal/convert -run 'TestRestore(AgentMessage|MultipleAgentMessages)' -count=1 -v
```

预期四个测试全部通过，覆盖单条回复、双 agent 顺序、后续 assistant 文本和后续 user 输入不被错位覆盖。

## 真实 app-server 端到端

```bash
APP_SERVER_E2E=1 go test ./internal/server -run TestAppServerSubAgentHistoryKeepsAgentMessage -count=3 -v
```

测试会自动完成：

1. 启动 mock Anthropic 上游。
2. 启动当前源码网关。
3. 写入临时 `CODEX_HOME` 与静态模型 catalog。
4. 启动真实 `codex app-server --stdio`。
5. 执行父 spawn → 子 final → 父 wait/final 完整流。
6. 断言父 agent 最终上游历史中 `CHILD_FINAL_A` 是独立 assistant 消息且位于 `wait_agent` 结果之后。

预期结果：测试通过，且 app-server 父 turn 最终答复包含 `PARENT_FINAL_A`。

## 真实 app-server r 路径端到端

```bash
APP_SERVER_E2E=1 go test ./internal/server -run 'TestAppServer.*AgentMessage' -count=3 -v
```

预期 a/r 两条端到端链路全部通过；r 链路会断言 outbound body 不泄漏 plaintext `agent_message`，且子任务、子答复和父汇总顺序正确。

## 真实 app-server c 路径端到端

```bash
APP_SERVER_E2E=1 go test ./internal/server -run TestAppServerChatBackendPreservesFollowUpQuestion -count=3 -v
```

预期链路通过且 follow-up 出站 Chat body 最后一条为 `role=user`，新问题文本必须以原样进入上游。

a/c 一致性验收：

```bash
APP_SERVER_E2E=1 go test ./internal/server -run 'TestAppServer.*FollowUpQuestion' -count=3 -v
```

预期 a/c 两条 follow-up 链路全部通过，最新用户输入均以原样出现在各自协议出站历史尾部。
