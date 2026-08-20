# Data Model: 剔除正文思维标签处理

## 实体

### 正文内容（用户可见文本）

- 来源：Chat 流 `delta.content`
- 行为：剔除后由上游到客户端原样透传（`output_text`），不做任何标签识别/剥离/改写
- 状态：无跨 chunk 缓冲、无标签状态，流末直接关闭 message item

### 推理内容（reasoning 通道）

- 来源：Chat 流独立推理字段（`delta.reasoning_content` / `delta.reasoning` /
  `delta.reasoning_text`）
- 行为：保持既有映射（reasoning item + `reasoning_text.delta/done`），不受本次变更影响
- 状态：既有 `feedReasoning` 状态机不变

## 状态机变化

- 删除：`isThinking` / `thinkBuf` / `thinkLastTag` 及其跨 chunk 暂存逻辑
- 删除：标签前缀尾部匹配（`tagPrefixLen` / `trailingTagPrefixLen`）与流末/丢弃路径的
  状态收口（`flushThinkEnd` / `resetThinkEnd`）
- 保留：原生推理字段映射、`content_filter` 丢弃路径（不再需要标签状态重置）

## 校验规则（映射自 spec FR）

- `delta.content` → `output_text`，逐字透传（FR-001/FR-002）
- 独立推理字段 → reasoning 通道（FR-003）
- 流末/并发不残留标签状态（FR-004/FR-005）
