# CodexApiGateway

让 Codex CLI 用上 Anthropic 兼容后端的本地网关。

![logo](docs/design/logo.svg)

Codex CLI 只能走 OpenAI Responses API。本网关在本地起一个 OpenAI Responses 兼容端点，把请求转换成 Anthropic Messages 协议转发到任意 Anthropic 兼容后端（官方 Anthropic、DeepSeek、火山、智谱等），再把上游 SSE 流转换回 Responses SSE 返回给 Codex。Codex 全程无感——它以为自己在跟 OpenAI 说话。

```text
Codex CLI
   │  POST /v1/responses  (OpenAI Responses 格式)
   ▼
codex-api-gateway  ── 协议转换 + 多源路由 + 熔断
   │  POST /v1/messages  (Anthropic Messages 格式, SSE)
   ▼
Anthropic 兼容后端 (官方 Anthropic、DeepSeek、火山 …)
```

## 功能

- **协议转换**：OpenAI Responses API ⇄ Anthropic Messages API，流式 SSE 双向转换。
- **多源路由**：多个 Anthropic 兼容后端，按配置顺序作为优先级，运行时重建。
- **首字节前故障转移**：上游未开始流式输出前可切换到下一个源；一旦出流即锁定该源。
- **断路器**：失败降级 → 熔断 → 冷却 → 半开探测 → 恢复，逐源可覆盖参数。
- **模型白名单**：`/v1/models` 只返回 `models` 段显式声明的模型，不暴露上游别名。
- **结构化日志**：等级过滤 + text/json 输出，全走 `slog`。
- **配置热重载**：管理页保存或编辑 `config.yaml` 后 `fsnotify` 自动生效，无需重启。
- **H5 管理页**：观测台、配置编辑、中英文/明暗主题，挂载在根路径 `/`。**所有配置都在网页里完成**，无需手动写 YAML。
- **系统托盘**：启动即常驻，点开即用；headless 环境自动降级为信号模式。
- **双击即用**：打包为单文件后双击运行即可，首次启动自动生成默认配置，无需命令行、无需提前准备配置文件。
- **环境变量**：YAML 内联 `${ENV}` 展开 + `CODEX_API_GATEWAY_` 前缀覆盖（可选，网页配置已足够）。

## 快速开始

**不需要命令行。** 构建（或下载）出二进制后，双击运行即可。

1. 把 `codex-api-gateway` 二进制放到任意目录，双击打开。
   - 首次运行会在同目录自动生成 `config.yaml`（最小默认配置，未含任何上游源）。
   - 进程启动后常驻在**系统托盘**，点托盘图标的「打开」菜单即可进入管理页。
2. 浏览器打开管理页 `http://localhost:8383/`（或托盘菜单「打开」）。
   - 在**配置管理**里添加上游源（粘贴 API Key、填 `base_url`、设 `model_map`），保存即热重载生效。
   - 未配置上游源前，转发请求会返回 503，配好即恢复。
3. 把 Codex 的 base URL 指向网关根（含 `/v1`）：

```text
http://127.0.0.1:8383/v1
```

Codex 会自动在 `base_url` 后拼接 `/responses` 和 `/models`。**不要把 `base_url` 写成 `…/v1/responses`**——那样 `/models` 会打到 `/v1/responses/models` 返回 404，Codex 拉不到模型列表。

退出时右键托盘图标选「退出」，或直接关闭托盘进程即可。

### 配置 Codex CLI 指向网关

网关启动并配好上游源后，让 Codex CLI 走网关而不是直连 OpenAI。Codex 支持三种等价方式，任选其一。

**方式一：改 `~/.codex/config.toml`（推荐，一劳永逸）**

在 `[model_providers]` 下加一个自定义 provider，`base_url` 指向网关根（含 `/v1`，不要带 `/responses` 或 `/models`），`wire_api` 必须设为 `responses`（网关是 OpenAI Responses 兼容端点）：

```toml
model_provider = "custom"

[model_providers.gateway]
name = "codex-api-gateway"
base_url = "http://127.0.0.1:8383/v1"
requires_openai_auth = true   # 网关不校验 key，但 Codex 需要此字段为 true 才会带 Authorization 头
wire_api = "responses"
```

`model` 填网关 `models` 段声明的别名（即 `model_map` 的键，例如 `gpt-5`），网关再映射成上游真实模型。

**方式二：命令行 `-c` 覆盖（临时、单次会话）**

