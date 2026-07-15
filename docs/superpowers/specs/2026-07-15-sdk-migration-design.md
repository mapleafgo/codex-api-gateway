# OpenAI2Response 协议 SDK 迁移设计

日期: 2026-07-15
状态: 待实现

## 1. 背景与动机

OpenAI2Response 是面向 Codex CLI 的 OpenAI Responses API ↔ Anthropic Messages 协议适配 + 多源主备容灾网关（Go）。当前 model 层（协议类型）完全手写、零依赖。已完成 17 个 SDD 任务、Codex 兼容性修复、5 项 Response 协议修复。

基于官方 Go SDK（openai-go v3.42.0、anthropic-sdk-go v1.57.0）做枚举审计，发现手写 model 存在 P0 协议正确性 bug 与 P1/P2 完整性缺口（见 §5）。根因：手写类型易遗漏字段、事件名易拼错，且需手动跟进协议演进。

决策：**引入双官方 SDK 作为类型层**，一次性两侧（OpenAI 入站 + Anthropic 后端）全量改造，根治协议完整性，同时保留 HTTP 多源容灾与 SSE 中继自研。

## 2. 目标与非目标

**目标**
- 用 SDK 类型替换手写 model（`response.go`、`anthropic.go`），覆盖 P0+P1+P2 全部协议差距
- 出站 Response 事件/对象字段严格齐全，事件名用 SDK 常量杜绝拼错
- 一次性两侧全量改造，不留手写/SDK 混合中间态

**非目标**
- 不替换 HTTP 容灾层（scheduler/breaker/failover）、SSE 中继（server）——SDK client 单源端到端，不适合多源主备
- 不接入 OpenAI 内置工具（web_search/file_search/code_interpreter/audio/image_gen/mcp/computer）——Anthropic 后端无对应，Codex 不用
- 不实现 refusal/background/service_tier/moderation 等 Anthropic 无映射字段

## 3. Spike 结论（已实测，/tmp/spike2）

| 验证项 | 结果 |
|---|---|
| `responses.ResponseNewParams.UnmarshalJSON` 解析 Codex 请求 | ✅ 成功，Model/MaxOutputTokens/Tools/Reasoning 全部正确 |
| `param.Opt[T]` 解析外部 JSON | ✅ 有 UnmarshalJSON |
| `ResponseStreamEventUnion` Marshal（出站） | ❌ 扁平聚合无 omitempty，产生全字段零值污染（几百字节） |
| SDK 单独事件类型 Marshal（出站） | ⚠️ 简单事件（TextDelta）干净；含嵌套 union/Response 的事件（OutputItemAdded/Completed）严重零值污染 |
| goproxy.cn 可用性 | ✅ openai-go/v3@v3.42.0、anthropic-sdk-go@v1.57.0 均可 go get |
| 依赖体积 | ✅ openai-go/v3 仅多带 tidwall/gjson（4 子包） |

**关键结论：入站用 SDK 类型（解析友好）；出站用自定义 omitempty 事件 struct（SDK union/事件类型有零值污染，不直接 Marshal；亦不手写 map/字符串）。**

## 4. 分层设计

| 数据流 | 现状（手写） | 改造后（SDK） |
|---|---|---|
| Codex 请求 → 网关 | `model.ResponseRequest` | `responses.ResponseNewParams` |
| 网关 → Anthropic 后端（请求） | `model.AnthropicRequest` | `anthropic.MessageNewParams` |
| Anthropic 后端 → 网关（SSE 流） | `model.AnthropicEvent` | `anthropic.MessageStreamEventUnion` |
| 网关 → Codex（SSE 事件） | 手写 `map[string]any` | **自定义带 omitempty 事件 struct**（type 引用 SDK constant；无 map、无字符串拼接、不 Marshal SDK 事件类型） |
| Response 对象（created/completed） | 手写 map | 带 omitempty 专用 struct，补齐 required 字段 |
| HTTP 容灾 / SSE 中继 | 自研 | **保留不动** |

### 4.1 出站事件 struct 化（不手写 JSON）
出站全部用自定义带 `omitempty` 的事件 struct，type 字段引用 SDK `shared/constant` 常量。**不用 `map[string]any`、不字符串拼接、也不直接 Marshal SDK 事件类型**（spike 实测：含嵌套 union/Response 的 SDK 事件类型会输出几十个无关零值字段）。例：
```go
type outputTextDeltaEvent struct {
    Type           string `json:"type"`                    // string(constant.ResponseOutputTextDelta("response.output_text.delta"))
    SequenceNumber int64  `json:"sequence_number,omitempty"`
    OutputIndex    int    `json:"output_index"`
    ItemID         string `json:"item_id"`
    Delta          string `json:"delta"`
}
```
每个事件一个 struct，`json.Marshal` 自动产出干净的协议 JSON。Response 对象（created/completed/incomplete/failed 内嵌）同样用自定义 struct 精确回显 P2 字段（§4.2），避免 SDK `Response` 类型的全字段零值污染。

