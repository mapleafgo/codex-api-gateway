# 运行时与可观测性重构设计

日期：2026-07-24

## 背景

全仓审查确认当前分层方向、协议转换和测试基础良好，但运行时边界存在以下问题：

- Responses 透传收到 `response.failed` 并正常结束时，观测状态会误记为
  `completed`。
- Chat 历史中的非空 `function.arguments` 会被 JSON 合法性判断改变语义。
- metrics Collector 的 `Record` 与 `Stop` 存在关闭竞态，consumer panic 会被静默
  吞掉并永久停止。
- 配置热重载回调 panic 被静默吞掉，最终仍记录“配置热重载完成”。
- 请求关联 ID 在上游执行结束后才生成，无法串联 server、scheduler 和 Backend
  日志。
- daemon 仍直接使用 `fmt.Print*` / stderr，另有 lint、格式和失真注释问题。

本次允许直接重构，但必须保持协议差异清晰，禁止为了形式统一引入难读的通用执行
框架。

## 目标

1. 三种 Backend 的终态与观测状态准确一致。
2. 请求日志可通过一个 `request_id` 从入口串联到每次上游尝试和出口。
3. Collector 在并发停止、事件处理 panic 和高负载丢弃场景下行为明确。
4. 配置热重载的每个应用阶段均可观察，部分失败不得伪装成全量成功。
5. Chat 参数继续遵守“形状透传，结果归上游”。
6. 全仓 lint、gofmt、test、race、vet 和 build 门禁通过。

## 非目标

- 不建立跨协议的通用流式状态机。
- 不改变 scheduler 的选源、重试、熔断和固定 backoff 策略。
- 不改变管理 API 或配置 YAML 的外部结构。
- 不增加第三方依赖。
- 不把指标持久化。
- 不记录完整敏感请求体、API key 或上游凭据。

## 设计原则

### 保留协议差异

Anthropic、Chat 和 Responses 的终态来源不同：

- Anthropic 由 `message_stop`、converter 终态和流错误共同决定。
- Chat 由 `finish_reason`、usage 尾包、`[DONE]` 和 converter 终态共同决定。
- Responses 本身已有明确的 `response.completed`、`response.incomplete`、
  `response.failed` 事件。

因此不抽象统一状态机。每个 Backend 保留直接、线性的控制流，只共享请求日志上下文
和已有 `UpstreamEvent` 结果结构。

### 小 API 优于层层参数

请求关联信息写入现有 `context.Context`，不在 server → scheduler → Backend 的每层
签名增加 `requestID string`。

## 请求日志上下文

在 L0 `internal/logging` 增加：

```go
func NewRequestID() string
func WithRequestID(ctx context.Context, id string) context.Context
func RequestID(ctx context.Context) string
func FromContext(ctx context.Context) *slog.Logger
```

行为：

- `NewRequestID` 使用标准库 `crypto/rand` 生成不可预测的短 ID；随机源失败时使用
  原子递增值和时间戳兜底。
- `WithRequestID` 不接受空 context；空 ID 时原样返回。
- `FromContext` 在存在 ID 时返回
  `slog.Default().With("request_id", id)`，否则返回 `slog.Default()`。

`handleResponses` 在读取 body 前创建 ID，并把新 context 写回请求：

```go
requestID := logging.NewRequestID()
r = r.WithContext(logging.WithRequestID(r.Context(), requestID))
log := logging.FromContext(r.Context())
```

server、scheduler 和三个 Backend 内与本请求相关的日志均从 context 获取 logger。
上游尝试日志同时带：

```text
request_id, source, backend_type, attempt
```

耗时敏感节点记录：

```text
elapsed, ttfb
```

`response_id` 仍表示 Responses 协议响应标识，不再承担请求日志关联职责。

## Responses 终态

`ResponsesBackend.Execute` 使用一个局部字符串记录上游终态：

```text
""                    尚未收到终态
"completed"           response.completed
"incomplete"          response.incomplete
"failed"              response.failed
```

处理规则：

- clean EOF + completed/incomplete：观测状态保持对应值。
- clean EOF + failed：观测状态 `failed`，code 保持 200，错误摘要从
  `response.error.message` 尽力提取。
- 客户端在 completed/incomplete 之后取消：保持已收到的业务终态。
- 客户端在 failed 之后取消：保持 failed。
- 无终态但 clean EOF：保持现有透传行为，不合成事件；观测层按已锁定流的当前结果
  处理，不替上游编造能力错误。

解析终态与 usage 继续是只读观测，不修改透传给客户端的 SSE 数据。

## Chat function arguments

删除 `chatFunctionArguments` 的 JSON 合法性裁决。

映射规则：

```text
空字符串 → "{}"
非空字符串 → 原样
```

不得 trim 后改变非空字符串内容，不得把非法或截断 JSON 包装为
`{"raw":"..."}`。是否接受该字符串由上游决定。

freeform、自定义工具和 tool search 已有独立 wire 形状，不与普通 function 参数处理
合并。

## Collector 生命周期

Collector 增加：

```go
acceptMu sync.RWMutex
closed   bool
wg       sync.WaitGroup
```

生命周期：

1. `New` 在启动 consumer 前 `wg.Add(1)`。
2. `Record` 持有 `acceptMu.RLock`：
   - closed 时直接返回；
   - 未关闭时使用现有 `select + default` 投递；
   - channel 满时递增 `DroppedEvents`。
3. `Stop` 持有 `acceptMu.Lock`：
   - 首次调用设置 closed 并关闭 events；
   - 后续调用直接等待或返回；
   - 解锁后 `wg.Wait()`。
