# Repository Guidelines

## Project Structure & Module Organization

本仓库是 Go 服务 `codex-api-gateway`，用于把 OpenAI Responses API 请求转换并转发到 Anthropic 兼容后端。

- `cmd/server/`：服务入口、HTTP 路由和启动逻辑。
- `internal/convert/`：Responses 请求到 Anthropic 请求的转换。
- `internal/streamconv/`：Anthropic SSE 到 Responses SSE 的流式转换。
- `internal/server/`、`internal/scheduler/`、`internal/breaker/`：服务编排、源选择与熔断。
- `internal/model/`：协议模型与事件类型。
- `internal/config/`：YAML 配置加载与校验。
- `docs/`：协议说明、设计文档和实施计划。
- `config.example.yaml`：可提交的配置模板；`config.yaml` 是本地运行配置，避免写入真实密钥。

测试文件与被测包放在同一目录，命名为 `*_test.go`。

## 技术架构

本网关是一个分层、单向依赖的 Go 服务，依赖方向严格自上而下，禁止反向引用（例如 `convert` 不得 import `server`）。

分层与职责（从下到上）：

- L0 基础层：`internal/config`（YAML 加载与校验）、`internal/logging`（结构化日志 handler）、`internal/model`（OpenAI Responses 协议 wire 类型，常量对齐官方 SDK）、`internal/breaker`（单源健康状态与熔断）、`internal/toolcatalog`（OpenAI Tool → Anthropic tool 声明映射）。这一层不依赖本仓其它 internal 包。
- L1 客户端层：`internal/anthropic`，Anthropic 兼容后端的低层 HTTP 客户端。
- L2 转换层：`internal/convert`（Responses 请求 → Anthropic 请求）、`internal/streamconv`（Anthropic SSE → Responses SSE）。依赖 `model`、`anthropic`、`toolcatalog`。
- L3 运行时层：`internal/scheduler`（多源路由 + 优先级重建），依赖 `breaker`、`anthropic`、`config`。
- L4 编排层：`internal/server`，唯一的 `/v1/responses` 与 `/v1/models` 入口，串联 holder → scheduler → convert → anthropic → streamconv。是唯一允许跨层组装的包。
- L5 管理/观测层（旁路）：`internal/admin`（H5 管理页 + `/admin/api/*` JSON 接口）、`internal/metrics`（指标聚合）。这一层**绝对不得**进入 `/v1/*` 转发路径。
- L6 配置热重载：`internal/configwatch`，监听 `config.yaml` 变化并通过 Holder 注入新配置。
- `cmd/server`：唯一组装入口，负责两阶段初始化、挂载管理页、启动 HTTP server。

两条贯穿全局的关键路径：

- **配置生效路径（单一真相源）**：磁盘 `config.yaml` → `config.Load` → `holder.Replace` → `scheduler.Reload`。管理页保存与外部编辑（vim 等）都走写盘 → fsnotify → 这条链路，不允许存在第二条直接改运行时配置的入口。
- **请求转发路径**：`/v1/responses` → `server.handleResponses` → `convert` → `scheduler.Pick` → `anthropic` 上游 → `streamconv` 回程 SSE。这条路径上的任何失败都必须以 error / SSE 错误事件形式返回客户端，不得 panic 逃逸。

## 协议转换职责边界（架构基础）

本网关的定位是 **OpenAI Responses ↔ 上游协议** 的纯协议转换与转发层，不是上游能力裁判，也不是工具运行时。下列边界适用于 **全部后端**（Anthropic `backend_type: a`、Chat Completions `backend_type: c`，以及未来新增 Backend），是架构级硬约束，与下文「设计规范」同等优先级。

### 核心原则：形状透传，结果归上游

