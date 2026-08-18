# 设计：c 路径正文思维标签 LangChain 式解析

**Status**: Approved（用户选择方案 A，要求一比一实现）
**Date**: 2026-08-19
**Scope**: `internal/chatstreamconv` 的 c 流式出站正文解析；不涉及历史回灌。

## 1. 背景

此前网关清理了正文思维标签解析，正文中的标签会原样进入 `output_text`。现在重新处理：

- 目标标签形态为 `<think>` 与 `</think>`。
- 按 LangChain DeepSeek 社区实现的语义一比一落地，旧网关的自研抽取实现不作参考。
- `</think>` 同时作为开始与结束标签（toggle）的情况也必须处理。
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
6. `</think>` 在非思维态下同样作为开标签（双角色扩展，LangChain 之外的用户要求）。

当前 main 分支同文件保持同一状态机（后续 streamEvents 重构未改变标签解析逻辑）；
Python 版 `langchain-deepseek` 只透传 `reasoning_content`、不解析正文标签，因此
一比一基线确定为 langchainjs 实现。

扩展说明：上述状态机中非思维态对 `</think>` 的处理是用户要求的双角色语义，其余行为
（`<think>` 开、`</think>` 闭、跨 chunk 拼接、闭标签后残余立即作正文、流末按当前态收口）
逐条对齐 LangChain。

### 对照证据（commit `1877454e6a501eba7bf36fc088335eaea149c8ce`）

| 行为 | 源码位置（`chat_models.ts`） | 语义 |
|---|---|---|
| 原生 reasoning 分片原样透传 | 485-488 | 整 chunk 不做标签解析，content 原样透传 |
| 空 text 分片原样透传 | 490-494 | 不清 buffer、不改变 isThinking |
| open tag 命中 | 500-529 | before 段作正文，进入 thinking，after 段留在 buffer |
| close tag 命中 + 残余处理 | 531-591 | thought 段作 reasoning；残余立即作正文并清空 buffer |
| thinking 内残缺 close tag 最长后缀暂存 | 577-633 | 安全部分先作 reasoning，前缀保留 |
| 非 thinking 残缺 open tag 最长后缀暂存 | 635-683 | 安全部分先作正文，前缀保留 |
| 流末收口 | 687-710 | thinking→reasoning，否则作正文 |

### Open WebUI（作为背景）

Open WebUI 的 `DEFAULT_REASONING_TAGS` 同样把 `('<think>', '</think>')` 作为默认第一组，并且按 provider 决定是否把 reasoning 回灌历史：

- `ollama` 需要 `<think>` 标签回灌。
- `llama.cpp` 支持 `reasoning_content` 字段回灌。
- 其他 provider 默认不回灌，避免模型模仿标签。

本设计只做流式解析，不把回灌逻辑纳入范围，符合用户确认的“历史回灌维持现状”。

## 3. 行为契约

### 3.1 原生 reasoning 优先

单个 Chat chunk 若同时携带 `reasoning_content` / `reasoning` / `reasoning_text` 与 content：

- 该 chunk 的 reasoning 字段仍走现有 `feedReasoning`。
- 该 chunk 的 content 不进入标签解析，直接作为 `output_text` 累积。
- 后续没有 reasoning 字段的 chunk，再按标签状态机解析。
- 只有空 content、无 reasoning 的分片（如 tool_calls / usage / finish_reason
  分片）原样透传，不触碰 buffer 与 isThinking。

### 3.2 标签状态机

常量：

- open tag：`"<think>"`
- close tag：`"</think>"`

状态：

- `thinkBuffer`：当前尚未完全消费的 content 文本。
- `isThinking`：当前是否处于思维块内。

处理顺序（每个 content chunk，且该 chunk 无原生 reasoning 字段时）：

1. 将 chunk 文本追加到 `thinkBuf`，循环扫描直到 buffer 消费完。
2. 非 thinking 状态：
   - 若 `thinkBuf` 含完整 `<think>` 或 `</think>`（取最早出现者），标签前的文本
     先输出为 `output_text`，消费标签进入 thinking，剩余内容继续同一次迭代扫描
     （LangChain 对同一 chunk 内先开后闭逐段处理）。
   - `</think>` 在非思维态下同样作为开标签（双角色扩展）。
   - 无完整标签：仅保留尾部可能是 `<think>` / `</think>` 前缀的文本，其余安全文本
     立即输出为 `output_text`。
3. thinking 状态：
   - 若 `thinkBuf` 含完整 `</think>`，闭标签前的文本作为 reasoning 输出，退出
     thinking；闭标签后的残余文本立即作为 `output_text` 输出并清空 buffer，不再
     对该残余做二次标签解析（LangChain 源码 `tokensBuffer = ""` 语义；残余即使含
     `<think>` 也按正文透传）。
   - 无完整闭标签：仅保留尾部可能是 `</think>` 前缀的文本，其余安全文本立即作为
     reasoning 输出。`<think>` 出现在思维块内按 LangChain 语义作为思维文本保留，
     不作为标签处理。
4. 空 content 分片：不追加 buffer、不改变 isThinking，分片本身原样透传。

### 3.3 事件顺序

解析后的连续输出顺序必须与 LangChain 切片顺序一致：

- `output_text` 开标签前内容。
- `reasoning` 思维块内容。
- `output_text` 闭标签后内容。

