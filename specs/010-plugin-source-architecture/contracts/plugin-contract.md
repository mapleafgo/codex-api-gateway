# Contract: Source Plugin

## Package Boundary

- `internal/plugin` 只定义契约、注册表和跨插件宿主能力。
- `internal/plugins/<plugin-id>` 是具体实现边界；一个源的全部协议客户端、缓存、路由、探测和管理动作都放在该包内。
- `scheduler`、`server`、`admin`、`health`、`configwatch` 可以 import `internal/plugin`，禁止 import `internal/plugins/*`。
- `internal/plugin` 可依赖 `config`、`model`、`logging` 等基础层；基础层不得反向 import `plugin`。
- `cmd/server` 是唯一构造和注册内置插件的位置。

## Mandatory Interface

```go
type ID string

type SourcePlugin interface {
    Descriptor() Descriptor
    ValidateSource(config.Source) error
    Backend() Backend
}
```

`ValidateSource` 必须返回包含 source name、字段名和原因的错误。它必须纯校验，不得修改 holder 或磁盘配置。

`Backend` 保持现有流式语义：

```go
type Backend interface {
    Execute(
        ctx context.Context,
        rawBody []byte,
        src config.Source,
        cfg *config.Config,
        onEvent func(model.SSEEvent) error,
        onUpstream func(UpstreamEvent),
        attempt int,
    ) error
}
```

Backend 返回 error 且未产生用户可见终态时允许 failover；一旦发出 failed/incomplete 终态，平台调度器必须锁定结果。插件不得自行实现整轮重试或全局熔断。

## Optional Interfaces

### Request Preparation

```go
type RequestPreparer interface {
    PrepareRequest(ctx context.Context, req *PrepareRequestInput) error
}
```

Server 对首启用源做预检；错误按现有协议转换错误返回 400，不建立 SSE 响应。该接口用于提前暴露不可映射字段，不是能力裁决点。

### Model Catalog

```go
type ModelCatalog interface {
    ListModels(ctx context.Context, src config.Source) ([]Model, error)
}

type DraftModelCatalog interface {
    ListDraftModels(ctx context.Context, src config.Source) ([]Model, error)
}
```

`ListModels` 服务已保存源；`ListDraftModels` 服务管理页未保存草稿，敏感 options 由 admin 先完成保留值合并。不支持时返回 `ErrCapabilityNotSupported`。

### Health Probe

```go
type HealthProbe interface {
    Probe(ctx context.Context, src config.Source) Result
}
```

Result 保持 operational / degraded / failed 三态和耗时、HTTP status。插件决定目标端点、header 和认证方式。

### Admin Extension

```go
type ActionRoute struct {
    ID     string
    Method string
    Path   string // 相对 /admin/api/source-plugins/<id>
}

type AdminExtension interface {
    InvokeAction(ctx context.Context, req ActionRequest) (ActionResult, error)
}
```

Action 元数据在 Descriptor 中声明；每个 action 必须声明可执行的 route 列表。共享 admin 只做路由、JSON 编解码、body limit、recover 和凭据红线检查，不解释 action 的业务语义。`ActionRequest` 至少包含 plugin ID、action ID、route ID、HTTP method、公开 source 草稿和已解析 body。路径冲突、非允许 method、schema 外 source 字段由共享 admin 拒绝。

## Delegation Host

```go
type DelegateHost interface {
    BackendByID(id ID) (Backend, error)
}

type DelegateConsumer interface {
    SetDelegateHost(DelegateHost)
}
```

规则：

- 分发型插件只通过稳定插件 ID 请求被委托 Backend，不 import 其他插件实现包。
- 宿主由 Registry 在 `cmd/server` 组装完成后注入。
- 被委托 Backend 返回的 UpstreamEvent 必须被分发型插件包装为自身稳定 ID；route/endpoint 只进入结构化日志或非敏感 metadata。
- 目标 ID 未注册时，请求失败并允许平台正常换源，不得伪造上游成功。

## Registry Validation

`Registry.ValidateSource(src)` 按序检查：

1. `src.Backend` 非空且存在。
2. source name 非空；同一 Config 的唯一性仍由 config 平台校验。
3. schema 外 option key 拒绝。
4. 字段类型、枚举、必填和自定义约束有效。
5. sensitive clear/keep 哨兵语义合法。

未知 backend 错误格式：

```text
source "copilot": unknown backend "github-copilotx"; registered backends: anthropic, github-copilot, openai-chat, openai-responses
```

无效 option 错误格式：

```text
source "copilot": options.github_token: must not be empty
```
