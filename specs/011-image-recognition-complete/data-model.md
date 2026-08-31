# Data Model: 图片识别三协议完全可用

## Decision（映射判定结果）

| 字段 | 类型 | 说明 |
|------|------|------|
| `Kind` | `Kind` | 映射判定类型：Mapped / FileID / Malformed |
| `URL` | `string` | wire 用原文（URL 或 data URI），仅 `KindMapped` 时有值 |
| `DataURI` | `bool` | 是否为内嵌数据图片 |
| `Detail` | `string` | detail 原文（low/high/auto/original），由各路径决定保留或丢弃 |
| `FileID` | `string` | 仅 `KindFileID` 时用于错误上下文 |

## Kind（判定类型枚举）

| 值 | 含义 |
|----|------|
| `KindMapped` | 有 image_url（URL 或 data URI），可安全映射到目标协议原生图片槽位 |
| `KindFileID` | 仅 file_id：网关无 OpenAI Files 凭据，协议不可映射 |
| `KindMalformed` | 无 URL 也无 file_id，畸形输入 |

## 三路径映射对照

| 角色 | a (Anthropic) | c (Chat) | r (Responses) |
|------|---------------|----------|----------------|
| user 消息（URL/data-URI） | `imageBlock(URL)` → image block | `image_url` part with detail | 透传 |
| user 消息（file_id） | 源级失败 error | 源级失败 error | 保留引用 |
| tool 结果（URL/data-URI） | `imageBlock(URL)` → tool_result image | 聚合 user `image_url` part | 透传 |
| tool 结果（file_id） | 源级失败 error | 源级失败 error | 保留引用 |
| system/developer（URL/data-URI） | 源级失败 error（Anthropic system 仅文本） | 源级失败 error（Chat system 仅文本 parts） | 透传 |
| system/developer（file_id） | 源级失败 error | 源级失败 error | 保留引用 |
| detail 档位 | 有损丢弃 + 矩阵登记 lossy | 保留透传 | 原生保留 |

## 状态迁移

每条图片输入经过 `Inspect*` → `Decision` → 各路径消费：

```text
[input_image] → [Inspect*] → Decision
                              ├─ KindMapped   → a/c 构造原生槽位（含 detail 取舍）
                              ├─ KindFileID   → a/c 返回 error 源级失败；r 保留引用
                              └─ KindMalformed → a/c 返回 error 源级失败
```

每次消费后该 Decision 可丢弃，无持久化状态。