4. 只有 `run` 消费 channel；`Stop` 不再成为第二 consumer。
5. `run` 使用 `for range events` drain 所有已接收事件，结束时 `wg.Done()`。

`RWMutex` 只保护极短的非阻塞投递窗口。相比无锁检查后关闭 channel，它以很小的
同步成本换取清晰、可证明的关闭协议。

### 单事件 panic 隔离

consumer 不在 goroutine 顶层静默 recover，而是每个事件调用：

```go
func (c *Collector) consume(ev RequestEvent)
```

`consume` recover 后记录 `slog.Error`，包含：

```text
recover, kind, source, model, backend_type
```

随后继续消费下一事件。正常事件不增加日志，保持热路径安静。

## 配置热重载

保留单一生效路径：

```text
config.Load → holder.Replace → scheduler.Reload → logging.Configure
```

新增一个局部 callback 执行辅助函数，把 panic 转为 `error`，但不建立回调框架。

`Watcher.reload` 分阶段记录：

- 配置加载失败：保留旧配置，`LastLoadErr` 记录错误。
- holder 替换成功：进入运行时应用阶段。
- scheduler 回调失败：记录 Error。
- logging 回调失败：记录 Error；旧日志 handler 由 logging 包自身保证继续可用。
- 全部成功：Info“配置热重载完成”。
- 任一回调失败：Warn“配置已加载但运行时应用不完整”，并让
  `LastLoadErr` 返回汇总错误。

`Server.ReloadScheduler` 改为返回 `error`，将 panic 转成带上下文的 error。
Watcher callback 类型相应改为：

```go
type ReloadCallback func() error
type LoggingCallback func(config.LoggingCfg) error
```

调用方直接返回真实错误，避免回调内部静默 recover。

## daemon 输出

`maybeDaemonize` 发生在正式日志配置前，但仍使用 `slog.Default()`：

- 启动错误：`slog.Error`
- go run 降级：`slog.Warn`
- 后台启动成功：`slog.Info`

消息携带 `pid`、`log_path`、`error` 等结构化字段。删除所有
`fmt.Print*` / `os.Stderr` 输出；`fmt.Errorf` 继续用于构造 error。

## 注释与格式

同步修复：

- `Record` 注释说明 channel 满时计数，不再称“静默”。
- `Server.Close` 注释说明会停止 metrics 并等待 drain。
- 删除悬空的 `summarizeAnthropicRequest` 注释。
- Responses SSE 使用无条件 `strings.TrimPrefix`，消除 staticcheck S1017。
- 对 gofmt 报告的三个文件执行 gofmt，不夹带行为改动。

## 日志数据流

### `handleResponses`

- 入口：request_id、method、path、query、body 字节数和截断 body snapshot。
- 解析后：model、input 类型计数、tools 摘要。
- 上游前后：由 scheduler/Backend 使用同一个 request_id。
- 出口：status、source、backend、usage、elapsed。

body snapshot 使用现有日志截断原则，不记录 Authorization/API key。JSON 中若存在
未来敏感字段，不做深层通用脱敏器；只记录限制长度的协议请求体并在文档中明确风险。
为避免直接泄露 prompt，默认 Info 只记录结构摘要，完整截断 body 仅 Debug。

### scheduler / Backend

- scheduler：attempt 开始、锁定、失败、状态迁移。
- Backend：转换结果、建连失败、TTFB、流结束状态。
- 所有节点都携带 request_id、source、backend_type、attempt。

## 测试策略

### logging

- ID 非空且格式稳定。
- `WithRequestID` / `RequestID` 往返。
- `FromContext` 输出记录包含 request_id。

### Responses Backend

- `response.failed + clean EOF` 上报 failed。
- completed/incomplete 状态分别正确。
- failed 后客户端取消仍保持 failed。
- usage 仍被采集。

### Chat convert

- 空 arguments 返回 `{}`。
- 合法 JSON 原样。
- 非 JSON、截断 JSON和带首尾空格字符串全部原样。

### metrics

- 高并发 `Record` 与 `Stop` 不 panic，通过 race。
- Stop drain 已接受事件并等待 consumer。
- Stop 幂等。
- 单事件 panic 后 consumer 继续处理后续事件。

单事件 panic 测试直接构造 `groups == nil` 的包内 Collector，让首个聚合事件在写 map
时触发真实 panic；随后初始化 map 并消费第二个事件，验证 consumer 仍可继续。生产
结构不为测试增加 seam。

### configwatch

- scheduler callback panic/error 会写入 `LastLoadErr`。
- logging callback panic/error 会写入 `LastLoadErr`。
- callback 失败时 holder 仍是磁盘最新配置。
- 全部成功时错误清空。

### 日志关联

使用测试 slog handler 捕获一次请求的入口、scheduler、Backend 和出口日志，断言相同
`request_id`，并断言上游日志带 attempt。

### 门禁

```bash
gofmt -w <涉及文件>
golangci-lint run ./...
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
task build
task check
git diff --check
```

## 兼容性

- HTTP、SSE、管理 API 和 YAML 配置不变。
- 日志新增 `request_id`、`attempt` 等字段，旧日志消费者应忽略未知字段。
- daemon 文本格式会变为 slog handler 的格式，这是为统一日志规范做出的有意变化。
- Chat 非 JSON arguments 从 `{"raw":...}` 恢复为原始字符串，是协议语义修复。
- `ReloadCallback` / `LoggingCallback` 是 internal API，只影响仓内组装和测试。
