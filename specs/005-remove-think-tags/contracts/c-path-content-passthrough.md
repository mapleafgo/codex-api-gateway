# Contract: c 路径正文思维标签剔除后的 wire 行为

## 场景

`/v1/responses`（客户端）→ `backend_type: c` 上游 → Responses SSE。

## 正文透传

| Chat 流 | Responses 事件 | 状态 |
|---|---|---|
| `delta.content`（含 `<thinking>...` 等任意文本） | `output_text` delta 原样透传 | `supported` |
| `choices[].logprobs.content` | `output_text` delta/done 概率 | `supported` |

规则：

- content 内的思维标签文本 MUST NOT 被识别、剥离或改写。
- content 内的标签文本 MUST NOT 产生 reasoning 内容。
- 标签跨分片碎片 MUST 按普通文本逐片透传，不拼接、不暂存。

## 独立推理字段（保持不变）

| Chat 流 | Responses 事件 | 状态 |
|---|---|---|
| `delta.reasoning_content` / `delta.reasoning` / `delta.reasoning_text` | reasoning item + `reasoning_text.delta/done` | `lossy_supported` |

## 流结束

- `finish_reason` / `[DONE]` 时直接关闭已开 item，MUST NOT 产生标签相关状态残留。
- `content_filter` 丢弃路径只清半截文本/工具/推理与 refusal 输出，无需标签状态重置。