- **网关只做 wire 对齐**：把客户端 Responses 请求转成上游可接受的请求形状，把上游流式/事件转回 Responses SSE。不替上游决定「这个模型/源能不能搜、能不能跑 code、能不能连 MCP」。
- **能力与错误交给上游**：上游接受、拒绝、空结果、部分字段缺失，均由上游自行表现（HTTP 4xx/5xx、SSE error、空 content、无 tool result 等）。网关按协议映射这些结果，**不代劳拒绝**。
- **禁止替上游做产品裁决**：不得因「我们推断某兼容后端可能没有 hosted/MCP/真搜索」而主动 fail-fast、改写为 failed、或编造「能力不足」终态。是否支持由上游运行时决定。
- **唯一可拒的是协议不可映射**：仅当客户端字段/item **无法安全翻译成目标协议**（无 wire 槽位、SDK 无变体、继续转发必然破坏协议）时，才允许明确转换错误、WARN + 丢弃、或矩阵登记的降级。这是翻译边界，不是替上游判能力。

### 推论（落地时对照）

| 场景 | 正确做法 | 错误做法 |
|---|---|---|
| Chat 路径把 `web_search` 变成 function 发给上游 | 形状透传，上游 400/忽略/执行均随上游 | 网关因「Chat 无真 hosted」主动 400 或出站改 failed |
| 上游 tool 调用后无 sources/logs | 按上游返回映射（空则空） | 网关编造失败文案或强制 incomplete/failed |
| a 路径 Anthropic 无等价字段 | 矩阵登记：映射 / WARN 忽略 / fail-fast（**仅**协议原因） | 以「某厂商可能不支持」为由全局禁止字段 |
| 熔断 / 选源 / 5xx 重试 | 传输与可用性（scheduler/breaker） | 与「协议能力裁判」混为一谈 |

文档（`docs/protocol-coverage.md`）中的 `lossy_supported` 表示 **协议有损或语义不保证**，**不**表示网关会替上游拦截请求。新增转换逻辑时必须先问：这是「无法映射」还是「替上游判能力」——后者一律不做。

## 设计规范

下列约束是本仓所有改动的硬性要求，违反其中任何一条都需要在 PR 中显式说明并给出权衡。

- **Holder 原子配置模式**：运行时配置通过 `*config.Holder` 暴露，`holder.Current()` 返回不可变快照，读者从不持锁。任何需要读取配置的代码都必须走 `holder.Current()`，禁止把 `*config.Config` 缓存到长生命周期对象里。热重载以整体替换实现，**不做字段级 in-place 修改**。
- **性能隔离**：`/v1/*` 转发路径是延迟敏感热路径。`metrics` 必须用 `select + default` 非阻塞投递到带缓冲 channel，channel 满直接丢弃事件；`admin` handler 必须被 `recoverMiddleware` 包裹，单次 panic 不得影响其他请求、更不得影响转发路径。新增旁路逻辑（观测、调试、profiling）同样不得阻塞或拖慢转发。
- **策略注册表优于特例 handler**：协议转换中类似的变体（如 `streamconv` 的各类 tool call）必须抽象为策略 + 注册表（`dispatchCallKind`），通过差异轴（itemType / 承载字段 / delta 模式 / result 处理）表达，而不是为每个变体写独立 handler。新增一种变体只改注册表，不再扩 handler 列表。
- **协议常量对齐官方 SDK**：wire 层的协议字面量（事件类型、content block 类型、finish reason 等）必须从 `github.com/openai/openai-go/v3` 和 `github.com/anthropics/anthropic-sdk-go` 的常量包派生（`constant.ValueOf[...]`），禁止硬编码字符串。新增协议字段时先查 SDK 是否已暴露常量。
- **分层编辑约束**：新增字段或逻辑必须落到职责对应的 `internal/*` 包，禁止在 `server` 里写协议转换、在 `convert` 里做路由决策、在 `admin` 里直接改运行时状态。跨层共享的类型放 `internal/model`。
- **可观测性优先结构化日志**：延续上文「日志规范」，业务/诊断日志一律走 `slog` 的结构化键值；error 正常返回而不是打印；静默跳过的分级判断见专节。
- **测试靠近实现**：被测包同目录 `*_test.go`，跨包行为用表驱动测试；涉及共享状态或 goroutine 的改动必须通过 `task test-race`。
- **配置项闭环**：新增配置项必须同时更新 `config.example.yaml`、`internal/config` 的校验与测试，并评估是否需要触发 `scheduler.Reload`。`config.yaml` 是本地运行配置，不得提交真实凭据。

