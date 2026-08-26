# Research: GitHub Copilot 原生后端接入

## 1. 认证方式：Zed vs copilot2api

### Decision
采用 Zed 方式：直接用 GitHub OAuth token 作为 Bearer token 调用 Copilot API，不经过 Copilot session token 交换。

### Rationale
- copilot2api 的方式是 VSCode 伪装（OAuth → `api.github.com/copilot_internal/v2/token` 换短效 session token → 解析 `proxy-ep`），需要维护整套 token 刷新子系统（并发安全、5 分钟过期余量、singleflight）。
- Zed 的方式更简单：GitHub OAuth token 长期有效，直接作为 Bearer 即可，无需刷新逻辑。
- 用户确认参照 Zed 实现。

### Alternatives considered
- copilot2api 方式（VSCode 伪装）：实现复杂度高，需要 token 刷新子系统，已否决。

## 2. Copilot API Endpoint 发现

### Decision
用 GraphQL 查询 `api.github.com/graphql` 获取 `copilotEndpoints.api`，失败时回退到 `https://api.githubcopilot.com`。

### Rationale
- Zed 通过 GraphQL 动态发现 endpoint，支持 enterprise 部署。
- 默认 endpoint `https://api.githubcopilot.com` 是 Zed 的 `DEFAULT_COPILOT_API_ENDPOINT`。

### 关键细节
- GraphQL 查询用 GitHub OAuth token 做 Bearer 认证。
- 查询体参照 Zed 的 `discover_api_endpoint` 函数：查询 GitHub GraphQL API 的 `copilotEndpoints.api` 字段。
- 发现结果缓存在 `copilotclient.Client` 的 per-source state 中，生命周期跟随 source。
- 显式 `source.base_url` 是运行时覆盖，优先于发现结果，适合固定 enterprise endpoint 或本地调试代理。

## 3. 请求 Header 集合

### Decision
采用 Zed 风格 header，不使用 VSCode 伪装头。

### Header 清单
| Header | 值 | 说明 |
|--------|----|------|
| `Authorization` | `Bearer <github_oauth_token>` | 直接用 GitHub OAuth token |
| `Content-Type` | `application/json` | 标准 |
| `Editor-Version` | `Zed/0.1.0` | 对齐 Zed `copilot_chat` crate 的 `CARGO_PKG_VERSION` |
| `X-GitHub-Api-Version` | `2025-10-01` | Zed 使用的 API 版本 |
| `Accept` | `text/event-stream` | 流式请求 |

Copilot Messages（`a` 路径）不发送普通 Anthropic 源的 `x-api-key` 与 `anthropic-version`；认证只用 Bearer token。

### 不使用的 header（copilot2api/VSCode 专属）
- `Copilot-Integration-Id: vscode-chat` — VSCode 伪装，Zed 不发
- `Editor-Plugin-Version: copilot-chat/0.58.0` — VSCode 伪装
- `Openai-Intent: conversation-agent` — Zed 用 `OpenAI-Intent` 但值为 interaction 类型，网关场景固定可不发或发 `conversation-panel`
- `X-Request-Id` — copilot2api 生成随机 ID，Zed 不发

### Rationale
参照 Zed 的 `copilot_request_headers` 函数，保持最小必要 header 集合。

## 4. 模型目录筛选条件

### Decision
参照 Zed 的 `get_models` 过滤逻辑：
- `model_picker_enabled == true`
- `capabilities.type == "chat"`
- `policy` 为空 或 `policy.state == "enabled"`

### 不使用 `billing.restricted_to`
- `restricted_to` 是声明性元数据（如 `["pro", "pro_plus", "max"]`），表示模型限制在哪些套餐。
- Zed 不用它做客户端筛选——实际权限由 Copilot 后端在请求时裁决。
- 客户端发一个无权限的模型，上游会返回 403，网关正常映射这个错误。

## 5. 模型路由优先级 r > a > c

### Decision
按模型 `supported_endpoints` 字段，优先级 r > a > c：
- `/responses` → 委托 `ResponsesBackend`（r）
- `/v1/messages` → 委托 `AnthropicBackend`（a）
- `/chat/completions` → 委托 `ChatBackend`（c）

### Rationale
- r（Responses 透传）是无损优先级最高的路径。
- a（Anthropic Messages）是 Copilot 原生支持的 Claude 模型路径。
- c（Chat Completions）是兜底转换路径。

### 回退策略
- 模型不在目录中或目录拉取失败 → 默认走 `r`。
- 模型的 `supported_endpoints` 不含已知端点 → 默认走 `r`，让上游返回错误。

## 6. URL 构造

### Decision
参照 Zed 的 URL 构造函数（拼接到发现的 api_endpoint 上）：
- `/responses` → `<api_endpoint>/responses`
- `/v1/messages` → `<api_endpoint>/v1/messages`
- `/chat/completions` → `<api_endpoint>/chat/completions`

### 与现有 Backend 的对接
现有 `ResponsesBackend` 用 `responsesclient.Stream(ctx, baseURL, apiKey, body, headers)`，其中 `baseURL` 和 `apiKey` 来自 `config.Source`。对于 `g` 源：
- `baseURL` 替换为生效的 Copilot endpoint（显式 `src.BaseURL` 覆盖或 GraphQL 发现结果）
- `apiKey` 替换为 GitHub OAuth token（而非 `src.APIKey`）
- `headers` 注入 Zed 风格 header

这意味着 `CopilotBackend.Execute` 不能直接传 `src` 给委托的 Backend（因为 src 的 BaseURL/APIKey 不对），而是要构造一个修改过的 source 副本，或直接调用底层 client。

## 7. singleflight 依赖确认

### 需确认
`golang.org/x/sync/singleflight` 是否已在 go.mod 中。copilot2api 用了它做模型缓存去重。本网关需要确认。

### 方案
若已有则直接用；若没有，模型缓存可用 `sync.Mutex` + 时间戳做简化版去重（只有一个 goroutine 做 fetch，其余等待结果），或引入 singleflight 依赖。
