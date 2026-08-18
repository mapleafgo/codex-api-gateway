# 设计：c 路径正文思维标签 LangChain 式解析

**Status**: Approved（用户选择方案 A，要求一比一实现）
**Date**: 2026-08-19
**Scope**: `internal/chatstreamconv` 的 c 流式出站正文解析；不涉及历史回灌。

## 1. 背景

此前网关清理了正文思维标签解析，正文中的标签会原样进入 `output_text`。现在重新处理：

- 目标标签形态为 ` thinking` 与 ` response`（带前导空格）。
- 按 LangChain DeepSeek 社区实现的语义一比一落地。
- 仅修改 Chat 上游 SSE → Responses SSE 的流式转换，不改 `chatconvert` 历史回灌。

## 2. 社区依据

### LangChain DeepSeek `ChatDeepSeek`

参考实现：

- PR: https://github.com/langchain-ai/langchainjs/pull/9726
- Commit: `1877454e6a501eba7bf36fc088335eaea149c8ce`
- 文件：`libs/providers/langchain-deepseek/src/chat_models.ts`

核心语义：

1. 上游 chunk 已带 `reasoning_content` 时，直接透传该 chunk，不做 content 标签解析。
2. 无原生 reasoning 时，用 `tokensBuffer` + `isThinking` 状态解析 content。
3. 开标签前文本作为正文输出；标签之间文本作为 reasoning 输出；闭标签后文本继续作为正文输出。
4. 标签可能跨 chunk 拆分，保留残缺标签前缀，优先做最长后缀匹配。
5. 流结束时若仍在 thinking 状态，剩余 buffer 作为 reasoning 收口；否则作为正文输出。

### Open WebUI（作为背景）

Open WebUI 的 `DEFAULT_REASONING_TAGS` 同样把 `(' thinking', ' response')` 作为默认第一组，并且按 provider 决定是否把 reasoning 回灌历史：

- `ollama` 需要 ` thinking` 标签回灌。
- `llama.cpp` 支持 `reasoning_content` 字段回灌。
- 其他 provider 默认不回灌，避免模型模仿标签。

本设计只做流式解析，不把回灌逻辑纳入范围，符合用户确认的“历史回灌维持现状”。

## 3. 行为契约

### 3.1 原生 reasoning 优先

单个 Chat chunk 若同时携带 `reasoning_content` / `reasoning` / `reasoning_text` 与 content：

- 该 chunk 的 reasoning 字段仍走现有 `feedReasoning`。
- 该 chunk 的 content 不进入标签解析，直接作为 `output_text` 累积。
- 后续没有 reasoning 字段的 chunk，再按标签状态机解析。

### 3.2 标签状态机

常量：

- open tag：`" thinking"`
- close tag：`" response"`

状态：

- `thinkBuffer`：当前尚未完全消费的 content 文本。
- `isThinking`：当前是否处于思维块内。

处理顺序（每个 content chunk）：

1. 将文本追加到 `thinkBuffer`。
2. 非 thinking 状态：若 `thinkBuffer` 含完整 open tag，则：
   - open tag 前的文本先输出为 `output_text`；
   - 消费 open tag，进入 thinking；
   - 剩余内容留在 buffer。
3. 非 thinking 状态：若文本尾部是 open tag 的前缀，则保留前缀在 buffer，其余安全文本输出为 `output_text`。
4. thinking 状态：若 `thinkBuffer` 含完整 close tag，则：
   - 闭标签前的文本作为 reasoning 输出；
   - 消费 close tag，退出 thinking；
   - 闭标签后的文本留在 buffer，当前 chunk 结束后按正文输出或进入下一 chunk。
5. thinking 状态：若文本尾部是 close tag 的前缀，保留前缀，其余作为 reasoning 输出。
6. thinking 状态且无标签边界：全部文本按 reasoning 输出。
7. 非 thinking 状态且无标签边界：全部文本按正文输出。

### 3.3 事件顺序

解析后的连续输出顺序必须与 LangChain 切片顺序一致：

- `output_text` 开标签前内容。
- `reasoning` 思维块内容。
- `output_text` 闭标签后内容。

Responses 转换层已有能力保证：`feedText` 遇到已开启的 reasoning 时先关闭 reasoning 再开 message，因此输出顺序正确。

### 3.4 流结束

`FeedDone` 或 finish_reason 收口时：

- `isThinking == true`：buffer 剩余内容作为 reasoning 收口。
- `isThinking == false`：buffer 剩余内容作为 `output_text` 收口。

### 3.5 工具调用交错

思维块内出现 tool_calls delta 时，按现有 `feedToolCall` 语义先关闭 reasoning，再进入工具 item。标签状态机在 tool_calls 后才开始的 content 上继续独立维护。

## 4. 边界与错误处理

- 孤立 close tag：非 thinking 状态下视为普通正文，不开启 thinking。
- 重复 open tag：thinking 状态内再次出现 open tag 文本原样作为 reasoning 内容，不改变状态。
- 重复 close tag：非 thinking 状态下视为普通正文；thinking 状态下第一个 close tag 关闭思维块，之后文本从最新位置继续判断。
- `content_filter`：按现有 refusal 路径丢弃半截文本/工具，不产生 reasoning 残留状态。
- 同一标签做 toggle：LangChain 语义下不支持；没有完整 open tag 就不会进入 thinking。本设计按 LangChain 一比一实现，不额外支持 toggle。

## 5. 测试矩阵

在 `internal/chatstreamconv` 增加表驱动测试：

1. 完整 ` thinking... response`：产出 reasoning + 正文，标签不泄漏。
2. open tag 与文本在同一 chunk，close tag 在下一 chunk。
3. open tag 跨 chunk 拆分（残缺前缀拼接）。
4. close tag 跨 chunk 拆分。
5. open tag 前有普通正文。
6. close tag 后有普通正文。
7. 流结束仍在 thinking：剩余 buffer 作为 reasoning。
8. 孤立 close tag：按普通正文透传，不生成 reasoning。
9. 原生 `reasoning_content` 与 content 同 chunk：content 不再按标签解析，标签原样保留。
10. thinking 输出后接 tool_calls：顺序为 reasoning 关闭 → tool item。
11. `content_filter`：不残留 thinking buffer。

## 6. 非目标

- 不修改 Responses → Chat 历史回灌的 `reasoning_content` 行为。
- 不新增多标签注册表（`<thinking>`、`<reasoning>` 等）。
- 不实现同一标签 toggle 语义。
- 不修改 Anthropic `a` 路径或 Responses `r` 路径。

## 7. 实现位置

- `internal/chatstreamconv/converter.go`：状态与解析函数。
- `internal/chatstreamconv/content_tags_pass_through_test.go`：替换为 LangChain 解析行为测试，或新增独立测试文件删除旧的透传测试。

## 8. 验收门禁

- `go test ./internal/chatstreamconv -count=1`
- `task check`
- `task test-race`
- `golangci-lint run ./...`