不改配置文件，用 `-c` 传 TOML 键值（点路径表示嵌套），优先级高于 `config.toml`：

```bash
codex -c 'model_provider="custom"' \
      -c 'model_providers.gateway.name="codex-api-gateway"' \
      -c 'model_providers.gateway.base_url="http://127.0.0.1:8383/v1"' \
      -c 'model_providers.gateway.requires_openai_auth=true' \
      -c 'model_providers.gateway.wire_api="responses"' \
      -m gpt-5
```

**方式三：环境变量（仅覆盖单个 provider 字段，需配合 config.toml 的 provider 壳）**

Codex 没有统一的 `OPENAI_BASE_URL` 环境变量，base_url 只能写在 `[model_providers]` 段。若整段配置都由脚本生成，可在脚本里把 `base_url` 写成变量渲染进 TOML，再 `codex -c` 覆盖。

**验证**：启动 Codex 后随便发条消息，网关管理页「观测台」应出现一条请求；若 Codex 报 404 / unauthorized，99% 是 `base_url` 多了 `/responses` 或 `wire_api` 没设 `responses`。

### 从源码构建（仅开发者）

```bash
task build                 # 产出 ./codex-api-gateway
# 或
go build -o codex-api-gateway ./cmd/server
```

> 想要发布包（双击即用），用 `task build` 产出单文件二进制即可，无需任何运行前配置。
> 跨平台交叉编译示例：
> ```bash
> # macOS (Apple Silicon)
> GOOS=darwin GOARCH=arm64 go build -o codex-api-gateway-darwin-arm64 ./cmd/server
> # Windows
> GOOS=windows GOARCH=amd64 go build -o codex-api-gateway.exe ./cmd/server
> # Linux
> GOOS=linux GOARCH=amd64 go build -o codex-api-gateway-linux-amd64 ./cmd/server
> ```

## 管理页

管理页是**唯一的配置入口**。双击启动后，浏览器打开 `http://localhost:<listen>/`（默认 `http://localhost:8383/`，或点托盘「打开」菜单）即进入 H5 控制台。所有上游源、模型白名单、断路器参数、全局设置都能在网页里图形化增删改，保存即热重载，不用碰 YAML、不用命令行。

- **观测台**：6 张指标卡（请求量 / 输入 / 输出 / 缓存创建 / 缓存命中 / 命中率）+ `供应商 × 模型` 用量聚合表 + 最近 1000 条请求历史（按模型/供应商/Code 过滤，耗时渐变色条）。
- **实时推送**：指标经 SSE（`/admin/api/events`）每 3 秒推送，无需手动刷新。
- **配置管理**：图形化编辑供应商（含 `model_map` 列表式编辑、单源断路器、上移/下移排序）、模型白名单、全局参数、引导语文件。
- **个性化**：中英文切换、亮/暗主题，均记忆在 localStorage。
- **性能隔离**：指标采集走独立 goroutine + 带缓冲 channel，请求路径只做一次非阻塞投递，channel 满即丢弃，绝不拖慢转发；管理端异常不影响 `/v1/*`。

JSON 接口（前端调用，也可独立集成）：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/admin/api/metrics` | 当前指标快照 |
| GET | `/admin/api/config` | 读取配置视图 |
| POST | `/admin/api/config` | 全量覆盖写回并热重载 |
| GET | `/admin/api/guidance` | 读取引导语文件内容 |
| POST | `/admin/api/guidance` | 保存引导语文件 |
| POST | `/admin/api/config/reload` | 手动触发从磁盘 reload |
| GET | `/admin/api/events` | SSE 推送 metrics 快照（每 3s 一次） |

## 配置

完整示例见 [config.example.yaml](config.example.yaml)。

### 服务监听

```yaml
server:
  listen: ":8383"
```

### 日志

```yaml
logging:
  level: info   # debug | info | warn | error
  format: text  # text | json
```

| 等级 | 内容 |
| --- | --- |
| `debug` | 输入项、会话回填、上游流事件、SSE 转换、上游错误详情 |
| `info` | 启动、请求进入、转换完成、上游流锁定、请求完成 |
| `warn` | 请求解析失败、上游失败、断路器跳过、重试 |
| `error` | 服务端写出失败、请求最终失败、HTTP 服务退出 |

### 环境变量（可选）

普通场景直接在管理页填写 API Key 即可，无需环境变量。仅当你希望密钥不落盘、或用脚本批量覆盖配置时，才用以下两种方式（后者优先级更高；加载顺序：先展开 `${ENV}` 并加载 YAML，再应用 `CODEX_API_GATEWAY_` 覆盖）。

```yaml
# 方式一：YAML 内联展开
sources:
  - name: anthropic-official
    api_key: ${ANTHROPIC_KEY}
