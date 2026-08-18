# Contract: Agent Message 恢复

## Input

Codex 发送的 Responses request `input[]` 中可以包含：

```json
{
  "type": "agent_message",
  "content": [
    {
      "type": "input_text",
      "text": "Message Type: FINAL_ANSWER\nTask name: /root\nSender: /root/child_a\nPayload:\nCHILD_FINAL_A"
    }
  ]
}
```

## Output

`internal/convert.DecodeResponseNewParams` 必须在相同 raw 位置恢复为：

```json
{
  "type": "message",
  "role": "assistant",
  "content": "Message Type: FINAL_ANSWER\nTask name: /root\nSender: /root/child_a\nPayload:\nCHILD_FINAL_A"
}
```

## 禁止行为

- 禁止把文本追加到任何 `function_call_output.output`。
- 禁止把 role 改成 `user`。
- 禁止把多条 `agent_message` 全部移动到 input 末尾。
- 禁止因索引错位丢失后续 assistant `output_text`。

## App-Server 验收

真实 Codex 0.147 app-server 场景必须产生并验证：

1. 父 agent 调 `collaboration.spawn_agent`。
2. 子 agent 收到 `NEW_TASK` 并返回 `CHILD_FINAL_A`。
3. 父 agent 调 `collaboration.wait_agent`。
4. `wait_agent` 输出保持原文本。
5. `CHILD_FINAL_A` 作为 `wait_agent` 后的独立 assistant 消息发给上游。
6. 父 agent 最终返回 `PARENT_FINAL_A`。

## Responses 透传上游

`backend_type=r` 时，完全明文的 `agent_message` 必须在 outbound body 原位置变为：

```json
{
  "type": "message",
  "role": "assistant",
  "content": []
}
```

`content` 保持原 `input_text` parts；`author` / `recipient` 移除。含 `encrypted_content` 的 `agent_message` 保持原样，交给原生支持该扩展的上游。
