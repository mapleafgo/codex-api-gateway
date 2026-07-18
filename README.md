# CodexApiGateway

CodexApiGateway 是一个本地 OpenAI Responses API 兼容网关，用于让 Codex CLI 通过 OpenAI 风格接口调用 Anthropic Messages 兼容后端。

它接收 `POST /v1/responses` 请求，转换为 Anthropic Messages 请求，转发到配置的上游源，再把 Anthropic SSE 流转换回 OpenAI Responses SSE 流返回给 Codex。

## 功能

- OpenAI Responses API 到 Anthropic Messages API 的协议转换。
- 支持多个 Anthropic 兼容后端源，按配置顺序作为优先级。
- 首字节前故障转移：上游未开始流式输出前可切换到下一个源。
- 断路器保护：失败降级、熔断、冷却、半开探测和恢复。
- `previous_response_id` 会话缓存，用于无状态后端的多轮对话和工具调用衔接。
- 尊重请求里的 `store: false`，此时不写入本地会话缓存，也不回填本地 `previous_response_id` 历史。
- `/v1/models` 只返回配置文件 `models` 段显式声明的模型。
- 结构化日志，支持等级过滤和 text/json 输出。
- 配置文件支持 `${ENV}` 展开，也支持 `CODEX_API_GATEWAY_` 环境变量覆盖。

## 快速开始

复制示例配置：

```bash
cp config.example.yaml config.yaml
```

设置上游密钥。可以使用 YAML 中的 `${ENV}`：

```bash
export ANTHROPIC_KEY=sk-ant-...
export ZHIPU_KEY=...
```

也可以使用 `koanf` 环境变量覆盖配置：

```bash
export CODEX_API_GATEWAY_SOURCES__0__API_KEY=sk-ant-...
```

启动服务：

```bash
go run ./cmd/server -config config.yaml
```

服务默认监听 `:8080`。Codex 的 base URL 指向：

```text
http://127.0.0.1:8080/v1
```

`base_url` 写到网关根（含 `/v1`）即可。Codex 会自动在 `base_url` 后拼接 `/responses` 发起对话、拼接 `/models` 拉取模型列表。**不要把 `base_url` 写成 `http://<host>:8080/v1/responses`**：那样 `/models` 请求会打到 `/v1/responses/models`，返回 404，导致 Codex 的 `/models` 命令拉不到模型。

## 配置

完整示例见 [config.example.yaml](config.example.yaml)。

### 服务监听

```yaml
server:
  listen: ":8080"
```

### 日志

```yaml
logging:
  level: info   # debug | info | warn | error
  format: text  # text | json
```

日志等级分流：

- `debug`：输入项、会话回填、上游流事件、SSE 转换、上游错误详情。
- `info`：启动、请求进入、转换完成、上游流锁定、请求完成。
- `warn`：请求解析失败、上游失败、断路器跳过、重试。
- `error`：服务端写出失败、请求最终失败、HTTP 服务退出。

### 环境变量

配置支持两种环境变量方式。

第一种是 YAML 内联展开：

```yaml
sources:
  - name: anthropic-official
    api_key: ${ANTHROPIC_KEY}
```

第二种是 `CODEX_API_GATEWAY_` 前缀覆盖。层级分隔符使用双下划线 `__`，字段名里的单下划线保留：

```bash
export CODEX_API_GATEWAY_LOGGING__LEVEL=debug
export CODEX_API_GATEWAY_SOURCES__0__API_KEY=sk-ant-...
export CODEX_API_GATEWAY_BREAKER__MAX_RETRIES=2
```

加载顺序是：先展开 `${ENV}` 并加载 YAML，再应用 `CODEX_API_GATEWAY_` 覆盖。因此覆盖变量优先级更高。

### 会话缓存

```yaml
session:
  path: data/session
  ttl: 1h
  max_bytes: 67108864
  max_entry_bytes: 2097152
```

说明：

- `path`：Badger 存储目录，默认 `data/session`。服务重启后仍可通过未过期的 `previous_response_id` 回填上下文。
- `ttl`：单条响应上下文的 Badger entry TTL，默认 1h。过期后不可读取，并由 Badger 后续 GC 清理。
- `max_bytes`：整个 `SessionStore` 的字节预算，默认 64 MiB。超过预算时按内存 LRU 索引淘汰最久未使用的记录，并同步删除 Badger key。
- `max_entry_bytes`：单条响应上下文的最大可缓存字节数，默认 2 MiB。超过后直接跳过保存，避免大响应长期占用上下文缓存。
- `max_entries` 已移除，不再按条目数限制。

### 后端源

```yaml
sources:
  - name: anthropic-official
    base_url: https://api.anthropic.com
    api_key: ${ANTHROPIC_KEY}
    model_map: { gpt-5: claude-sonnet-4-20250514 }
    default_model: claude-sonnet-4-20250514
```

说明：

- `sources` 列表顺序就是优先级，第一个源优先尝试。
- `base_url` 写到上游根地址，不需要包含 `/v1/messages`。
- `model_map` 把 Codex/OpenAI 侧模型名映射到上游真实模型名。
- `default_model` 用于请求模型未命中 `model_map` 时兜底。

不要把 Codex 的模型名直接设置成上游专有模型名，例如 `glm-5.2`。Codex 可能没有这些模型的本地元数据。建议让 Codex 使用 `gpt-5`、`gpt-5.5` 等别名，再通过 `model_map` 映射到上游模型。

