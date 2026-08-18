# Data Model: 子 Agent 对话归属修复

## Raw Response Input Item

- **字段**: `type=agent_message`、`content[]`、raw 数组位置。
- **不变量**: `content[].type=input_text` 的文本按顺序拼接；`encrypted_content` 不可本地解读，只保留可读 envelope 文本。
- **关系**: 是 Codex 客户端回灌历史的原始输入，不是网关运行时状态。

## Restored Agent Message

- **字段**: SDK `EasyInputMessageParam`、`role=assistant`、明文内容、插入位置。
- **不变量**: 插入位置等于 raw input 位置；不得并入 `function_call_output`，不得追加到末尾。
- **状态迁移**: SDK unknown item → raw 识别 → positional assistant message。

## Conversation History Alignment

- **字段**: raw item index、SDK item index、assistant `output_text`。
- **不变量**: 插入 `agent_message` 后，raw 与恢复列表长度和顺序一致；后续 assistant `output_text` 在相同 index 修复。
- **关系**: 保证 a/c 请求转换看到稳定的历史顺序。

## Tool Result

- **字段**: `call_id`、`output`、工具名。
- **不变量**: 仅承载真实工具执行结果；不得吸收 inter-agent 消息。
- **关系**: `wait_agent` 结果与子 agent `FINAL_ANSWER` 是相邻但独立的语义单元。
