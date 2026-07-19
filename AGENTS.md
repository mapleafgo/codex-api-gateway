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
