# Contract: Observability

## Backend Identity

观测身份统一为稳定插件 ID：

```go
Backend string
```

JSON wire 字段：

```json
{ "backend": "github-copilot" }
```

替换所有 `backend_type` / `a` / `c` / `g` / `r` 短码观测。内置稳定值：

| 插件 | 稳定 ID |
|---|---|
| Anthropic Messages | `anthropic` |
| OpenAI Chat Completions | `openai-chat` |
| OpenAI Responses 透传 | `openai-responses` |
| GitHub Copilot | `github-copilot` |

## UpstreamEvent

```go
type UpstreamEvent struct {
    SourceName    string
    Model         string
    ResolvedModel string
    StartedAt     time.Time
    Duration      time.Duration
    TTFB          time.Duration
    Status        string
    Code          int
    InputTokens   int
    OutputTokens  int
    CacheRead     int
    CacheCreate   int
    Error         string
    Attempt       int
    Backend       string
}
```

## Token Normalization

- 各插件在上报前统一为“完整输入 Token”。
- Anthropic 插件在 `InputTokens` 中加回 `CacheRead + CacheCreate`。
- metrics 删除 `backend_type == 'a'` 归一化分支，只信任事件中的值。
- 透传 Responses 插件传递上游 usage 完整值。

## Sensitive Data

禁止记录：Authorization、API key、`x-api-key`、Cookie、GitHub token、device code、access token、MCP authorization。

- 日志：不写入凭据字段；端点可记录，Authorization 值不可。
- 指标：不写入凭据。
- 管理响应与 SSE：脱敏。
- 分发器日志可记录被委托 route 与 endpoint，但不得记录 token。

错误详情在跨越进程边界前必须经过统一 sanitizer：上游 body/error、插件 error、Device Flow/OAuth error 都先移除 Authorization/key/token/cookie 值和常见 token 形态，再进入管理 JSON、SSE error 事件或 metrics。内部日志也使用同一结果，避免旁路输出绕过红线。

## Structured Log Keys

请求与上游日志统一携带：

```text
request id, source, backend, attempt, status/error
```

终点缺失时补默认值：`backend: "unknown"`。合法空观测量允许用 `"unknown"`，禁止伪造真实身份。