Responses 转换层已有能力保证：`feedText` 遇到已开启的 reasoning 时先关闭 reasoning 再开 message，因此输出顺序正确。

### 3.4 流结束

`FeedDone` 或 finish_reason 收口时：

- `isThinking == true`：`thinkBuf` 剩余内容（含残缺 `</think>` 前缀）作为 reasoning 收口。
- `isThinking == false`：`thinkBuf` 剩余内容（含残缺标签前缀）作为 `output_text` 收口。

与 LangChain 的 end-of-stream flush 一致：缓冲原样按当前态输出，不额外丢弃残缺前缀。

### 3.5 工具调用交错

思维块内出现 tool_calls delta 时，按现有 `feedToolCall` 语义先关闭 reasoning，
再进入工具 item。tool_calls 分片通常不带 content，按空 content 分片处理：不改变
buffer 与 isThinking；其后的 content 按当前状态继续解析。

## 4. 边界与错误处理

- 孤立 close tag：非 thinking 状态下的 `</think>` 字节形态作为开标签进入思维块（toggle 扩展）；thinking
  状态下作为闭标签退出。不沿用旧实现的 reasonOpen 特判。
- `<think>` 出现在思维块内：作为思维文本保留，不改变状态（LangChain 语义）。
- 连续相同标签去重：同一 chunk 内紧邻的相同标签合并为单个分隔符；跨 chunk 连续同标签
  同样去重，避免 `</think></think>` 先开后闭或 toggle 抖动。LangChain 原版不去重，
  连续标签去重是本特性用户要求的扩展。
- 闭标签后同 chunk 残余：立即作为正文输出并清空 buffer，不再二次解析；残余中的
  标签按普通正文透传（LangChain 语义）。
- 标签精确匹配：只识别 `<think>` 与 `</think>`；`< think>`、`</think >`、大小写
  变体不识别，按普通文本或思维文本原样输出。
- 思维文本中出现字面 `</think>`：LangChain 不处理转义，按闭标签立即关闭。
- 空 content 分片：不清空 buffer，不改变 isThinking。
- 原生 reasoning 分片：该 chunk 的 content 原样透传，不进入标签解析（LangChain
  整 chunk pass-through 语义）。
- `content_filter`：按现有 refusal 路径丢弃半截文本/工具，并清空思维标签状态。

## 5. 测试矩阵

在 `internal/chatstreamconv` 增加表驱动测试：

1. 标准完整 `<think>a</think>b`：reasoning=a、正文=b，标签不泄漏。
2. open tag 跨 chunk 拆分（残缺前缀拼接）。
3. close tag 跨 chunk 拆分。
4. 流末未闭合 `<think>a`：剩余 buffer 作为 reasoning 收口。
5. toggle 完整 `</think>a</think>b`：reasoning=a、正文=b。
6. toggle 跨 chunk 拆分。
7. toggle 开标签后思维块内出现 `<think>`：作为思维文本保留。
8. 闭标签后同 chunk 残余不二次解析：`<think>a</think><think>b` 的正文为 `<think>b`。
9. 空 content 分片（如 tool_calls 分片）插入思维流：buffer 与 thinking 状态保持不变。
10. 原生 `reasoning_content` 与 content 同 chunk：content 原样透传，不按标签解析。
11. 流末残缺标签前缀按当前态收口（不丢弃）。
12. `< think>` / `</think >` / 大小写变体：按普通文本或思维文本输出，不改变状态。
13. thinking 输出后接 tool_calls：顺序为 reasoning 关闭 → tool item。
14. `content_filter`：不残留 thinking 状态。
15. 连续闭标签去重：`</think>a</think></think>b` 的 reasoning=a、正文=b，不 toggle。
16. 连续 toggle 开标签去重：`</think></think>a` 的 reasoning=a，不产生空思维块。
17. 连续标准开标签去重：`<think><think>a` 的 reasoning=a。
18. 连续相同标签跨 chunk 拆分去重：上 chunk 以完整标签收尾，下 chunk 开头同标签。
19. 残缺标签前缀跨仅携带 logprobs 的空 content 分片保留：LangChain 空 text 分片原样透传、不改 `tokensBuffer`；本实现以 `origEmpty` 短路跳过缓冲扫描，保证 `<thi` 类前缀不被误判为安全文本下发。

## 6. 非目标

- 不修改 Responses → Chat 历史回灌的 `reasoning_content` 行为。
- 不新增多标签注册表（`<think>`、`<reasoning>` 等）。
- `<think>` 在思维块内不做开/闭切换（按 LangChain 语义作为思维文本）；不沿用旧实现
 的 reasonOpen 特判；`</think>` 双角色与连续同标签去重是本特性在 LangChain 之外的
  用户要求扩展。
- 不做标签空白与大小写容错。
- 不修改 Anthropic `a` 路径或 Responses `r` 路径。

## 7. 实现位置

- `internal/chatstreamconv/converter.go`：状态与解析函数。
- `internal/chatstreamconv/content_tags_pass_through_test.go`：替换为 LangChain 解析行为测试，或新增独立测试文件删除旧的透传测试。

## 8. 验收门禁

- `go test ./internal/chatstreamconv -count=1`
- `task check`
- `task test-race`
- `golangci-lint run ./...`
