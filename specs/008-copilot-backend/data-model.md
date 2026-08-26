# Data Model: GitHub Copilot 原生后端接入

## 核心结构体

### CopilotBackend（internal/backend/copilot.go）

`g` 后端的顶层结构，实现 `backend.Backend` 接口。组合已有的三个 Backend 做委托。

```go
type CopilotBackend struct {
    Responses *ResponsesBackend  // 委托 r 路径
    Anthropic *AnthropicBackend  // 委托 a 路径
    Chat      *ChatBackend       // 委托 c 路径
    Client    *copilotclient.Client // endpoint 与模型目录客户端
}
```

**生命周期**: 由 `scheduler.New()` 创建一次，所有 `g` 源共享同一个实例（与 a/c/r 一致）；`internal/copilotclient.Client` 内部按 source name 隔离 token、endpoint 与模型缓存，并在 token 或显式 endpoint 变更时重建该 source 状态。管理页旁路只创建自己的 `copilotclient.Client`，不持有 Backend。

**关键方法**:
- `Execute(ctx, rawBody, src, cfg, onEvent, onUpstream, attempt)` — 实现 Backend 接口

### sourceState（internal/copilotclient，per-source 认证与 endpoint 发现）

```go
type sourceState struct {
    githubToken string    // 从 config.Source.GithubToken 获取
    endpointOverride string // 显式 base_url
    endpoint    string    // GraphQL 发现的 Copilot API 地址
    mu          sync.Mutex // endpoint 发现的串行化
    discovered  bool       // 是否已完成 GraphQL 发现
    modelsCache *modelCache
}
```

**状态转换**:
- 初始：`discovered = false`
- 首次请求触发 GraphQL 发现 → `discovered = true, apiEndpoint = <url>`
- 发现失败 → `apiEndpoint = "https://api.githubcopilot.com"` (默认回退)
- 配置热重载（github_token 变更）→ 需要重新发现（通过 source 标识判断是否同一个 source）

**并发控制**: GraphQL 发现用 `endpointMu` 串行化，发现完成后只读 `apiEndpoint`。

### modelCache（internal/copilotclient，per-source 模型目录缓存）

```go
type modelCache struct {
    http     *http.Client
    sf       singleflight.Group
    mu       sync.RWMutex
    models   map[string]*ModelInfo  // model ID → 能力信息
    cachedAt time.Time
    ttl      time.Duration                 // 默认 5 分钟
    valid    bool
}
```

### ModelInfo（internal/copilotclient，筛选后的模型能力）

```go
type ModelInfo struct {
    ID                string   `json:"id"`
    SupportedEndpoints []string `json:"supported_endpoints"` // 如 ["/responses", "/chat/completions"]
}
```

## 配置模型变更

### config.Source 新增字段

```go
type Source struct {
    // ... 现有字段 ...
    GithubToken string `koanf:"github_token" yaml:"github_token,omitempty"`
}
```

### config 常量

```go
const BackendGitHubCopilot = "g"
```

### NormalizeBackendType 新增分支

```go
case BackendGitHubCopilot:
    return BackendGitHubCopilot, nil
```

## 路由决策数据流

```
客户端请求 (model=X)
  → CopilotBackend.Execute
    → resolveModel(src, modelX) → resolvedModel
    → copilotclient.Client.Directory(src) → endpoint + []ModelInfo
    → 查找 resolvedModel 对应 ModelInfo（未找到为 nil）
    → routeByEndpoints(modelInfo) → "r" | "a" | "c"
    → 委托对应 Backend.Execute（用 Copilot endpoint + OAuth token + Zed header）
```

`routeByEndpoints` 逻辑：
1. 若 modelInfo 为 nil（不在目录/缓存失败）→ 返回 "r"（默认）
2. 遍历优先级 ["/responses", "/v1/messages", "/chat/completions"]
3. 返回第一个 modelInfo.SupportedEndpoints 包含的端点对应的路径
4. 都不匹配 → 返回 "r"（默认）