### 4.2 Response 对象回显策略（P2）
completed/created 的 response 对象字段来源：
- **从请求回显**：instructions, temperature, top_p, max_output_tokens, tool_choice, tools, reasoning, previous_response_id, parallel_tool_calls, truncation, text, model
- **从 Anthropic 响应/运行时**：id（已有）、created_at（网关收到 message_start 时间）、completed_at（完成时间）、usage（已有）、status（已有）、output（已有）
- **置默认**：object="response"、metadata（空 map）、error（空）、incomplete_details（空或填 reason）、其余 nullable 字段按 SDK 默认

server 需把请求参数透传给出站构造（目前 converter 只有 respID/model）。

## 5. 范围（P0+P1+P2 全量）

### P0 协议正确性 bug
1. 事件名 `response.reasoning.delta` → SDK 常量；明文用 `response.reasoning_text.delta`，summarized 模式用 `response.reasoning_summary_text.delta`
2. completed 的 response 对象补 `object:"response"`
3. completed 的 response 对象补 `output` 数组

### P1 协议完整性
4. `response.in_progress`（created 后）
5. `response.incomplete` + `incomplete_details`（stop_reason 非 end_turn/tool_use）
6. `response.output_text.done`
7. `response.content_part.added` / `.done`
8. `response.reasoning_text.done` / `reasoning_summary_text.done` + `reasoning_summary_part.added/.done`
9. 中流错误：`response.error` vs `response.failed`（error 事件含 code/message/param）
10. `sequence_number` 加到所有出站事件
11. 入站 `include` 参数（控制 reasoning.encrypted_content）

### P2 回显与透传
12. Response 对象回显 §4.2 全部字段
13. 请求参数透传：parallel_tool_calls、metadata、prompt_cache_key、user、truncation、stream_options（能映射给 Anthropic 的映射，不能的接受后忽略）

### 排除（Anthropic 无映射 / Codex 不用）
background、service_tier、moderation、prompt、conversation、max_tool_calls、top_logprobs、safety_identifier；所有内置工具事件；refusal。

## 6. 影响文件

- **删除**：`internal/model/response.go`、`internal/model/anthropic.go`（SDK 替代）及对应 `_test.go`
- **重写**：`internal/convert/request.go`、`image.go`（ResponseNewParams → MessageNewParams）；`internal/streamconv/converter.go`（MessageStreamEventUnion → 出站事件）
- **改动**：`internal/anthropic/client.go`（SDK 类型）；`internal/store/session.go`（存 SDK InputItem）；`internal/server/server.go`（解析 ResponseNewParams、透传请求参数；SSE 中继保留）
- **新增**：`go.mod` 加 openai-go/v3 + anthropic-sdk-go；出站事件 struct 文件
- **不动**：`internal/config`、`internal/scheduler`、`internal/breaker`、`cmd/server`

## 7. 测试策略

- 各包现有测试改写为 SDK 类型，保持覆盖
- 新增：出站事件 type 用 SDK 常量断言（防拼错回归）；Response 对象 required 字段齐全断言；sequence_number 单调断言
- spike2 的入站解析用例纳入 convert 测试
- Anthropic 侧 MessageStreamEventUnion 解析用例纳入 streamconv 测试
- 收口：`go test ./...` 全绿 + `go build ./...`

## 8. 风险与对策

| 风险 | 对策 |
|---|---|
| Anthropic SDK 构造/解析模式未实测 | 实现第一步：spike `MessageNewParams` 构造 + `MessageStreamEventUnion` 解析 |
| 出站 union 零值污染 | 已规避：手写 struct + omitempty |
| SDK 类型构造（union/param）学习成本 | openai 侧 spike 已验证可行；Anthropic 侧同模式 |
| 二进制体积 | 仅 import responses + message 子包，Go 死代码消除未用 service |
| model 删除导致 store/server 大改 | store 存 SDK InputItem，server 适配，逐包编译验证 |

## 9. 实现顺序（预告，详由 writing-plans 展开）

1. Anthropic 侧 spike 验证（构造 + 解析）
2. go.mod 引依赖
3. 删手写 model，convert 重写（入站）
4. streamconv 重写（出站事件 struct + 常量）
5. anthropic client + store + server 适配
6. 测试改写 + 新增
7. `go test ./...` + `go build ./...` 全绿