```

```bash
# 方式二：CODEX_API_GATEWAY_ 前缀覆盖（层级用双下划线 __，字段单下划线保留）
export CODEX_API_GATEWAY_LOGGING__LEVEL=debug
export CODEX_API_GATEWAY_SOURCES__0__API_KEY=sk-ant-...
export CODEX_API_GATEWAY_BREAKER__MAX_RETRIES=2
```

### 后端源

```yaml
sources:
  - name: anthropic-official
    base_url: https://api.anthropic.com
    api_key: ${ANTHROPIC_KEY}
    model_map: { gpt-5: claude-sonnet-4-20250514 }
    default_model: claude-sonnet-4-20250514
```

- `sources` 列表顺序即优先级，第一个源优先尝试。
- `base_url` 写上游根地址，不含 `/v1/messages`。
- `model_map` 把 Codex/OpenAI 侧别名映射到上游真实模型名。
- `default_model` 用于请求模型未命中 `model_map` 时兜底。

建议让 Codex 使用 `gpt-5`、`gpt-5.5` 这类别名，再经 `model_map` 映射到上游专有模型（如 `glm-5.2`）。直接把 Codex 模型名设成上游专有名，Codex 可能缺本地元数据。

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

状态流转：`normal → degraded → circuitOpen → halfOpen → normal/degraded`

| 参数 | 含义 |
| --- | --- |
| `first_byte_timeout` | 等待上游首个流式事件的最长时间，超时计为失败 |
| `degrade_threshold` | 连续失败达阈值后降级，再达阈值后熔断 |
| `recover_threshold` | 降级恢复到正常所需的连续成功次数 |
| `cooldown` | 熔断后进入半开探测前的等待时间 |
| `half_open_probes` | 半开状态允许的探测请求数 |
| `recovery` | 半开探测成功后恢复到 `normal` 或 `degraded` |
| `max_retries` | 所有源失败后的整轮重试次数（仅全局，单源不覆盖） |

单源可覆盖部分断路器参数，零值字段继承全局：

```yaml
sources:
  - name: zhipu
    base_url: https://open.bigmodel.cn/api/anthropic
    breaker:
      first_byte_timeout: 8s
      cooldown: 10s
