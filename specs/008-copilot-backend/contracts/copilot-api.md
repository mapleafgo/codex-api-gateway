# Copilot API 契约

参照 Zed（`zed-industries/zed` 的 `crates/copilot_chat/src/copilot_chat.rs`）实现提取。

## 认证

### GitHub OAuth Token
- 直接用 GitHub OAuth token 作为 `Authorization: Bearer <token>` 调用 Copilot API。
- Token 来自 `config.yaml` 的 `source.github_token` 字段（支持 `${ENV_VAR}` 插值）。
- 不经过 Copilot session token 交换（与 copilot2api 的 VSCode 伪装方式不同）。

### GraphQL Endpoint 发现
- **端点**: `https://api.github.com/graphql`（enterprise 部署可能不同，本功能先支持默认）
- **认证**: `Authorization: Bearer <github_oauth_token>`
- **查询**: 查询 `copilotEndpoints.api` 字段获取 Copilot API base URL
- **回退**: 查询失败时使用 `https://api.githubcopilot.com`
- **覆盖**: `source.base_url` 非空时直接作为固定 endpoint，跳过 GraphQL 发现

## 请求 Header

| Header | 值 | 必需 |
|--------|----|------|
| `Authorization` | `Bearer <github_oauth_token>` | 是 |
| `Content-Type` | `application/json` | 是 |
| `Editor-Version` | `Zed/0.1.0` | 是 |
| `X-GitHub-Api-Version` | `2025-10-01` | 是 |
| `Accept` | `text/event-stream`（流式请求） | 是 |

不注入 VSCode 伪装头（`Copilot-Integration-Id`、`Editor-Plugin-Version`、`Openai-Intent`、`X-Request-Id`）。
`/v1/messages` 路径不发送 `x-api-key` 与 `anthropic-version`，认证只用 Bearer token。

## 模型目录

### GET /models
- **URL**: `<api_endpoint>/models`
- **认证**: 同上 Bearer token + Zed header
- **响应**: OpenAI 风格 `{ "data": [...] }`，每个模型包含：
  - `id`: 模型标识（如 `gpt-5.3-codex`）
  - `supported_endpoints`: 数组，如 `["/responses", "/chat/completions"]`
  - `model_picker_enabled`: bool
  - `capabilities.type`: 如 `"chat"`
  - `policy`: `{ "state": "enabled" | "pending" | ... }` 或 null
  - `billing.restricted_to`: 数组（不参与筛选）

### 筛选条件
- `model_picker_enabled == true`
- `capabilities.type == "chat"`
- `policy` 为 null 或 `policy.state == "enabled"`

### 缓存
- TTL: 5 分钟
- 并发去重: singleflight

## 上游端点

参照 Zed URL 构造：
- `/responses` → `<api_endpoint>/responses`
- `/v1/messages` → `<api_endpoint>/v1/messages`
- `/chat/completions` → `<api_endpoint>/chat/completions`

## 路由优先级

按模型 `supported_endpoints`，优先级 r > a > c：
1. `/responses` → `ResponsesBackend`（r）
2. `/v1/messages` → `AnthropicBackend`（a）
3. `/chat/completions` → `ChatBackend`（c）

模型不在目录或缓存失败 → 默认 `r`。

## Responses 扩展字段

- `contextTier` 不主动注入；客户端缺省该字段时不得补 `long_context`。
- r/Responses 路径按 map 语义原样透传已存在的 `contextTier`。
- a/c 路径没有 Responses 顶层 `contextTier` 槽位，按各自协议矩阵处理。