## Build, Test, and Development Commands

优先使用 `Taskfile.yml` 中的任务：

- `task build`：构建服务二进制 `codex-api-gateway`。
- `task run`：使用 `config.yaml` 本地运行服务。
- `task test`：运行全部 Go 测试。
- `task test-race`：使用 race detector 检查并发问题。
- `task cover`：生成 `coverage.out` 并打印总覆盖率。
- `task check`：依次执行格式检查、`go vet` 和测试，作为提交前门禁。

没有安装 Task 时，可直接运行等价命令，例如 `go test ./...` 或 `go run ./cmd/server -config config.yaml`。

## Coding Style & Naming Conventions

代码使用标准 Go 风格，提交前运行 `task fmt` 或 `gofmt -w internal/ cmd/`。包名保持短小、全小写，避免下划线。导出标识符使用 `PascalCase`，非导出标识符使用 `camelCase`。新增逻辑应放入职责对应的 `internal/*` 包，避免把协议转换、调度和 HTTP 处理混在同一层。

Lint 配置在 `.golangci.yml`，启用了 `errcheck`、`govet`、`staticcheck`、`unused`、`revive`、`misspell` 等检查。

## 日志规范

任何日志输出必须使用 `log/slog`，不允许使用 `fmt.Print*`、`log.Print*`、`os.Stderr.Write` 等其他方式打印信息。这是为了让进程内所有日志统一走 `internal/logging` 配置的 handler（level / format / file）。

- 业务/诊断日志：用 `slog.Info` / `slog.Warn` / `slog.Error` / `slog.Debug`，关键上下文以结构化键值传入，不要用 `fmt.Sprintf` 拼接消息。
- 错误返回：用 `fmt.Errorf` 构造 error 正常返回，不要把 error 文本当日志打印。
- 唯一例外是 `internal/logging` 中的 `log.SetOutput(io.Discard)`，它是用来压制标准库 `log`（防止第三方依赖污染 stdout），不是日志输出。

## Testing Guidelines

使用 Go 标准测试框架。单元测试靠近实现文件，集成测试可参考 `internal/server/integration_test.go`。新增转换、调度、熔断、配置解析或并发行为时，应补充表驱动测试；涉及共享状态或 goroutine 的改动应运行 `task test-race`。

## Commit & Pull Request Guidelines

历史提交采用 Conventional Commits 风格，例如 `fix(store): ...`、`feat(scheduler): ...`、`test: ...`。提交信息应说明变更范围和行为影响，必要时可在冒号后使用中文描述。

PR 应包含：变更摘要、测试结果（至少 `task check`）、相关 issue 或设计文档链接。涉及 API、配置或流式协议行为变化时，同步更新 `README.md`、`docs/` 或 `config.example.yaml`。

## Security & Configuration Tips

不要提交真实 API key、上游地址凭据或本地专用配置。新增配置项时同时更新 `config.example.yaml`，并在 `internal/config` 中添加校验和测试。

## 静默跳过与降级处理约定

静默跳过或忽略上游/请求数据（例如流式转换中遇到无 Responses 等价物的 Anthropic content block、忽略无法映射的字段或事件、边界输入的降级处理）在**可控范围内是允许的**，默认不强制 WARN。常规跳过可直接静默，或用 `slog.Debug` 记录以保留可观测性。

仅当丢弃**重要且不可预期**的数据（例如完整的工具调用结果、用户可见的输出内容、会导致功能缺失的关键字段、上游协议外的异常分支）时，才输出 WARN 级别结构化日志，至少包含：被丢弃内容的类型/标识、关联的 response_id 或上下文 id、影响说明（如"对应数据被丢弃"）。

判断标准：
- **可控范围内**（已知协议限制、明确的降级路径、边界情况、协议字段无等价物）：允许静默，可选 DEBUG 日志。
- **不可控 / 重要数据丢失**：必须 WARN。

禁止使用 `fmt.Println` 处理需要观测的跳过（日志规范见上）。新增静默跳过分支时，同步补充或更新对应的测试，验证跳过路径触发且不产生异常事件。