```

## API

### `POST /v1/responses`

核心转发入口。请求体为 OpenAI Responses API 格式，服务转换为 Anthropic Messages 流式请求，响应为 OpenAI Responses SSE 流。

### `GET /v1/models`

返回 Codex `ModelsResponse` 格式（`{ "models": [ModelInfo] }`）而非 OpenAI 的 `{ data: [] }`，Codex 据此直接解析 `ModelInfo` 能力字段（如 `supports_search_tool`）。只返回 `models` 段显式声明的模型，不拉取上游 `/v1/models`，也不暴露 `model_map` 别名。

## 架构

分层、单向依赖的 Go 服务，禁止反向引用：

```text
L5 观测/管理  internal/admin  internal/metrics        （绝不进入 /v1/* 转发路径）
L4 编排      internal/server                        （唯一跨层组装入口）
L3 运行时    internal/scheduler                    （多源路由 + 优先级重建）
L2 转换      internal/convert  internal/streamconv （请求/流式协议转换）
L1 客户端    internal/anthropic                   （上游低层 HTTP 客户端）
L0 基础      internal/config internal/logging internal/model internal/breaker internal/toolcatalog
```

两条贯穿路径：

- **配置生效路径（单一真相源）**：磁盘 `config.yaml` → `config.Load` → `holder.Replace` → `scheduler.Reload`。管理页保存与外部编辑都走写盘 → fsnotify → 这条链路。
- **请求转发路径**：`/v1/responses` → `server` → `convert` → `scheduler.Pick` → `anthropic` 上游 → `streamconv` 回程 SSE。任何失败以 error / SSE 错误事件返回，不 panic 逃逸。

## 开发

普通用户无需此节——双击二进制即可运行。以下仅面向想从源码改代码的开发者。

优先使用 Taskfile：

```bash
task build       # 构建二进制 ./codex-api-gateway（双击即用）
task run         # 开发调试：用当前目录 config.yaml 跑起来
task test        # 运行全部测试
task test-race   # race detector
task cover       # 覆盖率
task check       # gofmt 检查 + go vet + go test
```

未安装 Task 时直接用 Go：

```bash
go test ./...
go build -o codex-api-gateway ./cmd/server   # 构建双击即用二进制
go run ./cmd/server                          # 开发调试：默认读 ./config.yaml
```

## 维护与排错

### 清理 Codex 模型缓存

Codex CLI 会把 `/v1/models` 的返回缓存在 `~/.codex/models_cache.json`。当网关 `models` 段或某个源的 `model_map` 发生变化（新增/改名模型别名）后，Codex 可能仍用旧缓存、拉不到新模型。此时删掉缓存文件，下次启动 Codex 会自动重新拉取：

```bash
rm -f ~/.codex/models_cache.json
```

### 常见症状

| 症状 | 可能原因 | 处理 |
| --- | --- | --- |
| Codex 报 404 | `base_url` 写成 `…/v1/responses` | 改成 `…/v1`，Codex 自己拼 `/responses` 和 `/models` |
| Codex 报 unauthorized | `requires_openai_auth` 没设 `true` 或 `wire_api` 没设 `responses` | 见「配置 Codex CLI 指向网关」 |
| 转发返回 503 | 网关未配置任何上游源 | 管理页「配置管理」加源并保存 |
| Codex 拉不到新模型 | `models_cache.json` 旧缓存 | 删 `~/.codex/models_cache.json` 重启 Codex |

## 产品边界

本网关是 **Codex CLI → Anthropic 兼容后端** 的协议适配器，不是 OpenAI 全量 Responses 平台：

- 客户端**自带完整 `input`** 回灌：不做 `previous_response_id` / `store` enrich，非空 `previous_response_id` WARN + 忽略。通用 AI SDK（`store:true` + `item_reference`）需自带完整 input，或改用带 session 库的网关。
- **Responses ↔ Anthropic Messages 直转**：不走 Chat Completions 中枢，保留 Codex 专有 item / reasoning / hosted tool 形态。

## 已知限制

- **多模态**：`input_image.file_id` 不支持（网关无 OpenAI 凭据拉取文件；仅接受 base64 / URL）。
- **web_search**：出站完整（事件链 + `url_citation`，流式与终态 item annotations 都写）；历史回灌为 `server_tool_use` + 空 `web_search_tool_result` + sources 可见文本——OpenAI wire 无 Anthropic required 的 `encrypted_content`，无法做官方级 result round-trip。
- **code interpreter**：`container`（file_ids / memory_limit / 显式 container）、image 输出、`code_execution_tool_result_error` 不可转换。
- **MCP**：
  - `mcp_call` 历史 → beta `mcp_tool_use` / `mcp_tool_result`（`param.Override` + `anthropic-beta: mcp-client-2025-11-20`）
  - `mcp_list_tools` 历史 → developer marker（lossy）
  - `mcp_approval_request` / `response` **不实现**：Anthropic 无审批协议，历史 WARN + 丢弃；请求侧 `require_approval≠never` 降级 never + WARN
  - `headers` 仅提取 `Authorization: Bearer`；`connector_id`/`tunnel_id` fail-fast
- **tool_choice `allowed_tools`**：仅 `auto`/`required` 映射；条目按 type/namespace/name 精确匹配已声明工具；与 structured output 组合或含 hosted/MCP 条目时 fail-fast。
- **无等价历史 item**（`file_search_call` / `computer_call*` / `image_generation_call` / `program*` / `item_reference` / `additional_tools`）：WARN + 丢弃，不进 system context。
- **无等价请求参数**：`background` / `conversation` / `moderation` / `top_logprobs` / `prompt_cache_*` / `safety_identifier` / deprecated `user` 等按 WARN + 忽略；`service_tier` 非空时 WARN 且不透传。

完整状态见[协议覆盖矩阵](docs/protocol-coverage.md)。

## 设计文档

- [初始设计](docs/superpowers/specs/2026-07-14-codex-api-gateway-design.md)
- [故障转移重构设计](docs/superpowers/specs/2026-07-15-failover-redesign-design.md)
- [SDK 迁移设计](docs/superpowers/specs/2026-07-15-sdk-migration-design.md)
- [协议覆盖矩阵](docs/protocol-coverage.md)
- [logo 设计哲学](docs/design/logo-philosophy.md)