### 断路器

```yaml
breaker:
  first_byte_timeout: 12s
  degrade_threshold: 3
  recover_threshold: 1
  cooldown: 30s
  half_open_probes: 1
  recovery: normal
  max_retries: 0
```

状态流转：

```text
normal -> degraded -> circuitOpen -> halfOpen -> normal/degraded
```

- `first_byte_timeout`：等待上游首个流式事件的最长时间，超时计为失败。
- `degrade_threshold`：连续失败达到阈值后降级，再达到阈值后熔断。
- `recover_threshold`：降级状态恢复到正常所需的连续成功次数。
- `cooldown`：熔断后进入半开探测前的等待时间。
- `half_open_probes`：半开状态允许的探测请求数。
- `recovery`：半开探测成功后恢复到 `normal` 或 `degraded`。
- `max_retries`：所有源失败后的整轮重试次数；只支持全局配置。

单个源可以覆盖部分断路器参数：

```yaml
sources:
  - name: zhipu
    base_url: https://open.bigmodel.cn/api/anthropic
    breaker:
      first_byte_timeout: 8s
      cooldown: 10s
```

单源 `breaker` 中零值字段继承全局配置。`max_retries` 只取全局配置，不被单源覆盖。

## API

### `POST /v1/responses`

核心转发入口。请求体使用 OpenAI Responses API 格式，服务会转换为 Anthropic Messages 流式请求。

响应是 OpenAI Responses SSE 流。

### `GET /v1/models`

返回 Codex `ModelsResponse` 格式（`{ "models": [ModelInfo] }`），而非 OpenAI 的 `{ data: [] }`。Codex 用该格式直接解析 `ModelInfo` 能力字段（如 `supports_search_tool`）。

只返回配置文件 `models` 段显式声明的模型（`models.<slug>`），不拉取上游 `/v1/models`，也不暴露 `sources.model_map` 的别名。未列出的字段保持 `ModelInfo` 内置默认。

## 工作流程

```text
Codex
  -> POST /v1/responses
  -> 按 store 策略决定是否回填 previous_response_id 会话上下文
  -> Responses 请求转换为 Anthropic Messages 请求
  -> 按运行时优先级选择健康上游源
  -> 接收 Anthropic SSE
  -> 转换为 Responses SSE
  -> 保存本轮有效 input + output 供下一轮 previous_response_id 使用
  -> 返回给 Codex
```

故障转移只发生在上游首个事件到达之前。一旦某个源开始输出流，当前请求就锁定该源；后续中断会作为本次响应失败返回，不再切换源。

当请求未设置 `store` 或设置为 `true` 时，网关会把本轮有效 input 和本轮 output 一起保存到本地 `SessionStore`，供下一轮 `previous_response_id` 回填使用；这模拟 OpenAI Responses 的链式上下文语义。当请求设置 `store: false` 时，网关不会保存本轮上下文，也不会使用请求里的 `previous_response_id` 做本地历史回填；这种模式要求客户端在后续请求中自行携带完整历史，例如带有 `reasoning.encrypted_content` 的 stateless/ZDR transcript。

## 开发

优先使用 Taskfile：

```bash
task build       # 构建二进制
task run         # 使用 config.yaml 本地运行
task test        # 运行全部测试
task test-race   # race detector
task cover       # 覆盖率
task check       # gofmt 检查 + go vet + go test
```

没有安装 Task 时可直接使用 Go 命令：

```bash
go test ./...
go run ./cmd/server -config config.yaml
```

## 已知限制

- `input_image.file_id` 不支持。Anthropic 图片块只支持 base64 或 URL，网关无法在没有 OpenAI 凭据的情况下解析 OpenAI Files 的 `file_id`。
- **code interpreter**：`container`（file_ids / memory_limit / 显式 container）、代码生成文件（`file_id`→`url`）不可转换；`code_execution_tool_result_error` 无法转 completed。详见[协议覆盖矩阵](docs/protocol-coverage.md)。
- **MCP**：`mcp_list_tools` 工具列表、`require_approval` 审批流（≠never 时降级为 never + WARN）、自定义 `headers`（仅 `Authorization: Bearer` 提取到 `authorization_token`）、`connector_id`/`tunnel_id` 不可转换（fail-fast）；历史 MCP item 回灌暂不支持（丢弃 + WARN）；需后端支持 beta `mcp-client-2025-11-20`。详见[协议覆盖矩阵](docs/protocol-coverage.md)。
- `tool_choice: {type: "allowed_tools", tools: [...]}` 会按声明工具精确过滤，并仅将 `auto`/`required` 映射为 Anthropic `auto`/`any`；与 structured output 组合或包含不支持条目时会 fail-fast。详见[协议覆盖矩阵](docs/protocol-coverage.md)。
- 请求里没有 Anthropic 等价语义的 Responses 字段会被接受，但不保证映射到上游。

## 设计文档

- [初始设计](docs/superpowers/specs/2026-07-14-codex-api-gateway-design.md)
- [故障转移重构设计](docs/superpowers/specs/2026-07-15-failover-redesign-design.md)
- [SDK 迁移设计](docs/superpowers/specs/2026-07-15-sdk-migration-design.md)
