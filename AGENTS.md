# Repository Guidelines

## Project Structure & Module Organization

本仓库是 Go 服务 `codex-api-gateway`，用于把 OpenAI Responses API 请求转换并转发到 Anthropic 兼容后端。

- `cmd/server/`：服务入口、HTTP 路由和启动逻辑。
- `internal/convert/`：Responses 请求到 Anthropic 请求的转换。
- `internal/streamconv/`：Anthropic SSE 到 Responses SSE 的流式转换。
- `internal/server/`、`internal/scheduler/`、`internal/breaker/`、`internal/store/`：服务编排、源选择、熔断与会话状态。
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

## Testing Guidelines

使用 Go 标准测试框架。单元测试靠近实现文件，集成测试可参考 `internal/server/integration_test.go`。新增转换、调度、熔断、配置解析或并发行为时，应补充表驱动测试；涉及共享状态或 goroutine 的改动应运行 `task test-race`。

## Commit & Pull Request Guidelines

历史提交采用 Conventional Commits 风格，例如 `fix(store): ...`、`feat(scheduler): ...`、`test: ...`。提交信息应说明变更范围和行为影响，必要时可在冒号后使用中文描述。

PR 应包含：变更摘要、测试结果（至少 `task check`）、相关 issue 或设计文档链接。涉及 API、配置或流式协议行为变化时，同步更新 `README.md`、`docs/` 或 `config.example.yaml`。

## Security & Configuration Tips

不要提交真实 API key、上游地址凭据或本地专用配置。新增配置项时同时更新 `config.example.yaml`，并在 `internal/config` 中添加校验和测试。
