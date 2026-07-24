# Anthropic 终态观测一致性设计

## 背景

Anthropic 流在 `max_tokens`、`pause_turn` 或 `refusal` 时，`streamconv.Converter`
会正确输出 `response.incomplete`，但 `AnthropicBackend` 当前仍将上游观测事件记为
`completed`。这会使最终日志、上游指标和客户端指标与实际 Responses 终态不一致。

## 目标

- Anthropic Backend 的最终日志和 `UpstreamEvent.Status` 与转换器输出的终态一致。
- `max_tokens`、`pause_turn`、`refusal` 记为 `incomplete`。
- `end_turn`、`tool_use`、`stop_sequence` 继续记为 `completed`。
- 不改变 SSE 协议转换结果、重试和熔断行为。

## 设计

由 `streamconv.Converter` 暴露只读的终态查询方法，内部复用现有
`statusFor(stopReason)` 映射。`AnthropicBackend` 在流结束后读取该状态，并用于：

1. 最终结构化日志的 `status`。
2. `UpstreamEvent.Status`。
3. 间接驱动 server 层 client metrics 的最终状态。

转换层仍是 Anthropic stop reason 到 Responses status 的单一真相源，Backend 不复制
协议映射，也不从已经发出的 SSE 数据反向解析状态。

传输错误优先于业务终态：未锁流或非客户端取消的流读取错误仍记为 `failed`；只有流正常
结束，或业务终态已完整产生后发生客户端取消时，才采用转换器的业务终态。

## 测试

- Backend 回归测试：`max_tokens` 流必须产生 `UpstreamEvent.Status=incomplete`，最终日志
  同样为 `incomplete`。
- Server 集成测试：Anthropic `response.incomplete` 必须使 client metrics 记为
  `incomplete`。
- 保留并运行正常完成、流读取错误、客户端取消等现有测试，确认优先级未被破坏。

## 非目标

- 不调整默认 `max_tokens=4096`。
- 不修改 reasoning effort 或裁剪上下文、工具列表。
- 不改变 Responses、Chat Backend 的终态判定。
