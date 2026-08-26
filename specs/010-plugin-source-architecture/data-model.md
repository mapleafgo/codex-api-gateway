# Data Model: 插件式上游源架构

## Config v2

### Config

| 字段 | 类型 | 约束 |
|---|---|---|
| `server` / `logging` / `breaker` | 现有对象 | 保持现有语义 |
| `sources` | `[]Source` | 必须通过注入的 Registry 校验 |
| `models` | 现有模型覆盖表 | 不属于本次源身份改造 |

移除顶层 `anthropic` 配置。Anthropic 的默认输出上限与 cache 注入开关成为 `backend=anthropic` 源 options；插件 schema 提供缺省值。

### Source

```yaml
- name: copilot
  backend: github-copilot
  disabled: false
  base_url: ""                  # 是否可省略由插件 schema 声明
  api_key: ""
  headers: {}
  supports_web_search: true     # 跨源请求形状开关，保留在通用区
  default_model: gpt-5.3-codex
  model_map:
    gpt-5: gpt-5.3-codex
  breaker: {}
  options:
    github_token: ${COPILOT_GITHUB_TOKEN}
```

| 字段 | 类型 | 规则 |
|---|---|---|
| `name` | string | 非空且当前配置内唯一 |
| `backend` | string | 非空；必须命中已注册插件 ID |
| `base_url` / `api_key` / `headers` | 通用字段 | 是否必填由插件 schema 声明；header 名称校验保持现状 |
| `disabled` | bool | true 时保留配置但不参与调度 |
| `default_model` / `model_map` | string / ordered map | 平台级通用模型映射 |
| `breaker` | object | 继承全局并做非负校验 |
| `options` | map | 只能包含所选插件 schema 声明的键；类型、枚举、必填、敏感值由插件校验 |

禁止字段：

- `backend_type`：任何值都返回迁移错误。
- 通用区中的旧专属字段（如 `github_token`）：严格 YAML 解码按未知字段拒绝。
- 未注册 `backend` 或 schema 外 option key：加载或保存整体失败。

`${VAR}` 插值继续发生在配置解析后、Registry 校验前。敏感值只进入内存和磁盘，不进入日志、指标或管理响应明文。

## Plugin Contract

### Descriptor

```go
type ID string

type Descriptor struct {
    ID          ID
    Title       string
    Summary     string
    Capabilities []Capability
    Streaming   StreamingKind // converted | passthrough
    Schema      []Field
    Actions     []Action
}
```

`Capabilities` 使用协议/能力抽象，不使用源专属 ID。共享核心可理解的初始能力包括：Responses 透传、Chat Completions 映射、Anthropic Messages 映射。Server 用这些能力聚合混合源的丢弃警告，不判断具体源。

### Field

| 属性 | 说明 |
|---|---|
| `Name` | options 或通用扩展字段的稳定键 |
| `Label` / `Description` | 管理页展示文本 |
| `Type` | text / password / boolean / integer / select / string-map 等 |
| `Required` | 保存前必须有效 |
| `Default` | 缺省值；不写回用户未修改的默认噪音 |
| `Sensitive` | GET 脱敏，POST 应用保留/清空哨兵 |
| `Options` | select 的合法值 |
| `AppliesTo` | 标记作用于 `base_url`、`api_key` 或 `options` |

Schema 是声明式数据，不是前端代码。共享 admin 不理解某个具体字段名。

### Registry

```go
type Registry struct { /* immutable */ }

func New(plugins ...SourcePlugin) (*Registry, error)
func (r *Registry) Get(id plugin.ID) (SourcePlugin, bool)
func (r *Registry) Descriptors() []Descriptor
func (r *Registry) ValidateSource(src config.Source) error
```

约束：

- 构造时检测空 ID、重复 ID、nil Backend、重复 action ID。
- 构造完成后描述符和映射不可变，读取无需锁。
- Registry 实现 `config.SourceValidator` 窄接口，由组装入口传给所有加载/保存路径。

## Runtime State

### Scheduler Source Runtime

调度状态仍以 `source.name` 为 key：breaker、degraded/circuitOpen/halfOpen、运行时优先级和恢复计数不变。Reload 成功后重建顺序；失败时不替换 holder 快照。

执行时流程：

1. 从当前配置取 `Source`。
2. 用 `Backend` ID 在 Registry 查询插件。
3. 调用 `Backend.Execute`。
4. 按 `Descriptor.Streaming` 决定是否使用 EventGate。
5. 上游事件携带插件声明的稳定 `Backend` 身份。

### Copilot Per-Source State

状态迁移到 Copilot 插件内部：

| 状态 | 说明 |
|---|---|
| endpoint override / discovered endpoint | 显式 `base_url` 优先；发现失败缓存默认 endpoint 并记录诊断 |
| GitHub token fingerprint | token 变化时重建 endpoint 与目录缓存 |
| model catalog + cachedAt | 5 分钟 TTL，singleflight 合并并发刷新 |
| route decision | 目录可用时按 `/responses > /v1/messages > /chat/completions`；目录不可用回退 Responses |

Copilot 通过宿主拿被委托 Backend，委托事件包装为 `backend=github-copilot` 后再上报。

### Device Flow Session

会话归 Copilot 插件管理，进程内最多一个活跃会话。

| 状态 | 含义 |
|---|---|
| idle | 无活跃或保留会话 |
| starting | 正在获取 device code |
| awaiting_user | 已获得公开 user code 和 verification URI |
| saving | 正在原子写盘并热重载，不可取消 |
| authorized / cancelled / error | 终态，保留到下一次 start |

device code、access token、Authorization 不进入公开状态、响应、日志、指标或 SSE 事件。成功保存走 admin 注入的唯一配置写盘回调。

## Model Catalog And Probe Results

统一模型项：

```go
type Model struct {
    ID          string
    DisplayName string
    Metadata    map[string]any
}
```

`Metadata` 可携带插件私有信息，但不得携带凭据。不支持目录能力的插件返回 `ErrCapabilityNotSupported`。

健康探测结果复用现有 operational/degraded/failed 分类，并保留 HTTP status、耗时、消息和时间。探测目标、认证方式和降级解释完全由插件决定。
